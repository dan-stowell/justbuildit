// Command runnercheck exercises internal/docker against the real Docker and
// gVisor setup on a host prepared by deploy/setup.sh, including hostile
// builds. It prints PASS/FAIL per check and exits non-zero if any failed.
//
//	runnercheck [-run REGEXP]
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dan-stowell/justbuildit/internal/build"
	"github.com/dan-stowell/justbuildit/internal/docker"
)

type result struct {
	out  build.Outcome
	log  string
	err  error
	arts map[string]*entry
	dir  string
}

type entry struct {
	typ  byte
	link string
	body []byte
}

var cfg = docker.DefaultConfig()

func run(image, command string, src map[string]string, artifacts []string, timeout time.Duration) result {
	dir, err := os.MkdirTemp("", "runnercheck-")
	if err != nil {
		panic(err)
	}
	spec := build.Spec{
		ID:        fmt.Sprintf("%x", time.Now().UnixNano()),
		Image:     image,
		Command:   command,
		Artifacts: artifacts,
		Timeout:   timeout,
		Dir:       dir,
	}
	if src != nil {
		spec.Source = tarGz(src)
	}
	var log bytes.Buffer
	out, err := docker.Run(context.Background(), cfg, spec, &log)
	r := result{out: out, log: log.String(), err: err, dir: dir}
	r.arts = readTarGz(filepath.Join(dir, build.ArtifactsFile))
	os.RemoveAll(dir)
	return r
}

func (r result) String() string {
	return fmt.Sprintf("outcome=%+v err=%v artifacts=%d\n--- log tail ---\n%s", r.out, r.err, len(r.arts), tail(r.log, 15))
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func tarGz(files map[string]string) []byte {
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

func readTarGz(path string) map[string]*entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	tr := tar.NewReader(zr)
	m := map[string]*entry{}
	for {
		h, err := tr.Next()
		if err != nil {
			return m
		}
		b, _ := io.ReadAll(tr)
		m[h.Name] = &entry{typ: h.Typeflag, link: h.Linkname, body: b}
	}
}

func usedBytes(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		panic(err)
	}
	return (st.Blocks - st.Bfree) * uint64(st.Bsize)
}

func hostIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() &&
			!strings.HasPrefix(n.IP.String(), "172.") {
			return n.IP.String()
		}
	}
	return ""
}

type check struct {
	name string
	fn   func() (bool, string)
}

const goMain = `package main

import "fmt"

func main() { fmt.Println("hello from justbuildit") }
`

var checks = []check{
	{"go-hello-artifact", func() (bool, string) {
		r := run("golang:1.25", "go build -o hello . && ./hello",
			map[string]string{"go.mod": "module hello\n\ngo 1.25\n", "main.go": goMain},
			[]string{"hello"}, 5*time.Minute)
		e := r.arts["./hello"]
		ok := r.err == nil && r.out.ExitCode == 0 && strings.Contains(r.log, "hello from justbuildit") &&
			e != nil && e.typ == tar.TypeReg && bytes.HasPrefix(e.body, []byte("\x7fELF"))
		return ok, r.String()
	}},
	{"symlink-artifacts-no-host-leak", func() (bool, string) {
		hostPasswd, _ := os.ReadFile("/etc/passwd")
		r := run("golang:1.25",
			"ln -s /etc/passwd passwd; ln -s /proc/self/environ environ; ln -s / root; "+
				"cat /proc/self/environ | tr '\\0' '\\n' | sed 's/^/ENV /'",
			nil, []string{"passwd", "environ", "root"}, time.Minute)
		var all []byte
		for _, e := range r.arts {
			if e.typ != tar.TypeSymlink {
				return false, fmt.Sprintf("non-symlink entry type %c\n%s", e.typ, r)
			}
			all = append(all, e.body...)
		}
		leak := bytes.Contains(all, []byte("exedev")) || strings.Contains(r.log, "exedev") ||
			strings.Contains(r.log, "BUILDBUDDY") || bytes.Contains(all, hostPasswd)
		ok := r.err == nil && len(r.arts) == 3 && !leak
		return ok, fmt.Sprintf("leak=%v %s", leak, r)
	}},
	{"dereferenced-symlink-is-sandbox-file", func() (bool, string) {
		// Even if the build packs the target itself, it gets the sandbox's
		// files, never the host's.
		r := run("golang:1.25", "cp -L /etc/passwd p; cat /etc/hostname", nil, []string{"p"}, time.Minute)
		e := r.arts["./p"]
		ok := r.err == nil && e != nil && !bytes.Contains(e.body, []byte("exedev"))
		return ok, r.String()
	}},
	{"tar-dotdot-entries", func() (bool, string) {
		parent := os.TempDir()
		os.Remove(filepath.Join(parent, "jbi-escape.txt"))
		r := run("golang:1.25",
			"ls -la /work /; test ! -e /jbi-escape.txt && test ! -e /tmp/jbi-escape.txt && echo NO-ESCAPE-IN-SANDBOX",
			map[string]string{
				"ok.txt":                         "ok",
				"../jbi-escape.txt":              "x",
				"../../../../tmp/jbi-escape.txt": "x",
				"/jbi-abs.txt":                   "x",
			}, nil, time.Minute)
		_, hostErr := os.Stat(filepath.Join(parent, "jbi-escape.txt"))
		_, dirErr := os.Stat(filepath.Join(filepath.Dir(r.dir), "jbi-escape.txt"))
		ok := r.err == nil && os.IsNotExist(hostErr) && os.IsNotExist(dirErr)
		return ok, fmt.Sprintf("sandbox-clean=%v %s", strings.Contains(r.log, "NO-ESCAPE-IN-SANDBOX"), r)
	}},
	{"write-10GB-does-not-fill-host", func() (bool, string) {
		before := usedBytes("/var/lib/docker")
		peak := before
		stop := make(chan struct{})
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(200 * time.Millisecond):
					if u := usedBytes("/var/lib/docker"); u > peak {
						peak = u
					}
				}
			}
		}()
		r := run("golang:1.25", "dd if=/dev/zero of=big bs=1M count=10240; echo dd=$?; ls -la big",
			nil, nil, 5*time.Minute)
		close(stop)
		grew := int64(peak) - int64(before)
		ok := r.err == nil && r.out.ExitCode != 0 && grew < 1<<30 && strings.Contains(r.log, "memory limit")
		return ok, fmt.Sprintf("host disk grew %d MiB at peak\n%s", grew>>20, r)
	}},
	{"network-policy", func() (bool, string) {
		ln, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			return false, err.Error()
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("HOST-LISTENER\n"))
				c.Close()
			}
		}()
		port := ln.Addr().(*net.TCPAddr).Port
		ip := hostIP()
		// Positive control: the listener is reachable from the host.
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), 2*time.Second); err != nil {
			return false, "control dial failed: " + err.Error()
		} else {
			c.Close()
		}
		probe := func(url string) string {
			return fmt.Sprintf("curl -sS -m 8 -o /dev/null %s </dev/null; echo \"PROBE %s rc=$?\"; ", url, url)
		}
		cmd := probe("http://169.254.169.254/") +
			probe(fmt.Sprintf("telnet://%s:22", ip)) +
			probe(fmt.Sprintf("telnet://%s:%d", ip, port)) +
			probe(fmt.Sprintf("telnet://172.30.0.1:%d", port)) +
			probe(fmt.Sprintf("telnet://172.17.0.1:%d", port)) +
			probe("http://10.42.0.1/") +
			probe("https://proxy.golang.org/") +
			probe("https://github.com/")
		r := run("golang:1.25", cmd, nil, nil, 3*time.Minute)
		want := map[string]bool{ // true = must succeed
			"http://169.254.169.254/":                   false,
			fmt.Sprintf("telnet://%s:22", ip):           false,
			fmt.Sprintf("telnet://%s:%d", ip, port):     false,
			fmt.Sprintf("telnet://172.30.0.1:%d", port): false,
			fmt.Sprintf("telnet://172.17.0.1:%d", port): false,
			"http://10.42.0.1/":                         false,
			"https://proxy.golang.org/":                 true,
			"https://github.com/":                       true,
		}
		ok := r.err == nil
		var notes []string
		for url, succeed := range want {
			line := ""
			for _, l := range strings.Split(r.log, "\n") {
				if strings.HasPrefix(l, "PROBE "+url+" ") {
					line = l
				}
			}
			// Blocked traffic is dropped, so it must time out (curl 28)
			// rather than be refused.
			good := (succeed && strings.HasSuffix(line, "rc=0")) || (!succeed && strings.HasSuffix(line, "rc=28"))
			if !good {
				ok = false
				notes = append(notes, "BAD: "+url+" -> "+line)
			}
		}
		if strings.Contains(r.log, "HOST-LISTENER") {
			ok = false
		}
		return ok, strings.Join(notes, "\n") + "\n" + r.String()
	}},
	{"write-2GB-ok", func() (bool, string) {
		r := run("golang:1.25", "dd if=/dev/zero of=big bs=1M count=2048 status=none && ls -la big", nil, nil, 5*time.Minute)
		return r.err == nil && r.out.ExitCode == 0, r.String()
	}},
	{"timeout-kills", func() (bool, string) {
		start := time.Now()
		r := run("golang:1.25", "echo started; sleep 600", nil, nil, 5*time.Second)
		took := time.Since(start)
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+docker.Label).Output()
		ok := r.err == nil && r.out.TimedOut && took < 30*time.Second && len(bytes.TrimSpace(out)) == 0
		return ok, fmt.Sprintf("took %s, leftover=%q %s", took.Round(time.Millisecond), out, r)
	}},
	{"cleanup-removes-labelled", func() (bool, string) {
		if out, err := exec.Command("docker", "run", "-d", "--label", docker.Label+"=1", "golang:1.25", "sleep", "300").CombinedOutput(); err != nil {
			return false, string(out)
		}
		err := docker.Cleanup(context.Background(), cfg)
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+docker.Label).Output()
		return err == nil && len(bytes.TrimSpace(out)) == 0, fmt.Sprintf("err=%v leftover=%q", err, out)
	}},
	{"toolchain-go-with-deps", func() (bool, string) {
		r := run("golang:1.25", "go mod init x && go get rsc.io/quote@v1.5.2 && go build -o app . && ./app",
			map[string]string{"main.go": "package main\n\nimport (\"fmt\"; \"rsc.io/quote\")\n\nfunc main() { fmt.Println(quote.Go()) }\n"},
			[]string{"app"}, 10*time.Minute)
		return r.err == nil && r.out.ExitCode == 0 && r.arts["./app"] != nil, r.String()
	}},
	{"toolchain-cargo", func() (bool, string) {
		r := run("rust:1", "cargo build --release && ./target/release/app",
			map[string]string{
				"Cargo.toml":  "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[dependencies]\nitoa = \"1\"\n",
				"src/main.rs": "fn main() { let mut b = itoa::Buffer::new(); println!(\"cargo ok {}\", b.format(42)); }\n",
			}, []string{"target/release/app"}, 15*time.Minute)
		return r.err == nil && r.out.ExitCode == 0 && strings.Contains(r.log, "cargo ok 42") &&
			r.arts["./target/release/app"] != nil, r.String()
	}},
	{"toolchain-npm", func() (bool, string) {
		r := run("node:22", "npm install && node -e \"console.log('npm ok', require('left-pad')('x', 3))\"",
			map[string]string{"package.json": `{"name":"app","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}`},
			[]string{"package-lock.json"}, 10*time.Minute)
		return r.err == nil && r.out.ExitCode == 0 && strings.Contains(r.log, "npm ok") &&
			r.arts["./package-lock.json"] != nil, r.String()
	}},
	{"toolchain-pip", func() (bool, string) {
		r := run("python:3.13", "pip install --root-user-action=ignore -q requests && python -c 'import requests; print(\"pip ok\", requests.__version__)'",
			nil, nil, 10*time.Minute)
		return r.err == nil && r.out.ExitCode == 0 && strings.Contains(r.log, "pip ok"), r.String()
	}},
	{"toolchain-make-gcc", func() (bool, string) {
		r := run("gcc:14", "make && ./hello",
			map[string]string{
				"Makefile": "hello: hello.c\n\tgcc -O2 -o hello hello.c\n",
				"hello.c":  "#include <stdio.h>\nint main(void) { puts(\"make ok\"); return 0; }\n",
			}, []string{"hello"}, 10*time.Minute)
		return r.err == nil && r.out.ExitCode == 0 && strings.Contains(r.log, "make ok") && r.arts["./hello"] != nil, r.String()
	}},
}

func main() {
	pattern := flag.String("run", "", "only run checks matching this regexp")
	verbose := flag.Bool("v", false, "print details for passing checks too")
	flag.Parse()
	re := regexp.MustCompile(*pattern)
	failed := 0
	for _, c := range checks {
		if !re.MatchString(c.name) {
			continue
		}
		start := time.Now()
		ok, detail := c.fn()
		status := "PASS"
		if !ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s %s (%s)\n", status, c.name, time.Since(start).Round(time.Millisecond))
		if !ok || *verbose {
			fmt.Println(indent(detail))
		}
	}
	if failed > 0 {
		fmt.Printf("%d check(s) failed\n", failed)
		os.Exit(1)
	}
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}
