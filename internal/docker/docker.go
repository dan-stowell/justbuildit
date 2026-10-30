// Package docker runs builds in sandboxed Docker containers using the docker
// CLI.
//
// The host never reads or writes the build's files. The gzipped source tar is
// streamed to the container on stdin and extracted there by the image's own
// sh and tar; afterwards the same wrapper packs the matching artifacts into
// one gzipped tar and writes it to the container's stdout, which the host
// copies to Dir/artifacts.tar.gz. The build's own stdout and stderr both go
// to the container's stderr, which is the build log.
package docker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
)

// Label marks containers started by this package.
const Label = "justbuildit"

// Config holds host-wide settings for running builds.
type Config struct {
	Docker           string        // docker CLI binary; default "docker"
	Runtime          string        // OCI runtime, e.g. "runsc"; empty uses Docker's default
	Network          string        // Docker network; empty uses Docker's default
	Memory           string        // --memory, e.g. "4g"; also used as --memory-swap
	CPUs             string        // --cpus, e.g. "2"
	PidsLimit        int           // --pids-limit; 0 means unset
	DNS              []string      // nameservers written to the container's /etc/resolv.conf
	PullTimeout      time.Duration // default 5m
	MaxArtifactBytes int64         // cap on artifacts.tar.gz; default 1 GiB
}

// DefaultConfig is the production configuration; it expects deploy/setup.sh
// to have been run on the host.
func DefaultConfig() Config {
	return Config{
		Runtime:   "runsc",
		Network:   "justbuildit",
		Memory:    "4g",
		CPUs:      "2",
		PidsLimit: 1024,
		DNS:       []string{"1.1.1.1", "8.8.8.8"},
	}
}

func (c Config) withDefaults() Config {
	if c.Docker == "" {
		c.Docker = "docker"
	}
	if c.PullTimeout == 0 {
		c.PullTimeout = 5 * time.Minute
	}
	if c.MaxArtifactBytes == 0 {
		c.MaxArtifactBytes = 1 << 30
	}
	return c
}

// wrapper runs inside the container as `sh -c wrapper justbuildit CMD GLOB...`
// with the working directory set to /work. Its stdout is the artifacts
// channel; everything else goes to stderr.
const wrapper = `exec 3>&1 1>&2
if [ "${JUSTBUILDIT_SOURCE:-}" = 1 ]; then
  tar -xzf - || { echo "justbuildit: extracting source failed"; exit 1; }
fi
sh -c "$1" </dev/null 3>&-
status=$?
shift
n=$#
IFS=
for g in "$@"; do
  for f in $g; do
    if [ -e "$f" ] || [ -L "$f" ]; then
      case $f in /*) ;; *) f=./$f ;; esac
      set -- "$@" "$f"
    fi
  done
done
shift $n
if [ $# -gt 0 ]; then
  tar -czf - "$@" >&3 || echo "justbuildit: packing artifacts failed"
fi
exit $status
`

// Run runs spec in a fresh container, streaming the build log to log. It
// writes spec.Dir/artifacts.tar.gz only if some artifact matched. A non-nil
// error means the build could not be run, not that it failed.
func Run(ctx context.Context, cfg Config, spec build.Spec, log io.Writer) (build.Outcome, error) {
	cfg = cfg.withDefaults()
	if err := checkImage(spec.Image); err != nil {
		return build.Outcome{}, err
	}
	if err := ensureImage(ctx, cfg, spec.Image, log); err != nil {
		return build.Outcome{}, err
	}

	resolv := filepath.Join(spec.Dir, "resolv.conf")
	if err := os.WriteFile(resolv, resolvConf(cfg.DNS), 0o644); err != nil {
		return build.Outcome{}, err
	}
	defer os.Remove(resolv)

	name := "justbuildit-" + spec.ID
	cmd := exec.Command(cfg.Docker, runArgs(cfg, spec, name, resolv)...)
	cmd.Stdin = bytes.NewReader(spec.Source)
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return build.Outcome{}, err
	}
	if err := cmd.Start(); err != nil {
		return build.Outcome{}, fmt.Errorf("starting docker: %w", err)
	}

	done := make(chan runResult, 1)
	go func() {
		// Drain stdout before Wait, which closes it.
		note, aerr := saveArtifacts(stdout, spec.Dir, cfg.MaxArtifactBytes)
		done <- runResult{note, aerr, cmd.Wait()}
	}()

	var timeout <-chan time.Time
	if spec.Timeout > 0 {
		t := time.NewTimer(spec.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	var res runResult
	timedOut := false
	select {
	case res = <-done:
	case <-timeout:
		timedOut = true
		res = killUntilDone(cfg, name, done)
	case <-ctx.Done():
		killUntilDone(cfg, name, done)
		return build.Outcome{}, ctx.Err()
	}
	if res.artifacts != nil {
		return build.Outcome{}, fmt.Errorf("saving artifacts: %w", res.artifacts)
	}
	if res.note != "" {
		fmt.Fprintf(log, "justbuildit: %s\n", res.note)
	}
	werr := res.wait
	if timedOut {
		fmt.Fprintf(log, "\njustbuildit: build timed out after %s\n", spec.Timeout)
		return build.Outcome{ExitCode: -1, TimedOut: true}, nil
	}

	var exitErr *exec.ExitError
	switch {
	case werr == nil:
		return build.Outcome{ExitCode: 0}, nil
	case errors.As(werr, &exitErr):
		code := exitErr.ExitCode()
		if code == 125 {
			// docker run itself failed (bad flag, daemon error, ...).
			return build.Outcome{}, fmt.Errorf("docker run failed (exit 125); see log")
		}
		if code == 137 {
			fmt.Fprintf(log, "\njustbuildit: build was killed (exit 137), most likely for exceeding the %s memory limit; files written inside the container count toward it\n", cfg.Memory)
		}
		return build.Outcome{ExitCode: code}, nil
	default:
		return build.Outcome{}, werr
	}
}

func runArgs(cfg Config, spec build.Spec, name, resolv string) []string {
	args := []string{"run", "--rm", "-i",
		"--name", name,
		"--label", Label + "=1",
		"--entrypoint", "sh",
		"--workdir", "/work",
		"--mount", "type=bind,source=" + resolv + ",target=/etc/resolv.conf,readonly",
	}
	if len(spec.Source) > 0 {
		args = append(args, "--env", "JUSTBUILDIT_SOURCE=1")
	}
	if cfg.Runtime != "" {
		args = append(args, "--runtime", cfg.Runtime)
	}
	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	}
	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory, "--memory-swap", cfg.Memory)
	}
	if cfg.CPUs != "" {
		args = append(args, "--cpus", cfg.CPUs)
	}
	if cfg.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(cfg.PidsLimit))
	}
	args = append(args, spec.Image, "-c", wrapper, "justbuildit", spec.Command)
	return append(args, spec.Artifacts...)
}

func resolvConf(servers []string) []byte {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	return []byte(b.String())
}

// checkImage rejects refs the docker CLI could mistake for flags.
func checkImage(image string) error {
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n") {
		return fmt.Errorf("invalid image %q", image)
	}
	return nil
}

func ensureImage(ctx context.Context, cfg Config, image string, log io.Writer) error {
	if exec.CommandContext(ctx, cfg.Docker, "image", "inspect", image).Run() == nil {
		return nil
	}
	fmt.Fprintf(log, "justbuildit: pulling %s\n", image)
	pctx, cancel := context.WithTimeout(ctx, cfg.PullTimeout)
	defer cancel()
	pull := exec.CommandContext(pctx, cfg.Docker, "pull", "-q", image)
	pull.Stdout = log
	pull.Stderr = log
	if err := pull.Run(); err != nil {
		if pctx.Err() != nil {
			return fmt.Errorf("pulling %s: timed out after %s", image, cfg.PullTimeout)
		}
		return fmt.Errorf("pulling %s: %w", image, err)
	}
	return nil
}

type runResult struct {
	note            string // message about discarded artifacts, for the log
	artifacts, wait error
}

// killUntilDone kills the container, retrying in case it was still being
// created, until docker run exits.
func killUntilDone(cfg Config, name string, done <-chan runResult) runResult {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		// The container may not exist yet or may already be gone.
		_ = exec.Command(cfg.Docker, "kill", name).Run()
		select {
		case r := <-done:
			return r
		case <-tick.C:
		}
	}
}

// saveArtifacts copies the artifact stream to dir/artifacts.tar.gz, keeping
// it only if it is non-empty, within max bytes, and a valid gzipped tar;
// otherwise note says why it was dropped. It always drains r so the container
// never blocks on its stdout.
func saveArtifacts(r io.Reader, dir string, max int64) (note string, err error) {
	defer io.Copy(io.Discard, r)
	f, err := os.CreateTemp(dir, ".artifacts-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", err
	case n == 0:
		return "", nil
	case n > max:
		return fmt.Sprintf("artifacts exceed %d bytes; discarded", max), nil
	}
	if err := validTarGz(tmp); err != nil {
		return fmt.Sprintf("artifacts are not a valid tar.gz (%v); discarded", err), nil
	}
	return "", os.Rename(tmp, filepath.Join(dir, build.ArtifactsFile))
}

func validTarGz(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		_, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

// Cleanup force-removes containers left over from a previous run of the
// service. Call it once at startup, before any builds start.
func Cleanup(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	out, err := exec.CommandContext(ctx, cfg.Docker, "ps", "-aq", "--filter", "label="+Label).Output()
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	rm := exec.CommandContext(ctx, cfg.Docker, append([]string{"rm", "-f"}, ids...)...)
	if out, err := rm.CombinedOutput(); err != nil {
		return fmt.Errorf("removing containers: %v: %s", err, out)
	}
	return nil
}
