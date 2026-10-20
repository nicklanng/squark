// Package lsp is squark's language server (squark lsp): it speaks the Language Server Protocol
// over stdin and stdout, so an editor shows the compiler's errors as you type, a declaration's
// signature and documentation on hover, jumps to definitions and formats with squark fmt.
package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"squark"
	"squark/internal/compiler"
)

// Serve runs the server on in and out until the client exits; it returns nil after an orderly
// shutdown and exit.
func Serve(in io.Reader, out io.Writer) error {
	s := &server{out: out, docs: map[string]string{}, analyses: map[string]*analysis{}, pending: map[string]*time.Timer{}, Debounce: 150 * time.Millisecond}
	defer s.cleanup()
	r := bufio.NewReader(in)
	for {
		msg, err := readMessage(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("lsp: the client closed the connection without exit")
			}
			return err
		}
		if msg.Method == "exit" {
			if !s.shutdown {
				return errors.New("lsp: exit before shutdown")
			}
			return nil
		}
		s.handle(msg)
	}
}

// ---- the protocol ----

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func readMessage(r *bufio.Reader) (*message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if length, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", v)
			}
		}
	}
	if length < 0 {
		return nil, errors.New("lsp: a message without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("lsp: %v", err)
	}
	return &m, nil
}

func (s *server) send(m *message) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	fmt.Fprintf(s.out, "Content-Length: %d\r\n\r\n", len(b))
	s.out.Write(b)
}

func (s *server) reply(id *json.RawMessage, result any) {
	if result == nil {
		result = json.RawMessage("null")
	}
	s.send(&message{ID: id, Result: result})
}

func (s *server) notify(method string, params any) {
	b, _ := json.Marshal(params)
	s.send(&message{Method: method, Params: b})
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type rangeLSP struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string   `json:"uri"`
	Range rangeLSP `json:"range"`
}

type diagnostic struct {
	Range    rangeLSP `json:"range"`
	Severity int      `json:"severity"`
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

type textDocumentPosition struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position position `json:"position"`
}

// ---- the server ----

type server struct {
	out      io.Writer
	outMu    sync.Mutex
	shutdown bool
	Debounce time.Duration

	mu       sync.Mutex
	docs     map[string]string    // open documents' text, by path
	analyses map[string]*analysis // the latest check of each package directory
	pending  map[string]*time.Timer
	checkMu  sync.Mutex // one check at a time
	stdDir   string     // the standard library's files, written out for go to definition
}

func (s *server) handle(m *message) {
	switch m.Method {
	case "initialize":
		s.reply(m.ID, map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":           map[string]any{"openClose": true, "change": 1, "save": map[string]any{}},
				"hoverProvider":              true,
				"definitionProvider":         true,
				"documentFormattingProvider": true,
			},
			"serverInfo": map[string]any{"name": "squark"},
		})
	case "initialized", "$/cancelRequest", "$/setTrace", "workspace/didChangeConfiguration":
	case "shutdown":
		s.shutdown = true
		s.reply(m.ID, nil)
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			s.setDoc(p.TextDocument.URI, p.TextDocument.Text, true)
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(m.Params, &p) == nil && len(p.ContentChanges) > 0 {
			s.setDoc(p.TextDocument.URI, p.ContentChanges[len(p.ContentChanges)-1].Text, false)
		}
	case "textDocument/didSave":
	case "textDocument/didClose":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if path := uriPath(p.TextDocument.URI); path != "" {
				s.mu.Lock()
				delete(s.docs, path)
				s.mu.Unlock()
				s.schedule(filepath.Dir(path), true) // the file on disk is the truth again
			}
		}
	case "textDocument/hover":
		var p textDocumentPosition
		if json.Unmarshal(m.Params, &p) != nil {
			s.reply(m.ID, nil)
			return
		}
		s.reply(m.ID, s.hover(p))
	case "textDocument/definition":
		var p textDocumentPosition
		if json.Unmarshal(m.Params, &p) != nil {
			s.reply(m.ID, nil)
			return
		}
		s.reply(m.ID, s.definition(p))
	case "textDocument/formatting":
		var p struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		json.Unmarshal(m.Params, &p)
		s.reply(m.ID, s.format(p.TextDocument.URI))
	default:
		if m.ID != nil { // a request this server doesn't answer
			s.send(&message{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not supported: " + m.Method}})
		}
	}
}

func (s *server) setDoc(uri, text string, now bool) {
	path := uriPath(uri)
	if path == "" {
		return
	}
	s.mu.Lock()
	s.docs[path] = text
	s.mu.Unlock()
	s.schedule(filepath.Dir(path), now)
}

// schedule checks the package in dir after a pause, so a burst of keystrokes is checked once.
func (s *server) schedule(dir string, now bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.pending[dir]; t != nil {
		t.Stop()
	}
	d := s.Debounce
	if now {
		d = 0
	}
	s.pending[dir] = time.AfterFunc(d, func() { s.check(dir) })
}

func (s *server) cleanup() {
	s.mu.Lock()
	for _, t := range s.pending {
		t.Stop()
	}
	s.mu.Unlock()
	if s.stdDir != "" {
		os.RemoveAll(s.stdDir)
	}
}

// ---- checking ----

// analysis is a checked package: its errors and, for hover and definition, every identifier
// with what it names.
type analysis struct {
	prog  *compiler.Program
	files map[string]*compiler.File // by path
	index map[string]*index
	texts map[string]string // the text each file was checked with
}

func (s *server) check(dir string) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.mu.Lock()
	overlay := map[string][]byte{}
	texts := map[string]string{}
	for p, t := range s.docs {
		overlay[p] = []byte(t)
		if filepath.Dir(p) == dir {
			texts[p] = t
		}
	}
	prev := s.analyses[dir]
	s.mu.Unlock()

	// a test build checks a module as well as a program, and the package's tests with it
	l := &compiler.Loader{Std: squark.Files, MainDir: dir, Tests: true, Overlay: overlay}
	prog, err := l.Load()
	if err == nil {
		_, err = compiler.Compile(prog)
	}
	a := &analysis{prog: prog, files: map[string]*compiler.File{}, index: map[string]*index{}, texts: texts}
	if prog != nil {
		for _, f := range prog.Main.Files {
			if !strings.HasSuffix(f.Path, compiler.TestEntry+".sq") {
				a.files[f.Path] = f
				a.index[f.Path] = indexOf(f)
			}
		}
	}
	for p := range a.files {
		if _, ok := texts[p]; !ok {
			if b, err := os.ReadFile(p); err == nil {
				texts[p] = string(b)
			}
		}
	}

	// diagnostics for each of the package's files (none clears what was shown)
	byFile := map[string][]diagnostic{}
	if names, err := os.ReadDir(dir); err == nil {
		for _, e := range names {
			if strings.HasSuffix(e.Name(), ".sq") {
				byFile[filepath.Join(dir, e.Name())] = nil
			}
		}
	}
	for p := range texts {
		byFile[p] = nil
	}
	if prev != nil {
		for p := range prev.texts {
			if _, ok := byFile[p]; !ok {
				byFile[p] = nil
			}
		}
	}
	for _, e := range errorList(err) {
		file := e.Pos.File
		if file == "" || file == dir || !filepath.IsAbs(file) {
			// no position in this package (a std file, or the package as a whole): show it at
			// the top of the package's open files
			for p := range texts {
				byFile[p] = append(byFile[p], diagnostic{Range: rangeLSP{}, Severity: 1, Source: "squark", Message: e.Error()})
			}
			continue
		}
		text, ok := texts[file]
		if !ok {
			if b, err := os.ReadFile(file); err == nil {
				text = string(b)
			}
		}
		byFile[file] = append(byFile[file], diagnostic{Range: wordRange(text, e.Pos), Severity: 1, Source: "squark", Message: e.Msg})
	}
	s.mu.Lock()
	if prog != nil || prev == nil {
		s.analyses[dir] = a
	} else {
		// keep the last analysis that loaded for hover and definition, with the new texts
		prev.texts = texts
	}
	s.mu.Unlock()
	for p, ds := range byFile {
		if ds == nil {
			ds = []diagnostic{}
		}
		s.notify("textDocument/publishDiagnostics", map[string]any{"uri": pathURI(p), "diagnostics": ds})
	}
}

// errorList turns what the loader and the checker return into positioned errors.
func errorList(err error) []*compiler.Error {
	if err == nil {
		return nil
	}
	var el compiler.ErrorList
	if errors.As(err, &el) {
		return el
	}
	var e *compiler.Error
	if errors.As(err, &e) {
		return []*compiler.Error{e}
	}
	return []*compiler.Error{{Msg: err.Error()}}
}

// ---- hover and definition ----

func (s *server) lookup(p textDocumentPosition) (*index, *compiler.Ident) {
	path := uriPath(p.TextDocument.URI)
	s.mu.Lock()
	a := s.analyses[filepath.Dir(path)]
	s.mu.Unlock()
	if a == nil || a.index[path] == nil {
		return nil, nil
	}
	ix := a.index[path]
	line := p.Position.Line + 1
	col := runeCol(lineOf(a.texts[path], line), p.Position.Character)
	for _, id := range ix.idents {
		if id.P.Line == line && col >= id.P.Col && col < id.P.Col+utf8.RuneCountInString(id.Name) {
			return ix, id
		}
	}
	return ix, nil
}

func (s *server) hover(p textDocumentPosition) any {
	ix, id := s.lookup(p)
	if id == nil {
		return nil
	}
	var text string
	if fd, n := ix.fieldOf(id); fd != nil {
		text = "```squark\n" + n.Name + " " + compiler.ExprString(fd.Type) + "\n```"
		if len(fd.Doc) > 0 {
			text += "\n\n" + strings.Join(fd.Doc, "\n")
		}
	} else if id.Obj != nil {
		text = describe(id.Obj)
	}
	if text == "" {
		return nil
	}
	return map[string]any{"contents": map[string]any{"kind": "markdown", "value": text}}
}

// describe is the hover text for what o names: its declaration and its doc comment.
func describe(o *compiler.Object) string {
	var decl string
	switch o.Kind {
	case compiler.ObjFunc:
		if o.Func == nil {
			return ""
		}
		decl = compiler.DocLine(o.Func)
	case compiler.ObjVar:
		kind := "var"
		if v := o.Var; v != nil && v.Global {
			switch {
			case v.Shared:
				kind = "shared"
			case v.ReadOnly:
				kind = "const"
			default:
				kind = "thread"
			}
		}
		decl = kind + " " + o.Name + " " + typeString(o.Type)
		if o.Var != nil {
			decl = kind + " " + o.Name + " " + typeString(o.Var.Type)
		}
	case compiler.ObjConst:
		decl = "const " + o.Name + " " + typeString(o.Type)
		if v, ok := o.Const.(fmt.Stringer); ok && v != nil {
			decl += " = " + v.String()
		}
	case compiler.ObjType:
		decl = o.Name + ": " + typeString(o.Type.Underlying())
		if o.TypeDecl != nil && o.TypeDecl.Form == "struct" {
			decl = "struct " + o.Name
		} else if o.TypeDecl != nil && o.TypeDecl.Form == "enum" {
			decl = "enum " + o.Name + " " + typeString(o.Type.Underlying())
		}
	case compiler.ObjPkg:
		if o.PkgRef == nil {
			return ""
		}
		decl = "use " + o.PkgRef.Path
	default:
		return ""
	}
	if o.Pkg != nil && o.Pkg.Path != "main" && o.Kind != compiler.ObjPkg && (o.Var == nil || o.Var.Global) {
		decl = "// " + pkgName(o.Pkg) + "\n" + decl
	}
	out := "```squark\n" + decl + "\n```"
	if doc := o.DocComment(); doc != "" {
		out += "\n\n" + doc
	}
	return out
}

// pkgName is how a package is imported: its path for std, its name for the program's own.
func pkgName(p *compiler.Package) string {
	if p.Std {
		return p.Path
	}
	return p.Name
}

func typeString(t compiler.Type) string {
	if t == nil {
		return ""
	}
	return t.String()
}

func (s *server) definition(p textDocumentPosition) any {
	ix, id := s.lookup(p)
	if id == nil {
		return nil
	}
	var pos compiler.Pos
	if _, n := ix.fieldOf(id); n != nil {
		pos = n.P
	} else if id.Obj != nil {
		pos = id.Obj.Pos
	}
	if pos.File == "" || pos.Line == 0 {
		return nil
	}
	file := pos.File
	if rest, ok := strings.CutPrefix(file, "$SQUARK/"); ok {
		dir, err := s.std()
		if err != nil {
			return nil
		}
		file = filepath.Join(dir, filepath.FromSlash(rest))
	}
	if !filepath.IsAbs(file) {
		return nil
	}
	s.mu.Lock()
	text, open := s.docs[file]
	s.mu.Unlock()
	if !open {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil
		}
		text = string(b)
	}
	return location{URI: pathURI(file), Range: wordRange(text, pos)}
}

// std writes the standard library out once, read-only, so a definition in it can be opened.
func (s *server) std() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdDir != "" {
		return s.stdDir, nil
	}
	dir, err := os.MkdirTemp("", "squark-lsp-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(squark.Files, "std", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(squark.Files, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o444)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	s.stdDir = dir
	return dir, nil
}

// ---- formatting ----

func (s *server) format(uri string) any {
	path := uriPath(uri)
	s.mu.Lock()
	text, ok := s.docs[path]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	out, err := compiler.Format(path, []byte(text))
	if err != nil || string(out) == text {
		return []any{}
	}
	lines := strings.Count(text, "\n")
	return []map[string]any{{"range": rangeLSP{End: position{Line: lines + 1}}, "newText": string(out)}}
}

// ---- positions ----

// uriPath is the file path of a file: URI ("" for anything else).
func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(u.Path))
}

func pathURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func lineOf(text string, line int) string {
	for i := 1; i < line; i++ {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 {
			return ""
		}
		text = text[nl+1:]
	}
	if nl := strings.IndexByte(text, '\n'); nl >= 0 {
		text = text[:nl]
	}
	return text
}

// runeCol is the 1-based rune column of a UTF-16 offset in line.
func runeCol(line string, utf16Off int) int {
	col, n := 1, 0
	for _, r := range line {
		if n >= utf16Off {
			break
		}
		n += len(utf16.Encode([]rune{r}))
		col++
	}
	return col
}

// utf16Col is the UTF-16 offset of a 1-based rune column in line.
func utf16Col(line string, col int) int {
	n, c := 0, 1
	for _, r := range line {
		if c >= col {
			break
		}
		n += len(utf16.Encode([]rune{r}))
		c++
	}
	return n
}

// wordRange is the range of the word (or the one character) at pos in text.
func wordRange(text string, pos compiler.Pos) rangeLSP {
	if pos.Line == 0 {
		return rangeLSP{}
	}
	line := lineOf(text, pos.Line)
	rs := []rune(line)
	start := max(pos.Col-1, 0)
	end := start
	isWord := func(r rune) bool {
		return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127
	}
	for end < len(rs) && isWord(rs[end]) {
		end++
	}
	if end == start && end < len(rs) {
		end++
	}
	return rangeLSP{
		Start: position{Line: pos.Line - 1, Character: utf16Col(line, start+1)},
		End:   position{Line: pos.Line - 1, Character: utf16Col(line, end+1)},
	}
}
