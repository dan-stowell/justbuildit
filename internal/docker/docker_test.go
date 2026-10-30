package docker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
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

// fakeImage is an image in the fake docker's image store, state/images.json.
type fakeImage struct {
	ID      string
	Size    int64
	Volumes map[string]struct{}
}

func loadImages(state string) map[string]fakeImage {
	m := map[string]fakeImage{}
	b, _ := os.ReadFile(filepath.Join(state, "images.json"))
	json.Unmarshal(b, &m)
	return m
}

func saveImages(state string, m map[string]fakeImage) {
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(state, "images.json"), b, 0o644)
}

// lookupImage finds ref (a ref or ID) in the store. Refs not in the store
// exist unless their name starts with "missing".
func lookupImage(state, ref string) (string, fakeImage, bool) {
	for r, img := range loadImages(state) {
		if r == ref || img.ID == ref {
			return r, img, true
		}
	}
	if strings.HasPrefix(path.Base(ref), "missing") || strings.HasPrefix(ref, "sha256:") {
		return "", fakeImage{}, false
	}
	img := fakeImage{ID: "sha256:" + ref, Size: 1e9}
	if strings.Contains(ref, "badvol") {
		img.Volumes = map[string]struct{}{"/a,b": {}}
	} else if strings.Contains(ref, "vol") {
		img.Volumes = map[string]struct{}{"/data": {}, "/var/x": {}}
	}
	return ref, img, true
}

func fakeDocker(state string, args []string) int {
	record := func(name string, lines []string) {
		os.WriteFile(filepath.Join(state, name), []byte(strings.Join(lines, "\n")), 0o644)
	}
	switch args[0] {
	case "-S": // fake iptables -S CHAIN
		b, err := os.ReadFile(filepath.Join(state, "iptables-"+args[1]))
		if err != nil {
			os.Stderr.WriteString("No chain/target/match by that name.\n")
			return 1
		}
		os.Stdout.Write(b)
		return 0
	case "image":
		switch args[1] {
		case "inspect": // image inspect REF
			ref, img, ok := lookupImage(state, args[2])
			if !ok {
				return 1
			}
			var out [1]struct {
				Id       string
				RepoTags []string
				Config   struct{ Volumes map[string]struct{} }
			}
			out[0].Id, out[0].RepoTags, out[0].Config.Volumes = img.ID, []string{ref}, img.Volumes
			json.NewEncoder(os.Stdout).Encode(out)
			return 0
		case "ls":
			for _, img := range loadImages(state) {
				fmt.Printf("%s %.3gGB\n", img.ID, float64(img.Size)/1e9)
			}
			return 0
		case "rm":
			m := loadImages(state)
			for _, r := range args[2:] {
				delete(m, r)
			}
			saveImages(state, m)
			record("image-rm", args[2:])
			return 0
		}
		return 125
	case "pull":
		img := args[len(args)-1]
		if strings.HasPrefix(img, "missing-bad") {
			os.Stderr.WriteString("Error response from daemon: manifest unknown: secret internal detail 10.1.2.3\n")
			return 1
		}
		m := loadImages(state)
		size := int64(1e9)
		if strings.HasPrefix(img, "missing-huge") {
			size = 9e9
		}
		m[img] = fakeImage{ID: "sha256:" + img, Size: size}
		saveImages(state, m)
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
	case "container": // container inspect --format F NAME
		code, err := os.ReadFile(filepath.Join(state, args[len(args)-1]+".exit"))
		if err != nil {
			return 1
		}
		fmt.Printf("%s|2026-01-01T00:00:00Z|\n", code)
		return 0
	case "ps":
		os.Stdout.WriteString("aaa\nbbb\n")
		return 0
	case "rm":
		if len(args) == 3 && strings.HasPrefix(args[2], "justbuildit-") {
			record("rm-"+args[2], args)
			return 0
		}
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
		"--pids-limit": true, "--log-driver": true, "--user": true}
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
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if code < 0 {
			code = 137
		}
	}
	os.WriteFile(filepath.Join(state, name+".exit"), []byte(strconv.Itoa(code)), 0o644)
	return code
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
	f.spec.SourceFile = filepath.Join(t.TempDir(), "source.tar.gz")
	if err := os.WriteFile(f.spec.SourceFile, tarGz(t, map[string]string{"hello.txt": "hi from source\n"}), 0o644); err != nil {
		t.Fatal(err)
	}
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
		"--log-driver\nnone", "--user\n0:0", "--runtime\nrunsc", "--network\njustbuildit", "--memory\n4g",
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
	if strings.Contains(args, "\n--rm\n") {
		t.Errorf("docker run uses --rm; the runner removes the container itself")
	}
	if _, err := os.Stat(filepath.Join(f.state, "rm-justbuildit-"+f.spec.ID)); err != nil {
		t.Errorf("container was not force-removed: %v", err)
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
	} else if !strings.Contains(log, "image not found") || strings.Contains(log, "10.1.2.3") {
		t.Errorf("log should say the image was not found without daemon details:\n%s", log)
	}
}

func TestRunErrors(t *testing.T) {
	for _, image := range []string{"", "-v", "a b", "fail125", "Upper:1",
		"localhost/x", "localhost:5000/x", "10.0.0.1/x", "10.0.0.1:5000/x", "[::1]/x",
		"ghcr.io:443/x/y", "evil.example.com/x", "registry-1.docker.io.evil.com/x",
		"golang:1.25@sha256:abc", "golang:bad/tag", "badvol:1"} {
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

func TestRunAllowedImages(t *testing.T) {
	for _, image := range []string{"golang", "golang:1.25", "library/golang:1.25", "docker.io/library/golang:1.25",
		"ghcr.io/foo/bar:v1.2", "quay.io/a/b", "gcr.io/distroless/static", "mcr.microsoft.com/dotnet/sdk:9.0",
		"public.ecr.aws/docker/library/alpine:3", "registry.k8s.io/pause:3.10",
		"golang@sha256:" + strings.Repeat("a", 64)} {
		f := setup(t)
		f.spec.Image = image
		f.spec.Command = "true"
		if out, log, err := f.run(t); err != nil || out.ExitCode != 0 {
			t.Errorf("image %q: Run = %+v, %v\n%s", image, out, err, log)
		}
	}
}

func TestRunVolumesAreTmpfs(t *testing.T) {
	f := setup(t)
	f.spec.Image = "withvol:1"
	f.spec.Command = "true"
	if _, log, err := f.run(t); err != nil {
		t.Fatalf("Run: %v\n%s", err, log)
	}
	b, _ := os.ReadFile(filepath.Join(f.state, "run"))
	for _, want := range []string{
		"--mount\ntype=tmpfs,destination=/data,tmpfs-size=1g\n",
		"--mount\ntype=tmpfs,destination=/var/x,tmpfs-size=1g\n",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("docker run args missing %q:\n%s", want, b)
		}
	}
}

func TestRunBuildExit125(t *testing.T) {
	f := setup(t)
	f.spec.Command = "exit 125"
	out, log, err := f.run(t)
	if err != nil || out.ExitCode != 125 {
		t.Errorf("Run = %+v, %v; want build exit 125\n%s", out, err, log)
	}
}

func TestRunImageTooBig(t *testing.T) {
	f := setup(t)
	f.spec.Image = "missing-huge:1"
	f.spec.Command = "true"
	_, log, err := f.run(t)
	if err == nil || !strings.Contains(log, "too big") {
		t.Errorf("Run err = %v, want too big\n%s", err, log)
	}
	if _, ok := loadImages(f.state)["missing-huge:1"]; ok {
		t.Errorf("oversized image was not removed")
	}
}

func TestRunPrunesImages(t *testing.T) {
	f := setup(t)
	saveImages(f.state, map[string]fakeImage{
		"golang:1.25": {ID: "sha256:golang", Size: 30e9},
		"old:1":       {ID: "sha256:old1", Size: 10e9},
		"old:2":       {ID: "sha256:old2", Size: 10e9},
	})
	f.cfg.ImageBudgetBytes = 45e9
	f.spec.Image = "missing-new:1"
	f.spec.Command = "true"
	if _, log, err := f.run(t); err != nil {
		t.Fatalf("Run: %v\n%s", err, log)
	}
	m := loadImages(f.state)
	_, old1 := m["old:1"]
	_, old2 := m["old:2"]
	_, gol := m["golang:1.25"]
	_, nw := m["missing-new:1"]
	if !gol || !nw || old1 == old2 {
		t.Errorf("after prune images = %v; want golang, the new image and one old image", m)
	}
}

// fakeTar puts a tar on PATH that ignores its arguments and writes body,
// standing in for a build that replaced the image's tar.
func fakeTar(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tar"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func block(name string, typ byte, size int64, gnu bool) []byte {
	b := make([]byte, 512)
	copy(b[0:], name)
	copy(b[100:], "0000644\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	b[156] = typ
	if gnu {
		copy(b[257:], "ustar  \x00")
	} else {
		copy(b[257:], "ustar\x0000")
	}
	return b
}

func checksum(b []byte) []byte {
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func pad(b []byte) []byte {
	return append(b, make([]byte, (512-len(b)%512)%512)...)
}

// sparseBombs are gzipped tars whose one entry, 1 byte on the wire, reads
// back as 1 TiB of zeros.
func sparseBombs(t *testing.T) map[string][]byte {
	const realSize = 1 << 40
	// Old GNU sparse ('S') header: one data extent at 0 of length 1.
	gnu := block("bomb", tar.TypeGNUSparse, 1, true)
	copy(gnu[386:], "00000000000\x00") // sparse[0].offset
	copy(gnu[398:], "00000000001\x00") // sparse[0].numbytes
	gnu[483] = 0x80                    // realsize, base-256
	for i, v := 494, int64(realSize); i > 483; i, v = i-1, v>>8 {
		gnu[i] = byte(v)
	}
	oldGNU := append(checksum(gnu), pad([]byte("x"))...)

	// PAX GNU sparse 0.1 header.
	var recs []byte
	for _, kv := range []string{"GNU.sparse.map=0,1", "GNU.sparse.numblocks=1", "GNU.sparse.size=" + strconv.Itoa(realSize)} {
		n := len(kv) + 3
		for len(fmt.Sprintf("%d %s\n", n, kv)) != n {
			n++
		}
		recs = append(recs, fmt.Sprintf("%d %s\n", n, kv)...)
	}
	pax := append(checksum(block("PaxHeaders/bomb", tar.TypeXHeader, int64(len(recs)), false)), pad(recs)...)
	pax = append(pax, checksum(block("bomb", tar.TypeReg, 1, false))...)
	pax = append(pax, pad([]byte("x"))...)

	out := map[string][]byte{}
	for name, raw := range map[string][]byte{"gnu": oldGNU, "pax": pax} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(raw)
		zw.Write(make([]byte, 1024))
		zw.Close()
		out[name] = buf.Bytes()
	}
	return out
}

func TestRunSparseBombArtifacts(t *testing.T) {
	for name, bomb := range sparseBombs(t) {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			p := filepath.Join(t.TempDir(), "bomb.tgz")
			os.WriteFile(p, bomb, 0o644)
			fakeTar(t, "cat "+p)
			f.spec.Command = "touch x"
			f.spec.Artifacts = []string{"x"}
			f.spec.Timeout = 10 * time.Second
			start := time.Now()
			out, log, err := f.run(t)
			if d := time.Since(start); d > f.spec.Timeout+5*time.Second {
				t.Errorf("Run took %s", d)
			}
			if err != nil || out.TimedOut {
				t.Errorf("Run = %+v, %v\n%s", out, err, log)
			}
			if !strings.Contains(log, "sparse") {
				t.Errorf("log does not say the sparse artifact was rejected:\n%s", log)
			}
			noArtifacts(t, f.spec.Dir)
		})
	}
}

func TestRunEndlessArtifactStreamTimesOut(t *testing.T) {
	f := setup(t)
	fakeTar(t, `printf '\037\213'; exec sleep 600`)
	f.spec.Command = "touch x"
	f.spec.Artifacts = []string{"x"}
	f.spec.Timeout = time.Second
	start := time.Now()
	out, log, err := f.run(t)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("Run took %s", d)
	}
	if err != nil || !out.TimedOut {
		t.Errorf("Run = %+v, %v; want timed out\n%s", out, err, log)
	}
	noArtifacts(t, f.spec.Dir)
}

func TestRunArtifactLimits(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		set                 func(*docker.Config)
	}{
		{"unpacked", "head -c 2000000 /dev/zero > x", "unpack to more than", func(c *docker.Config) { c.MaxArtifactUnpackedBytes = 1 << 20 }},
		{"entries", "mkdir x; touch x/1 x/2 x/3 x/4", "more than 3 entries", func(c *docker.Config) { c.MaxArtifactEntries = 3 }},
		{"fifo", "mkfifo x", "unsupported type", func(*docker.Config) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			tc.set(&f.cfg)
			f.spec.Command = tc.command
			f.spec.Artifacts = []string{"x"}
			out, log, err := f.run(t)
			if err != nil || out.ExitCode != 0 {
				t.Fatalf("Run = %+v, %v\n%s", out, err, log)
			}
			if !strings.Contains(log, tc.want) {
				t.Errorf("log lacks %q:\n%s", tc.want, log)
			}
			noArtifacts(t, f.spec.Dir)
		})
	}
}

func TestCheckFirewall(t *testing.T) {
	good := map[string]string{
		"DOCKER-USER": "-N DOCKER-USER\n-A DOCKER-USER -i jbi0 -j JUSTBUILDIT-FWD\n-A DOCKER-USER -j RETURN\n",
		"INPUT":       "-P INPUT ACCEPT\n-A INPUT -i jbi0 -j JUSTBUILDIT-IN\n-A INPUT -j ACCEPT\n",
		"JUSTBUILDIT-IN": "-N JUSTBUILDIT-IN\n" +
			"-A JUSTBUILDIT-IN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN\n-A JUSTBUILDIT-IN -j DROP\n",
		"JUSTBUILDIT-FWD": "-N JUSTBUILDIT-FWD\n",
	}
	for _, cidr := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4"} {
		good["JUSTBUILDIT-FWD"] += "-A JUSTBUILDIT-FWD -d " + cidr + " -j DROP\n"
	}
	good["JUSTBUILDIT-FWD"] += "-A JUSTBUILDIT-FWD -j RETURN\n"

	for _, tc := range []struct {
		name   string
		change func(map[string]string)
		ok     bool
	}{
		{"good", func(map[string]string) {}, true},
		{"missing chain", func(m map[string]string) { delete(m, "JUSTBUILDIT-FWD") }, false},
		{"missing jump", func(m map[string]string) { m["DOCKER-USER"] = "-N DOCKER-USER\n" }, false},
		{"jump not first", func(m map[string]string) {
			m["INPUT"] = "-P INPUT ACCEPT\n-A INPUT -j ACCEPT\n-A INPUT -i jbi0 -j JUSTBUILDIT-IN\n"
		}, false},
		{"early return", func(m map[string]string) {
			m["JUSTBUILDIT-FWD"] = strings.Replace(m["JUSTBUILDIT-FWD"], "-N JUSTBUILDIT-FWD\n", "-N JUSTBUILDIT-FWD\n-A JUSTBUILDIT-FWD -j RETURN\n", 1)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			m := map[string]string{}
			for k, v := range good {
				m[k] = v
			}
			tc.change(m)
			for chain, rules := range m {
				os.WriteFile(filepath.Join(f.state, "iptables-"+chain), []byte(rules), 0o644)
			}
			f.cfg.IPTables = f.cfg.Docker
			err := docker.CheckFirewall(context.Background(), f.cfg)
			if (err == nil) != tc.ok {
				t.Errorf("CheckFirewall = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
