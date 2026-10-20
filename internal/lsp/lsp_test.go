package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// client drives a server as an editor would, over pipes.
type client struct {
	t      *testing.T
	w      io.Writer
	nextID int
	resps  chan *message
	notes  chan *message
	done   chan error
}

func start(t *testing.T) *client {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	c := &client{t: t, w: cw, resps: make(chan *message, 16), notes: make(chan *message, 64), done: make(chan error, 1)}
	go func() {
		c.done <- Serve(sr, sw)
		sw.Close()
	}()
	go func() {
		r := bufio.NewReader(cr)
		for {
			m, err := readMessage(r)
			if err != nil {
				return
			}
			if m.ID != nil {
				c.resps <- m
			} else {
				c.notes <- m
			}
		}
	}()
	return c
}

func (c *client) write(id *int, method string, params any) {
	m := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != nil {
		m["id"] = *id
	}
	b, _ := json.Marshal(m)
	fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

// call sends a request and returns its result.
func (c *client) call(method string, params any) json.RawMessage {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.write(&id, method, params)
	select {
	case m := <-c.resps:
		if m.Error != nil {
			c.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		b, _ := json.Marshal(m.Result)
		return b
	case <-time.After(20 * time.Second):
		c.t.Fatalf("%s: no response", method)
	}
	return nil
}

// diagnostics waits for the next diagnostics published for uri.
func (c *client) diagnostics(uri string) []diagnostic {
	c.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case m := <-c.notes:
			if m.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var p struct {
				URI         string       `json:"uri"`
				Diagnostics []diagnostic `json:"diagnostics"`
			}
			json.Unmarshal(m.Params, &p)
			if p.URI == uri {
				return p.Diagnostics
			}
		case <-deadline:
			c.t.Fatalf("no diagnostics for %s", uri)
		}
	}
}

func at(uri string, line, char int) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": line, "character": char}}
}

const broken = `program

use strings

// add adds two numbers.
fn add(a int, b int) -> int {
	return a + b
}

struct Point {
	// x is across.
	x int
}

fn main() {
	p := Point{x: 1}
	head, _, _ := strings.cut("a=b", "=")
	println(add(p.x, 2), head, missing)
}
`

func TestServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.sq")
	os.WriteFile(path, []byte("program\n\nfn main() {}\n"), 0o644)
	uri := pathURI(path)
	c := start(t)

	var init struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	json.Unmarshal(c.call("initialize", map[string]any{"processId": nil, "rootUri": pathURI(dir), "capabilities": map[string]any{}}), &init)
	for _, cap := range []string{"hoverProvider", "definitionProvider", "documentFormattingProvider", "textDocumentSync"} {
		if init.Capabilities[cap] == nil {
			t.Errorf("no %s in %v", cap, init.Capabilities)
		}
	}
	c.write(nil, "initialized", map[string]any{})

	// an unsaved buffer is checked, not the file on disk
	c.write(nil, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "squark", "version": 1, "text": broken}})
	ds := c.diagnostics(uri)
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "undefined: missing") {
		t.Fatalf("diagnostics %+v, want undefined: missing", ds)
	}
	if r := ds[0].Range; r.Start.Line != 17 || r.Start.Character != 28 || r.End.Character != 35 {
		t.Errorf("range %+v, want line 17, characters 28-35", r)
	}

	hover := func(line, char int) string {
		var h struct {
			Contents struct {
				Value string `json:"value"`
			} `json:"contents"`
		}
		json.Unmarshal(c.call("textDocument/hover", at(uri, line, char)), &h)
		return h.Contents.Value
	}
	for _, tc := range []struct {
		line, char int
		want       []string
	}{
		{17, 10, []string{"fn add(a int, b int) -> int", "add adds two numbers."}}, // add(
		{16, 24, []string{"// strings", "fn cut(s str, sep str) -> (str, str, bool) in s"}},
		{17, 15, []string{"x int", "x is across."}}, // p.x
		{15, 1, []string{"var p Point"}},
		{15, 12, []string{"x int", "x is across."}}, // Point{x: 1}
		{16, 16, []string{"use strings"}},
	} {
		got := hover(tc.line, tc.char)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("hover %d:%d = %q, want %q", tc.line, tc.char, got, w)
			}
		}
	}

	definition := func(line, char int) location {
		var l location
		json.Unmarshal(c.call("textDocument/definition", at(uri, line, char)), &l)
		return l
	}
	if l := definition(17, 10); l.URI != uri || l.Range.Start.Line != 5 || l.Range.Start.Character != 3 {
		t.Errorf("definition of add: %+v", l)
	}
	if l := definition(17, 15); l.URI != uri || l.Range.Start.Line != 11 {
		t.Errorf("definition of p.x: %+v", l)
	}
	l := definition(16, 24)
	if !strings.HasSuffix(l.URI, "/std/strings/strings.sq") {
		t.Errorf("definition of strings.cut: %+v", l)
	} else if b, err := os.ReadFile(uriPath(l.URI)); err != nil || !strings.Contains(lineOf(string(b), l.Range.Start.Line+1), "fn cut(") {
		t.Errorf("definition of strings.cut doesn't point at it: %v", err)
	}

	// fixing it clears the diagnostics
	fixed := strings.Replace(broken, ", missing)", ")", 1)
	c.write(nil, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2}, "contentChanges": []any{map[string]any{"text": fixed}}})
	if ds := c.diagnostics(uri); len(ds) != 0 {
		t.Errorf("after the fix: %+v", ds)
	}

	// formatting replaces the document with squark fmt's output
	messy := strings.Replace(fixed, "\treturn a + b", "return a+ b", 1)
	c.write(nil, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": 3}, "contentChanges": []any{map[string]any{"text": messy}}})
	var edits []struct {
		NewText string `json:"newText"`
	}
	json.Unmarshal(c.call("textDocument/formatting", map[string]any{"textDocument": map[string]any{"uri": uri}, "options": map[string]any{"tabSize": 4, "insertSpaces": false}}), &edits)
	if len(edits) != 1 || edits[0].NewText != fixed {
		t.Errorf("formatting: %+v", edits)
	}

	c.call("shutdown", nil)
	c.write(nil, "exit", nil)
	select {
	case err := <-c.done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("no exit")
	}
}

// TestModuleDiagnostics: a module is checked too (it has no main), with its tests, and an error
// in a test file shows there.
func TestModuleDiagnostics(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "m.sq"), []byte("module m\n\npub fn one() -> int { return 1 }\n"), 0o644)
	test := filepath.Join(dir, "m_test.sq")
	os.WriteFile(test, []byte("module m\n\nfn test_one() { assert(one() == \"1\") }\n"), 0o644)
	c := start(t)
	c.call("initialize", map[string]any{"capabilities": map[string]any{}})
	b, _ := os.ReadFile(test)
	c.write(nil, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": pathURI(test), "languageId": "squark", "version": 1, "text": string(b)}})
	ds := c.diagnostics(pathURI(test))
	if len(ds) != 1 || !strings.Contains(ds[0].Message, "cannot use \"1\"") || ds[0].Range.Start.Line != 2 {
		t.Errorf("diagnostics %+v", ds)
	}
	c.call("shutdown", nil)
	c.write(nil, "exit", nil)
	<-c.done
}

func TestPositions(t *testing.T) {
	line := "x := \"é😀\" + y"
	// é is one UTF-16 unit, 😀 two: y is rune column 13, UTF-16 offset 13
	if got := utf16Col(line, 13); got != 13 {
		t.Errorf("utf16Col = %d", got)
	}
	if got := runeCol(line, 13); got != 13 {
		t.Errorf("runeCol = %d", got)
	}
	if got := utf16Col(line, 9); got != 9 { // the closing quote, after the emoji
		t.Errorf("utf16Col after the emoji = %d, want 9", got)
	}
}
