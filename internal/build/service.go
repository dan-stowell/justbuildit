package build

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Status is the lifecycle state of a build.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusTimedOut  Status = "timed_out"
	StatusError     Status = "error"
)

// Done reports whether s is a final state.
func (s Status) Done() bool {
	return s != StatusQueued && s != StatusRunning
}

// Limits that are not configurable.
const (
	// MaxSourceBytes caps a build's workspace: the uploaded tarball, or the
	// decoded content of files and files_base64.
	MaxSourceBytes = 50 << 20
	// MaxSourceFiles caps the number of entries in files plus files_base64.
	MaxSourceFiles = 10000
	// MaxPathLen and MaxPathDepth bound each path in files and files_base64.
	MaxPathLen   = 1024
	MaxPathDepth = 64
	// MaxArtifactsBytes caps the artifacts tarball a runner may produce.
	MaxArtifactsBytes = 500 << 20
	// MaxLogBytes caps the log kept on disk for each build.
	MaxLogBytes = 4 << 20
	// TailLines is the number of log lines in Result.LogTail.
	TailLines = 200

	defaultTimeout      = 10 * time.Minute
	maxCommandLen       = 64 << 10
	maxImageLen         = 256
	maxArtifactPatterns = 100
	// maxRetained caps finished builds kept; the oldest are deleted first.
	maxRetained = 2000
)

// The errors below are worded for the agents that call the service, so
// front ends can pass them on as they are.
var (
	// ErrNotFound means no such build.
	ErrNotFound = errors.New("build not found: the id is unknown, or the build was deleted (builds are kept for a few hours after they finish)")
	// ErrArtifactNotFound means a build has no such artifact (yet).
	ErrArtifactNotFound = errors.New("artifact not found: the build is unknown or not finished, or it has no artifact at that path (the build result lists its artifacts)")
	// ErrQueueFull means the service has too many builds waiting.
	ErrQueueFull = errors.New("the build queue is full; retry in about 30 seconds")
	// ErrClientLimit means this client already has too many builds queued or running.
	ErrClientLimit = errors.New("too many builds queued or running for this client; wait for one to finish, then retry")
	// ErrTooLarge means the workspace exceeds MaxSourceBytes.
	ErrTooLarge = fmt.Errorf("workspace too large: the tarball, or the decoded files and files_base64, may total at most %d MB", MaxSourceBytes>>20)
)

// InvalidError is a problem with the request itself.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return "invalid request: " + e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// Request describes a build to run.
type Request struct {
	Image          string            `json:"image,omitempty"`
	Toolchain      string            `json:"toolchain,omitempty"`
	Command        string            `json:"command"`
	Files          map[string]string `json:"files,omitempty"`
	FilesBase64    map[string]string `json:"files_base64,omitempty"`
	Artifacts      []string          `json:"artifacts,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`

	// SourceFile is the path of a gzipped tar of the workspace, used instead
	// of Files. Submit moves it into the build's directory, so it must be on
	// the data directory's filesystem; NewUploadFile creates such files.
	SourceFile string `json:"-"`
	// Client identifies the caller (an IP address) for per-client limits.
	Client string `json:"-"`
}

// Result is the externally visible state of a build.
type Result struct {
	ID              string     `json:"id"`
	Status          Status     `json:"status"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
	Image           string     `json:"image"`
	Command         string     `json:"command"`
	CreatedAt       time.Time  `json:"created_at"`
	DurationSeconds float64    `json:"duration_seconds"`
	LogTail         string     `json:"log_tail"`
	LogURL          string     `json:"log_url,omitempty"`
	Artifacts       []Artifact `json:"artifacts,omitempty"`
	ArtifactsURL    string     `json:"artifacts_url,omitempty"`
}

// Config configures a Service. Zero values get defaults.
type Config struct {
	DataDir       string
	Run           RunFunc
	MaxConcurrent int           // builds running at once (default 4)
	MaxQueue      int           // builds waiting to run (default 32)
	MaxPerClient  int           // builds queued or running per client (default 2)
	MaxTimeout    time.Duration // max build timeout (default 30m)
	Retention     time.Duration // how long finished builds are kept (default 6h)
	Logger        *slog.Logger
}

func (c *Config) setDefaults() {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.MaxConcurrent, 4)
	def(&c.MaxQueue, 32)
	def(&c.MaxPerClient, 2)
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = 30 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 6 * time.Hour
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Service queues builds, runs them with a RunFunc, and indexes the results.
type Service struct {
	cfg  Config
	sem  chan struct{}
	base context.Context
	stop context.CancelFunc

	mu      sync.Mutex
	jobs    map[string]*job
	queued  int
	clients map[string]int // queued+running per client
}

type job struct {
	id      string
	client  string
	image   string
	command string
	created time.Time
	root    string // DataDir/<id>
	log     *logWriter
	done    chan struct{}

	// Guarded by Service.mu.
	status    Status
	started   time.Time
	finished  time.Time
	outcome   Outcome
	err       string
	artifacts []Artifact
}

// New creates a Service. Its data directory is created if needed.
func New(cfg Config) (*Service, error) {
	cfg.setDefaults()
	if cfg.Run == nil {
		return nil, errors.New("build: Config.Run is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("build: Config.DataDir is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	// The index is in memory, so build directories and uploads from a
	// previous run are unreachable; remove them.
	if entries, err := os.ReadDir(cfg.DataDir); err == nil {
		for _, e := range entries {
			if idRE.MatchString(e.Name()) {
				os.RemoveAll(filepath.Join(cfg.DataDir, e.Name()))
			}
		}
	}
	uploads := filepath.Join(cfg.DataDir, "uploads")
	os.RemoveAll(uploads)
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		return nil, err
	}
	base, stop := context.WithCancel(context.Background())
	return &Service{
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		base:    base,
		stop:    stop,
		jobs:    map[string]*job{},
		clients: map[string]int{},
	}, nil
}

// Close cancels running builds. It does not wait for them.
func (s *Service) Close() { s.stop() }

// Config returns the effective configuration.
func (s *Service) Config() Config { return s.cfg }

// DefaultTimeout is the build timeout used when a request gives none.
func (s *Service) DefaultTimeout() time.Duration { return min(defaultTimeout, s.cfg.MaxTimeout) }

// NewUploadFile creates a temporary file in the data directory, for a request
// body that may become Request.SourceFile. The caller removes it if Submit
// does not take it.
func (s *Service) NewUploadFile() (*os.File, error) {
	return os.CreateTemp(filepath.Join(s.cfg.DataDir, "uploads"), "body-*")
}

var idRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Submit validates req and enqueues it, returning the new build's ID.
func (s *Service) Submit(req Request) (string, error) {
	if strings.TrimSpace(req.Command) == "" {
		return "", invalid("command is required (it runs with sh -c in /work)")
	}
	if len(req.Command) > maxCommandLen {
		return "", invalid("command is longer than %d KB; put long scripts in a file in the workspace", maxCommandLen>>10)
	}
	if len(req.Image) > maxImageLen {
		return "", invalid("image is longer than %d characters", maxImageLen)
	}
	image, err := ResolveImage(req.Image, req.Toolchain)
	if err != nil {
		return "", invalid("%v", err)
	}
	timeout := s.DefaultTimeout()
	if req.TimeoutSeconds < 0 {
		return "", invalid("timeout_seconds must be positive")
	}
	if req.TimeoutSeconds > 0 {
		if req.TimeoutSeconds > int(s.cfg.MaxTimeout/time.Second) {
			return "", invalid("timeout_seconds %d exceeds the maximum of %d", req.TimeoutSeconds, int(s.cfg.MaxTimeout/time.Second))
		}
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	if len(req.Artifacts) > maxArtifactPatterns {
		return "", invalid("at most %d artifact patterns are allowed", maxArtifactPatterns)
	}
	var globs []string
	for _, g := range req.Artifacts {
		if len(g) > MaxPathLen {
			return "", invalid("artifact pattern is longer than %d characters", MaxPathLen)
		}
		c, err := CleanPath(strings.TrimPrefix(g, "./"))
		if err != nil {
			return "", invalid("artifact pattern: %v", err)
		}
		globs = append(globs, c)
	}
	hasFiles := len(req.Files) > 0 || len(req.FilesBase64) > 0
	if req.SourceFile != "" {
		if hasFiles {
			return "", invalid("send either files or a tarball body, not both")
		}
		fi, err := os.Stat(req.SourceFile)
		if err != nil {
			return "", err
		}
		if fi.Size() > MaxSourceBytes {
			return "", ErrTooLarge
		}
	}

	j := &job{
		id:      newID(),
		client:  req.Client,
		image:   image,
		command: req.Command,
		created: time.Now(),
		done:    make(chan struct{}),
		status:  StatusQueued,
	}
	j.root = filepath.Join(s.cfg.DataDir, j.id)
	spec := Spec{
		ID:        j.id,
		Image:     image,
		Command:   req.Command,
		Artifacts: globs,
		Timeout:   timeout,
		Dir:       filepath.Join(j.root, "work"),
	}
	if req.SourceFile != "" || hasFiles {
		spec.SourceFile = filepath.Join(j.root, "source.tar.gz")
	}

	// Reserve the queue and client slots before the (possibly slow) work of
	// writing the source.
	s.mu.Lock()
	if s.queued >= s.cfg.MaxQueue {
		s.mu.Unlock()
		return "", ErrQueueFull
	}
	if s.clients[req.Client] >= s.cfg.MaxPerClient {
		s.mu.Unlock()
		return "", ErrClientLimit
	}
	s.queued++
	s.clients[req.Client]++
	s.mu.Unlock()

	logFile, err := s.prepare(j, spec, req)
	if err != nil {
		os.RemoveAll(j.root)
		s.mu.Lock()
		s.queued--
		s.releaseClient(j.client)
		s.mu.Unlock()
		return "", err
	}
	s.mu.Lock()
	s.jobs[j.id] = j
	s.mu.Unlock()
	go s.run(j, spec, logFile)
	return j.id, nil
}

// releaseClient drops one queued or running build from a client's count.
// It is called with s.mu held.
func (s *Service) releaseClient(client string) {
	s.clients[client]--
	if s.clients[client] <= 0 {
		delete(s.clients, client)
	}
}

// prepare creates the build's directories, source tarball and log file.
func (s *Service) prepare(j *job, spec Spec, req Request) (*os.File, error) {
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	switch {
	case req.SourceFile != "":
		if err := os.Rename(req.SourceFile, spec.SourceFile); err != nil {
			return nil, err
		}
	case spec.SourceFile != "":
		f, err := os.Create(spec.SourceFile)
		if err != nil {
			return nil, err
		}
		err = TarFromFiles(f, req.Files, req.FilesBase64)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	f, err := os.Create(filepath.Join(j.root, "build.log"))
	if err != nil {
		return nil, err
	}
	j.log = newLogWriter(f, MaxLogBytes)
	return f, nil
}

func (s *Service) run(j *job, spec Spec, logFile *os.File) {
	var (
		outcome Outcome
		runErr  error
	)
	select {
	case s.sem <- struct{}{}:
	case <-s.base.Done():
		runErr = errors.New("service shutting down")
	}
	s.mu.Lock()
	s.queued--
	if runErr == nil {
		j.status = StatusRunning
		j.started = time.Now()
	}
	s.mu.Unlock()

	if runErr == nil {
		outcome, runErr = s.execute(spec, j.log)
		<-s.sem
	}
	logFile.Close()
	var arts []Artifact
	if runErr == nil {
		arts, runErr = s.listArtifacts(spec)
	}

	s.mu.Lock()
	j.finished = time.Now()
	if j.started.IsZero() {
		j.started = j.finished
	}
	j.outcome = outcome
	j.artifacts = arts
	switch {
	case runErr != nil:
		j.status = StatusError
		j.err = runErr.Error()
		s.cfg.Logger.Error("build error", "build", j.id, "err", runErr)
	case outcome.TimedOut:
		j.status = StatusTimedOut
	case outcome.ExitCode == 0:
		j.status = StatusSucceeded
	default:
		j.status = StatusFailed
	}
	s.releaseClient(j.client)
	evicted := s.evictLocked()
	s.mu.Unlock()
	close(j.done)
	if evicted != nil {
		s.remove(evicted)
	}
}

// execute calls the RunFunc, turning a panic into an error so the build's
// slots are still released.
func (s *Service) execute(spec Spec, log io.Writer) (o Outcome, err error) {
	// The runner enforces spec.Timeout; this is a backstop.
	ctx, cancel := context.WithTimeout(s.base, spec.Timeout+2*time.Minute)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("build runner panicked", "build", spec.ID, "panic", r, "stack", string(debug.Stack()))
			o, err = Outcome{}, fmt.Errorf("internal error: the build runner crashed: %v", r)
		}
	}()
	return s.cfg.Run(ctx, spec, log)
}

// listArtifacts lists a finished build's artifacts. An oversized tarball is
// deleted and reported as an error.
func (s *Service) listArtifacts(spec Spec) ([]Artifact, error) {
	p := filepath.Join(spec.Dir, ArtifactsFile)
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Size() > MaxArtifactsBytes {
		os.Remove(p)
		return nil, fmt.Errorf("the artifacts are %d MB compressed, over the %d MB limit; use narrower artifact patterns", fi.Size()>>20, MaxArtifactsBytes>>20)
	}
	arts, err := ListArtifacts(p)
	if err != nil {
		s.cfg.Logger.Warn("listing artifacts", "build", spec.ID, "err", err)
	}
	return arts, nil
}

// evictLocked drops the oldest finished build from the index when more than
// maxRetained are kept, and returns it so the caller can delete its files.
// It is called with s.mu held.
func (s *Service) evictLocked() *job {
	var (
		n      int
		oldest *job
	)
	for _, j := range s.jobs {
		if j.status.Done() {
			n++
			if oldest == nil || j.finished.Before(oldest.finished) {
				oldest = j
			}
		}
	}
	if n <= maxRetained {
		return nil
	}
	delete(s.jobs, oldest.id)
	return oldest
}

func (s *Service) remove(j *job) {
	if err := os.RemoveAll(j.root); err != nil {
		s.cfg.Logger.Warn("removing build", "build", j.id, "err", err)
	}
}

// Build submits req and waits up to wait for it to finish.
func (s *Service) Build(ctx context.Context, req Request, wait time.Duration) (Result, error) {
	id, err := s.Submit(req)
	if err != nil {
		return Result{}, err
	}
	return s.Get(ctx, id, wait)
}

// Get returns the state of a build, first waiting up to wait for it to
// finish. It returns the current state when the wait or ctx runs out.
func (s *Service) Get(ctx context.Context, id string, wait time.Duration) (Result, error) {
	j := s.lookup(id)
	if j == nil {
		return Result{}, ErrNotFound
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-j.done:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	return s.result(j), nil
}

func (s *Service) lookup(id string) *job {
	if !idRE.MatchString(id) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Service) result(j *job) Result {
	s.mu.Lock()
	r := Result{
		ID:        j.id,
		Status:    j.status,
		Error:     j.err,
		Image:     j.image,
		Command:   j.command,
		CreatedAt: j.created,
		Artifacts: j.artifacts,
	}
	switch {
	case !j.finished.IsZero():
		r.DurationSeconds = j.finished.Sub(j.started).Seconds()
	case !j.started.IsZero():
		r.DurationSeconds = time.Since(j.started).Seconds()
	}
	if j.status == StatusSucceeded || j.status == StatusFailed {
		code := j.outcome.ExitCode
		r.ExitCode = &code
	}
	lw := j.log
	s.mu.Unlock()
	if lw != nil {
		r.LogTail = lw.Tail(TailLines)
	}
	return r
}

// LogPath returns the path of a build's log file.
func (s *Service) LogPath(id string) (string, error) {
	j := s.lookup(id)
	if j == nil {
		return "", ErrNotFound
	}
	return filepath.Join(j.root, "build.log"), nil
}

// ArtifactsPath returns the path of a finished build's artifacts tarball, or
// ErrArtifactNotFound if the build is unknown, unfinished, or has no artifacts.
func (s *Service) ArtifactsPath(id string) (string, error) {
	j := s.lookup(id)
	if j == nil {
		return "", ErrArtifactNotFound
	}
	select {
	case <-j.done:
	default:
		return "", ErrArtifactNotFound
	}
	p := filepath.Join(j.root, "work", ArtifactsFile)
	if _, err := os.Stat(p); err != nil {
		return "", ErrArtifactNotFound
	}
	return p, nil
}

// OpenArtifact streams one file out of a finished build's artifacts tarball.
func (s *Service) OpenArtifact(id, name string) (io.ReadCloser, int64, error) {
	p, err := s.ArtifactsPath(id)
	if err != nil {
		return nil, 0, err
	}
	return OpenArtifact(p, name)
}

// Sweep deletes finished builds that finished before now minus the retention.
func (s *Service) Sweep(now time.Time) {
	cutoff := now.Add(-s.cfg.Retention)
	var old []*job
	s.mu.Lock()
	for id, j := range s.jobs {
		if j.status.Done() && j.finished.Before(cutoff) {
			delete(s.jobs, id)
			old = append(old, j)
		}
	}
	s.mu.Unlock()
	for _, j := range old {
		s.remove(j)
	}
}

// RunSweeper calls Sweep every interval until ctx is done.
func (s *Service) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.Sweep(now)
		}
	}
}
