package driver

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shutdownServer serves GET /slow?ms=N (answers after N ms) and GET / on os.arg(1), and prints
// "served" once listen_and_serve returns.
const shutdownServer = `program

use http, os, strconv, time

fn handle(w *http.ResponseWriter, r *http.Request) {
	ms, _ := strconv.atoi(r.query_value("ms"))
	time.sleep(time.Duration(ms) * time.millisecond)
	w.text(200, "done\n")
}

fn main() {
	if err := http.listen_and_serve(os.arg(1), handle); err != nil {
		println("error:", err.message())
		return
	}
	println("served")
}
`

// startServer runs bin on a free port and waits until it answers.
func startServer(t *testing.T, bin string, env ...string) (*exec.Cmd, string, *bytes.Buffer, chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, addr)
	cmd.Env = append(cmd.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i == 200 {
			t.Fatalf("server didn't start: %s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cmd, addr, &out, done
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// TestGracefulStop: SIGTERM lets a request in progress finish (answered with Connection: close),
// refuses new connections, doesn't wait for idle keep-alive connections, and listen_and_serve
// returns nil so main returns with status 0.
func TestGracefulStop(t *testing.T) {
	bin := buildSource(t, shutdownServer)
	cmd, addr, out, done := startServer(t, bin)

	// an idle keep-alive connection, which must not hold up the stop
	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	idle.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	if resp, err := http.ReadResponse(bufio.NewReader(idle), nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("idle connection's request: %v", err)
	}

	type result struct {
		resp *http.Response
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow?ms=600")
		if err != nil {
			slow <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		slow <- result{resp, string(b), err}
	}()
	time.Sleep(200 * time.Millisecond) // the slow request is in its handler
	began := time.Now()
	cmd.Process.Signal(syscall.SIGTERM)

	time.Sleep(100 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Errorf("a new connection was accepted during the stop")
	}

	r := <-slow
	if r.err != nil || r.resp.StatusCode != 200 || r.body != "done\n" {
		t.Fatalf("slow request: %v %q", r.err, r.body)
	}
	if !r.resp.Close {
		t.Errorf("the response during the stop didn't say Connection: close")
	}
	select {
	case err := <-done:
		if code := exitCode(err); code != 0 || !strings.Contains(out.String(), "served") {
			t.Errorf("exit %d (want 0), output %q", code, out.String())
		}
		if d := time.Since(began); d > 2*time.Second {
			t.Errorf("stopping took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the server didn't exit after the stop: %s", out.String())
	}
}

// TestGracefulStopLimits: a request still running at the end of SQUARK_SHUTDOWN ends the program
// with status 1, and a second signal ends it at once.
func TestGracefulStopLimits(t *testing.T) {
	bin := buildSource(t, shutdownServer)
	for _, second := range []bool{false, true} {
		cmd, addr, out, done := startServer(t, bin, "SQUARK_SHUTDOWN=400ms")
		go http.Get("http://" + addr + "/slow?ms=10000")
		time.Sleep(200 * time.Millisecond)
		began := time.Now()
		cmd.Process.Signal(syscall.SIGTERM)
		if second {
			time.Sleep(100 * time.Millisecond)
			cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-done:
			d := time.Since(began)
			code := exitCode(err)
			switch {
			case second && (code != 128+int(syscall.SIGTERM) || d > 350*time.Millisecond):
				t.Errorf("second signal: exit %d after %v, want %d at once", code, d, 128+int(syscall.SIGTERM))
			case !second && (code != 1 || d < 350*time.Millisecond || !strings.Contains(out.String(), "grace period")):
				t.Errorf("grace period: exit %d after %v, output %q", code, d, out.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("still running: %s", out.String())
		}
	}
}

// TestStopSignalDefault: a program that doesn't ask for graceful stops (no http server) still ends
// at once on SIGTERM, as before.
func TestStopSignalDefault(t *testing.T) {
	bin := buildSource(t, `program

use time

fn main() {
	println("ready")
	time.sleep(time.hour)
}
`)
	cmd := exec.Command(bin)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	bufio.NewReader(stdout).ReadString('\n')
	cmd.Process.Signal(syscall.SIGTERM)
	if code := exitCode(cmd.Wait()); code != 128+int(syscall.SIGTERM) {
		t.Errorf("exit %d, want %d", code, 128+int(syscall.SIGTERM))
	}
}
