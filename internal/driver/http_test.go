package driver

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpProtoServer is the server for TestHTTPProtocol. Its pool of 4 connection goroutines lets the
// slowloris test fill it; the read deadlines are lowered to keep the test fast.
const httpProtoServer = `package main

import (
	"http"
	"os"
	"strconv"
	"time"
)

pool http.serveConn 4

func handle(w *http.ResponseWriter, r *http.Request) {
	switch r.Path {
	case "/echo-id":
		w.SetHeader("X-Request-Id", r.HeaderValue("X-Request-Id"))
		w.WriteString("ok")
	case "/split":
		w.SetHeader("X-A", "1\r\nSet-Cookie: evil=1")
		w.SetHeader("X-B", "2\nSet-Cookie: evil=2")
		w.SetHeader("Bad Name", "x")
		w.SetHeader("X-C\r\nSet-Cookie", "evil=3")
		w.WriteString("ok")
	case "/body":
		w.Write(r.Body)
	case "/status":
		code, _ := strconv.Atoi(r.QueryValue("c"))
		w.WriteHeader(code)
		n, _ := strconv.Atoi(r.QueryValue("n"))
		for i := 0; i < n; i++ {
			w.WriteByte(byte('a' + i%26))
		}
	default:
		w.WriteString("hi")
	}
}

func main() {
	http.ReadHeaderTimeout = 300 * time.Millisecond
	http.ReadTimeout = 600 * time.Millisecond
	if err := http.ListenAndServe(os.Arg(1), handle); err != nil {
		panic(err)
	}
}
`

// startHTTPProto builds src and runs it on a free port, returning the address.
func startHTTPProto(t *testing.T, src string) (string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "server")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	stderr := &bytes.Buffer{}
	cmd := exec.Command(bin, addr)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return addr, stderr
		}
		if i == 100 {
			t.Fatalf("server did not start: %v\n%s", err, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rawExchange sends req on a new connection, half-closes it and returns everything the server
// sends until it closes (or 3 seconds pass).
func rawExchange(t *testing.T, addr, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	return string(b)
}

func statusOf(resp string) string {
	line, _, _ := strings.Cut(resp, "\r\n")
	return line
}

func TestHTTPProtocol(t *testing.T) {
	addr, stderr := startHTTPProto(t, httpProtoServer)
	alive := func(t *testing.T) {
		t.Helper()
		cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := cl.Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("server not answering: %v\nstderr: %s", err, stderr.String())
		}
		resp.Body.Close()
	}

	// F11: CR or LF on its own in the request is a 400, never a header of its own
	t.Run("bare CR/LF in request", func(t *testing.T) {
		for _, req := range []string{
			"GET /echo-id HTTP/1.1\r\nHost: x\r\nX-Request-Id: 1\nSet-Cookie: session=evil\r\n\r\n",
			"GET /echo-id HTTP/1.1\r\nHost: x\r\nX-Request-Id: 1\rSet-Cookie: session=evil\r\n\r\n",
			"GET /echo-id HTTP/1.1\r\nHost: x\r\nX-Request-Id: 1\x00\r\n\r\n",
			"GET / HTTP/1.1\nHost: x\n\n",               // LF-only lines (F37)
			"GET /\r HTTP/1.1\r\nHost: x\r\n\r\n",       // bare CR in the request line
			"GET / HTTP/1.1\r\nHost: x\r\n\n",           // bare LF ending the head
		} {
			start := time.Now()
			resp := rawExchange(t, addr, req)
			if !strings.HasPrefix(resp, "HTTP/1.1 400 ") || strings.Contains(resp, "Set-Cookie") {
				t.Errorf("%q: got %q, want a 400", req, resp)
			}
			if d := time.Since(start); d > 250*time.Millisecond {
				t.Errorf("%q: answered after %v, want at once (not by the read deadline)", req, d)
			}
		}
	})

	// F11: CR and LF in a response header can't start another header
	t.Run("response splitting", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/split")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if v := resp.Header.Values("Set-Cookie"); len(v) != 0 {
			t.Errorf("Set-Cookie headers %q", v)
		}
		if got := resp.Header.Get("X-A"); got != "1  Set-Cookie: evil=1" {
			t.Errorf("X-A: %q", got)
		}
		for name := range resp.Header {
			if strings.ContainsAny(name, " \r\n") {
				t.Errorf("header name %q sent", name)
			}
		}
		resp, err = http.Get("http://" + addr + "/echo-id")
		if err == nil {
			resp.Body.Close()
		}
	})

	// F12: the body is never read as a second request
	t.Run("Content-Length smuggling", func(t *testing.T) {
		for _, req := range []string{
			"POST /body HTTP/1.1\r\nHost: x\r\nContent-Length : 5\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n",
			"POST /body HTTP/1.1\r\nHost: x\r\n Content-Length: 5\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n",
			"POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nContent-Length: 39\r\n\r\nGET /smuggled HTTP/1.1\r\nHost: x\r\n\r\n",
			"POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: +5\r\n\r\nhello",
			"POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 5, 5\r\n\r\nhello",
			"POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: \r\n\r\n",
			"POST /body HTTP/1.1\r\nHo st: x\r\n\r\n",
		} {
			resp := rawExchange(t, addr, req)
			if !strings.HasPrefix(resp, "HTTP/1.1 400 ") || strings.Count(resp, "HTTP/1.1 ") != 1 {
				t.Errorf("%q: got %q, want one 400", req, resp)
			}
		}
		// the same length twice is fine
		resp := rawExchange(t, addr, "POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello")
		if !strings.HasPrefix(resp, "HTTP/1.1 200 ") || !strings.HasSuffix(resp, "\r\n\r\nhello") {
			t.Errorf("repeated Content-Length: got %q", resp)
		}
	})

	// the header scanner resumes where it stopped and starts afresh for each pipelined request
	t.Run("pipelining and split reads", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(c)
		io.WriteString(c, "POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 3\r\n\r\nabcGET / HTTP/1.1\r\nHost: x\r\n\r\nPOST /body HTTP/1.1\r")
		time.Sleep(50 * time.Millisecond)
		io.WriteString(c, "\nHost: x\r\nContent-Length: 2\r\n\r")
		time.Sleep(50 * time.Millisecond)
		io.WriteString(c, "\nx")
		time.Sleep(50 * time.Millisecond)
		io.WriteString(c, "y")
		for _, want := range []string{"abc", "hi", "xy"} {
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("want %q: %v", want, err)
			}
			b, _ := io.ReadAll(resp.Body)
			if string(b) != want {
				t.Errorf("got %q, want %q", b, want)
			}
		}
	})

	// F37
	t.Run("HTTP/1.0 keep-alive", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(c)
		for i := 0; i < 2; i++ {
			io.WriteString(c, "GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n")
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "hi" || !strings.EqualFold(resp.Header.Get("Connection"), "keep-alive") {
				t.Fatalf("request %d: body %q, Connection %q", i, b, resp.Header.Get("Connection"))
			}
		}
	})
	t.Run("HTTP/1.0 long body is not chunked", func(t *testing.T) {
		for _, req := range []string{
			"GET /status?c=200&n=40000 HTTP/1.0\r\n\r\n",
			"GET /status?c=200&n=40000 HTTP/1.0\r\nConnection: keep-alive\r\n\r\n",
		} {
			resp := rawExchange(t, addr, req)
			head, body, _ := strings.Cut(resp, "\r\n\r\n")
			if strings.Contains(head, "chunked") || len(body) != 40000 || !strings.Contains(head, "Connection: close") {
				t.Errorf("%q: head %q, %d body bytes", req, head, len(body))
			}
		}
	})
	t.Run("204 and 304 have no body", func(t *testing.T) {
		for _, code := range []string{"204", "304"} {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			c.SetDeadline(time.Now().Add(3 * time.Second))
			br := bufio.NewReader(c)
			io.WriteString(c, "GET /status?c="+code+"&n=40000 HTTP/1.1\r\nHost: x\r\n\r\n")
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.Header.Get("Transfer-Encoding") != "" || len(resp.TransferEncoding) != 0 || resp.StatusCode != atoi(code) {
				t.Errorf("%s: status %d, Transfer-Encoding %q", code, resp.StatusCode, resp.TransferEncoding)
			}
			// the connection is still in step: the next response is the next request's
			io.WriteString(c, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
			resp, err = http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("%s: next request: %v", code, err)
			}
			b, _ := io.ReadAll(resp.Body)
			if string(b) != "hi" {
				t.Errorf("%s: next request got %q", code, b)
			}
			c.Close()
		}
	})
	t.Run("Expect: 100-continue", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		br := bufio.NewReader(c)
		io.WriteString(c, "POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n")
		c.SetReadDeadline(time.Now().Add(time.Second))
		line, err := br.ReadString('\n')
		if err != nil || line != "HTTP/1.1 100 Continue\r\n" {
			t.Fatalf("got %q, %v; want 100 Continue", line, err)
		}
		if blank, _ := br.ReadString('\n'); blank != "\r\n" {
			t.Fatalf("100 Continue followed by %q", blank)
		}
		io.WriteString(c, "hello")
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(b) != "hello" {
			t.Errorf("got %d %q", resp.StatusCode, b)
		}
		// an expectation the server can't meet
		r := rawExchange(t, addr, "POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nExpect: something\r\n\r\nhello")
		if !strings.HasPrefix(r, "HTTP/1.1 417 ") {
			t.Errorf("unknown Expect: got %q", r)
		}
	})
	t.Run("WriteHeader out of range", func(t *testing.T) {
		for _, code := range []string{"0", "99", "1000", "99999", "-5"} {
			resp := rawExchange(t, addr, "GET /status?c="+code+" HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			if statusOf(resp) != "HTTP/1.1 500 Internal Server Error" {
				t.Errorf("WriteHeader(%s): status line %q", code, statusOf(resp))
			}
		}
		resp := rawExchange(t, addr, "GET /status?c=599 HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		if statusOf(resp) != "HTTP/1.1 599 Unknown" {
			t.Errorf("WriteHeader(599): status line %q", statusOf(resp))
		}
	})

	// F13: connections that send their request slowly are closed at the read deadline, so they
	// can't hold every connection goroutine (4 here) forever
	t.Run("slowloris", func(t *testing.T) {
		var wg sync.WaitGroup
		closed := make(chan time.Duration, 8)
		slow := func(first string, trickle string) {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			start := time.Now()
			io.WriteString(c, first)
			// one byte every 50ms: each read gets data, but the request never completes
			go func() {
				for i := 0; i < 200; i++ {
					time.Sleep(50 * time.Millisecond)
					if _, err := io.WriteString(c, trickle); err != nil {
						return
					}
				}
			}()
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			io.ReadAll(c)
			closed <- time.Since(start)
		}
		wg.Add(4)
		go slow("GET / HTTP/1.1\r\nHost: x\r\n", "X")       // headers never end
		go slow("GET / HTTP/1.1\r\nHost: x\r\nX: ", "y")     // still in a header value
		go slow("GET / HTTP/1.1\r\n", "Y")                                              // nor here
		go slow("POST /body HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n", "z") // body never ends
		time.Sleep(100 * time.Millisecond)
		alive(t) // a fifth connection gets served once the deadlines free a goroutine
		wg.Wait()
		close(closed)
		for d := range closed {
			if d > 2*time.Second {
				t.Errorf("a slow connection was closed after %v, want about the read deadline", d)
			}
		}
	})
	alive(t)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
