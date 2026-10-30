// Command justbuildit runs the justbuildit build service.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/dan-stowell/justbuildit/internal/web"
)

// run executes builds. Replace with docker.Run once the runner lands.
var run build.RunFunc = func(ctx context.Context, spec build.Spec, log io.Writer) (build.Outcome, error) {
	return build.Outcome{}, errors.New("build runner is not configured on this server")
}

func main() {
	var (
		addr       = flag.String("addr", ":8000", "listen address")
		dataDir    = flag.String("data-dir", "/var/lib/justbuildit", "directory for build logs and artifacts")
		publicURL  = flag.String("public-url", "", "externally visible base URL (default: derived from each request)")
		ipHeader   = flag.String("client-ip-header", "", "header set by a trusted proxy holding the client IP, e.g. X-Forwarded-For")
		concurrent = flag.Int("max-concurrent", 4, "builds running at once")
		queue      = flag.Int("max-queue", 32, "builds waiting to run")
		perClient  = flag.Int("max-per-client", 2, "builds queued or running per client IP")
		maxTimeout = flag.Duration("max-timeout", 30*time.Minute, "maximum build timeout")
		retention  = flag.Duration("retention", 6*time.Hour, "how long finished builds are kept")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	svc, err := build.New(build.Config{
		DataDir:       *dataDir,
		Run:           run,
		MaxConcurrent: *concurrent,
		MaxQueue:      *queue,
		MaxPerClient:  *perClient,
		MaxTimeout:    *maxTimeout,
		Retention:     *retention,
		Logger:        logger,
	})
	if err != nil {
		logger.Error("starting build service", "err", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunSweeper(ctx, 10*time.Minute)

	srv := &http.Server{
		Addr: *addr,
		Handler: web.New(web.Options{
			Service:        svc,
			PublicURL:      *publicURL,
			ClientIPHeader: *ipHeader,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	logger.Info("listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serving", "err", err)
		os.Exit(1)
	}
}
