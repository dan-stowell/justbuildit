package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dan-stowell/justbuildit/internal/build"
)

const guideTemplate = `# justbuildit

justbuildit runs your build or test command in a fresh, sandboxed Linux
container and gives you back the exit code, the log, and any output files.
Use it instead of installing compilers and toolchains yourself. No account or
API key is needed.

Each build: your files are unpacked into /work, then your command runs with
"sh -c" in /work as root. The container can reach the public internet (to
download dependencies) but nothing else. Nothing persists between builds:
send all the files the build needs every time.

## Quick start (curl)

    curl -sS {{BASE}}/v1/builds -H 'Content-Type: application/json' -d '{
      "toolchain": "go",
      "files": {
        "go.mod": "module hello\n\ngo 1.25\n",
        "main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n"
      },
      "command": "go build -o out/hello . && ./out/hello",
      "artifacts": ["out/*"]
    }'

The response is JSON:

    {
      "id": "3f0c...",
      "status": "succeeded",       // queued | running | succeeded | failed | timed_out | error
      "exit_code": 0,              // set when status is succeeded or failed
      "error": "",                 // set when status is error (a problem on our side, not your build)
      "duration_seconds": 4.2,
      "log_tail": "...",           // last {{TAIL}} lines of combined stdout+stderr
      "log_url": "{{BASE}}/v1/builds/3f0c.../log",
      "artifacts": [{"path": "out/hello", "size": 2215000}],
      "artifacts_url": "{{BASE}}/v1/builds/3f0c.../artifacts.tar.gz"
    }

POST waits up to wait_seconds (default 45, max 600) for the build to finish.
If status is still "queued" or "running" (HTTP 202), poll:

    curl -sS '{{BASE}}/v1/builds/ID?wait=60'

## Request fields (POST /v1/builds, JSON)

    command          required. Shell command, run with sh -c in /work.
    toolchain        optional short name, see the table below.
    image            optional. Any public docker image (e.g. "ubuntu:24.04",
                     "golang:1.25-alpine"). Overrides toolchain.
                     Default when neither is given: {{DEFAULT}}.
    files            {"relative/path": "text content", ...}
    files_base64     {"relative/path": "base64 content", ...} for binary files.
                     Paths must be relative and must not contain "..".
    artifacts        glob patterns relative to /work of files to keep, e.g.
                     ["out/*", "target/release/app"].
    timeout_seconds  default {{TIMEOUT}}, max {{MAXTIMEOUT}}.
    wait_seconds     default 45, max 600.

Larger workspaces: send a gzipped tarball as the body instead, with the other
fields as query parameters (repeat artifact= for several patterns):

    tar czf - . | curl -sS --data-binary @- -H 'Content-Type: application/gzip' \
      '{{BASE}}/v1/builds?toolchain=node&command=npm%20ci%20%26%26%20npm%20test&artifact=dist/*'

Uploads are limited to {{UPLOAD}} MB.

## Other endpoints

    GET {{BASE}}/v1/builds/ID?wait=N              build status (waits up to N s)
    GET {{BASE}}/v1/builds/ID/log                 full log (text, capped at {{LOG}} MB)
    GET {{BASE}}/v1/builds/ID/artifacts.tar.gz    all artifacts
    GET {{BASE}}/v1/builds/ID/artifacts/PATH      one artifact, e.g. .../artifacts/out/hello

## Toolchains

{{TOOLCHAINS}}
## MCP

Streamable HTTP MCP server at {{BASE}}/mcp with tools:

    build          same fields as the JSON request above; waits up to 50 s
    get_build      {"id": ..., "wait_seconds": N} (max 50)
    read_artifact  {"id": ..., "path": ...}; text up to 256KB inline, else a URL

For Claude Code:

    claude mcp add --transport http justbuildit {{BASE}}/mcp

## Limits

- Each build gets 2 CPUs and 4 GB of memory; files written in the container
  count against the memory limit.
- At most {{PERCLIENT}} builds queued or running per client; more get HTTP 429
  (retry after the Retry-After header). The service also returns 429 when
  its queue is full.
- Builds and their logs and artifacts are deleted after {{RETENTION}}.

## Tips

- Status "failed" means your command exited non-zero: read log_tail.
  Status "error" means the service could not run it: read "error".
- Install extra packages in the command, e.g.
  "apt-get update && apt-get install -y cmake && cmake ...".
- Chain steps with && so a failing step stops the build.
`

func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	cfg := s.svc.Config()
	var tc strings.Builder
	for _, t := range build.Toolchains() {
		fmt.Fprintf(&tc, "    %-8s %s\n", t.Name, t.Image)
	}
	text := strings.NewReplacer(
		"{{BASE}}", s.baseURL(r),
		"{{TAIL}}", fmt.Sprint(cfg.TailLines),
		"{{DEFAULT}}", build.DefaultImage,
		"{{UPLOAD}}", fmt.Sprint(s.opts.MaxUploadBytes>>20),
		"{{LOG}}", fmt.Sprint(cfg.MaxLogBytes>>20),
		"{{TOOLCHAINS}}", tc.String(),
		"{{PERCLIENT}}", fmt.Sprint(cfg.MaxPerClient),
		"{{TIMEOUT}}", fmt.Sprint(int(cfg.DefaultTimeout.Seconds())),
		"{{MAXTIMEOUT}}", fmt.Sprint(int(cfg.MaxTimeout.Seconds())),
		"{{RETENTION}}", cfg.Retention.String(),
	).Replace(guideTemplate)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(text))
}
