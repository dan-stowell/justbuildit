package web_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/dan-stowell/justbuildit/internal/fakerun"
	"github.com/dan-stowell/justbuildit/internal/web"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newMCPServer starts a server backed by fakerun and returns its URL.
func newMCPServer(t *testing.T, opts web.Options) string {
	t.Helper()
	release := make(chan struct{})
	svc, err := build.New(build.Config{DataDir: t.TempDir(), Run: fakerun.New(release)})
	if err != nil {
		t.Fatal(err)
	}
	opts.Service = svc
	srv := httptest.NewServer(web.New(opts))
	t.Cleanup(func() {
		close(release)
		srv.Close()
		svc.Close()
	})
	return srv.URL
}

func connect(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decoding %s: %v", name, b, err)
		}
	}
	return res
}

func TestMCPBuildRoundTrip(t *testing.T) {
	url := newMCPServer(t, web.Options{})
	cs := connect(t, url)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "build,get_build,read_artifact" {
		t.Errorf("tools = %s", got)
	}

	var res build.Result
	r := call(t, cs, "build", map[string]any{
		"command":   "make",
		"toolchain": "python",
		"files":     map[string]any{"out/report.txt": "all good\n", "main.py": "print(1)"},
		"artifacts": []string{"out/*"},
	}, &res)
	if r.IsError || res.Status != build.StatusSucceeded || res.Image != "python:3.13" || len(res.Artifacts) != 1 {
		t.Fatalf("build: %+v %+v", r, res)
	}
	if !strings.HasPrefix(res.LogURL, url+"/v1/builds/") {
		t.Errorf("log_url = %q", res.LogURL)
	}

	var got build.Result
	call(t, cs, "get_build", map[string]any{"id": res.ID}, &got)
	if got.ID != res.ID || got.Status != build.StatusSucceeded {
		t.Errorf("get_build = %+v", got)
	}

	var art web.ArtifactContent
	call(t, cs, "read_artifact", map[string]any{"id": res.ID, "path": "out/report.txt"}, &art)
	if art.Text != "all good\n" || art.URL != url+"/v1/builds/"+res.ID+"/artifacts/out/report.txt" {
		t.Errorf("read_artifact = %+v", art)
	}

	if r := call(t, cs, "read_artifact", map[string]any{"id": res.ID, "path": "main.py"}, nil); !r.IsError {
		t.Errorf("read_artifact of a path not in the tarball: want tool error, got %+v", r)
	}
	if r := call(t, cs, "build", map[string]any{"command": "x", "files": map[string]any{"../x": ""}}, nil); !r.IsError {
		t.Errorf("build with bad path: want tool error")
	}
}

func TestMCPBuildStillRunning(t *testing.T) {
	cs := connect(t, newMCPServer(t, web.Options{MaxMCPWait: 100 * time.Millisecond}))
	var res build.Result
	call(t, cs, "build", map[string]any{"command": "block"}, &res)
	if res.Status != build.StatusRunning && res.Status != build.StatusQueued {
		t.Fatalf("status = %q, want running or queued", res.Status)
	}
}
