package docker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/dan-stowell/justbuildit/internal/docker"
)

// The test binary doubles as a fake docker CLI: when FAKE_DOCKER_STATE is
// set, it handles the docker subcommands Run and Cleanup use, running the
// container's `sh` locally in a scratch directory instead of /work.
func TestMain(m *testing.M) {
	if state := os.Getenv("FAKE_DOCKER_STATE"); state != "" {
		os.Exit(fakeDocker(state, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeDocker(state string, args []string) int {
	record := func(name string, lines []string) {
		os.WriteFile(filepath.Join(state, name), []byte(strings.Join(lines, "\n")), 0o644)
	}
	switch args[0] {
	case "image": // image inspect IMAGE
		if strings.HasPrefix(args[2], "missing") {
			return 1
		}
		return 0
	case "pull":
		img := args[len(args)-1]
		if strings.HasPrefix(img, "missing-bad") {
			os.Stderr.WriteString("manifest unknown\n")
			return 1
		}
		os.Stdout.WriteString("pulled " + img + "\n")
		return 0
	case "kill":
		pid, err := os.ReadFile(filepath.Join(state, args[1]+".pid"))
		if err != nil {
			return 1
		}
		n, _ := strconv.Atoi(string(pid))
		syscall.Kill(-n, syscall.SIGKILL)
		return 0
	case "ps":
		os.Stdout.WriteString("aaa\nbbb\n")
		return 0
	case "rm":
		record("rm", args)
		return 0
	case "run":
	default:
		return 125
	}

	record("run", args)
	withValue := map[string]bool{"--name": true, "--label": true, "--entrypoint": true,
		"--workdir": true, "--mount": true, "--env": true, "--runtime": true,
		"--network": true, "--memory": true, "--memory-swap": true, "--cpus": true,
		"--pids-limit": true}
	var name string
	var env []string
	i := 1
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if withValue[args[i]] {
			switch args[i] {
			case "--name":
				name = args[i+1]
			case "--env":
				env = append(env, args[i+1])
			}
			i++
		}
	}
	if args[i] == "fail125" {
		return 125
	}
	work, err := os.MkdirTemp(state, "work-")
	if err != nil {
		return 125
	}
	cmd := exec.Command("sh", args[i+1:]...)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 125
	}
	os.WriteFile(filepath.Join(state, name+".pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() < 0 {
			return 137
		}
		return exitErr.ExitCode()
	}
	return 0
}

type fixture struct {
	state string
	cfg   docker.Config
	spec  build.Spec
}

func setup(t *testing.T) *fixture {
	t.Helper()
	state := t.TempDir()
	t.Setenv("FAKE_DOCKER_STATE", state)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := docker.DefaultConfig()
	cfg.Docker = exe
	return &fixture{
		state: state,
		cfg:   cfg,
		spec: build.Spec{
			ID:      "0123456789abcdef0123456789abcdef",
			Image:   "golang:1.25",
			Timeout: time.Minute,
			Dir:     t.TempDir(),
		},
	}
}

func (f *fixture) run(t *testing.T) (build.Outcome, string, error) {
	t.Helper()
	var log bytes.Buffer
	out, err := docker.Run(context.Background(), f.cfg, f.spec, &log)
	return out, log.String(), err
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// readArtifacts maps entry names to their contents, or to "-> target" for
// symlinks, and "/" for directories.
func readArtifacts(t *testing.T, dir string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, build.ArtifactsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		switch h.Typeflag {
		case tar.TypeSymlink:
			got[h.Name] = "-> " + h.Linkname
		case tar.TypeDir:
			got[h.Name] = "/"
		default:
			b, _ := io.ReadAll(tr)
			got[h.Name] = string(b)
		}
	}
}

func noArtifacts(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, build.ArtifactsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("artifacts file exists (err=%v), want none", err)
	}
}

func TestRunSourceLogAndArtifacts(t *testing.T) {
	f := setup(t)
	f.spec.Source = tarGz(t, map[string]string{"hello.txt": "hi from source\n"})
	f.spec.Command = `cat hello.txt; echo to-stderr >&2; echo bin > out.bin
mkdir -p d; echo y > d/y; echo s > "my file.txt"; ln -s /etc/passwd link`
	f.spec.Artifacts = []string{"out.bin", "d", "my*", "link", "nomatch*"}

	out, log, err := f.run(t)
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if out != (build.Outcome{ExitCode: 0}) {
		t.Errorf("outcome = %+v, want exit 0", out)
	}
	for _, want := range []string{"hi from source", "to-stderr"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	got := readArtifacts(t, f.spec.Dir)
	want := map[string]string{
		"./out.bin":     "bin\n",
		"./d/":          "/",
		"./d/y":         "y\n",
		"./my file.txt": "s\n",
		"./link":        "-> /etc/passwd",
	}
	if len(got) != len(want) {
		t.Errorf("artifacts = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("artifact %q = %q, want %q", k, got[k], v)
		}
	}
	entries, _ := os.ReadDir(f.spec.Dir)
	if len(entries) != 1 {
		t.Errorf("Dir has %d entries, want only %s", len(entries), build.ArtifactsFile)
	}
}

func TestRunExitCode(t *testing.T) {
	f := setup(t)
	f.spec.Command = "touch a; exit 3"
	f.spec.Artifacts = []string{"none"}
	out, log, err := f.run(t)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log)
	}
	if out != (build.Outcome{ExitCode: 3}) {
		t.Errorf("outcome = %+v, want exit 3", out)
	}
	noArtifacts(t, f.spec.Dir)
}

func TestRunArtifactsOfFailedBuild(t *testing.T) {
	f := setup(t)
	f.spec.Command = "echo partial > p; exit 1"
	f.spec.Artifacts = []string{"p"}
	out, _, err := f.run(t)
	if err != nil || out.ExitCode != 1 {
		t.Fatalf("Run = %+v, %v; want exit 1", out, err)
	}
	if got := readArtifacts(t, f.spec.Dir)["./p"]; got != "partial\n" {
		t.Errorf("artifact p = %q", got)
	}
}

func TestRunCommandCannotWriteArtifactChannel(t *testing.T) {
	f := setup(t)
	f.spec.Command = "echo junk >&3"
	out, _, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode == 0 {
		t.Errorf("writing to fd 3 succeeded")
	}
	noArtifacts(t, f.spec.Dir)
}

func TestRunTimeout(t *testing.T) {
	f := setup(t)
	f.spec.Command = "sleep 60"
	f.spec.Timeout = 300 * time.Millisecond
	start := time.Now()
	out, log, err := f.run(t)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, log)
	}
	if !out.TimedOut {
		t.Errorf("outcome = %+v, want timed out", out)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s to time out", d)
	}
	if !strings.Contains(log, "timed out") {
		t.Errorf("log does not mention timeout:\n%s", log)
	}
}

func TestRunContextCanceled(t *testing.T) {
	f := setup(t)
	f.spec.Command = "sleep 60"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := docker.Run(ctx, f.cfg, f.spec, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

func TestRunArgs(t *testing.T) {
	f := setup(t)
	f.spec.Command = "true"
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.state, "run"))
	if err != nil {
		t.Fatal(err)
	}
	args := "\n" + string(b) + "\n"
	for _, want := range []string{
		"--rm", "--runtime\nrunsc", "--network\njustbuildit", "--memory\n4g",
		"--memory-swap\n4g", "--cpus\n2", "--pids-limit\n1024",
		"--label\njustbuildit=1", "--name\njustbuildit-" + f.spec.ID,
		"--entrypoint\nsh", "--workdir\n/work", "target=/etc/resolv.conf,readonly",
		"golang:1.25\n-c\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("docker run args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "JUSTBUILDIT_SOURCE=1") {
		t.Errorf("source flag set with empty source")
	}
}

func TestRunPull(t *testing.T) {
	f := setup(t)
	f.spec.Image = "missing-ok:1"
	f.spec.Command = "true"
	out, log, err := f.run(t)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("Run = %+v, %v\n%s", out, err, log)
	}
	if !strings.Contains(log, "pulling missing-ok:1") {
		t.Errorf("log does not mention pull:\n%s", log)
	}

	f.spec.Image = "missing-bad:1"
	if _, log, err := f.run(t); err == nil {
		t.Errorf("Run with unpullable image succeeded\n%s", log)
	} else if !strings.Contains(log, "manifest unknown") {
		t.Errorf("log lacks pull error:\n%s", log)
	}
}

func TestRunErrors(t *testing.T) {
	for _, image := range []string{"", "-v", "a b", "fail125"} {
		f := setup(t)
		f.spec.Image = image
		f.spec.Command = "true"
		if out, _, err := f.run(t); err == nil {
			t.Errorf("image %q: Run = %+v, want error", image, out)
		}
	}
}

func TestCleanup(t *testing.T) {
	f := setup(t)
	if err := docker.Cleanup(context.Background(), f.cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.state, "rm"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(b)); strings.Join(got, " ") != "rm -f aaa bbb" {
		t.Errorf("rm args = %q", got)
	}
}
