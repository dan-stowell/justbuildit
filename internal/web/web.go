// Package web serves the justbuildit HTTP API, the agent guide, and MCP.
package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"golang.org/x/time/rate"
)

// Options configures the web server. Zero values get defaults.
type Options struct {
	Service *build.Service
	// PublicURL is the externally visible base URL (e.g.
	// https://justbuildit.example.com). If empty it is derived per request.
	PublicURL string
	// ClientIPHeader, if set, names a header (like X-Forwarded-For) set by a
	// trusted proxy; its last entry is used as the client IP.
	ClientIPHeader string
	// MaxUploadBytes caps request bodies (default 50MB).
	MaxUploadBytes int64
	// SubmitRate and SubmitBurst limit build submissions per client
	// (default 1 per 6s, burst 10).
	SubmitRate  rate.Limit
	SubmitBurst int
	// DefaultWait and MaxWait bound how long POST /v1/builds waits
	// (default 45s, max 600s). MaxMCPWait bounds MCP waits (default 50s,
	// since MCP clients often time out tool calls after about 60s).
	DefaultWait time.Duration
	MaxWait     time.Duration
	MaxMCPWait  time.Duration
}

// Server is the HTTP front end.
type Server struct {
	opts    Options
	svc     *build.Service
	limiter *limiter
	mux     *http.ServeMux
}

// Internal headers the server sets on MCP requests (overwriting anything the
// client sent) so tool handlers know the client and the base URL.
const (
	hdrClient  = "X-Justbuildit-Internal-Client"
	hdrBaseURL = "X-Justbuildit-Internal-Base-Url"
)

// New returns a Server.
func New(opts Options) *Server {
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = 50 << 20
	}
	if opts.SubmitRate <= 0 {
		opts.SubmitRate = rate.Every(6 * time.Second)
	}
	if opts.SubmitBurst <= 0 {
		opts.SubmitBurst = 10
	}
	if opts.DefaultWait <= 0 {
		opts.DefaultWait = 45 * time.Second
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = 600 * time.Second
	}
	if opts.MaxMCPWait <= 0 {
		opts.MaxMCPWait = 50 * time.Second
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	s := &Server{
		opts:    opts,
		svc:     opts.Service,
		limiter: newLimiter(opts.SubmitRate, opts.SubmitBurst),
		mux:     http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /{$}", s.handleGuide)
	s.mux.HandleFunc("GET /llms.txt", s.handleGuide)
	s.mux.HandleFunc("POST /v1/builds", s.handleCreate)
	s.mux.HandleFunc("GET /v1/builds/{id}", s.handleGet)
	s.mux.HandleFunc("GET /v1/builds/{id}/log", s.handleLog)
	s.mux.HandleFunc("GET /v1/builds/{id}/artifacts.tar.gz", s.handleArtifactsTar)
	s.mux.HandleFunc("GET /v1/builds/{id}/artifacts/{path...}", s.handleArtifact)
	mcpHandler := s.mcpHandler()
	s.mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(hdrClient, s.clientIP(r))
		r.Header.Set(hdrBaseURL, s.baseURL(r))
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBody())
		mcpHandler.ServeHTTP(w, r)
	}))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// maxBody allows for JSON and base64 overhead on top of the upload cap.
func (s *Server) maxBody() int64 { return s.opts.MaxUploadBytes*4/3 + 1<<20 }

func (s *Server) clientIP(r *http.Request) string {
	if h := s.opts.ClientIPHeader; h != "" {
		if v := r.Header.Values(h); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) baseURL(r *http.Request) string {
	if s.opts.PublicURL != "" {
		return s.opts.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// decorate fills in the URLs of a result.
func decorate(r build.Result, base string) build.Result {
	prefix := base + "/v1/builds/" + r.ID
	r.LogURL = prefix + "/log"
	if len(r.Artifacts) > 0 {
		r.ArtifactsURL = prefix + "/artifacts.tar.gz"
	}
	return r
}

func artifactURL(base, id, p string) string {
	return base + "/v1/builds/" + id + "/artifacts/" + p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeServiceError maps errors from the build service to HTTP responses.
func writeServiceError(w http.ResponseWriter, err error) {
	var inv *build.InvalidError
	switch {
	case errors.As(err, &inv):
		writeError(w, http.StatusBadRequest, inv.Msg)
	case errors.Is(err, build.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found (builds are deleted some hours after they finish)")
	case errors.Is(err, build.ErrQueueFull), errors.Is(err, build.ErrClientLimit):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func parseWait(v string, def, max time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("wait must be a non-negative number of seconds")
	}
	return clampWait(n, def, max), nil
}

// clampWait turns a wait in seconds into a duration capped at max. Zero
// means def; to not wait at all, callers pass the default as 0.
func clampWait(n int, def, max time.Duration) time.Duration {
	d := time.Duration(n) * time.Second
	if n == 0 {
		d = def
	}
	if d > max {
		d = max
	}
	return d
}

func resultCode(r build.Result) int {
	if r.Status.Done() {
		return http.StatusOK
	}
	return http.StatusAccepted
}

type createRequest struct {
	build.Request
	WaitSeconds *int `json:"wait_seconds,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	client := s.clientIP(r)
	if !s.limiter.allow(client) {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, "too many build submissions; slow down")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody()))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large or unreadable (upload limit %d MB)", s.opts.MaxUploadBytes>>20))
		return
	}
	var req createRequest
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isGzip := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	switch {
	case ct == "application/json" || (!isGzip && bytes.HasPrefix(bytes.TrimSpace(body), []byte("{"))):
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	case isGzip:
		if int64(len(body)) > s.opts.MaxUploadBytes {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("tarball exceeds the upload limit of %d MB", s.opts.MaxUploadBytes>>20))
			return
		}
		q := r.URL.Query()
		req.Image = q.Get("image")
		req.Toolchain = q.Get("toolchain")
		req.Command = q.Get("command")
		req.Artifacts = q["artifact"]
		for _, a := range strings.Split(q.Get("artifacts"), ",") {
			if a = strings.TrimSpace(a); a != "" {
				req.Artifacts = append(req.Artifacts, a)
			}
		}
		if v := q.Get("timeout_seconds"); v != "" {
			if req.TimeoutSeconds, err = strconv.Atoi(v); err != nil {
				writeError(w, http.StatusBadRequest, "timeout_seconds must be a number")
				return
			}
		}
		if v := q.Get("wait_seconds"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeError(w, http.StatusBadRequest, "wait_seconds must be a number")
				return
			}
			req.WaitSeconds = &n
		}
		req.Source = body
	default:
		writeError(w, http.StatusBadRequest, "send a JSON body (Content-Type: application/json) or a gzipped tarball with query parameters; see "+s.baseURL(r)+"/llms.txt")
		return
	}

	wait := s.opts.DefaultWait
	if req.WaitSeconds != nil {
		if *req.WaitSeconds < 0 {
			writeError(w, http.StatusBadRequest, "wait_seconds must be non-negative")
			return
		}
		wait = min(time.Duration(*req.WaitSeconds)*time.Second, s.opts.MaxWait)
	}
	req.Client = client
	res, err := s.svc.Build(r.Context(), req.Request, wait)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, resultCode(res), decorate(res, s.baseURL(r)))
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("wait")
	if v == "" {
		v = r.URL.Query().Get("wait_seconds")
	}
	wait, err := parseWait(v, 0, s.opts.MaxWait)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.svc.Get(r.Context(), r.PathValue("id"), wait)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, resultCode(res), decorate(res, s.baseURL(r)))
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.LogPath(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeServiceError(w, build.ErrNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.Copy(w, f)
}

func (s *Server) handleArtifactsTar(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.ArtifactsPath(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no artifacts (the build is unknown, still running, or matched no artifact patterns)")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="artifacts.tar.gz"`)
	http.ServeFile(w, r, p)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("path")
	rc, size, err := s.svc.OpenArtifact(r.PathValue("id"), name)
	if err != nil {
		if errors.Is(err, build.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("artifact %q not found; the build result lists available artifacts", name))
			return
		}
		writeServiceError(w, err)
		return
	}
	defer rc.Close()
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	io.Copy(w, rc)
}
