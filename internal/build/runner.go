// Package build runs builds: it queues requests, hands them to a RunFunc,
// and keeps an in-memory index of results, logs and artifacts.
package build

import (
	"context"
	"io"
	"time"
)

// Spec is everything a runner needs to execute one build.
type Spec struct {
	ID         string        // 128-bit random hex
	Image      string        // resolved docker image ref
	Command    string        // run with `sh -c` in /work
	SourceFile string        // host path of a gzipped tar of the workspace (outside Dir), or "" if none
	Artifacts  []string      // glob patterns relative to /work
	Timeout    time.Duration // kill the build after this long
	Dir        string        // host dir owned by the runner for this build
}

// Outcome is the result of a build that the runner managed to execute.
type Outcome struct {
	ExitCode int
	TimedOut bool
}

// RunFunc executes a build. It streams combined stdout+stderr to log and
// writes Dir/artifacts.tar.gz (only if any artifact matched). A non-nil error
// means a setup or infrastructure failure, not a failed build.
type RunFunc func(ctx context.Context, spec Spec, log io.Writer) (Outcome, error)

// ArtifactsFile is the name of the artifacts tarball a runner writes in Spec.Dir.
const ArtifactsFile = "artifacts.tar.gz"
