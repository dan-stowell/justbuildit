package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxInlineArtifact is the largest artifact read_artifact returns inline.
const maxInlineArtifact = 256 << 10

// BuildArgs are the arguments of the MCP build tool.
type BuildArgs struct {
	Command        string            `json:"command" jsonschema:"shell command, run with sh -c in /work (the workspace root)"`
	Toolchain      string            `json:"toolchain,omitempty" jsonschema:"short toolchain name (go, node, python, rust, c, cpp, java); ignored if image is set"`
	Image          string            `json:"image,omitempty" jsonschema:"any public docker image, e.g. golang:1.25 or ubuntu:24.04"`
	Files          map[string]string `json:"files,omitempty" jsonschema:"workspace files as relative path -> text content"`
	FilesBase64    map[string]string `json:"files_base64,omitempty" jsonschema:"binary workspace files as relative path -> base64 content"`
	Artifacts      []string          `json:"artifacts,omitempty" jsonschema:"glob patterns, relative to /work, of output files to keep"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" jsonschema:"build timeout in seconds (default 600, max 1800)"`
	WaitSeconds    int               `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the build before returning its current state (default and max 50); call get_build to keep waiting"`
}

// GetBuildArgs are the arguments of the MCP get_build tool.
type GetBuildArgs struct {
	ID          string `json:"id" jsonschema:"build id returned by build"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"wait up to this long (max 50) for the build to finish; 0 returns immediately"`
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
	// Text is set for UTF-8 artifacts up to 256KB.
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
	mcp.AddTool(srv, &mcp.Tool{
		Name: "build",
		Description: "Run a shell command in a fresh sandboxed container with the given files in /work. " +
			"Returns status (queued, running, succeeded, failed, timed_out, error), exit_code, log_tail and artifacts. " +
			"The container has public internet access, so it can download dependencies. Nothing persists between builds.",
	}, s.mcpBuild)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_build",
		Description: "Get the state of a build, optionally waiting for it to finish.",
	}, s.mcpGetBuild)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_artifact",
		Description: "Read one artifact of a finished build. Text files up to 256KB are returned inline; otherwise a download URL.",
	}, s.mcpReadArtifact)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: s.maxBody(),
	})
}

// toolError turns service errors into messages for the model.
func toolError(err error) error {
	var inv *build.InvalidError
	switch {
	case errors.As(err, &inv):
		return fmt.Errorf("invalid request: %s", inv.Msg)
	case errors.Is(err, build.ErrNotFound):
		return errors.New("not found (builds are deleted some hours after they finish)")
	case errors.Is(err, build.ErrQueueFull), errors.Is(err, build.ErrClientLimit):
		return fmt.Errorf("%v (retry in about 30 seconds)", err)
	}
	return err
}

func (s *Server) mcpBuild(ctx context.Context, req *mcp.CallToolRequest, args BuildArgs) (*mcp.CallToolResult, build.Result, error) {
	client := req.Extra.Header.Get(hdrClient)
	if !s.limiter.allow(client) {
		return nil, build.Result{}, errors.New("too many build submissions; slow down and retry in about 10 seconds")
	}
	if args.WaitSeconds < 0 {
		return nil, build.Result{}, errors.New("wait_seconds must be non-negative")
	}
	res, err := s.svc.Build(ctx, build.Request{
		Image:          args.Image,
		Toolchain:      args.Toolchain,
		Command:        args.Command,
		Files:          args.Files,
		FilesBase64:    args.FilesBase64,
		Artifacts:      args.Artifacts,
		TimeoutSeconds: args.TimeoutSeconds,
		Client:         client,
	}, clampWait(args.WaitSeconds, s.opts.MaxMCPWait, s.opts.MaxMCPWait))
	if err != nil {
		return nil, build.Result{}, toolError(err)
	}
	return nil, decorate(res, req.Extra.Header.Get(hdrBaseURL)), nil
}

func (s *Server) mcpGetBuild(ctx context.Context, req *mcp.CallToolRequest, args GetBuildArgs) (*mcp.CallToolResult, build.Result, error) {
	if args.WaitSeconds < 0 {
		return nil, build.Result{}, errors.New("wait_seconds must be non-negative")
	}
	res, err := s.svc.Get(ctx, args.ID, clampWait(args.WaitSeconds, 0, s.opts.MaxMCPWait))
	if err != nil {
		return nil, build.Result{}, toolError(err)
	}
	return nil, decorate(res, req.Extra.Header.Get(hdrBaseURL)), nil
}

func (s *Server) mcpReadArtifact(ctx context.Context, req *mcp.CallToolRequest, args ReadArtifactArgs) (*mcp.CallToolResult, ArtifactContent, error) {
	rc, size, err := s.svc.OpenArtifact(args.ID, args.Path)
	if err != nil {
		if errors.Is(err, build.ErrNotFound) {
			return nil, ArtifactContent{}, fmt.Errorf("artifact %q not found in build %s (unknown build, unfinished build, or path not in the build's artifacts list)", args.Path, args.ID)
		}
		return nil, ArtifactContent{}, toolError(err)
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
