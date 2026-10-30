package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BuildArgs are the arguments of the MCP build tool.
type BuildArgs struct {
	Command        string            `json:"command" jsonschema:"shell command, run with sh -c in /work (the workspace root)"`
	Toolchain      string            `json:"toolchain,omitempty" jsonschema:"short toolchain name (go, node, python, rust, c, cpp, java); ignored if image is set"`
	Image          string            `json:"image,omitempty" jsonschema:"any public docker image, e.g. golang:1.25 or ubuntu:24.04"`
	Files          map[string]string `json:"files,omitempty" jsonschema:"workspace files as relative path -> text content"`
	FilesBase64    map[string]string `json:"files_base64,omitempty" jsonschema:"binary workspace files as relative path -> base64 content"`
	Artifacts      []string          `json:"artifacts,omitempty" jsonschema:"glob patterns, relative to /work, of output files to keep"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"build timeout in seconds (the tool description gives the default and maximum)"`
	WaitSeconds    *int              `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the build before returning its current state (the tool description gives the default and maximum); call get_build to keep waiting"`
}

// GetBuildArgs are the arguments of the MCP get_build tool.
type GetBuildArgs struct {
	ID          string `json:"id" jsonschema:"build id returned by build"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"wait up to this long for the build to finish (the tool description gives the maximum); 0 or unset returns immediately"`
}

// ReadArtifactArgs are the arguments of the MCP read_artifact tool.
type ReadArtifactArgs struct {
	ID   string `json:"id" jsonschema:"build id"`
	Path string `json:"path" jsonschema:"artifact path as listed in the build result"`
}

// ArtifactContent is the result of read_artifact.
type ArtifactContent struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Text is set for small UTF-8 artifacts.
	Text string `json:"text,omitempty"`
	// URL downloads the artifact with plain HTTP GET.
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

func (s *Server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "justbuildit", Version: "1"}, &mcp.ServerOptions{
		Instructions: "justbuildit runs builds and tests in a fresh sandboxed container so you do not need to install toolchains. " +
			"Call build with your source files and a shell command; it returns the exit code, the tail of the log, and artifacts. " +
			"If the result status is queued or running, call get_build with wait_seconds until it is done.",
	})
	cfg := s.svc.Config()
	mcp.AddTool(srv, &mcp.Tool{
		Name: "build",
		Description: "Run a shell command in a fresh sandboxed container with the given files in /work. " +
			"Returns status (queued, running, succeeded, failed, timed_out, error), exit_code, log_tail and artifacts. " +
			"The container has public internet access, so it can download dependencies. Nothing persists between builds. " +
			fmt.Sprintf("timeout_seconds defaults to %d (max %d). wait_seconds defaults to %d, which is also the max. ",
				int(s.svc.DefaultTimeout().Seconds()), int(cfg.MaxTimeout.Seconds()), int(maxMCPWait.Seconds())) +
			fmt.Sprintf("The files may total at most %d MB and %d files.", build.MaxSourceBytes>>20, build.MaxSourceFiles),
	}, s.mcpBuild)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_build",
		Description: fmt.Sprintf("Get the state of a build, optionally waiting up to wait_seconds (max %d) for it to finish.", int(maxMCPWait.Seconds())),
	}, s.mcpGetBuild)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_artifact",
		Description: fmt.Sprintf("Read one artifact of a finished build. Text files up to %d KB are returned inline; otherwise a download URL.", maxInlineArtifact>>10),
	}, s.mcpReadArtifact)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: maxBody,
	})
}

func (s *Server) mcpBuild(ctx context.Context, req *mcp.CallToolRequest, args BuildArgs) (*mcp.CallToolResult, build.Result, error) {
	client := req.Extra.Header.Get(hdrClient)
	wait, err := waitFor(args.WaitSeconds, maxMCPWait, maxMCPWait)
	if err != nil {
		return nil, build.Result{}, err
	}
	done, err := s.startWait(client, wait)
	if err != nil {
		return nil, build.Result{}, err
	}
	defer done()
	res, err := s.svc.Build(ctx, build.Request{
		Image:          args.Image,
		Toolchain:      args.Toolchain,
		Command:        args.Command,
		Files:          args.Files,
		FilesBase64:    args.FilesBase64,
		Artifacts:      args.Artifacts,
		TimeoutSeconds: args.TimeoutSeconds,
		Client:         client,
	}, wait)
	if err != nil {
		return nil, build.Result{}, err
	}
	return nil, decorate(res, req.Extra.Header.Get(hdrBaseURL)), nil
}

func (s *Server) mcpGetBuild(ctx context.Context, req *mcp.CallToolRequest, args GetBuildArgs) (*mcp.CallToolResult, build.Result, error) {
	wait, err := waitFor(args.WaitSeconds, 0, maxMCPWait)
	if err != nil {
		return nil, build.Result{}, err
	}
	done, err := s.startWait(req.Extra.Header.Get(hdrClient), wait)
	if err != nil {
		return nil, build.Result{}, err
	}
	defer done()
	res, err := s.svc.Get(ctx, args.ID, wait)
	if err != nil {
		return nil, build.Result{}, err
	}
	return nil, decorate(res, req.Extra.Header.Get(hdrBaseURL)), nil
}

func (s *Server) mcpReadArtifact(ctx context.Context, req *mcp.CallToolRequest, args ReadArtifactArgs) (*mcp.CallToolResult, ArtifactContent, error) {
	done, err := s.startArtifactRead()
	if err != nil {
		return nil, ArtifactContent{}, err
	}
	defer done()
	rc, size, err := s.svc.OpenArtifact(args.ID, args.Path)
	if err != nil {
		return nil, ArtifactContent{}, err
	}
	defer rc.Close()
	out := ArtifactContent{
		Path: args.Path,
		Size: size,
		URL:  artifactURL(req.Extra.Header.Get(hdrBaseURL), args.ID, args.Path),
	}
	if size > maxInlineArtifact {
		out.Note = "too large to return inline; download it from url"
		return nil, out, nil
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxInlineArtifact+1))
	if err != nil {
		return nil, ArtifactContent{}, err
	}
	if !utf8.Valid(data) {
		out.Note = "binary file; download it from url"
		return nil, out, nil
	}
	out.Text = string(data)
	return nil, out, nil
}
