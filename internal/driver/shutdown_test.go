package driver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// TestShutdownDirective: program { shutdown 300ms } sets the grace period (and SQUARK_SHUTDOWN
// still overrides it).
func TestShutdownDirective(t *testing.T) {
	bin := buildSource(t, strings.Replace(shutdownServer, "program\n", "program {\n\tshutdown 300ms\n}\n", 1))
	for _, env := range []string{"", "SQUARK_SHUTDOWN=900ms"} {
		var envs []string
		if env != "" {
			envs = append(envs, env)
		}
		cmd, addr, out, done := startServer(t, bin, envs...)
		go http.Get("http://" + addr + "/slow?ms=10000")
		time.Sleep(200 * time.Millisecond)
		began := time.Now()
		cmd.Process.Signal(syscall.SIGTERM)
		err := <-done
		d := time.Since(began)
		lo, hi := 250*time.Millisecond, 700*time.Millisecond
		if env != "" {
			lo, hi = 850*time.Millisecond, 1500*time.Millisecond
		}
		if code := exitCode(err); code != 1 || d < lo || d > hi {
			t.Errorf("%q: exit %d after %v, want 1 after %v-%v: %s", env, code, d, lo, hi, out.String())
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

// upstreamServer's handler reads from a slow upstream (os.arg(2), which never answers): with
// ?deadline it gives up after 100ms by itself; without, only a graceful stop's cap cuts it off.
// Either way it answers 504.
const upstreamServer = `program

use http, net, os, time

fn ask_upstream() -> ! {
	c := net.dial(os.arg(2))!
	defer c.close()
	var b u8[16]
	c.read(b[:])!
	return nil
}

fn handle(w *http.ResponseWriter, r *http.Request) {
	var err error
	if r.query_value("deadline") != "" {
		with deadline 100 * time.millisecond {
			err = ask_upstream()
		}
	} else {
		err = ask_upstream()
	}
	if err == net.Timeout {
		w.text(504, "upstream timed out\n")
		return
	}
	w.text(200, "upstream answered\n")
}

fn main() {
	if err := http.listen_and_serve(os.arg(1), handle); err != nil {
		println("error:", err.message())
	}
}
`

// TestDeadlineUpstream: a handler's slow upstream call returns net.Timeout at its with-deadline,
// and, with no deadline of its own, before the end of a graceful stop's grace period, so the
// client still gets an answer.
func TestDeadlineUpstream(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() { // accept and never answer
		var held []net.Conn
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	bin := buildSource(t, upstreamServer)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, addr, up.Addr().String())
	cmd.Env = append(cmd.Environ(), "SQUARK_SHUTDOWN=1s")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	get := func(path string) (int, time.Duration) {
		start := time.Now()
		for i := 0; ; i++ {
			resp, err := http.Get("http://" + addr + path)
			if err == nil {
				io.ReadAll(resp.Body)
				resp.Body.Close()
				return resp.StatusCode, time.Since(start)
			}
			if i == 200 {
				t.Fatalf("GET %s: %v\n%s", path, err, out.String())
			}
			time.Sleep(10 * time.Millisecond)
			start = time.Now()
		}
	}
	if code, d := get("/?deadline=1"); code != 504 || d > 600*time.Millisecond {
		t.Errorf("with deadline: %d after %v, want 504 after about 100ms", code, d)
	}

	type res struct {
		code int
		d    time.Duration
	}
	slow := make(chan res, 1)
	go func() { c, d := get("/"); slow <- res{c, d} }()
	time.Sleep(200 * time.Millisecond) // the handler is waiting on the upstream
	cmd.Process.Signal(syscall.SIGTERM)
	r := <-slow
	if r.code != 504 {
		t.Errorf("during the stop: %d, want 504 (the cap cut the upstream read off)", r.code)
	}
	select {
	case err := <-done:
		if code := exitCode(err); code != 0 {
			t.Errorf("exit %d, want 0: %s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server didn't exit")
	}
}

// TestShedLoad: with http.shed_load, a connection that finds every serve_conn slot busy gets a
// 503 with Retry-After at once instead of waiting; without it, it waits for a slot.
func TestShedLoad(t *testing.T) {
	for _, shed := range []bool{true, false} {
		src := strings.Replace(shutdownServer, "fn main() {\n", fmt.Sprintf("fn main() {\n\thttp.shed_load = %v\n", shed), 1)
		src = strings.Replace(src, "program\n", "program {\n\tpool http.serve_conn 2\n}\n", 1)
		bin := buildSource(t, src)
		_, addr, out, _ := startServer(t, bin)
		codes := make(chan int, 2)
		for i := 0; i < 2; i++ { // fill both slots
			go func() {
				resp, err := http.Get("http://" + addr + "/slow?ms=600")
				if err != nil {
					codes <- -1
					return
				}
				resp.Body.Close()
				codes <- resp.StatusCode
			}()
		}
		time.Sleep(200 * time.Millisecond)
		start := time.Now()
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("shed=%v: %v\n%s", shed, err, out.String())
		}
		resp.Body.Close()
		d := time.Since(start)
		if shed && (resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" || d > 300*time.Millisecond) {
			t.Errorf("shedding: %d (Retry-After %q) after %v, want 503 at once", resp.StatusCode, resp.Header.Get("Retry-After"), d)
		}
		if !shed && (resp.StatusCode != 200 || d < 250*time.Millisecond) {
			t.Errorf("waiting: %d after %v, want 200 once a slot freed", resp.StatusCode, d)
		}
		for i := 0; i < 2; i++ {
			if c := <-codes; c != 200 {
				t.Errorf("shed=%v: a request that had a slot got %d", shed, c)
			}
		}
	}
}

// TestIsolateHandlerPanic: a handler that panics ends only its own connection (closed; the panic
// is on stderr) and the server goes on serving.
func TestIsolateHandlerPanic(t *testing.T) {
	src := strings.Replace(shutdownServer, "fn handle(w *http.ResponseWriter, r *http.Request) {\n", "fn handle(w *http.ResponseWriter, r *http.Request) {\n\tif r.path == \"/boom\" {\n\t\tvar xs int[2]\n\t\tprintln(xs[len(r.path)])\n\t}\n", 1)
	bin := buildSource(t, src)
	_, addr, out, done := startServer(t, bin)
	// no keep-alive: Go's client retries a GET once on a fresh connection when a reused one closes
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 3; i++ {
		if resp, err := client.Get("http://" + addr + "/boom"); err == nil {
			resp.Body.Close()
			t.Fatalf("/boom answered %d, want the connection closed", resp.StatusCode)
		}
		resp, err := client.Get("http://" + addr + "/")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("after a panic: %v\n%s", err, out.String())
		}
		resp.Body.Close()
	}
	select {
	case err := <-done:
		t.Fatalf("the server exited: %v\n%s", err, out.String())
	default:
	}
	if n := strings.Count(out.String(), "the goroutine ends; the program goes on"); n != 3 {
		t.Errorf("%d isolated panics reported, want 3:\n%s", n, out.String())
	}
}

// TestIsolateReleasesLocks: a goroutine lock (sq_gmutex, which C shims such as sqlite's use) held
// by an isolate task's goroutine when it panics is released, so others don't wait forever.
func TestIsolateReleasesLocks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lock.c"), []byte(`#include "sq_rt.h"
static sq_gmutex mu;
static int ready;
void lk_lock(void) {
	if (!ready) {
		sq_gmutex_init(&mu);
		ready = 1;
	}
	sq_gmutex_lock(&mu);
}
void lk_unlock(void) { sq_gmutex_unlock(&mu); }
`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(`program

use time

extern fn lk_lock() stack 8K
extern fn lk_unlock() stack 8K

task holder() pool 1 isolate {
	lk_lock()
	panic("while holding the lock")
}

fn main() {
	start holder()
	time.sleep(20 * time.millisecond)
	lk_lock()
	println("the lock was released")
	lk_unlock()
}
`), 0o644)
	bin := filepath.Join(dir, "prog")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runFor(t, 5*time.Second, bin)
	if code != 0 || stdout != "the lock was released\n" || !strings.Contains(stderr, "panic: while holding the lock") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// TestServeAdmin: http.serve_admin answers /healthz, /readyz and /metrics from its own pool, so
// it answers while every serve_conn slot is busy; /readyz follows set_ready and a graceful stop.
func TestServeAdmin(t *testing.T) {
	src := strings.Replace(shutdownServer, "fn main() {\n", "fn main() {\n\thttp.handle_metrics(\"GET /metrics\")\n\tif err := http.serve_admin(os.arg(2)); err != nil {\n\t\tpanic(err)\n\t}\n", 1)
	src = strings.Replace(src, "fn handle(w *http.ResponseWriter, r *http.Request) {\n", "fn handle(w *http.ResponseWriter, r *http.Request) {\n\tif r.path == \"/unready\" {\n\t\thttp.set_ready(false)\n\t}\n", 1)
	src = strings.Replace(src, "program\n", "program {\n\tpool http.serve_conn 2\n}\n", 1)
	bin := buildSource(t, src)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	admin := l.Addr().String()
	l.Close()
	l, _ = net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, addr, admin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(base, path string) (int, string) {
		t.Helper()
		for i := 0; ; i++ {
			resp, err := client.Get("http://" + base + path)
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				return resp.StatusCode, string(b)
			}
			if i == 100 {
				t.Fatalf("GET %s%s: %v\n%s", base, path, err, out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if c, b := get(admin, "/healthz"); c != 200 || b != "ok\n" {
		t.Errorf("/healthz: %d %q", c, b)
	}
	if c, _ := get(admin, "/readyz"); c != 200 {
		t.Errorf("/readyz: %d", c)
	}
	get(addr, "/")
	if c, b := get(admin, "/metrics"); c != 200 || !strings.Contains(b, `http_requests_total{code="2xx"}`) || !strings.Contains(b, `squark_pool_goroutines{task="http.serve_conn"}`) {
		t.Errorf("/metrics: %d\n%s", c, b)
	}
	// both serve_conn slots busy: the admin port still answers at once
	for i := 0; i < 2; i++ {
		go client.Get("http://" + addr + "/slow?ms=1500")
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if c, _ := get(admin, "/healthz"); c != 200 || time.Since(start) > 500*time.Millisecond {
		t.Errorf("/healthz with serve_conn full: %d after %v", c, time.Since(start))
	}
	// a graceful stop: /readyz says so while the slow requests drain
	cmd.Process.Signal(syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	if c, b := get(admin, "/readyz"); c != 503 || b != "stopping\n" {
		t.Errorf("/readyz during the stop: %d %q", c, b)
	}
	select {
	case err := <-done:
		if code := exitCode(err); code != 0 {
			t.Errorf("exit %d: %s", code, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server didn't exit")
	}

	// set_ready(false)
	cmd = exec.Command(bin, addr, admin)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	get(addr, "/unready")
	if c, b := get(admin, "/readyz"); c != 503 || b != "not ready\n" {
		t.Errorf("/readyz after set_ready(false): %d %q", c, b)
	}
}
