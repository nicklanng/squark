package driver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpClientProg runs requests with the std http client and prints one line per request:
//
//	prog addr [flags and requests...]
//
// A request is "METHOD path" or "METHOD path body". Flags apply from then on: -H name:value adds a
// header, -timeout ms sets Client.Timeout, -discard sets DiscardBody, -raw sends the following
// requests through AppendRequest and RoundTrip, -close closes the connection. With -nc as the
// first flag the client is made with NewClient on a connection from net.Dial.
const httpClientProg = `package main

import (
	"http"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

var buf [8K]byte
var reqBuf [4K]byte

func show(resp http.Response, err error) {
	var line [512]byte
	b := line[:0]
	if err != nil {
		b = append(b, "error: "...)
		b = append(b, err.Error()...)
		b = append(b, ' ')
	}
	b = strconv.AppendInt(b, resp.StatusCode)
	b = append(b, ' ')
	b = append(b, resp.Reason...)
	b = append(b, " proto="...)
	b = append(b, resp.Proto...)
	b = append(b, " cl="...)
	b = strconv.AppendInt(b, resp.ContentLength)
	b = append(b, " close="...)
	if resp.Close {
		b = append(b, "true"...)
	} else {
		b = append(b, "false"...)
	}
	b = append(b, " x-test="...)
	b = append(b, resp.HeaderValue("x-TEST")...)
	b = append(b, " headers="...)
	b = strconv.AppendInt(b, resp.NumHeaders())
	b = append(b, " len="...)
	b = strconv.AppendInt(b, len(resp.Body))
	sum := 0
	for i := 0; i < len(resp.Body); i++ {
		sum = (sum*31 + int(resp.Body[i])) % 1000000007
	}
	b = append(b, " sum="...)
	b = strconv.AppendInt(b, sum)
	if len(resp.Body) <= 64 {
		b = append(b, " body="...)
		b = append(b, resp.Body...)
	}
	println(string(b))
}

func main() {
	addr := os.Arg(1)
	first := 2
	var c http.Client
	if os.Arg(2) == "-nc" {
		conn, err := net.Dial(addr)
		if err != nil {
			println("dial error:", err.Error())
			return
		}
		c = http.NewClient(conn, "test.host", buf[:])
		first = 3
	} else {
		cl, err := http.Dial(addr, buf[:])
		if err != nil {
			println("dial error:", err.Error())
			return
		}
		c = cl
	}
	var hdrs [..8]http.Header
	raw := false
	for i := first; i < os.NArgs(); i++ {
		a := os.Arg(i)
		switch a {
		case "-H":
			i++
			name, value, _ := strings.Cut(os.Arg(i), ":")
			hdrs.Push(http.Header{Name: name, Value: value})
			continue
		case "-timeout":
			i++
			ms, _ := strconv.Atoi(os.Arg(i))
			c.Timeout = time.Duration(ms) * time.Millisecond
			continue
		case "-discard":
			c.DiscardBody = true
			continue
		case "-raw":
			raw = true
			continue
		case "-close":
			c.Close()
			continue
		}
		method, rest, _ := strings.Cut(a, " ")
		path, body, _ := strings.Cut(rest, " ")
		var bb [1K]byte
		n := copy(bb[:], body)
		if raw {
			req, err := http.AppendRequest(reqBuf[:0], method, "raw.host", path, hdrs[:], bb[:n])
			if err != nil {
				println("error:", err.Error())
				continue
			}
			resp, err := c.RoundTrip(req)
			show(resp, err)
			continue
		}
		resp, err := c.Do(method, path, hdrs[:], bb[:n])
		show(resp, err)
	}
}
`

var httpClientBin struct {
	once sync.Once
	path string
	err  error
}

// clientProg builds httpClientProg once for all the client tests.
func clientProg(t *testing.T) string {
	t.Helper()
	httpClientBin.once.Do(func() {
		dir, err := os.MkdirTemp("", "squark-http-client-")
		if err != nil {
			httpClientBin.err = err
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(httpClientProg), 0o644); err != nil {
			httpClientBin.err = err
			return
		}
		httpClientBin.path = filepath.Join(dir, "client")
		_, httpClientBin.err = Build(Options{Dir: dir, Output: httpClientBin.path})
	})
	if httpClientBin.err != nil {
		t.Fatal(httpClientBin.err)
	}
	return httpClientBin.path
}

// runClient runs the client program against addr and returns its output lines.
func runClient(t *testing.T, addr string, args ...string) []string {
	t.Helper()
	stdout, stderr, code := runFor(t, 20*time.Second, clientProg(t), append([]string{addr}, args...)...)
	if code != 0 {
		t.Fatalf("client exited with %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return strings.Split(strings.TrimRight(stdout, "\n"), "\n")
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) && !strings.Contains(got[i], want[i]) {
			t.Errorf("line %d:\n got  %s\n want %s", i+1, got[i], want[i])
		}
	}
}

// bodySum is the program's checksum of a body.
func bodySum(b string) int {
	sum := 0
	for i := 0; i < len(b); i++ {
		sum = (sum*31 + int(b[i])) % 1000000007
	}
	return sum
}

func pattern(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(byte('a' + i%26))
	}
	return sb.String()
}

// rawServer serves each connection with fn, which reads requests with readRequest and writes raw
// response bytes. It returns the address.
func rawServer(t *testing.T, fn func(c net.Conn, r *bufio.Reader)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				fn(c, bufio.NewReader(c))
			}()
		}
	}()
	return l.Addr().String()
}

// readRequest reads one request head (and a Content-Length body) and returns the head.
func readRequest(r *bufio.Reader) (string, error) {
	req, err := http.ReadRequest(r)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, req.Body)
	return req.Method + " " + req.URL.RequestURI(), nil
}

func TestHTTPClient(t *testing.T) {
	big := pattern(20000)
	var mu sync.Mutex
	remotes := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr]++
		mu.Unlock()
		w.Header().Set("X-Test", "yes")
		w.Header().Set("Content-Length", "5")
		io.WriteString(w, "hello")
	})
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		var code int
		fmt.Sscan(strings.TrimPrefix(r.URL.Path, "/status/"), &code)
		w.WriteHeader(code)
		if code != 204 && code != 304 {
			fmt.Fprintf(w, "status %d", code)
		}
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		// 5000 bytes in 50 flushed chunks: more bytes on the wire than the body, so the client
		// has to reclaim the chunk headers' room in its 8 KiB buffer as it goes
		body := pattern(5000)
		w.Header().Set("X-Test", "chunks")
		for i := 0; i < len(body); i += 100 {
			io.WriteString(w, body[i:i+100])
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		io.WriteString(w, big)
	})
	mux.HandleFunc("/bigchunked", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < len(big); i += 1000 {
			io.WriteString(w, big[i:i+1000])
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Test", r.Method+" "+r.Header.Get("X-Echo")+" "+r.Header.Get("Content-Type")+" "+r.Host)
		w.Write(b)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		io.WriteString(w, "late")
	})
	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		io.WriteString(w, "bye")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	t.Run("status codes and headers", func(t *testing.T) {
		got := runClient(t, addr, "GET /hello", "GET /status/404", "GET /status/500", "GET /status/204", "GET /status/299")
		wantLines(t, got,
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes headers=4 len=5",
			"404 Not Found proto=HTTP/1.1 cl=10 close=false x-test= headers=3 len=10 sum="+fmt.Sprint(bodySum("status 404"))+" body=status 404",
			"500 Internal Server Error proto=HTTP/1.1 cl=10",
			"204 No Content proto=HTTP/1.1 cl=-1 close=false x-test= headers=1 len=0",
			"299 status code 299 proto=HTTP/1.1 cl=10")
	})

	t.Run("keep-alive", func(t *testing.T) {
		mu.Lock()
		clear(remotes)
		mu.Unlock()
		got := runClient(t, addr, "GET /hello", "GET /hello", "GET /chunked", "GET /hello", "POST /echo x", "GET /hello")
		for _, l := range got {
			if !strings.HasPrefix(l, "200 OK") {
				t.Errorf("unexpected response: %s", l)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(remotes) != 1 {
			t.Errorf("4 requests to /hello came on %d connections, want 1: %v", len(remotes), remotes)
		}
	})

	t.Run("chunked body", func(t *testing.T) {
		got := runClient(t, addr, "GET /chunked", "GET /chunked")
		want := fmt.Sprintf("200 OK proto=HTTP/1.1 cl=-1 close=false x-test=chunks headers=4 len=5000 sum=%d", bodySum(pattern(5000)))
		wantLines(t, got, want, want)
	})

	t.Run("request body and headers", func(t *testing.T) {
		got := runClient(t, addr, "-H", "X-Echo:a\r\nInjected: 1", "-H", "Content-Type:text/x", "POST /echo some body", "PUT /echo", "-raw", "POST /echo raw")
		wantLines(t, got,
			"200 OK proto=HTTP/1.1 cl=9 close=false x-test=POST a  Injected: 1 text/x "+addr+" headers=4 len=9 sum="+fmt.Sprint(bodySum("some body"))+" body=some body",
			"200 OK proto=HTTP/1.1 cl=0 close=false x-test=PUT a  Injected: 1 text/x "+addr,
			"200 OK proto=HTTP/1.1 cl=3 close=false x-test=POST a  Injected: 1 text/x raw.host headers=4 len=3")
		got = runClient(t, addr, "GET /bad\x01path", "GE(T /x", "-H", "Bad Name:x", "GET /echo")
		wantLines(t, got,
			"error: http: invalid method, path or header name",
			"error: http: invalid method, path or header name",
			"error: http: invalid method, path or header name")
	})

	t.Run("HEAD", func(t *testing.T) {
		got := runClient(t, addr, "HEAD /big", "GET /hello", "-raw", "HEAD /big", "GET /hello")
		wantLines(t, got,
			"200 OK proto=HTTP/1.1 cl=20000 close=false x-test= headers=3 len=0",
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes",
			"200 OK proto=HTTP/1.1 cl=20000 close=false x-test= headers=3 len=0",
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes")
	})

	t.Run("body too large", func(t *testing.T) {
		got := runClient(t, addr, "GET /big", "GET /hello", "GET /bigchunked", "GET /hello", "-discard", "GET /big", "GET /bigchunked", "GET /hello")
		wantLines(t, got,
			"error: http: response body larger than the client's buffer 200 OK proto=HTTP/1.1 cl=20000 close=false x-test= headers=3 len=",
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes",
			"error: http: response body larger than the client's buffer 200 OK proto=HTTP/1.1 cl=-1",
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes",
			"200 OK proto=HTTP/1.1 cl=20000 close=false x-test= headers=3 len=0",
			"200 OK proto=HTTP/1.1 cl=-1 close=false x-test= headers=3 len=0",
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test=yes headers=4 len=0")
	})

	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		got := runClient(t, addr, "-timeout", "300", "GET /slow", "GET /hello")
		wantLines(t, got, "error: i/o timeout 0", "200 OK")
		if d := time.Since(start); d > 1400*time.Millisecond {
			t.Errorf("timed-out request took %v", d)
		}
	})

	t.Run("connection close", func(t *testing.T) {
		got := runClient(t, addr, "GET /close", "GET /hello", "-close", "GET /hello")
		wantLines(t, got, "200 OK proto=HTTP/1.1 cl=3 close=true", "200 OK proto=HTTP/1.1 cl=5 close=false", "200 OK")
		got = runClient(t, addr, "-nc", "GET /echo", "GET /close", "GET /hello")
		wantLines(t, got, "x-test=GET   test.host", "close=true", "error: http: client connection closed")
	})

	t.Run("raw responses", func(t *testing.T) {
		responses := map[string]string{
			"/until-close":   "HTTP/1.1 200 OK\r\nX-Test: eof\r\n\r\nall of it",
			"/http10":        "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nok",
			"/short":         "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort",
			"/short-chunk":   "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n10\r\nabc",
			"/bad-status":    "HTXP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n",
			"/bad-code":      "HTTP/1.1 2x0 OK\r\nContent-Length: 0\r\n\r\n",
			"/bad-header":    "HTTP/1.1 200 OK\r\nX Test: 1\r\nContent-Length: 0\r\n\r\n",
			"/bare-lf":       "HTTP/1.1 200 OK\nContent-Length: 0\n\n",
			"/two-lengths":   "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 2\r\n\r\nok",
			"/bad-chunk":     "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nabc\r\n0\r\n\r\n",
			"/chunk-no-crlf": "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabcX\r\n0\r\n\r\n",
			"/interim":       "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nX-Test: final\r\nContent-Length: 2\r\n\r\nok",
			"/trailers":      "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3;ext=1\r\nabc\r\n2\r\nde\r\n0\r\nX-Trailer: 1\r\n\r\n",
			"/no-reason":     "HTTP/1.1 200\r\nContent-Length: 2\r\n\r\nok",
			"/huge-header":   "HTTP/1.1 200 OK\r\nX-Big: " + strings.Repeat("x", 9000) + "\r\n\r\n",
			"/nothing":       "",
		}
		addr := rawServer(t, func(c net.Conn, r *bufio.Reader) {
			for {
				req, err := readRequest(r)
				if err != nil {
					return
				}
				path := strings.TrimPrefix(req, "GET ")
				resp, ok := responses[path]
				if !ok {
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
					continue
				}
				io.WriteString(c, resp)
				if path != "/interim" && path != "/trailers" && path != "/no-reason" {
					return // close
				}
			}
		})
		got := runClient(t, addr,
			"GET /until-close", "GET /http10", "GET /short", "GET /short-chunk", "GET /bad-status",
			"GET /bad-code", "GET /bad-header", "GET /bare-lf", "GET /two-lengths", "GET /bad-chunk",
			"GET /chunk-no-crlf", "GET /interim", "GET /trailers", "GET /no-reason", "GET /huge-header",
			"GET /nothing", "GET /x")
		wantLines(t, got,
			"200 OK proto=HTTP/1.1 cl=-1 close=true x-test=eof headers=1 len=9 sum="+fmt.Sprint(bodySum("all of it"))+" body=all of it",
			"200 OK proto=HTTP/1.0 cl=2 close=true x-test= headers=1 len=2",
			"error: unexpected EOF 200 OK",
			"error: unexpected EOF 200 OK",
			"error: http: malformed response",
			"error: http: malformed response",
			"error: http: malformed response",
			"error: http: malformed response",
			"error: http: malformed response",
			"error: http: malformed response",
			"error: http: malformed response",
			"200 OK proto=HTTP/1.1 cl=2 close=false x-test=final headers=2 len=2",
			"200 OK proto=HTTP/1.1 cl=-1 close=false x-test= headers=1 len=5 sum="+fmt.Sprint(bodySum("abcde"))+" body=abcde",
			"200  proto=HTTP/1.1 cl=2 close=false x-test= headers=1 len=2",
			"error: http: response header larger than the client's buffer",
			"error: EOF",
			"200 OK proto=HTTP/1.1 cl=2")
	})

	t.Run("retry on a closed keep-alive connection", func(t *testing.T) {
		// the server answers each connection's first request as keep-alive, then closes it
		var conns sync.WaitGroup
		n := 0
		var nmu sync.Mutex
		addr := rawServer(t, func(c net.Conn, r *bufio.Reader) {
			conns.Add(1)
			defer conns.Done()
			nmu.Lock()
			n++
			nmu.Unlock()
			if _, err := readRequest(r); err != nil {
				return
			}
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
		})
		got := runClient(t, addr, "GET /a", "GET /b", "HEAD /c", "POST /d x", "GET /e")
		wantLines(t, got, "200 OK", "200 OK", "200 OK", "error: ", "200 OK")
		nmu.Lock()
		defer nmu.Unlock()
		if n != 4 {
			t.Errorf("%d connections, want 4 (GET and HEAD are sent again on a fresh connection, POST is not)", n)
		}
	})

	t.Run("squark server", func(t *testing.T) {
		saddr, _ := startHTTPProto(t, httpProtoServer)
		got := runClient(t, saddr, "GET /", "GET /status?c=404&n=3", "GET /status?c=200&n=6000", "POST /body hello", "GET /status?c=200&n=20000", "GET /", "-discard", "GET /status?c=200&n=20000")
		wantLines(t, got,
			"200 OK proto=HTTP/1.1 cl=2 close=false x-test= headers=3 len=2 sum="+fmt.Sprint(bodySum("hi"))+" body=hi",
			"404 Not Found proto=HTTP/1.1 cl=3 close=false x-test= headers=3 len=3 sum="+fmt.Sprint(bodySum("abc")),
			"200 OK proto=HTTP/1.1 cl=6000 close=false x-test= headers=3 len=6000 sum="+fmt.Sprint(bodySum(pattern(6000))),
			"200 OK proto=HTTP/1.1 cl=5 close=false x-test= headers=3 len=5 sum="+fmt.Sprint(bodySum("hello")),
			"error: http: response body larger than the client's buffer 200 OK proto=HTTP/1.1 cl=-1",
			"200 OK proto=HTTP/1.1 cl=2",
			"200 OK proto=HTTP/1.1 cl=-1 close=false x-test= headers=3 len=0")
	})
}

// TestLoadGenerator runs bench/squark/load, the squark port of bench/load, against a Go server
// that answers like the notes server, and checks that its -json output has the Go load
// generator's shape (the harness reads either).
func TestLoadGenerator(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs both load generators")
	}
	dir := t.TempDir()
	sqload := filepath.Join(dir, "sq-load")
	if _, err := Build(Options{Dir: "../../bench/squark/load", Output: sqload}); err != nil {
		t.Fatal(err)
	}
	goload := filepath.Join(dir, "load")
	if out, err := exec.Command("go", "build", "-o", goload, "squark/bench/load").CombinedOutput(); err != nil {
		t.Fatalf("go build bench/load: %v\n%s", err, out)
	}
	var mu sync.Mutex
	nextID := 0
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			nextID += 3 // ids the load generator must read back, not count
			id := nextID
			posts++
			mu.Unlock()
			if string(b) != `{"title":"x","body":"y"}` || r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":%d}`, id)
			return
		}
		io.WriteString(w, `[{"id":1,"title":"x","body":"y"}]`)
	})
	mux.HandleFunc("/notes/", func(w http.ResponseWriter, r *http.Request) {
		var id int
		fmt.Sscan(strings.TrimPrefix(r.URL.Path, "/notes/"), &id)
		mu.Lock()
		ok := id > 0 && id%3 == 0 && id <= nextID
		mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"title":"x","body":"y"}`, id)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL

	run := func(bin string, args ...string) (string, string, int) {
		t.Helper()
		return runFor(t, 30*time.Second, bin, args...)
	}
	decode := func(t *testing.T, out string) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
			t.Fatalf("output isn't one JSON object: %v\n%s", err, out)
		}
		return m
	}
	shape := func(m map[string]any) string {
		var keys []string
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok && k != "status" {
				for k2 := range sub {
					keys = append(keys, k+"."+k2)
				}
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, " ")
	}

	t.Run("json", func(t *testing.T) {
		args := []string{"-c", "4", "-d", "300ms", "-json", "-url", base + "/health"}
		sqOut, sqErr, code := run(sqload, args...)
		if code != 0 {
			t.Fatalf("sq-load exited with %d: %s", code, sqErr)
		}
		goOut, goErr, code := run(goload, args...)
		if code != 0 {
			t.Fatalf("load exited with %d: %s", code, goErr)
		}
		sq, gl := decode(t, sqOut), decode(t, goOut)
		if shape(sq) != shape(gl) {
			t.Errorf("JSON keys differ:\n squark %s\n go     %s", shape(sq), shape(gl))
		}
		if sq["mode"] != "GET /health" || sq["connections"] != 4.0 || sq["url"] != base+"/health" || sq["errors"] != 0.0 || sq["non2xx"] != 0.0 {
			t.Errorf("unexpected result: %s", sqOut)
		}
		n, _ := sq["requests"].(float64)
		st, _ := sq["status"].(map[string]any)
		if n < 100 || st["200"] != n {
			t.Errorf("requests %v, status %v", sq["requests"], st)
		}
		lat, _ := sq["latency_us"].(map[string]any)
		if !(lat["min"].(float64) > 0 && lat["min"].(float64) <= lat["p50"].(float64) && lat["p50"].(float64) <= lat["p99"].(float64) && lat["p99"].(float64) <= lat["max"].(float64)) {
			t.Errorf("latencies out of order: %v", lat)
		}
		if d, _ := sq["duration_s"].(float64); d < 0.3 || d > 2 {
			t.Errorf("duration_s %v for -d 300ms", d)
		}
	})

	t.Run("mix", func(t *testing.T) {
		out, stderr, code := run(sqload, "-url="+base+"/notes", "--c", "40", "-d", "0.3s", "-mix", "-seed", "50", "-json=true")
		if code != 0 {
			t.Fatalf("sq-load exited with %d: %s", code, stderr)
		}
		m := decode(t, out)
		st, _ := m["status"].(map[string]any)
		if m["mode"] != "mix" || m["seeded"] != 50.0 || m["non2xx"] != 0.0 || m["errors"] != 0.0 || st["200"] == nil || st["201"] == nil {
			t.Errorf("unexpected result: %s", out)
		}
		mu.Lock()
		defer mu.Unlock()
		if p := int(st["201"].(float64)) + 50; posts != p {
			t.Errorf("server saw %d POSTs, load generator counted %d", posts, p)
		}
	})

	t.Run("text and errors", func(t *testing.T) {
		out, stderr, code := run(sqload, "-c", "2", "-d", "100ms", "-url", base+"/nothing")
		if code != 0 || !strings.Contains(out, "mode: GET /nothing  conns: 2") || !strings.Contains(out, "status:   404=") || !strings.Contains(out, "non-2xx:  ") {
			t.Errorf("exit %d\n%s%s", code, out, stderr)
		}
		for _, c := range []struct {
			args []string
			want string
		}{
			{[]string{"-startup", "3", "--", "./server"}, "use the Go load generator"},
			{[]string{"-url", "https://x"}, "only http:// URLs are supported"},
			{[]string{"-d", "10x"}, "invalid duration for -d: 10x"},
			{[]string{"-bogus"}, "flag provided but not defined: -bogus"},
			{[]string{"-c", "0"}, "-c must be >= 1"},
			{[]string{"-c"}, "flag needs an argument: -c"},
		} {
			_, stderr, code := run(sqload, c.args...)
			if code != 1 || !strings.Contains(stderr, c.want) {
				t.Errorf("%v: exit %d, stderr %q, want %q", c.args, code, stderr, c.want)
			}
		}
		// nothing listening: every dial fails and is counted
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := l.Addr().String()
		l.Close()
		out, _, code = run(sqload, "-c", "2", "-d", "100ms", "-json", "-url", "http://"+dead+"/")
		if m := decode(t, out); code != 0 || m["requests"] != 0.0 || m["errors"].(float64) < 2 {
			t.Errorf("exit %d: %s", code, out)
		}
	})
}
