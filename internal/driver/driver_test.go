package driver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var errRe = regexp.MustCompile(`// ERROR "((?:[^"\\]|\\.)*)"`)

// TestErrors checks every `// ERROR "substring"` annotation in testdata/errors against the
// diagnostics the front end reports on that line: one diagnostic per annotation, and no others.
func TestErrors(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/errors/*.sq")
	if len(files) == 0 {
		t.Fatal("no error tests found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "main.sq"), src, 0o644); err != nil {
				t.Fatal(err)
			}
			want := map[int][]string{}
			for i, line := range strings.Split(string(src), "\n") {
				for _, m := range errRe.FindAllStringSubmatch(line, -1) {
					s, err := strconv.Unquote(`"` + m[1] + `"`)
					if err != nil {
						t.Fatalf("bad annotation on line %d: %v", i+1, err)
					}
					want[i+1] = append(want[i+1], s)
				}
			}
			err = Check(dir)
			if err == nil {
				t.Fatalf("expected errors, got none")
			}
			got := map[int][]string{}
			for _, l := range strings.Split(err.Error(), "\n") {
				parts := strings.SplitN(l, ":", 4)
				if len(parts) < 4 || !strings.HasSuffix(parts[0], "main.sq") {
					continue
				}
				n, _ := strconv.Atoi(parts[1])
				got[n] = append(got[n], strings.TrimSpace(parts[3]))
			}
			// each annotation must match its own diagnostic, and a line may have no others
			for line, subs := range want {
				left := append([]string(nil), got[line]...)
				for _, sub := range subs {
					found := false
					for i, g := range left {
						if strings.Contains(g, sub) {
							left = append(left[:i], left[i+1:]...)
							found = true
							break
						}
					}
					if !found {
						t.Errorf("line %d: want error containing %q, got %q", line, sub, got[line])
					}
				}
				if len(left) > 0 {
					t.Errorf("line %d: unexpected error(s) %q", line, left)
				}
			}
			for line, msgs := range got {
				if _, ok := want[line]; !ok {
					t.Errorf("line %d: unexpected error %q", line, msgs)
				}
			}
		})
	}
}

// TestRun builds each testdata/run program (and each example with an expected.out) and compares
// its stdout with the .out file.
// Programs whose name starts with "panic_" must exit with status 2.
func TestRun(t *testing.T) { runPrograms(t, "") }

// runPrograms is TestRun, optionally with a sanitizer: under "address" a program must also run
// without an AddressSanitizer report.
func runPrograms(t *testing.T, sanitize string) {
	files, _ := filepath.Glob("../../testdata/run/*.sq")
	// examples with an expected.out run too (servers run a scripted client against themselves)
	outs, _ := filepath.Glob("../../examples/*/expected.out")
	var examples []string
	for _, o := range outs {
		examples = append(examples, filepath.Dir(o))
	}
	for _, f := range files {
		examples = append(examples, f)
	}
	for _, f := range examples {
		name := strings.TrimSuffix(filepath.Base(f), ".sq")
		t.Run(name, func(t *testing.T) {
			dir := f
			if strings.HasSuffix(f, ".sq") {
				dir = t.TempDir()
				src, _ := os.ReadFile(f)
				os.WriteFile(filepath.Join(dir, "main.sq"), src, 0o644)
			}
			bin := filepath.Join(t.TempDir(), name)
			if _, err := Build(Options{Dir: dir, Output: bin, Sanitize: sanitize}); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin)
			if sanitize == "address" {
				cmd.Env = append(os.Environ(), "ASAN_OPTIONS="+asanOptions)
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if strings.Contains(stderr.String(), "AddressSanitizer") {
				t.Fatalf("AddressSanitizer report (the lifetime checker let an unsafe reference through?):\n%s", stderr.String())
			}
			if strings.HasPrefix(name, "panic_") {
				ee, ok := err.(*exec.ExitError)
				if !ok || ee.ExitCode() != 2 || !strings.Contains(stderr.String(), "panic:") {
					t.Errorf("want panic exit 2, got %v\nstderr: %s", err, stderr.String())
				}
			} else if err != nil {
				t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
			}
			wantFile := strings.TrimSuffix(f, ".sq") + ".out"
			if !strings.HasSuffix(f, ".sq") {
				wantFile = filepath.Join(f, "expected.out")
			}
			want, err := os.ReadFile(wantFile)
			if err != nil {
				t.Fatalf("missing expected output: %v", err)
			}
			if stdout.String() != string(want) {
				t.Errorf("output mismatch\n--- got\n%s\n--- want\n%s", stdout.String(), want)
			}
		})
	}
}

// TestStdlibDocCurrent keeps docs/stdlib.md in sync with the standard library.
func TestStdlibDocCurrent(t *testing.T) {
	want, err := os.ReadFile("../../docs/stdlib.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Doc("std", "")
	if err != nil {
		t.Fatal(err)
	}
	// the generated API is exactly the ```go block (gen.sh adds a trailing newline)
	doc := string(want)
	start := strings.Index(doc, "```go\n")
	end := strings.LastIndex(doc, "```")
	if start < 0 || end <= start {
		t.Fatal("docs/stdlib.md has no ```go block")
	}
	if block := doc[start+len("```go\n") : end]; strings.TrimRight(block, "\n") != strings.TrimRight(got, "\n") {
		t.Errorf("docs/stdlib.md is stale: run docs/gen.sh")
	}
}

// TestImports covers local import resolution: packages are found by directory, never by name or
// path suffix, and a local package never stands in for a std one.
func TestImports(t *testing.T) {
	tests := []struct {
		name    string
		main    string // directory of the main package, relative to the tree root
		chdir   bool   // build with Dir "." from inside main
		files   map[string]string
		want    string // stdout, or
		wantErr string // a substring of the build error
	}{
		{
			name: "suffix-alike dirs",
			main: "app",
			files: map[string]string{
				"app/main.sq":    "package main\n\nimport (\n\t\"./mylib\"\n\t\"./lib\"\n\tl2 \"./lib\"\n)\n\nfunc main() { println(mylib.Name(), lib.Name(), l2.Name()) }\n",
				"app/mylib/a.sq": "package mylib\n\nfunc Name() string { return \"mylib\" }\n",
				"app/lib/a.sq":   "package lib\n\nfunc Name() string { return \"lib\" }\n",
			},
			want: "mylib lib lib\n",
		},
		{
			name: "parent dir and shared package",
			main: "app",
			files: map[string]string{
				"app/main.sq": "package main\n\nimport (\n\t\"../util\"\n\t\"./b\"\n)\n\nfunc main() { b.Do(); util.Inc(); println(util.N) }\n",
				"app/b/b.sq":  "package b\n\nimport \"../../util\"\n\nfunc Do() { util.Inc() }\n",
				"util/u.sq":   "package util\n\nvar N int\n\nfunc Inc() { N++ }\n",
			},
			want: "2\n",
		},
		{
			name:  "local package named like std",
			main:  "app",
			chdir: true,
			files: map[string]string{
				"app/main.sq":  "package main\n\nimport (\n\tmyio \"./io\"\n\t\"os\"\n)\n\nfunc main() { os.Stdout.WriteString(myio.Hello()) }\n",
				"app/io/io.sq": "package io\n\nfunc Hello() string { return \"hi\\n\" }\n",
			},
			want: "hi\n",
		},
		{
			name: "duplicate import name",
			main: "app",
			files: map[string]string{
				"app/main.sq":      "package main\n\nimport (\n\t\"strings\"\n\t\"./strings\"\n)\n\nfunc main() { println(strings.Contains(\"abc\", \"z\")) }\n",
				"app/strings/s.sq": "package strings\n\nfunc Contains(a, b string) bool { return true }\n",
			},
			wantErr: `strings redeclared in this block (also imported from "strings")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, src := range tc.files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(root, tc.main)
			if tc.chdir {
				t.Chdir(dir)
				dir = "."
			}
			bin := filepath.Join(t.TempDir(), "prog")
			_, err := Build(Options{Dir: dir, Output: bin})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin).Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Errorf("got %q, want %q", out, tc.want)
			}
		})
	}
}

// TestHTTP runs testdata/http and checks response framing around the write buffer's size, and that
// hostile requests don't take the server down.
func TestHTTP(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "server")
	if _, err := Build(Options{Dir: "../../testdata/http", Output: bin}); err != nil {
		t.Fatal(err)
	}
	freeAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	addr, addr2 := freeAddr(), freeAddr()
	var stderr bytes.Buffer
	cmd := exec.Command(bin, addr, addr2)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			if c, err = net.Dial("tcp", addr2); err == nil {
				c.Close()
				break
			}
		}
		if i == 100 {
			t.Fatalf("server did not start: %v\n%s", err, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	alive := func() {
		t.Helper()
		resp, err := http.Get("http://" + addr + "/n?n=1")
		if err != nil {
			t.Fatalf("server died: %v\nstderr: %s", err, stderr.String())
		}
		resp.Body.Close()
	}

	get := func(url string) string {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("%s: %v\nstderr: %s", url, err, stderr.String())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	const wbuf = 16384 // http.WriteBufferSize
	for _, path := range []string{"/n", "/p"} {
		for _, n := range []int{0, 1, 1000, wbuf - 8, wbuf - 7, wbuf - 6, wbuf - 1, wbuf, wbuf + 1, 20000, 2 * wbuf, 2*wbuf + 3, 100000} {
			resp, err := http.Get(fmt.Sprintf("http://%s%s?n=%d", addr, path, n))
			if err != nil {
				t.Fatalf("%s n=%d: %v\nstderr: %s", path, n, err, stderr.String())
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("%s n=%d: reading body: %v", path, n, err)
			}
			if len(got) != n {
				t.Fatalf("%s n=%d: got %d bytes", path, n, len(got))
			}
			for i, b := range got {
				if b != byte('a'+i%26) {
					t.Fatalf("%s n=%d: byte %d is %q", path, n, i, b)
				}
			}
		}
	}

	// HEAD: the GET length, no body, and the connection stays usable
	for _, n := range []int{10, 20000} {
		resp, err := http.Head(fmt.Sprintf("http://%s/n?n=%d", addr, n))
		if err != nil {
			t.Fatalf("HEAD n=%d: %v", n, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.ContentLength != int64(n) || len(b) != 0 {
			t.Errorf("HEAD n=%d: Content-Length %d, %d body bytes", n, resp.ContentLength, len(b))
		}
		if got := get("http://" + addr + "/who"); got != "one" {
			t.Errorf("after HEAD n=%d: got %q", n, got)
		}
	}

	// a Content-Length near MaxInt must get 413, not overflow the size check
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 9223372036854775807\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, _ := io.ReadAll(c)
	c.Close()
	if !strings.Contains(string(reply), "413") {
		t.Errorf("huge Content-Length: got %q, want a 413", reply)
	}
	alive()

	// two servers in one process, each keeping its own handler across idle parking (100ms)
	for round := 0; round < 3; round++ {
		if got := get("http://" + addr + "/who"); got != "one" {
			t.Errorf("round %d: server 1 said %q", round, got)
		}
		if got := get("http://" + addr2 + "/who"); got != "two" {
			t.Errorf("round %d: server 2 said %q", round, got)
		}
		time.Sleep(300 * time.Millisecond) // keep-alive connections park in the idle set
	}
}

// TestSourceEncoding: a leading BOM is ignored and invalid UTF-8 is an error, as in Go.
func TestSourceEncoding(t *testing.T) {
	write := func(src string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if err := Check(write("\xEF\xBB\xBFpackage main\n\nfunc main() {}\n")); err != nil {
		t.Errorf("BOM: %v", err)
	}
	for _, src := range []string{
		"package main\n\nfunc main() { s := \"\xff\"; println(s) }\n",
		"package main\n\nfunc main() { s := `\xff`; println(s) }\n",
		"package main\n\n// \xff\nfunc main() {}\n",
	} {
		err := Check(write(src))
		if err == nil || !strings.Contains(err.Error(), "invalid UTF-8 encoding") {
			t.Errorf("%q: want invalid UTF-8 error, got %v", src, err)
		}
	}
}

// TestShortWrite: a write that stops early (here: file size limit) reports an error.
func TestShortWrite(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import "os"

var big [1M]byte

func main() {
	n, err := os.Stdout.WriteString(string(big[:]))
	if err != nil && n < len(big) {
		os.Stderr.WriteString("WriteString: ")
		os.Stderr.WriteString(err.Error())
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `trap '' XFSZ; ulimit -f 100; "$0" > "$1"`, bin, filepath.Join(dir, "out"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Run()
	if !strings.Contains(stderr.String(), "WriteString: short write") {
		t.Errorf("want a short write error, got stderr %q", stderr.String())
	}
}

// TestStdinDoesNotBlock: a goroutine waiting on an idle stdin pipe parks alone; others keep running.
func TestStdinDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"os"
	"time"
)

var buf [64]byte

task reader() pool 1 {
	n, _ := os.Stdin.Read(buf[:])
	println("read", n)
}

func main() {
	go reader()
	for i := range 3 {
		time.Sleep(20 * time.Millisecond)
		println("tick", i)
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe() // stays open and empty while the program runs
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || stdout.String() != "tick 0\ntick 1\ntick 2\n" {
			t.Errorf("got %q, %v", stdout.String(), err)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Errorf("program blocked on stdin; output so far %q", stdout.String())
	}
}

// TestBudget: static data counts against the declared budget, and unreadable sizes are an error.
func TestBudget(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nimport \"os\"\n\nbudget 16M\n\nvar big [64 << 20]byte\n\nfunc main() {\n\tbig[os.NArgs()*1000] = 1\n\tprintln(big[os.NArgs()*999])\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Build(Options{Dir: dir, Output: filepath.Join(dir, "prog")})
	if err == nil || !strings.Contains(err.Error(), "memory budget exceeded") {
		t.Errorf("want budget error, got %v", err)
	}
	if _, _, err := segmentSizes(filepath.Join(dir, "main.sq")); err == nil {
		t.Errorf("segmentSizes on a text file: want an error")
	}

	// with workers N everything but code counts N times: 6 MiB fits a 16 MiB budget, 4 workers don't
	src = "package main\n\nimport \"os\"\n\nbudget 16M\nworkers WORKERS\n\nvar big [6 << 20]byte\n\nfunc main() {\n\tbig[os.NArgs()*1000] = 1\n\tprintln(big[os.NArgs()*999])\n}\n"
	for _, tc := range []struct {
		workers string
		fits    bool
	}{{"1", true}, {"4", false}} {
		if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(strings.Replace(src, "WORKERS", tc.workers, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Build(Options{Dir: dir, Output: filepath.Join(dir, "prog")})
		if tc.fits && err != nil || !tc.fits && (err == nil || !strings.Contains(err.Error(), "memory budget exceeded")) {
			t.Errorf("workers %s: %v", tc.workers, err)
		}
	}
}

// TestPackageC: a package's C files see its own headers, failures in them name the package, and
// CC may be a command with arguments.
func TestPackageC(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"app/main.sq":      "package main\n\nimport \"./util\"\n\nfunc main() { println(util.Add(1, 2)) }\n",
		"app/util/util.sq": "package util\n\nextern \"util_add\" func Add(a, b int) int stack 1K\n",
		"app/util/shim.h":  "#include <stdint.h>\n#define SHIM_BIAS 100\n",
		"app/util/shim.c":  "#include \"shim.h\"\nint64_t util_add(int64_t a, int64_t b) { return a + b + SHIM_BIAS; }\n",
	}
	for name, src := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := filepath.Join(root, "app")
	bin := filepath.Join(root, "prog")
	for _, cc := range []string{"", "clang -DUNUSED=1"} {
		if _, err := Build(Options{Dir: app, Output: bin, CC: cc}); err != nil {
			t.Fatalf("CC=%q: %v", cc, err)
		}
		if out, err := exec.Command(bin).Output(); err != nil || string(out) != "103\n" {
			t.Errorf("CC=%q: got %q, %v", cc, out, err)
		}
	}
	if _, err := Build(Options{Dir: app, Output: bin, CC: "no-such-cc"}); err == nil || !strings.Contains(err.Error(), `C compiler "no-such-cc" not found`) {
		t.Errorf("missing CC: %v", err)
	}
	os.WriteFile(filepath.Join(app, "util", "shim.c"), []byte("int64_t util_add(oops"), 0o644)
	_, err := Build(Options{Dir: app, Output: bin})
	if err == nil || !strings.Contains(err.Error(), "C code in package util") || strings.Contains(err.Error(), "squark compiler bug") {
		t.Errorf("broken package C: %v", err)
	}
}

// TestErrorPositions: errors point where the statement or expression starts, as Go's do
// (TestErrors compares lines only).
func TestErrorPositions(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nfunc f() (int, int) { return 1, 2 }\n\nfunc main() {\n\taaaa, bbbb, cccc := f()\n\tprintln(aaaa, bbbb, cccc)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Check(dir)
	if err == nil || !strings.Contains(err.Error(), "main.sq:6:2: assignment mismatch") {
		t.Errorf("got %v", err)
	}
}

func TestDocUnknownPackage(t *testing.T) {
	if _, err := Doc("std", "nosuchpkg"); err == nil || !strings.Contains(err.Error(), "no package nosuchpkg") {
		t.Errorf("got %v", err)
	}
	if out, err := Doc("std", "strings"); err != nil || !strings.Contains(out, "package strings") {
		t.Errorf("strings: %v", err)
	}
}

// TestExamplesBuild compiles every example and benchmark program; TestRun runs hello and pipeline,
// and TestNotes drives the notes server. The other servers listen on fixed ports, so they are
// only built here.
func TestExamplesBuild(t *testing.T) {
	dirs, _ := filepath.Glob("../../examples/*")
	bench, _ := filepath.Glob("../../bench/squark/*")
	for _, dir := range append(dirs, bench...) {
		if _, err := os.Stat(filepath.Join(dir, "main.sq")); err != nil {
			continue
		}
		t.Run(filepath.Base(dir), func(t *testing.T) {
			if _, err := Build(Options{Dir: dir, Output: filepath.Join(t.TempDir(), "prog")}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestNotes runs examples/notes on a free port with a fresh database.
func TestNotes(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "notes")
	if _, err := Build(Options{Dir: "../../examples/notes", Output: bin}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-port", strconv.Itoa(port), "-db", filepath.Join(dir, "notes.db"))
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v\nstderr: %s", method, path, err, stderr.String())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", base[len("http://"):]); err == nil {
			c.Close()
			break
		}
		if i == 100 {
			t.Fatalf("notes did not start: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, body := do("GET", "/health", ""); code != 200 {
		t.Fatalf("health: %d %q", code, body)
	}
	code, body := do("POST", "/notes", `{"title": "hi", "body": "first \"note\""}`)
	if code != 201 || !strings.Contains(body, `"id":1`) {
		t.Fatalf("create: %d %q", code, body)
	}
	if code, body := do("GET", "/notes/1", ""); code != 200 || !strings.Contains(body, `"title":"hi"`) || !strings.Contains(body, `first \"note\"`) {
		t.Errorf("get: %d %q", code, body)
	}
	if code, body := do("POST", "/notes", `{"title": "`+strings.Repeat("t", 201)+`", "body": ""}`); code != 400 {
		t.Errorf("create with a title over 200 bytes: %d %q", code, body)
	}
	if code, _ := do("DELETE", "/notes/1", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do("GET", "/notes/1", ""); code != 404 {
		t.Errorf("get after delete: %d", code)
	}
	if code, _ := do("DELETE", "/notes/1", ""); code != 404 {
		t.Errorf("delete again: %d", code)
	}
	code, body = do("POST", "/notes", `{"title": "`+strings.Repeat("t", 200)+`", "body": "second"}`)
	if code != 201 || !strings.Contains(body, `"id":1`) { // the table was empty: SQLite reuses rowid 1
		t.Errorf("create with a 200-byte title: %d %q", code, body)
	}
}

// TestStaleFdEvent: after WaitReadable times out, data arriving later must not wake the goroutine
// from an unrelated wait (here time.Sleep). On Linux the epoll registration has to be disarmed.
func TestStaleFdEvent(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"net"
	"os"
	"time"
)

task writer(c net.Conn) pool 1 {
	time.Sleep(100 * time.Millisecond)
	c.WriteString("x")
}

func main() {
	l, err := net.Listen(os.Arg(1))
	if err != nil {
		panic(err)
	}
	client, err := net.Dial(os.Arg(1))
	if err != nil {
		panic(err)
	}
	server, err := l.Accept()
	if err != nil {
		panic(err)
	}
	println(server.WaitReadable(30 * time.Millisecond))
	go writer(client)
	start := time.Now()
	time.Sleep(400 * time.Millisecond)
	println(time.Since(start) >= 390*time.Millisecond)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	out, err := exec.Command(bin, addr).CombinedOutput()
	if err != nil || string(out) != "false\ntrue\n" {
		t.Errorf("got %q, %v (want the sleep to last its full time)", out, err)
	}
}

// TestNoHeapAtRuntime runs testdata/heapcount/prog with an allocation counter injected and checks
// that nothing allocates after startup: not the runtime, not the std packages, and not the libc
// functions they call (glibc's strerror and macOS's first gmtime_r both used to).
func TestNoHeapAtRuntime(t *testing.T) {
	var counter, env string
	switch runtime.GOOS {
	case "darwin":
		counter, env = "count_darwin.c", "DYLD_INSERT_LIBRARIES"
	case "linux":
		counter, env = "count_linux.c", "LD_PRELOAD"
	default:
		t.Skip("no allocation counter for " + runtime.GOOS)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "count.so")
	if out, err := exec.Command("clang", "-O2", "-shared", "-fPIC", "-o", lib, filepath.Join("../../testdata/heapcount", counter)).CombinedOutput(); err != nil {
		t.Fatalf("building the counter: %v\n%s", err, out)
	}
	src, err := os.ReadFile("../../testdata/heapcount/prog/main.sq")
	if err != nil {
		t.Fatal(err)
	}
	// once as one scheduler thread, once as four: each shard then runs the whole program
	for _, threads := range []int{1, 4} {
		t.Run(fmt.Sprintf("threads=%d", threads), func(t *testing.T) {
			noHeapRun(t, dir, lib, env, src, threads)
		})
	}
}

func noHeapRun(t *testing.T, dir, lib, env string, src []byte, threads int) {
	pdir := filepath.Join(dir, fmt.Sprintf("prog%d", threads))
	os.MkdirAll(pdir, 0o755)
	os.WriteFile(filepath.Join(pdir, "main.sq"), src, 0o644)
	os.WriteFile(filepath.Join(pdir, "threads.sq"), []byte(fmt.Sprintf("package main\n\nthreads %d\n", threads)), 0o644)
	bin := filepath.Join(pdir, "prog")
	if _, err := Build(Options{Dir: pdir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	counts := filepath.Join(pdir, "counts")
	cmd := exec.Command(bin, free(), free(), filepath.Join(pdir, "file.txt"), free())
	cmd.Stdin = strings.NewReader("from stdin\n") // os/exec feeds this through a pipe
	cmd.Env = append(os.Environ(), env+"="+lib, "HEAPCOUNT_FILE="+counts)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// the first launch of a fresh binary can be slow on macOS: wait for the counter's first report
	// (100ms after it loads), which comes well inside the program's initial 400ms pause
	var before []byte
	for i := 0; i < 500 && len(before) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		before, _ = os.ReadFile(counts)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	after, _ := os.ReadFile(counts)
	if len(before) == 0 || out.String() != strings.Repeat("true\n", threads) {
		t.Fatalf("counter or program failed: counts %q, output %q", before, out.String())
	}
	if string(before) != string(after) {
		t.Errorf("heap allocations after startup: %q before the work, %q after", before, after)
	}
}

// TestWorkers runs testdata/workers (workers 4): four processes serve one address, a stop signal
// or a crashing worker stops them all, and on Linux the kernel spreads connections across them.
func TestWorkers(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "workers")
	res, err := Build(Options{Dir: "../../testdata/workers", Output: bin})
	if err != nil {
		t.Fatal(err)
	}
	if res.Memory.Workers != 4 {
		t.Errorf("memory report: workers %d, want 4", res.Memory.Workers)
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	start := func() (*exec.Cmd, string, *bytes.Buffer, []int) {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		cmd := exec.Command(bin, addr)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var kids []int
		for i := 0; i < 200 && len(kids) < 4; i++ {
			time.Sleep(20 * time.Millisecond)
			out, _ := exec.Command("pgrep", "-P", strconv.Itoa(cmd.Process.Pid)).Output()
			kids = kids[:0]
			for _, f := range strings.Fields(string(out)) {
				n, _ := strconv.Atoi(f)
				kids = append(kids, n)
			}
		}
		if len(kids) != 4 {
			cmd.Process.Kill()
			t.Fatalf("want 4 worker processes, found %v", kids)
		}
		for i := 0; ; i++ {
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				break
			}
			if i == 200 {
				t.Fatalf("workers did not start serving: %s", stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		return cmd, addr, &stderr, kids
	}
	wait := func(cmd *exec.Cmd) int {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("supervisor did not exit")
		}
		return cmd.ProcessState.ExitCode()
	}
	gone := func(kids []int) {
		t.Helper()
		for i := 0; i < 100; i++ {
			left := 0
			for _, k := range kids {
				if alive(k) {
					left++
				}
			}
			if left == 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("worker processes still running: %v", kids)
	}

	// every worker serves; the kernel spreads new connections on Linux
	cmd, addr, _, kids := start()
	pids := map[string]bool{}
	for i := 0; i < 200; i++ {
		tr := &http.Transport{DisableKeepAlives: true}
		resp, err := (&http.Client{Transport: tr}).Get("http://" + addr + "/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		pids[string(b)] = true
	}
	for p := range pids {
		n, _ := strconv.Atoi(p)
		if !slicesContains(kids, n) {
			t.Errorf("response from pid %s, not one of the workers %v", p, kids)
		}
	}
	if runtime.GOOS == "linux" && len(pids) < 2 {
		t.Errorf("200 connections all reached one worker: %v", pids)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if code := wait(cmd); code != 128+int(syscall.SIGTERM) {
		t.Errorf("after SIGTERM: exit status %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	gone(kids)

	// a failing worker stops the program with its status
	cmd, addr, stderr, kids := start()
	http.Get("http://" + addr + "/crash")
	if code := wait(cmd); code != 3 {
		t.Errorf("after a worker crash: exit status %d, want 3", code)
	}
	if !strings.Contains(stderr.String(), "exited with status 3; stopping the other workers") {
		t.Errorf("stderr: %q", stderr.String())
	}
	gone(kids)

	// Linux: workers stop when the supervisor is killed outright
	if runtime.GOOS == "linux" {
		cmd, _, _, kids := start()
		cmd.Process.Kill()
		cmd.Wait()
		gone(kids)
	}
}

func slicesContains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestWorkersSQLite: workers setting up one new SQLite database at the same moment all succeed
// (SQLite reports "database is locked" there without waiting; the std package retries).
func TestWorkersSQLite(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: "../../testdata/workers_sqlite", Output: bin}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		db := filepath.Join(dir, fmt.Sprintf("run%d.db", i))
		if out, err := exec.Command(bin, db).CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
		rows, err := exec.Command("sqlite3", db, "SELECT count(DISTINCT pid) FROM seen").Output()
		if err != nil {
			t.Skipf("sqlite3 command not available to check the rows: %v", err)
		}
		if strings.TrimSpace(string(rows)) != "4" {
			t.Fatalf("run %d: %s distinct workers wrote a row, want 4", i, rows)
		}
	}
}

// TestThreads runs testdata/threads (threads 4): four scheduler threads in one process serve one
// address, each shard counts its own requests in a package variable (so counts per shard must run
// 1, 2, 3, ... with no repeats or gaps, under concurrent load), and stop signals and panics end the
// whole process.
func TestThreads(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "threads")
	res, err := Build(Options{Dir: "../../testdata/threads", Output: bin})
	if err != nil {
		t.Fatal(err)
	}
	if res.Memory.Threads != 4 {
		t.Errorf("memory report: threads %d, want 4", res.Memory.Threads)
	}
	start := func() (*exec.Cmd, string, *bytes.Buffer) {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		cmd := exec.Command(bin, addr)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				break
			}
			if i == 200 {
				cmd.Process.Kill()
				t.Fatalf("server did not start: %s", stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		return cmd, addr, &stderr
	}
	wait := func(cmd *exec.Cmd) int {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Fatal("process did not exit")
		}
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()) // killed by the signal, as a shell reports it
		}
		return cmd.ProcessState.ExitCode()
	}

	cmd, addr, stderr := start()
	var mu sync.Mutex
	counts := map[string][]int{} // shard -> the counts it reported
	record := func(body string) {
		shard, n, ok := strings.Cut(body, ":")
		k, err := strconv.Atoi(n)
		if !ok || err != nil {
			t.Errorf("bad response %q", body)
			return
		}
		mu.Lock()
		counts[shard] = append(counts[shard], k)
		mu.Unlock()
	}
	get := func(cl *http.Client) {
		resp, err := cl.Get("http://" + addr + "/")
		if err != nil {
			t.Errorf("%v\nstderr: %s", err, stderr.String())
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		record(string(b))
	}
	for i := 0; i < 200; i++ { // fresh connections: spread across shards
		get(&http.Client{Transport: &http.Transport{DisableKeepAlives: true}})
	}
	var wg sync.WaitGroup
	for w := 0; w < 64; w++ { // concurrent keep-alive load
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
			for i := 0; i < 100; i++ {
				get(cl)
			}
		}()
	}
	wg.Wait()
	if len(counts) < 2 {
		t.Errorf("connections reached %d shard(s), want several: %v", len(counts), len(counts))
	}
	total := 0
	for shard, ks := range counts {
		sort.Ints(ks)
		for i, k := range ks {
			if k != i+1 {
				t.Fatalf("shard %s: counts %v... are not 1..%d: per-shard state was shared or raced", shard, ks[:min(len(ks), 10)], len(ks))
			}
		}
		total += len(ks)
	}
	if total != 200+64*100 {
		t.Errorf("%d responses, want %d", total, 200+64*100)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if code := wait(cmd); code != 128+int(syscall.SIGTERM) {
		t.Errorf("after SIGTERM: exit status %d", code)
	}

	// each shard listens on an address once; a second Listen from the same shard is "in use"
	lbin := filepath.Join(dir, "listen")
	if _, err := Build(Options{Dir: "../../testdata/threads_listen", Output: lbin}); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	laddr := l.Addr().String()
	l.Close()
	if out, err := exec.Command(lbin, laddr).CombinedOutput(); err != nil || string(out) != "ok in-use relisten-ok\nok in-use relisten-ok\n" {
		t.Errorf("listen semantics with threads 2: %v\n%s", err, out)
	}

	// a panic on any shard ends the process
	cmd, addr, stderr = start()
	http.Get("http://" + addr + "/crash")
	if code := wait(cmd); code != 2 || !strings.Contains(stderr.String(), "panic: crash requested") {
		t.Errorf("after a panic: exit status %d, stderr %q", code, stderr.String())
	}
}

// TestThreadsRace builds testdata/threads (4 scheduler threads) with ThreadSanitizer and serves
// concurrent load: shards share no mutable state, so TSan must report nothing.
func TestThreadsRace(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "threads")
	if _, err := Build(Options{Dir: "../../testdata/threads", Output: bin, Sanitize: "thread"}); err != nil {
		t.Skipf("no ThreadSanitizer build here: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, addr)
	cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i == 300 {
			t.Fatalf("server did not start: %s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
			for i := 0; i < 100; i++ {
				resp, err := cl.Get("http://" + addr + "/")
				if err != nil {
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < 100; i++ { // fresh connections: accepts on every shard
		if resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get("http://" + addr + "/"); err == nil {
			resp.Body.Close()
		}
	}
	wg.Wait()
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
	if strings.Contains(stderr.String(), "ThreadSanitizer") {
		t.Errorf("ThreadSanitizer reports:\n%s", stderr.String())
	}
}

// TestSharedChanRace runs testdata/run/shared_chan.sq (4 threads exchanging values over shared
// channels) and select_shared.sq (select over them, with timeouts) under ThreadSanitizer: the
// channel locks, select claims and cross-shard wakeups must be race-free.
func TestSharedChanRace(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	// shared channels, and select over them with timeouts (select_shared.sq)
	for _, name := range []string{"shared_chan", "select_shared", "shared_chan_array"} {
		dir := t.TempDir()
		src, err := os.ReadFile("../../testdata/run/" + name + ".sq")
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile("../../testdata/run/" + name + ".out")
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "main.sq"), src, 0o644)
		bin := filepath.Join(dir, "prog")
		if _, err := Build(Options{Dir: dir, Output: bin, Sanitize: "thread"}); err != nil {
			t.Skipf("no ThreadSanitizer build here: %v", err)
		}
		for i := 0; i < 5; i++ {
			cmd := exec.Command(bin)
			cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || stdout.String() != string(want) || strings.Contains(stderr.String(), "ThreadSanitizer") {
				t.Fatalf("%s run %d: %v, output %q\n%s", name, i, err, stdout.String(), stderr.String())
			}
		}
	}
}

// TestThreadsEnv: SQUARK_THREADS sets the thread count of a program written for threads, like
// GOMAXPROCS; it is ignored without a threads directive and capped by a budget's proven count.
func TestThreadsEnv(t *testing.T) {
	dir := t.TempDir()
	build := func(name, directives string) string {
		t.Helper()
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o755)
		src := "package main\n\nimport \"runtime\"\n\n" + directives + "\n\nfunc main() {\n\tif runtime.Shard() == 0 {\n\t\tprintln(runtime.Shards())\n\t}\n}\n"
		os.WriteFile(filepath.Join(d, "main.sq"), []byte(src), 0o644)
		bin := filepath.Join(d, "prog")
		if _, err := Build(Options{Dir: d, Output: bin}); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	run := func(bin, env string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "SQUARK_THREADS="+env)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v\n%s", err, stderr.String())
		}
		return strings.TrimSpace(stdout.String()), stderr.String()
	}
	auto := build("auto", "threads auto")
	if got, _ := run(auto, ""); got != strconv.Itoa(runtime.NumCPU()) {
		t.Errorf("threads auto: %s threads, want %d (the cores)", got, runtime.NumCPU())
	}
	if got, _ := run(auto, "3"); got != "3" {
		t.Errorf("threads auto, SQUARK_THREADS=3: %s threads", got)
	}
	if got, warn := run(auto, "lots"); got != strconv.Itoa(runtime.NumCPU()) || !strings.Contains(warn, "ignoring SQUARK_THREADS") {
		t.Errorf("bad SQUARK_THREADS: %s threads, stderr %q", got, warn)
	}
	if got, _ := run(build("none", ""), "3"); got != "1" {
		t.Errorf("no threads directive, SQUARK_THREADS=3: %s threads, want 1 (not written for threads)", got)
	}
	if got, _ := run(build("fixed", "threads 2"), "5"); got != "5" {
		t.Errorf("threads 2, SQUARK_THREADS=5: %s threads", got)
	}
	if got, warn := run(build("budget", "budget 512M\nthreads 2"), "5"); got != "2" || !strings.Contains(warn, "memory budget was proven for") {
		t.Errorf("threads 2 with a budget, SQUARK_THREADS=5: %s threads, stderr %q", got, warn)
	}
}

// TestPerThreadNote: with threads, the front end lists the package variables each scheduler thread
// has its own copy of and the program changes (testdata/perthread says which should be listed).
func TestPerThreadNote(t *testing.T) {
	note, err := CheckNotes("../../testdata/perthread")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"stats: changed by method bump in main",
		"jobs: sent on in main (a channel",
		"seen: passed to a function in main (a map",
		"cell: address taken in main",
		"buf: sliced (a writable view) in main",
		"Total: written in Add",
		"vec: sliced (a writable view) in main",
		"aliased: aliased in main",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	for _, not := range []string{"limit", "thread:", "mine", "all:", "strings"} {
		if strings.Contains(note, not) {
			t.Errorf("note lists %q:\n%s", not, note)
		}
	}
	// without a threads directive there is one copy of everything: no note
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte("package main\n\nvar n int\n\nfunc main() {\n\tn++\n\tprintln(n)\n}\n"), 0o644)
	if note, err := CheckNotes(dir); err != nil || note != "" {
		t.Errorf("no threads: note %q, err %v", note, err)
	}
}

// TestBlocking runs testdata/blocking: blocking externs run on helper threads, so a slow C call
// neither stops the other goroutines of its thread nor waits for another slow call. It runs on
// one thread, on four, and on four under ThreadSanitizer.
func TestBlocking(t *testing.T) {
	src := "../../testdata/blocking"
	dir := t.TempDir()
	for _, n := range []string{"main.sq", "slow.c"} {
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	run := func(name, sanitize string, copies int) {
		t.Helper()
		bin := filepath.Join(dir, name)
		if _, err := Build(Options{Dir: dir, Output: bin, Sanitize: sanitize}); err != nil {
			if sanitize != "" {
				t.Logf("no ThreadSanitizer build here: %v", err)
				return
			}
			t.Fatal(err)
		}
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		out := stdout.String()
		if err != nil || strings.Count(out, "ok\n") != copies || strings.Count(out, "600\n") != copies ||
			strings.Count(out, "7 7\n") != copies || strings.Contains(stderr.String(), "ThreadSanitizer") {
			t.Fatalf("%s: %v\n%s\n%s", name, err, out, stderr.String())
		}
		// slow calls overlap and don't stop the ticker (the program says so otherwise)
		if strings.Contains(out, "did not overlap") || strings.Contains(out, "were blocked") {
			t.Errorf("%s:\n%s", name, out)
		}
	}
	run("one", "", 1)
	os.WriteFile(filepath.Join(dir, "threads.sq"), []byte("package main\n\nthreads 4\n"), 0o644)
	run("four", "", 4)
	if !testing.Short() {
		run("race", "thread", 4)
	}
}

// TestSQLiteThreads runs testdata/sqlite_threads: 4 threads × 8 goroutines insert into one database
// and read each row back. Locked writers wait on helper threads while the connection lock keeps
// other goroutines out of their statement; every read must match and every row must be there.
func TestSQLiteThreads(t *testing.T) {
	dir := t.TempDir()
	run := func(name, sanitize string, runs int) {
		t.Helper()
		bin := filepath.Join(dir, name)
		if _, err := Build(Options{Dir: "../../testdata/sqlite_threads", Output: bin, Sanitize: sanitize}); err != nil {
			if sanitize != "" {
				t.Logf("no ThreadSanitizer build here: %v", err)
				return
			}
			t.Fatal(err)
		}
		for i := 0; i < runs; i++ {
			db := filepath.Join(dir, fmt.Sprintf("%s%d.db", name, i))
			cmd := exec.Command(bin, db)
			cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || stdout.String() != "wrong reads: 0\n" || strings.Contains(stderr.String(), "ThreadSanitizer") {
				t.Fatalf("%s run %d: %v\n%s\n%s", name, i, err, stdout.String(), stderr.String())
			}
			rows, err := exec.Command("sqlite3", db, "SELECT count(*), count(DISTINCT v) FROM t").Output()
			if err != nil {
				continue // no sqlite3 command to count with
			}
			if strings.TrimSpace(string(rows)) != "3200|3200" {
				t.Fatalf("%s run %d: rows %s, want 3200|3200", name, i, rows)
			}
		}
	}
	run("prog", "", 5)
	if !testing.Short() {
		run("race", "thread", 1)
	}
}

// TestSQLiteSlow runs testdata/sqlite_slow: a slow query and a slow write inside a transaction move
// to a helper thread mid-call (sq_detach), so a ticker on the same thread keeps running; also under
// ThreadSanitizer, since the goroutine changes OS threads in the middle of SQLite.
func TestSQLiteSlow(t *testing.T) {
	dir := t.TempDir()
	want := "2000001000000\nslow read ok\nslow write in a transaction ok\n1000000 500000500000\n"
	run := func(name, sanitize string) {
		t.Helper()
		bin := filepath.Join(dir, name)
		if _, err := Build(Options{Dir: "../../testdata/sqlite_slow", Output: bin, Sanitize: sanitize}); err != nil {
			if sanitize != "" {
				t.Logf("no ThreadSanitizer build here: %v", err)
				return
			}
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			cmd := exec.Command(bin, filepath.Join(dir, fmt.Sprintf("%s%d.db", name, i)))
			cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || stdout.String() != want || strings.Contains(stderr.String(), "ThreadSanitizer") {
				t.Fatalf("%s run %d: %v\n%s\n%s", name, i, err, stdout.String(), stderr.String())
			}
		}
	}
	run("prog", "")
	if !testing.Short() {
		run("race", "thread")
	}
}

// TestDetach runs testdata/detach: C code moves its goroutine to a helper thread mid-call and back
// (sq_detach, sq_reattach), without blocking externs; on one thread, four, and under TSan.
func TestDetach(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"main.sq", "crunch.c"} {
		b, err := os.ReadFile(filepath.Join("../../testdata/detach", n))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, n), b, 0o644)
	}
	run := func(name, sanitize string, copies int) {
		t.Helper()
		bin := filepath.Join(dir, name)
		if _, err := Build(Options{Dir: dir, Output: bin, Sanitize: sanitize}); err != nil {
			if sanitize != "" {
				t.Logf("no ThreadSanitizer build here: %v", err)
				return
			}
			t.Fatal(err)
		}
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if out := stdout.String(); err != nil || out != strings.Repeat("moved and back: 2\n", copies) || strings.Contains(stderr.String(), "ThreadSanitizer") {
			t.Fatalf("%s: %v\n%s\n%s", name, err, out, stderr.String())
		}
	}
	run("one", "", 1)
	os.WriteFile(filepath.Join(dir, "threads.sq"), []byte("package main\n\nthreads 4\n"), 0o644)
	run("four", "", 4)
	if !testing.Short() {
		run("race", "thread", 4)
	}
}

// TestPreempt runs testdata/preempt: a preemptible task's loop yields to a ticker on its thread, an
// ordinary task's loop can't be interrupted (rule 10) and the watchdog names it, and Gosched yields.
// A goroutine running alone gets no warning: nothing waits for it. Also under TSan (the watchdog
// thread reads the schedulers' state).
func TestPreempt(t *testing.T) {
	dir := t.TempDir()
	want := "preemptible: ticker kept running true\nordinary task: ticks while it ran 0\nGosched: ticker kept running true\n"
	run := func(src, name, sanitize string) (string, string) {
		t.Helper()
		bin := filepath.Join(dir, name)
		if _, err := Build(Options{Dir: src, Output: bin, Sanitize: sanitize}); err != nil {
			if sanitize != "" {
				t.Logf("no ThreadSanitizer build here: %v", err)
				return "", ""
			}
			t.Fatal(err)
		}
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "TSAN_OPTIONS=halt_on_error=0 report_signal_unsafe=0")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, stderr.String())
		}
		return stdout.String(), stderr.String()
	}
	for _, sanitize := range []string{"", "thread"} {
		if sanitize != "" && testing.Short() {
			continue
		}
		out, errs := run("../../testdata/preempt", "prog"+sanitize, sanitize)
		if sanitize != "" && out == "" {
			continue
		}
		if out != want || strings.Contains(errs, "ThreadSanitizer") {
			t.Fatalf("%s: got\n%s\nwant\n%s\n%s", sanitize, out, want, errs)
		}
		if !strings.Contains(errs, "goroutine [main.hog] has run for") || strings.Contains(errs, "main.cruncher") {
			t.Errorf("%s: watchdog warnings:\n%s", sanitize, errs)
		}
	}
	alone := filepath.Join(dir, "alone")
	os.MkdirAll(alone, 0o755)
	os.WriteFile(filepath.Join(alone, "main.sq"), []byte("package main\n\nimport \"time\"\n\nfunc main() {\n\tstart := time.Now()\n\tn := 0\n\tfor time.Since(start).Milliseconds() < 150 {\n\t\tn++\n\t}\n\tprintln(n > 0)\n}\n"), 0o644)
	if out, errs := run(alone, "aloneprog", ""); out != "true\n" || errs != "" {
		t.Errorf("a goroutine running alone: output %q, stderr %q (want no warning)", out, errs)
	}
}

// TestDocVarKinds: squark doc names shared and thread variables as declared (it used to show a
// shared var as a const with its initializer).
func TestDocVarKinds(t *testing.T) {
	dir := t.TempDir()
	src := "package main\n\nshared var Table = makeTable()\nshared var Jobs chan int cap 4\nthread var Hits int\nvar Plain int\n\nfunc makeTable() [4]int { return [4]int{1, 2, 3, 4} }\n\nfunc main() {}\n"
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	out, err := Doc(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shared var Table [4]int", "shared var Jobs chan int cap 4", "thread var Hits int", "var Plain int"} {
		if !strings.Contains(out, want) {
			t.Errorf("doc lacks %q:\n%s", want, out)
		}
	}
}

// sigDigits returns the significant digits of a formatted number ("-0.00150e+3" -> "15").
func sigDigits(s string) string {
	if e := strings.IndexAny(s, "eE"); e >= 0 {
		s = s[:e]
	}
	s = strings.Replace(strings.TrimLeft(s, "+-"), ".", "", 1)
	return strings.Trim(s, "0")
}

// TestFloatConversions checks strconv's shortest formatting and ParseFloat against Go's: random
// float64s across the whole range (and edge cases) must print with as few digits as Go's shortest
// form and read back exactly, and decimal strings (shortest, 17-digit, and long ones that take the
// slow path) must parse to the same bits as Go's ParseFloat.
func TestFloatConversions(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "floatconv")
	if _, err := Build(Options{Dir: "../../testdata/floatconv", Output: bin}); err != nil {
		t.Fatal(err)
	}
	rnd := rand.New(rand.NewSource(1))
	var vals []float64
	for _, v := range []float64{0, 1, -1, 0.1, 0.5, 1e21, 1e20, 1e-6, 1e-7, 5e-324, 2.2250738585072014e-308,
		2.225073858507201e-308, math.MaxFloat64, math.SmallestNonzeroFloat64, 123456789, 1 << 53, 9007199254740993} {
		vals = append(vals, v)
	}
	for i := -330; i <= 310; i++ {
		vals = append(vals, math.Pow(10, float64(i)))
	}
	for len(vals) < 20000 {
		b := rnd.Uint64()
		if f := math.Float64frombits(b); !math.IsNaN(f) && !math.IsInf(f, 0) {
			vals = append(vals, f)
		}
	}
	var in bytes.Buffer
	var texts []string
	for i, v := range vals {
		var text string
		switch i % 3 {
		case 0:
			text = strconv.FormatFloat(v, 'g', -1, 64)
		case 1:
			text = strconv.FormatFloat(v, 'e', 16, 64)
		default: // a long decimal string: the slow path
			digits := make([]byte, 17+rnd.Intn(14))
			for j := range digits {
				digits[j] = byte('0' + rnd.Intn(10))
			}
			digits[0] = byte('1' + rnd.Intn(9))
			text = string(digits[:1]) + "." + string(digits[1:]) + "e" + strconv.Itoa(rnd.Intn(640)-330)
		}
		texts = append(texts, text)
		fmt.Fprintf(&in, "%d %s\n", math.Float64bits(v), text)
	}
	cmd := exec.Command(bin)
	cmd.Stdin = &in
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != len(vals) {
		t.Fatalf("%d lines for %d inputs", len(lines), len(vals))
	}
	bad := 0
	for i, l := range lines {
		short, parsed, _ := strings.Cut(l, " ")
		v := vals[i]
		got, err := strconv.ParseFloat(short, 64)
		goShort := strconv.FormatFloat(v, 'e', -1, 64)
		if err != nil || got != v || sigDigits(short) != sigDigits(goShort) {
			bad++
			if bad <= 5 {
				t.Errorf("format %v (bits %d): got %q (Go %q)", v, math.Float64bits(v), short, goShort)
			}
		}
		want, err := strconv.ParseFloat(texts[i], 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			t.Fatalf("Go can't parse %q: %v", texts[i], err)
		}
		if parsed != strconv.FormatUint(math.Float64bits(want), 10) {
			bad++
			if bad <= 10 {
				t.Errorf("ParseFloat(%q): got bits %s, want %d (%v)", texts[i], parsed, math.Float64bits(want), want)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d mismatches", bad)
	}
}
