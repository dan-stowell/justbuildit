package build_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
)

// blockingRunner returns a RunFunc that reports each started build on
// started and finishes it when release is closed.
func blockingRunner(started chan<- build.Spec, release <-chan struct{}) build.RunFunc {
	return func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
		fmt.Fprintln(log, "starting")
		started <- spec
		select {
		case <-release:
		case <-ctx.Done():
			return build.Outcome{}, ctx.Err()
		}
		return build.Outcome{ExitCode: 0}, nil
	}
}

func newService(t *testing.T, cfg build.Config) *build.Service {
	t.Helper()
	cfg.DataDir = t.TempDir()
	svc, err := build.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func TestWaitTimeoutReturnsRunning(t *testing.T) {
	started := make(chan build.Spec, 1)
	release := make(chan struct{})
	svc := newService(t, build.Config{Run: blockingRunner(started, release)})

	res, err := svc.Build(context.Background(), build.Request{Command: "make", Client: "a"}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != build.StatusRunning && res.Status != build.StatusQueued {
		t.Fatalf("status = %q, want running or queued", res.Status)
	}
	if res.ExitCode != nil {
		t.Errorf("exit code set on unfinished build")
	}
	<-started
	close(release)

	res, err = svc.Get(context.Background(), res.ID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != build.StatusSucceeded || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("got %+v, want succeeded with exit 0", res)
	}
	if !strings.Contains(res.LogTail, "starting") {
		t.Errorf("log tail %q missing runner output", res.LogTail)
	}
}

func TestSpecResolved(t *testing.T) {
	var got build.Spec
	svc := newService(t, build.Config{Run: func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
		got = spec
		return build.Outcome{ExitCode: 3}, nil
	}})
	res, err := svc.Build(context.Background(), build.Request{
		Toolchain:      "go",
		Command:        "go test ./...",
		Files:          map[string]string{"main.go": "package main"},
		Artifacts:      []string{"./out/*"},
		TimeoutSeconds: 30,
	}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != build.StatusFailed || *res.ExitCode != 3 {
		t.Fatalf("got %+v, want failed with exit 3", res)
	}
	if got.Image != "golang:1.25" || got.Timeout != 30*time.Second || len(got.Source) == 0 ||
		len(got.Artifacts) != 1 || got.Artifacts[0] != "out/*" || got.ID != res.ID {
		t.Errorf("spec = %+v", got)
	}
	if fi, err := os.Stat(got.Dir); err != nil || !fi.IsDir() {
		t.Errorf("spec.Dir %q not created: %v", got.Dir, err)
	}
}

func TestStatuses(t *testing.T) {
	for _, tc := range []struct {
		outcome build.Outcome
		err     error
		want    build.Status
	}{
		{build.Outcome{ExitCode: 0}, nil, build.StatusSucceeded},
		{build.Outcome{ExitCode: 1}, nil, build.StatusFailed},
		{build.Outcome{ExitCode: 137, TimedOut: true}, nil, build.StatusTimedOut},
		{build.Outcome{}, errors.New("docker exploded"), build.StatusError},
	} {
		svc := newService(t, build.Config{Run: func(context.Context, build.Spec, io.Writer) (build.Outcome, error) {
			return tc.outcome, tc.err
		}})
		res, err := svc.Build(context.Background(), build.Request{Command: "true"}, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != tc.want {
			t.Errorf("outcome %+v err %v: status %q, want %q", tc.outcome, tc.err, res.Status, tc.want)
		}
		if tc.err != nil && res.Error != tc.err.Error() {
			t.Errorf("error = %q, want %q", res.Error, tc.err)
		}
	}
}

func TestQueueFull(t *testing.T) {
	started := make(chan build.Spec, 1)
	release := make(chan struct{})
	defer close(release)
	svc := newService(t, build.Config{Run: blockingRunner(started, release), MaxConcurrent: 1, MaxQueue: 1, MaxPerClient: 10})

	if _, err := svc.Submit(build.Request{Command: "a"}); err != nil {
		t.Fatal(err)
	}
	<-started // first build holds the only slot
	if _, err := svc.Submit(build.Request{Command: "b"}); err != nil {
		t.Fatal(err) // waits in the queue
	}
	if _, err := svc.Submit(build.Request{Command: "c"}); !errors.Is(err, build.ErrQueueFull) {
		t.Fatalf("third submit: err = %v, want ErrQueueFull", err)
	}
}

func TestPerClientCap(t *testing.T) {
	started := make(chan build.Spec, 10)
	release := make(chan struct{})
	svc := newService(t, build.Config{Run: blockingRunner(started, release), MaxPerClient: 2})

	for i := 0; i < 2; i++ {
		if _, err := svc.Submit(build.Request{Command: "x", Client: "1.2.3.4"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Submit(build.Request{Command: "x", Client: "1.2.3.4"}); !errors.Is(err, build.ErrClientLimit) {
		t.Fatalf("third submit from same client: err = %v, want ErrClientLimit", err)
	}
	if _, err := svc.Submit(build.Request{Command: "x", Client: "5.6.7.8"}); err != nil {
		t.Fatalf("other client: %v", err)
	}
	<-started
	<-started
	<-started
	close(release)

	// Once builds finish, the client may submit again.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := svc.Submit(build.Request{Command: "x", Client: "1.2.3.4"})
		if err == nil {
			break
		}
		if !errors.Is(err, build.ErrClientLimit) || time.Now().After(deadline) {
			t.Fatalf("submit after release: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInvalidRequests(t *testing.T) {
	svc := newService(t, build.Config{Run: func(context.Context, build.Spec, io.Writer) (build.Outcome, error) {
		return build.Outcome{}, nil
	}})
	for name, req := range map[string]build.Request{
		"no command":        {},
		"absolute file":     {Command: "x", Files: map[string]string{"/etc/passwd": ""}},
		"dotdot file":       {Command: "x", Files: map[string]string{"a/../../b": ""}},
		"unknown toolchain": {Command: "x", Toolchain: "cobol"},
		"timeout too long":  {Command: "x", TimeoutSeconds: 99999},
		"bad artifact glob": {Command: "x", Artifacts: []string{"../x"}},
		"bad base64":        {Command: "x", FilesBase64: map[string]string{"a": "!!"}},
	} {
		var inv *build.InvalidError
		if _, err := svc.Submit(req); !errors.As(err, &inv) {
			t.Errorf("%s: err = %v, want InvalidError", name, err)
		}
	}
}

func TestLogCappedTailKept(t *testing.T) {
	svc := newService(t, build.Config{
		MaxLogBytes: 1000,
		TailLines:   3,
		Run: func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
			for i := 0; i < 500; i++ {
				fmt.Fprintf(log, "line %d\n", i)
			}
			return build.Outcome{}, nil
		},
	})
	res, err := svc.Build(context.Background(), build.Request{Command: "x"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := "line 497\nline 498\nline 499\n"; res.LogTail != want {
		t.Errorf("tail = %q, want %q", res.LogTail, want)
	}
	p, err := svc.LogPath(res.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 1200 || !strings.Contains(string(data), "truncated") {
		t.Errorf("log is %d bytes, truncated notice present: %v", len(data), strings.Contains(string(data), "truncated"))
	}
}

func TestSweep(t *testing.T) {
	svc := newService(t, build.Config{Retention: time.Hour, Run: func(context.Context, build.Spec, io.Writer) (build.Outcome, error) {
		return build.Outcome{}, nil
	}})
	res, err := svc.Build(context.Background(), build.Request{Command: "x"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := svc.LogPath(res.ID)
	svc.Sweep(time.Now())
	if _, err := svc.Get(context.Background(), res.ID, 0); err != nil {
		t.Fatalf("build swept too early: %v", err)
	}
	svc.Sweep(time.Now().Add(2 * time.Hour))
	if _, err := svc.Get(context.Background(), res.ID, 0); !errors.Is(err, build.ErrNotFound) {
		t.Fatalf("after sweep: err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("log file still exists after sweep: %v", err)
	}
}

func TestGetUnknown(t *testing.T) {
	svc := newService(t, build.Config{Run: func(context.Context, build.Spec, io.Writer) (build.Outcome, error) {
		return build.Outcome{}, nil
	}})
	for _, id := range []string{"nope", "../../etc", strings.Repeat("a", 32)} {
		if _, err := svc.Get(context.Background(), id, 0); !errors.Is(err, build.ErrNotFound) {
			t.Errorf("Get(%q): err = %v, want ErrNotFound", id, err)
		}
	}
}
