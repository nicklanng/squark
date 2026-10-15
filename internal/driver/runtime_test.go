package driver

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildSource builds a one-file program and returns the binary's path.
func buildSource(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runFor runs bin with args, killing it (and any worker processes) after d; it returns stdout,
// stderr, the exit code (-1 if it had to be killed).
func runFor(t *testing.T, d time.Duration, bin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	case <-time.After(d):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return stdout.String(), stderr.String(), -1
	}
}

// freeAddr returns a local address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func wantDeadlock(t *testing.T, name, bin string) {
	t.Helper()
	_, stderr, code := runFor(t, 10*time.Second, bin)
	if code != 2 || !strings.Contains(stderr, "all goroutines are asleep") {
		t.Errorf("%s: exit %d, stderr %q; want the deadlock report (exit 2)", name, code, stderr)
	}
}

// TestSharedChanDeadlock: with one scheduler thread nothing else can use a shared channel, so a
// program blocked on one is deadlocked and says so (spec §9) instead of sleeping forever.
func TestSharedChanDeadlock(t *testing.T) {
	for _, tc := range []struct{ name, directive, body string }{
		{"recv", "", "<-c"},
		{"send", "", "c <- 1"},
		{"threads1", "threads 1\n", "<-c"},
		{"select", "", "select {\n\tcase <-c:\n\tcase d <- 1:\n\t}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildSource(t, "package main\n\n"+tc.directive+"\nshared var c chan int cap 0\nshared var d chan int cap 0\n\nfunc main() {\n\t"+tc.body+"\n}\n")
			wantDeadlock(t, tc.name, bin)
		})
	}
}

// TestWaitReadableDeadlock: a WaitReadable that timed out leaves nothing registered with the
// poller, so a goroutine that then blocks for good is reported as a deadlock instead of hanging.
func TestWaitReadableDeadlock(t *testing.T) {
	bin := buildSource(t, `package main

import (
	"net"
	"strconv"
	"time"
)

var never chan int cap 1

func main() {
	l, err := net.Listen("127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	var b [32]byte
	c, err := net.Dial(string(strconv.AppendInt(append(b[:0], "127.0.0.1:"...), l.Port())))
	if err != nil {
		panic(err)
	}
	_ = c
	server, err := l.Accept()
	if err != nil {
		panic(err)
	}
	println(server.WaitReadable(10 * time.Millisecond))
	<-never
}
`)
	wantDeadlock(t, "WaitReadable", bin)
}

// TestListenerCloseThreads: under threads (and workers), closing the only listener on an address
// stops listening there: the runtime's own copy of the socket (macOS shares one between threads)
// is closed too, and listening again works.
func TestListenerCloseThreads(t *testing.T) {
	src := `package main

import (
	"net"
	"os"
	"runtime"
	"time"
)

%s

func main() {
	if runtime.Shard() != 0 {
		return
	}
	l, err := net.Listen(os.Arg(1))
	if err != nil {
		println("listen:", err)
		return
	}
	l.Close()
	time.Sleep(200 * time.Millisecond) // every worker has closed its listener
	if c, err := net.Dial(os.Arg(1)); err == nil {
		println("dial after close: connected")
		c.Close()
	} else {
		println("dial after close: refused")
	}
	if os.Arg(2) != "relisten" {
		return
	}
	l, err = net.Listen(os.Arg(1))
	if err != nil {
		println("listen again:", err)
		return
	}
	c, err := net.Dial(os.Arg(1))
	if err != nil {
		println("dial:", err)
		return
	}
	s, err := l.Accept()
	if err != nil {
		println("accept:", err)
		return
	}
	println("relisten: ok")
	s.Close()
	c.Close()
	l.Close()
}
`
	// (with workers, which process gets a connection is the kernel's choice: no relistening)
	for _, tc := range []struct{ name, directive, arg, want string }{
		{"threads", "threads 2", "relisten", "dial after close: refused\nrelisten: ok\n"},
		{"workers", "workers 2", "", strings.Repeat("dial after close: refused\n", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildSource(t, strings.Replace(src, "%s", tc.directive, 1))
			stdout, stderr, code := runFor(t, 20*time.Second, bin, freeAddr(t), tc.arg)
			if code != 0 || stdout != tc.want {
				t.Errorf("exit %d, stdout %q (stderr %q); want %q", code, stdout, stderr, tc.want)
			}
		})
	}
}

// TestHugeDurations runs testdata/huge_durations: time.Sleep, time.After and WaitReadable with
// time.Duration(math.MaxInt64) wait forever, without firing at once (the deadline used to wrap
// into the past) or stopping the thread's other timers.
func TestHugeDurations(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "prog")
	if _, err := Build(Options{Dir: "../../testdata/huge_durations", Output: bin}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runFor(t, 10*time.Second, bin)
	if want := "true true\nmain's select timed out\n"; code != 0 || stdout != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
}

// TestSQLiteNULPath:a database path with a NUL byte is invalid (EINVAL, as os.Open reports it)
// instead of opening the file named by the part before the NUL.
func TestSQLiteNULPath(t *testing.T) {
	bin := buildSource(t, `package main

import (
	"os"
	"sqlite"
)

arena a 256

func main() {
	_, err := sqlite.Open(os.Arg(1) + "/db\x00.evil" in a)
	println(err)
}
`)
	dir := t.TempDir()
	stdout, stderr, code := runFor(t, 20*time.Second, bin, dir)
	if code != 0 || stdout != "Invalid argument\n" {
		t.Errorf("exit %d, stdout %q, stderr %q; want Invalid argument", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "db")); err == nil {
		t.Errorf("sqlite.Open created %s", filepath.Join(dir, "db"))
	}
}
