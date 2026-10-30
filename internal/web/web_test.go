package web_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/dan-stowell/justbuildit/internal/fakerun"
	"github.com/dan-stowell/justbuildit/internal/web"
)

type env struct {
	srv     *httptest.Server
	release chan struct{}
}

func newEnv(t *testing.T, cfg build.Config, opts web.Options) *env {
	t.Helper()
	e := &env{release: make(chan struct{})}
	cfg.DataDir = t.TempDir()
	cfg.Run = fakerun.New(e.release)
	svc, err := build.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts.Service = svc
	e.srv = httptest.NewServer(web.New(opts))
	t.Cleanup(func() {
		close(e.release)
		e.srv.Close()
		svc.Close()
	})
	return e
}

func (e *env) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(e.srv.URL+"/v1/builds", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return resp.StatusCode, m
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestBuildAndArtifacts(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	code, res := e.post(t, `{"command": "make", "files": {"src.c": "int main;", "out/app.txt": "built!"}, "artifacts": ["out/*"]}`)
	if code != http.StatusOK || res["status"] != "succeeded" || res["exit_code"] != 0.0 {
		t.Fatalf("POST = %d %v", code, res)
	}
	id := res["id"].(string)
	if want := e.srv.URL + "/v1/builds/" + id + "/artifacts.tar.gz"; res["artifacts_url"] != want {
		t.Errorf("artifacts_url = %v, want %v", res["artifacts_url"], want)
	}

	if code, body := get(t, e.srv.URL+"/v1/builds/"+id+"/artifacts/out/app.txt"); code != 200 || body != "built!" {
		t.Errorf("artifact = %d %q", code, body)
	}
	if code, _ := get(t, e.srv.URL+"/v1/builds/"+id+"/artifacts/src.c"); code != http.StatusNotFound {
		t.Errorf("artifact not in tar: status %d, want 404", code)
	}
	if code, _ := get(t, e.srv.URL+"/v1/builds/"+id+"/artifacts/out/../../../etc/passwd"); code != http.StatusNotFound {
		t.Errorf("traversal: status %d, want 404", code)
	}
	if code, body := get(t, e.srv.URL+"/v1/builds/"+id+"/log"); code != 200 || body != "running make\n" {
		t.Errorf("log = %d %q", code, body)
	}
	if code, body := get(t, e.srv.URL+"/v1/builds/"+id+"/artifacts.tar.gz"); code != 200 || len(body) == 0 {
		t.Errorf("artifacts.tar.gz = %d, %d bytes", code, len(body))
	}
	if code, _ := get(t, e.srv.URL+"/v1/builds/"+strings.Repeat("0", 32)); code != http.StatusNotFound {
		t.Errorf("unknown build: status %d, want 404", code)
	}
}

func TestTarballUpload(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	var src bytes.Buffer
	if err := build.TarFromFiles(&src, map[string]string{"out/x": "xx"}, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.srv.URL+"/v1/builds?command=make&toolchain=go&artifact=out/*", "application/gzip", &src)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res build.Result
	json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != 200 || res.Status != build.StatusSucceeded || res.Image != "golang:1.25" ||
		len(res.Artifacts) != 1 || res.Artifacts[0].Path != "out/x" {
		t.Fatalf("got %d %+v", resp.StatusCode, res)
	}
}

func TestWaitReturnsRunning(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	code, res := e.post(t, `{"command": "block", "wait_seconds": 0}`)
	if code != http.StatusAccepted || (res["status"] != "running" && res["status"] != "queued") {
		t.Fatalf("POST = %d %v, want 202 running/queued", code, res)
	}
	if _, ok := res["exit_code"]; ok {
		t.Errorf("exit_code present on unfinished build")
	}
	code, body := get(t, e.srv.URL+"/v1/builds/"+res["id"].(string)+"?wait_seconds=1")
	if code != http.StatusAccepted || !strings.Contains(body, `"running"`) {
		t.Errorf("GET wait_seconds=1 = %d %s, want 202 running", code, body)
	}
}

func TestPathValidation(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	for _, path := range []string{"/etc/passwd", "../x", "a/../../x"} {
		code, res := e.post(t, fmt.Sprintf(`{"command": "make", "files": {%q: "x"}}`, path))
		if code != http.StatusBadRequest || res["error"] == nil {
			t.Errorf("file %q: %d %v, want 400", path, code, res)
		}
	}
	if code, res := e.post(t, `{"command": "make", "bogus": 1}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d %v, want 400", code, res)
	}
}

func TestQueueFull(t *testing.T) {
	e := newEnv(t, build.Config{MaxConcurrent: 1, MaxQueue: 1, MaxPerClient: 10}, web.Options{})
	if code, res := e.post(t, `{"command": "block", "wait_seconds": 1}`); code != http.StatusAccepted || res["status"] != "running" {
		t.Fatalf("first: %d %v", code, res)
	}
	if code, res := e.post(t, `{"command": "block", "wait_seconds": 0}`); code != http.StatusAccepted || res["status"] != "queued" {
		t.Fatalf("second: %d %v", code, res)
	}
	resp, err := http.Post(e.srv.URL+"/v1/builds", "application/json", strings.NewReader(`{"command": "block", "wait_seconds": 0}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("third: %d (Retry-After %q), want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func (e *env) postFrom(t *testing.T, ip string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/builds", strings.NewReader(`{"command": "block", "wait_seconds": 0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "9.9.9.9, "+ip) // last entry is the trusted one
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPerClientCap(t *testing.T) {
	e := newEnv(t, build.Config{MaxPerClient: 2}, web.Options{ClientIPHeader: "X-Forwarded-For"})
	for i := 0; i < 2; i++ {
		if code := e.postFrom(t, "10.0.0.1"); code != http.StatusAccepted {
			t.Fatalf("build %d: status %d", i, code)
		}
	}
	if code := e.postFrom(t, "10.0.0.1"); code != http.StatusTooManyRequests {
		t.Errorf("third build from same IP: status %d, want 429", code)
	}
	if code := e.postFrom(t, "10.0.0.2"); code != http.StatusAccepted {
		t.Errorf("other IP: status %d, want 202", code)
	}
}

func TestGuide(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{PublicURL: "https://jbi.example"})
	for _, p := range []string{"/", "/llms.txt"} {
		code, body := get(t, e.srv.URL+p)
		if code != 200 || !strings.Contains(body, "https://jbi.example/v1/builds") || !strings.Contains(body, "golang:1.25") || strings.Contains(body, "{{") {
			t.Errorf("GET %s = %d:\n%s", p, code, body)
		}
	}
	if code, _ := get(t, e.srv.URL+"/nope"); code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", code)
	}
}

func TestIPv6ClientsKeyedBy64(t *testing.T) {
	e := newEnv(t, build.Config{MaxPerClient: 2}, web.Options{ClientIPHeader: "X-Forwarded-For"})
	for _, ip := range []string{"2001:db8::1", "2001:db8::2:3"} {
		if code := e.postFrom(t, ip); code != http.StatusAccepted {
			t.Fatalf("%s: status %d", ip, code)
		}
	}
	if code := e.postFrom(t, "2001:db8::ffff"); code != http.StatusTooManyRequests {
		t.Errorf("third build from the same /64: status %d, want 429", code)
	}
	if code := e.postFrom(t, "2001:db8:0:1::1"); code != http.StatusAccepted {
		t.Errorf("other /64: status %d, want 202", code)
	}
}

func TestWaiterCap(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	_, res := e.post(t, `{"command": "block", "wait_seconds": 0}`)
	url := e.srv.URL + "/v1/builds/" + res["id"].(string) + "?wait_seconds="
	for i := 0; i < 16; i++ {
		// Each holds a waiter slot until the build is released at cleanup,
		// retrying if a probe below briefly holds the slot it needs.
		go func() {
			for {
				resp, err := http.Get(url + "60")
				if err != nil {
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusTooManyRequests {
					return
				}
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, _ := get(t, url+"1")
		if code == http.StatusTooManyRequests {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiter over the per-client cap: status %d, want 429", code)
		}
	}
	if code, _ := get(t, url+"0"); code != http.StatusAccepted {
		t.Errorf("non-waiting GET: status %d, want 202", code)
	}
}

func TestArtifactURLEscaped(t *testing.T) {
	e := newEnv(t, build.Config{}, web.Options{})
	_, res := e.post(t, `{"command": "make", "files": {"out/a b#1?.txt": "hi"}, "artifacts": ["out/*"]}`)
	id := res["id"].(string)
	if code, body := get(t, e.srv.URL+"/v1/builds/"+id+"/artifacts/out/a%20b%231%3F.txt"); code != 200 || body != "hi" {
		t.Errorf("escaped artifact URL = %d %q", code, body)
	}
}
