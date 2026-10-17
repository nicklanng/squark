package driver

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMux serves testdata/mux (patterns registered with http.HandleFunc) and checks which pattern
// answers each request, the wildcard and query values, and the automatic 404 and 405 answers.
func TestMux(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mux")
	if _, err := Build(Options{Dir: "../../testdata/mux", Output: bin}); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(bin, strconv.Itoa(port))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			c.Close()
			break
		}
		if i == 100 {
			t.Fatal("server did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, tc := range []struct {
		method, path string
		code         int
		body, allow  string
	}{
		{"GET", "/health", 200, "ok", ""},
		{"HEAD", "/health", 200, "", ""},
		{"POST", "/health", 405, "405 Method Not Allowed\n", "GET, HEAD"},
		{"GET", "/nope", 404, "404 Not Found\n", ""},
		{"GET", "/notes", 200, "list", ""},
		{"POST", "/notes", 201, "create", ""},
		{"DELETE", "/notes", 405, "405 Method Not Allowed\n", "GET, HEAD, POST"},
		{"GET", "/notes/12", 200, "note 12", ""},
		{"GET", "/notes/abc", 404, "404 Not Found\n", ""}, // the handler's choice
		{"GET", "/notes/12/extra", 404, "404 Not Found\n", ""},
		{"GET", "/notes/", 404, "404 Not Found\n", ""},
		{"DELETE", "/notes/5", 200, "delete 5", ""},
		{"PUT", "/notes/5", 405, "405 Method Not Allowed\n", "GET, HEAD, DELETE"},
		{"GET", "/static/a/b", 200, "static /static/a/b", ""},
		{"POST", "/static/", 200, "static /static/", ""},
		{"GET", "/static", 404, "404 Not Found\n", ""},
		{"GET", "/files/x/y.txt", 200, "file x/y.txt", ""},
		{"GET", "/files/", 200, "file ", ""},
		{"GET", "/files", 404, "404 Not Found\n", ""},
		{"GET", "/", 200, "root", ""},
		{"GET", "/users/bob/posts/7", 200, "user bob post 7", ""},
		{"GET", "/users/admin/posts/7", 200, "admin post 7", ""}, // the literal segment wins
		{"GET", "/q?name=a%20b+c&n=42", 200, "a b c 42", ""},
		{"GET", "/q?n=x", 200, " none", ""},
		{"GET", "/enc/a%20b", 200, "a b", ""},
		{"GET", "/n%6Ftes", 200, "list", ""}, // escapes in literal segments compare decoded
		{"PATCH", "/any", 200, "any PATCH", ""},
	} {
		req, _ := http.NewRequest(tc.method, base+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.code || string(b) != tc.body || resp.Header.Get("Allow") != tc.allow {
			t.Errorf("%s %s: got %d %q (Allow %q), want %d %q (Allow %q)", tc.method, tc.path, resp.StatusCode, b, resp.Header.Get("Allow"), tc.code, tc.body, tc.allow)
		}
	}
	testMuxJSON(t, base)
}

// testMuxJSON checks /doc (http.JSON: nesting, escapes, a string longer than its chunk, Raw)
// and /misuse (a value without a key is reported, not written).
func testMuxJSON(t *testing.T, base string) {
	resp, err := http.Get(base + "/doc")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("/doc: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var doc struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Long string `json:"long"`
		List []struct {
			I    int     `json:"i"`
			Half float64 `json:"half"`
		} `json:"list"`
		OK   bool            `json:"ok"`
		None *int            `json:"none"`
		Raw  json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("/doc is not JSON: %v\n%s", err, b)
	}
	var long strings.Builder
	for i := range 600 {
		long.WriteByte(byte('a' + i%26))
	}
	if doc.ID != 7 || doc.Name != "quote \" and \\ and \n" || doc.Long != long.String() || len(doc.List) != 3 ||
		doc.List[1].I != 1 || doc.List[1].Half != 0.5 || doc.List[2].Half != 1 || !doc.OK || doc.None != nil || string(doc.Raw) != `{"a":[1,2]}` {
		t.Errorf("/doc: %s", b)
	}
	resp, err = http.Get(base + "/big")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil || len(items) != 3002 {
		t.Fatalf("/big: %v, %d items (%d bytes)", err, len(items), len(b))
	}
	for i := range 3000 {
		var it struct {
			I int    `json:"i"`
			S string `json:"s"`
		}
		if err := json.Unmarshal(items[i], &it); err != nil || it.I != i || it.S != long.String()[:i%600] {
			t.Fatalf("/big item %d: %v %s", i, err, items[i])
		}
	}
	var huge string
	json.Unmarshal(items[3000], &huge)
	var raw []int
	json.Unmarshal(items[3001], &raw)
	if len(huge) != 3000 || huge[:3] != "ABC" || len(raw) != 700 || raw[699] != 12 {
		t.Errorf("/big: tail %q... %d", huge[:min(len(huge), 10)], len(raw))
	}
	resp, err = http.Get(base + "/misuse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "{ json: encoder call out of place" {
		t.Errorf("/misuse: %q", b)
	}
}

// TestMuxBadPatterns: a malformed or duplicate pattern panics when it is registered.
func TestMuxBadPatterns(t *testing.T) {
	for _, tc := range []struct{ patterns, want string }{
		{`"GET /a", "GET /a"`, "registered twice"},
		{`"notes"`, "must start with /"},
		{`"GET /a/{x"`, "whole segment"},
		{`"GET /{rest...}/b"`, "must end the pattern"},
		{`"GET /a/{$}/b"`, "must end the pattern"},
		{`"GET /{1x}"`, "invalid wildcard name"},
		{`"G T /a"`, "path must start with /"},
		{`"GET /a//b"`, "empty segment"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			var regs strings.Builder
			for _, p := range strings.Split(tc.patterns, ", ") {
				regs.WriteString("\thttp.handle_func(" + p + ", h)\n")
			}
			src := "program\n\nuse http\n\nfn h(w *http.ResponseWriter, r *http.Request) {}\n\nfn main() {\n" + regs.String() + "}\n"
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
			bin := filepath.Join(dir, "prog")
			if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin).CombinedOutput()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), tc.want) {
				t.Errorf("want a panic containing %q, got %v\n%s", tc.want, err, out)
			}
		})
	}
}
