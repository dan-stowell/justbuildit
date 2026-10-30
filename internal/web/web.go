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
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
)

const (
	// defaultBuildWait and maxWait bound how long REST calls wait for a build.
	defaultBuildWait = 45 * time.Second
	maxWait          = 600 * time.Second
	// maxMCPWait bounds MCP waits, since MCP clients often time out tool
	// calls after about 60s.
	maxMCPWait = 50 * time.Second

	// maxBody caps request bodies, allowing for JSON and base64 overhead on
	// top of the workspace limit, which is enforced on the decoded content.
	maxBody = build.MaxSourceBytes*4/3 + 1<<20
	// uploadTimeout bounds reading a request body.
	uploadTimeout = 2 * time.Minute
	// maxUploads bounds request bodies being read and decoded at once.
	maxUploads = 8
	// uploadQueueWait bounds how long a request waits for an upload slot.
	uploadQueueWait = 5 * time.Second
	// smallMCPBody is the size up to which an MCP request skips the upload
	// limit; maxSmallMCP bounds such requests in flight at once.
	smallMCPBody = 1 << 20
	maxSmallMCP  = 128

	// maxWaiters and maxWaitersPerClient bound requests waiting on builds.
	maxWaiters          = 512
	maxWaitersPerClient = 16
	// maxArtifactReads bounds artifact downloads at once, and
	// artifactWriteTimeout how long each may take.
	maxArtifactReads     = 8
	artifactWriteTimeout = 10 * time.Minute
	// maxInlineArtifact is the largest artifact read_artifact returns inline.
	maxInlineArtifact = 256 << 10
)

var (
	errTooManyWaiters = errors.New("too many requests are waiting on builds (from this client or overall); retry in a few seconds, or poll with a shorter wait_seconds")
	errArtifactsBusy  = errors.New("too many artifact downloads in progress; retry in a few seconds")
	errUploadsBusy    = errors.New("the server is busy receiving other requests; retry in a few seconds")
	errBodyTooLarge   = fmt.Errorf("request body too large; the workspace may total at most %d MB", build.MaxSourceBytes>>20)
)

// Options configures the web server.
type Options struct {
	Service *build.Service
	// PublicURL is the externally visible base URL (e.g.
	// https://justbuildit.example.com). If empty it is derived per request.
	PublicURL string
	// ClientIPHeader, if set, names a header (like X-Forwarded-For) set by a
	// trusted proxy; its last entry is used as the client IP.
	ClientIPHeader string
}

// Server is the HTTP front end.
type Server struct {
	opts          Options
	svc           *build.Service
	mux           *http.ServeMux
	uploads       chan struct{}
	smallMCP      chan struct{}
	artifactReads chan struct{}

	waitMu    sync.Mutex
	waiting   int
	waitingBy map[string]int
}

// Internal headers the server sets on MCP requests (overwriting anything the
// client sent) so tool handlers know the client and the base URL.
const (
	hdrClient  = "X-Justbuildit-Internal-Client"
	hdrBaseURL = "X-Justbuildit-Internal-Base-Url"
)

// New returns a Server.
func New(opts Options) *Server {
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	s := &Server{
		opts:          opts,
		svc:           opts.Service,
		mux:           http.NewServeMux(),
		uploads:       make(chan struct{}, maxUploads),
		smallMCP:      make(chan struct{}, maxSmallMCP),
		artifactReads: make(chan struct{}, maxArtifactReads),
		waitingBy:     map[string]int{},
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
		r.Header.Set(hdrClient, s.clientKey(r))
		r.Header.Set(hdrBaseURL, s.baseURL(r))
		if r.Method == http.MethodPost {
			release, err := s.readMCPBody(w, r)
			if err != nil {
				writeError(w, err)
				return
			}
			defer release()
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// clientKey identifies the client for per-client limits: its IP address, or
// for IPv6 its /64, since one host commonly has a whole /64.
func (s *Server) clientKey(r *http.Request) string {
	ip := ""
	if h := s.opts.ClientIPHeader; h != "" {
		if v := r.Header.Values(h); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			ip = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if ip == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip = host
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		p, _ := addr.Prefix(64)
		return p.String()
	}
	return addr.String()
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
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return base + "/v1/builds/" + id + "/artifacts/" + strings.Join(segs, "/")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// errorStatus maps an error to an HTTP status. The errors' messages are
// already worded for the caller.
func errorStatus(err error) int {
	var inv *build.InvalidError
	switch {
	case errors.As(err, &inv):
		return http.StatusBadRequest
	case errors.Is(err, build.ErrNotFound), errors.Is(err, build.ErrArtifactNotFound):
		return http.StatusNotFound
	case errors.Is(err, build.ErrTooLarge), errors.Is(err, errBodyTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, build.ErrQueueFull), errors.Is(err, build.ErrClientLimit),
		errors.Is(err, errTooManyWaiters), errors.Is(err, errArtifactsBusy):
		return http.StatusTooManyRequests
	case errors.Is(err, errUploadsBusy):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, err error) {
	code := errorStatus(err)
	switch code {
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "30")
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func invalidf(format string, args ...any) error {
	return &build.InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// waitFor turns an optional wait_seconds into a duration: def when unset,
// capped at max.
func waitFor(n *int, def, max time.Duration) (time.Duration, error) {
	switch {
	case n == nil:
		return def, nil
	case *n < 0:
		return 0, invalidf("wait_seconds must be non-negative")
	case *n >= int(max/time.Second):
		return max, nil
	}
	return time.Duration(*n) * time.Second, nil
}

// startWait reserves a waiter slot for client if wait is positive. The
// returned func releases it.
func (s *Server) startWait(client string, wait time.Duration) (func(), error) {
	if wait <= 0 {
		return func() {}, nil
	}
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	if s.waiting >= maxWaiters || s.waitingBy[client] >= maxWaitersPerClient {
		return nil, errTooManyWaiters
	}
	s.waiting++
	s.waitingBy[client]++
	return func() {
		s.waitMu.Lock()
		defer s.waitMu.Unlock()
		s.waiting--
		if s.waitingBy[client]--; s.waitingBy[client] <= 0 {
			delete(s.waitingBy, client)
		}
	}, nil
}

// startArtifactRead reserves an artifact download slot. If w is not nil it
// also bounds the response write time, so a slow reader cannot hold the
// slot forever.
func (s *Server) startArtifactRead(w http.ResponseWriter) (func(), error) {
	select {
	case s.artifactReads <- struct{}{}:
	default:
		return nil, errArtifactsBusy
	}
	if w != nil {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(artifactWriteTimeout))
	}
	return func() { <-s.artifactReads }, nil
}

// startUpload waits up to uploadQueueWait for an upload slot and sets the
// body read deadline. The returned func releases the slot.
func (s *Server) startUpload(w http.ResponseWriter, r *http.Request) (func(), error) {
	t := time.NewTimer(uploadQueueWait)
	defer t.Stop()
	select {
	case s.uploads <- struct{}{}:
	case <-t.C:
		return nil, errUploadsBusy
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
	return func() { <-s.uploads }, nil
}

// bodyError explains a failure to read a request body.
func bodyError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errBodyTooLarge
	}
	return invalidf("reading request body: %v", err)
}

// readMCPBody reads an MCP request body into memory (the MCP SDK decodes it
// in memory anyway) within the upload deadline. Bodies over smallMCPBody
// hold an upload slot until the returned func is called, since the SDK keeps
// the decoded arguments for the whole tool call; smaller ones hold one of
// maxSmallMCP slots and are read with a smallMCPBody limit.
func (s *Server) readMCPBody(w http.ResponseWriter, r *http.Request) (func(), error) {
	var release func()
	limit := int64(maxBody)
	if r.ContentLength < 0 || r.ContentLength > smallMCPBody {
		var err error
		if release, err = s.startUpload(w, r); err != nil {
			return nil, err
		}
	} else {
		select {
		case s.smallMCP <- struct{}{}:
		default:
			return nil, errUploadsBusy
		}
		release = func() { <-s.smallMCP }
		limit = smallMCPBody
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		release()
		return nil, bodyError(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return release, nil
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
	client := s.clientKey(r)
	id, wait, done, err := s.create(w, r, client)
	if err != nil {
		writeError(w, err)
		return
	}
	defer done()
	res, err := s.svc.Get(r.Context(), id, wait)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, resultCode(res), decorate(res, s.baseURL(r)))
}

// create reads a build request, spooling the body to a file while holding an
// upload slot, and submits it. On success the caller must call done, which
// releases its waiter slot.
func (s *Server) create(w http.ResponseWriter, r *http.Request, client string) (id string, wait time.Duration, done func(), err error) {
	release, err := s.startUpload(w, r)
	if err != nil {
		return "", 0, nil, err
	}
	defer release()
	f, err := s.svc.NewUploadFile()
	if err != nil {
		return "", 0, nil, err
	}
	defer os.Remove(f.Name()) // a no-op once Submit has taken it
	defer f.Close()
	_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, maxBody))
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		return "", 0, nil, bodyError(err)
	}

	var req createRequest
	var magic [2]byte
	n, _ := f.ReadAt(magic[:], 0)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if n == 2 && magic == [2]byte{0x1f, 0x8b} && ct != "application/json" {
		if err := queryRequest(r.URL.Query(), &req); err != nil {
			return "", 0, nil, err
		}
		req.SourceFile = f.Name()
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", 0, nil, err
		}
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return "", 0, nil, invalidf("invalid JSON body: %v (send JSON, or a gzipped tarball with query parameters; see %s/llms.txt)", err, s.baseURL(r))
		}
	}

	if wait, err = waitFor(req.WaitSeconds, defaultBuildWait, maxWait); err != nil {
		return "", 0, nil, err
	}
	if done, err = s.startWait(client, wait); err != nil {
		return "", 0, nil, err
	}
	req.Client = client
	if id, err = s.svc.Submit(req.Request); err != nil {
		done()
		return "", 0, nil, err
	}
	return id, wait, done, nil
}

// queryRequest fills req from the query parameters of a tarball upload.
func queryRequest(q url.Values, req *createRequest) error {
	req.Image = q.Get("image")
	req.Toolchain = q.Get("toolchain")
	req.Command = q.Get("command")
	req.Artifacts = q["artifact"]
	for _, a := range strings.Split(q.Get("artifacts"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			req.Artifacts = append(req.Artifacts, a)
		}
	}
	timeout, err := queryInt(q, "timeout_seconds")
	if err != nil {
		return err
	}
	if timeout != nil {
		req.TimeoutSeconds = *timeout
	}
	req.WaitSeconds, err = queryInt(q, "wait_seconds")
	return err
}

func queryInt(q url.Values, name string) (*int, error) {
	v := q.Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, invalidf("%s must be a whole number", name)
	}
	return &n, nil
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	n, err := queryInt(r.URL.Query(), "wait_seconds")
	if err != nil {
		writeError(w, err)
		return
	}
	wait, err := waitFor(n, 0, maxWait)
	if err != nil {
		writeError(w, err)
		return
	}
	done, err := s.startWait(s.clientKey(r), wait)
	if err != nil {
		writeError(w, err)
		return
	}
	defer done()
	res, err := s.svc.Get(r.Context(), r.PathValue("id"), wait)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, resultCode(res), decorate(res, s.baseURL(r)))
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.LogPath(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeError(w, build.ErrNotFound)
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
		writeError(w, err)
		return
	}
	done, err := s.startArtifactRead(w)
	if err != nil {
		writeError(w, err)
		return
	}
	defer done()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="artifacts.tar.gz"`)
	http.ServeFile(w, r, p)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	done, err := s.startArtifactRead(w)
	if err != nil {
		writeError(w, err)
		return
	}
	defer done()
	name := r.PathValue("path")
	rc, size, err := s.svc.OpenArtifact(r.PathValue("id"), name)
	if err != nil {
		writeError(w, err)
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
