package driver

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
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

// TestRun builds each testdata/run program and compares its stdout with the .out file.
// Programs whose name starts with "panic_" must exit with status 2.
func TestRun(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/run/*.sq")
	examples := []string{"../../examples/hello", "../../examples/pipeline"}
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
			if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
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
	if code, _ := do("DELETE", "/notes/1", ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := do("GET", "/notes/1", ""); code != 404 {
		t.Errorf("get after delete: %d", code)
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
	var src, env string
	switch runtime.GOOS {
	case "darwin":
		src, env = "count_darwin.c", "DYLD_INSERT_LIBRARIES"
	case "linux":
		src, env = "count_linux.c", "LD_PRELOAD"
	default:
		t.Skip("no allocation counter for " + runtime.GOOS)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "count.so")
	if out, err := exec.Command("clang", "-O2", "-shared", "-fPIC", "-o", lib, filepath.Join("../../testdata/heapcount", src)).CombinedOutput(); err != nil {
		t.Fatalf("building the counter: %v\n%s", err, out)
	}
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: "../../testdata/heapcount/prog", Output: bin}); err != nil {
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
	counts := filepath.Join(dir, "counts")
	cmd := exec.Command(bin, free(), free(), filepath.Join(dir, "file.txt"), free())
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
	if len(before) == 0 || out.String() != "true\n" {
		t.Fatalf("counter or program failed: counts %q, output %q", before, out.String())
	}
	if string(before) != string(after) {
		t.Errorf("heap allocations after startup: %q before the work, %q after", before, after)
	}
}
