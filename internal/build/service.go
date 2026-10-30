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

var (
	// ErrNotFound means no such build (or artifact).
	ErrNotFound = errors.New("not found")
	// ErrQueueFull means the service has too many builds waiting.
	ErrQueueFull = errors.New("build queue is full; retry later")
	// ErrClientLimit means this client already has too many builds queued or running.
	ErrClientLimit = errors.New("too many builds queued or running for this client; wait for one to finish")
)

// InvalidError is a problem with the request itself.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

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

	// Source is a raw gzipped tar of the workspace, used instead of Files.
	Source []byte `json:"-"`
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
	DataDir        string
	Run            RunFunc
	MaxConcurrent  int           // builds running at once (default 4)
	MaxQueue       int           // builds waiting to run (default 32)
	MaxPerClient   int           // builds queued or running per client (default 2)
	MaxLogBytes    int64         // log size kept on disk (default 10MB)
	TailLines      int           // lines in Result.LogTail (default 200)
	DefaultTimeout time.Duration // default build timeout (default 10m)
	MaxTimeout     time.Duration // max build timeout (default 30m)
	Retention      time.Duration // how long finished builds are kept (default 6h)
	Logger         *slog.Logger
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
	def(&c.TailLines, 200)
	if c.MaxLogBytes <= 0 {
		c.MaxLogBytes = 10 << 20
	}
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = 10 * time.Minute
	}
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
	// The index is in memory, so build directories from a previous run are
	// unreachable; remove them.
	if entries, err := os.ReadDir(cfg.DataDir); err == nil {
		for _, e := range entries {
			if idRE.MatchString(e.Name()) {
				os.RemoveAll(filepath.Join(cfg.DataDir, e.Name()))
			}
		}
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
	image, err := ResolveImage(req.Image, req.Toolchain)
	if err != nil {
		return "", invalid("%v", err)
	}
	timeout := s.cfg.DefaultTimeout
	if req.TimeoutSeconds < 0 {
		return "", invalid("timeout_seconds must be positive")
	}
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
		if timeout > s.cfg.MaxTimeout {
			return "", invalid("timeout_seconds %d exceeds the maximum of %d", req.TimeoutSeconds, int(s.cfg.MaxTimeout/time.Second))
		}
	}
	var globs []string
	for _, g := range req.Artifacts {
		c, err := CleanPath(strings.TrimPrefix(g, "./"))
		if err != nil {
			return "", invalid("artifact pattern: %v", err)
		}
		globs = append(globs, c)
	}
	source := req.Source
	if len(req.Files) > 0 || len(req.FilesBase64) > 0 {
		if len(source) > 0 {
			return "", invalid("send either files or a tarball body, not both")
		}
		source, err = TarFromFiles(req.Files, req.FilesBase64)
		if err != nil {
			return "", invalid("%v", err)
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
		Source:    source,
		Artifacts: globs,
		Timeout:   timeout,
		Dir:       filepath.Join(j.root, "work"),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queued >= s.cfg.MaxQueue {
		return "", ErrQueueFull
	}
	if s.clients[req.Client] >= s.cfg.MaxPerClient {
		return "", ErrClientLimit
	}
	logFile, err := s.prepare(j, spec)
	if err != nil {
		os.RemoveAll(j.root)
		return "", err
	}
	s.jobs[j.id] = j
	s.queued++
	s.clients[req.Client]++
	go s.run(j, spec, logFile)
	return j.id, nil
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
		// The runner enforces spec.Timeout; this is a backstop.
		ctx, cancel := context.WithTimeout(s.base, spec.Timeout+2*time.Minute)
		outcome, runErr = s.cfg.Run(ctx, spec, j.log)
		cancel()
		<-s.sem
	}
	logFile.Close()
	var arts []Artifact
	if runErr == nil {
		var err error
		arts, err = ListArtifacts(filepath.Join(spec.Dir, ArtifactsFile))
		if err != nil {
			s.cfg.Logger.Warn("listing artifacts", "build", j.id, "err", err)
		}
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
		s.cfg.Logger.Error("build infrastructure error", "build", j.id, "err", runErr)
	case outcome.TimedOut:
		j.status = StatusTimedOut
	case outcome.ExitCode == 0:
		j.status = StatusSucceeded
	default:
		j.status = StatusFailed
	}
	s.clients[j.client]--
	if s.clients[j.client] <= 0 {
		delete(s.clients, j.client)
	}
	s.mu.Unlock()
	close(j.done)
}

// prepare creates the build's directories and log file. It is called with
// s.mu held; creating a couple of directories is cheap enough.
func (s *Service) prepare(j *job, spec Spec) (*os.File, error) {
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(j.root, "build.log"))
	if err != nil {
		return nil, err
	}
	j.log = newLogWriter(f, s.cfg.MaxLogBytes)
	return f, nil
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
		r.LogTail = lw.Tail(s.cfg.TailLines)
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
// ErrNotFound if the build is unknown, unfinished, or has no artifacts.
func (s *Service) ArtifactsPath(id string) (string, error) {
	j := s.lookup(id)
	if j == nil {
		return "", ErrNotFound
	}
	select {
	case <-j.done:
	default:
		return "", ErrNotFound
	}
	p := filepath.Join(j.root, "work", ArtifactsFile)
	if _, err := os.Stat(p); err != nil {
		return "", ErrNotFound
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
		if err := os.RemoveAll(j.root); err != nil {
			s.cfg.Logger.Warn("removing build", "build", j.id, "err", err)
		}
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
