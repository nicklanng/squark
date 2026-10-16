package driver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jsonEncoderProg drives std/json's Encoder from commands on stdin, one per line, through three
// encoders at once: one with a large buffer, one streaming through a 64-byte buffer (draining and
// retrying on ErrFull, with RawStart for values longer than the buffer) and one growing into an
// arena from nil. Commands: { [ ] (Object, Array, End), k/s/r <hex> (Key, String, Raw),
// i <int>, f <float64 bits>, t, F, n (true, false, null), I <hex>,<hex> (SetIndent with that
// prefix and indent), and . to finish the document, which prints "ok <length>" and the output if
// all three agree and Finish succeeds, or "err <message>".
const jsonEncoderProg = `package main

import (
	"bufio"
	"bytes"
	"io"
	"json"
	"math"
	"os"
	"strconv"
)

var lineStore [256K]byte
var bigStore [256K]byte
var streamStore [256K]byte
var small [64]byte
var arg [128K]byte
var rawStore [256K]byte

// SetIndent's arguments, and how deeply the document is nested, for laying out a Raw value that
// goes through RawStart
var prefix, indent, nested [256]byte
var plen, ilen, depth int

func unhex(s string) []byte static {
	return unhexTo(s, arg[:])
}

func unhexTo(s string, dst []byte) []byte from dst {
	n := 0
	for i := 0; i+1 < len(s); i += 2 {
		dst[n] = nib(s[i])<<4 | nib(s[i+1])
		n++
	}
	return dst[:n]
}

// setIndent parses "I <hex>,<hex>" into prefix and indent.
func setIndent(line string) {
	i := 2
	for line[i] != ',' {
		i++
	}
	plen = len(unhexTo(line[2:i], prefix[:]))
	ilen = len(unhexTo(line[i+1:], indent[:]))
}

func nib(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

func parseUint(s string) uint64 {
	var v uint64
	for i := 0; i < len(s); i++ {
		v = v*10 + uint64(s[i]-'0')
	}
	return v
}

func apply(e *json.Encoder, line string, b []byte) {
	switch line[0] {
	case '{':
		e.Object()
	case '[':
		e.Array()
	case ']':
		e.End()
	case 'k':
		e.Key(string(b))
	case 's':
		e.String(string(b))
	case 'r':
		e.Raw(b)
	case 'i':
		v, _ := strconv.Atoi(line[2:])
		e.Int(v)
	case 'f':
		e.Float(math.Float64frombits(parseUint(line[2:])))
	case 't':
		e.Bool(true)
	case 'F':
		e.Bool(false)
	case 'n':
		e.Null()
	case 'I':
		e.SetIndent(string(prefix[:plen]), string(indent[:ilen]))
	}
}

// stream applies a command to an encoder writing through the small buffer into out
func stream(e *json.Encoder, out *bytes.Buffer, line string, b []byte) {
	apply(e, line, b)
	if e.Err != json.ErrFull {
		return
	}
	out.Write(e.Bytes())
	e.Clear()
	e.Err = nil
	apply(e, line, b)
	if e.Err != json.ErrFull {
		return
	}
	e.Err = nil
	if line[0] != 's' && line[0] != 'r' {
		e.Err = json.ErrFull // a key longer than the buffer can't be streamed
		return
	}
	if e.RawStart() {
		out.Write(e.Bytes())
		e.Clear()
		if line[0] == 's' {
			json.WriteString(out, string(b))
		} else if plen < 0 {
			out.Write(b)
		} else {
			// laid out as Raw would: the prefix carries the indentation of the open containers
			n := copy(nested[:], prefix[:plen])
			for i := 0; i < depth; i++ {
				n += copy(nested[n:], indent[:ilen])
			}
			r := json.NewEncoder(rawStore[:0])
			r.SetIndent(string(nested[:n]), string(indent[:ilen]))
			r.Raw(b)
			out.Write(r.Bytes())
		}
	}
}

func main() {
	r := bufio.NewReader(lineStore[:])
	for {
		big := json.NewEncoder(bigStore[:0])
		streamed := json.NewEncoder(small[:0])
		out := bytes.NewBuffer(streamStore[:0])
		plen, ilen, depth = -1, 0, 0
		arena a 1M {
			grown := json.NewEncoderIn(nil, a)
			for {
				line, err := r.ReadLine(os.Stdin)
				if err == io.EOF {
					return
				}
				if line == "." {
					break
				}
				var b []byte
				switch line[0] {
				case 'k', 's', 'r':
					b = unhex(line[2:])
				case 'I':
					setIndent(line)
				}
				apply(&big, line, b)
				apply(&grown, line, b)
				stream(&streamed, &out, line, b)
				switch line[0] {
				case '{', '[':
					depth++
				case ']':
					depth--
				}
			}
			err := big.Finish()
			serr := streamed.Finish()
			gerr := grown.Finish()
			out.Write(streamed.Bytes())
			if err != nil {
				if serr != err || gerr != err {
					println("err differ", err.Error(), serr.Error(), gerr.Error())
				} else {
					println("err", err.Error())
				}
			} else if serr != nil || gerr != nil || string(big.Bytes()) != string(out.Bytes()) || string(big.Bytes()) != string(grown.Bytes()) {
				println("differ")
			} else {
				var head [32]byte
				h := strconv.AppendInt(append(head[:0], "ok "...), len(big.Bytes()))
				os.Stdout.Write(append(h, '\n'))
				os.Stdout.Write(big.Bytes())
			}
		}
	}
}
`

// jsonGen generates random documents as encoder commands, along with what Go's encoding/json
// should decode from the output.
type jsonGen struct {
	r    *rand.Rand
	cmds strings.Builder
	size int
}

func (g *jsonGen) cmd(format string, args ...any) {
	fmt.Fprintf(&g.cmds, format+"\n", args...)
}

// str returns a random string: ASCII, quotes and backslashes, control characters, multi-byte
// UTF-8, U+2028/U+2029, invalid bytes; occasionally long if long is set (keys aren't: a key must
// fit in the streaming encoder's buffer).
func (g *jsonGen) str(maxLen int, long bool) string {
	n := g.r.Intn(maxLen + 1)
	if long && g.r.Intn(20) == 0 {
		n = 64 + g.r.Intn(200) // longer than the streaming buffer
	}
	var b []byte
	for len(b) < n {
		switch g.r.Intn(10) {
		case 0:
			b = append(b, byte(g.r.Intn(0x20)))
		case 1:
			b = append(b, `"\/<>&`[g.r.Intn(6)])
		case 2:
			b = append(b, string([]rune{'é', '世', '😀', ' ', ' ', '�', 0x7f}[g.r.Intn(7)])...)
		case 3:
			b = append(b, byte(0x80+g.r.Intn(0x80))) // usually invalid
		default:
			b = append(b, byte('a'+g.r.Intn(26)))
		}
	}
	g.size += len(b)
	return string(b)
}

// canon is what Go decodes a string into after a round trip through JSON (invalid UTF-8 becomes
// U+FFFD).
func canon(s string) string {
	b, _ := json.Marshal(s)
	var out string
	json.Unmarshal(b, &out)
	return out
}

// numInt and numFloat are expected numbers, compared exactly.
type numInt int64
type numFloat float64

func (g *jsonGen) value(depth int, raw bool) any {
	k := g.r.Intn(10)
	if depth == 0 && g.r.Intn(10) != 0 {
		k = g.r.Intn(2) // mostly an object or array at the top
	} else if depth >= 5 || g.size > 32<<10 {
		k = 2 + g.r.Intn(8) // scalars only
	}
	switch k {
	case 0: // object
		if !raw {
			g.cmd("{")
		}
		m := map[string]any{}
		for n := g.r.Intn(8); n > 0; n-- {
			key := g.str(3, false)
			if _, dup := m[canon(key)]; dup {
				continue
			}
			if !raw {
				g.cmd("k %x", key)
			}
			m[canon(key)] = g.value(depth+1, raw)
		}
		if !raw {
			g.cmd("]")
		}
		return m
	case 1: // array
		if !raw {
			g.cmd("[")
		}
		a := []any{}
		for n := g.r.Intn(9); n > 0; n-- {
			a = append(a, g.value(depth+1, raw))
		}
		if !raw {
			g.cmd("]")
		}
		return a
	case 2, 3:
		s := g.str(12, true)
		if !raw {
			g.cmd("s %x", s)
		}
		return canon(s)
	case 4:
		v := g.r.Int63() >> uint(g.r.Intn(63))
		if g.r.Intn(2) == 0 {
			v = -v
		}
		switch g.r.Intn(10) {
		case 0:
			v = math.MaxInt64
		case 1:
			v = math.MinInt64
		}
		if !raw {
			g.cmd("i %d", v)
		}
		return numInt(v)
	case 5:
		var f float64
		switch g.r.Intn(4) {
		case 0:
			f = float64(g.r.Intn(2000) - 1000)
		case 1:
			f = g.r.NormFloat64() * math.Pow(10, float64(g.r.Intn(40)-20))
		default:
			for {
				f = math.Float64frombits(g.r.Uint64())
				if !math.IsNaN(f) && !math.IsInf(f, 0) {
					break
				}
			}
		}
		if !raw {
			g.cmd("f %d", math.Float64bits(f))
		}
		return numFloat(f)
	case 6:
		b := g.r.Intn(2) == 0
		if !raw {
			if b {
				g.cmd("t")
			} else {
				g.cmd("F")
			}
		}
		return b
	case 7:
		if !raw {
			g.cmd("n")
		}
		return nil
	default: // pre-encoded JSON: a subtree marshaled by Go
		if raw {
			return g.value(depth, raw)
		}
		v := g.value(depth+1, true)
		b, err := json.Marshal(toGo(v))
		if err != nil {
			panic(err)
		}
		if g.r.Intn(3) == 0 {
			var ind bytes.Buffer
			json.Indent(&ind, b, " ", "\t")
			b = ind.Bytes()
		}
		g.cmd("r %x", b)
		return v
	}
}

// toGo converts an expected tree to values json.Marshal writes as the same JSON.
func toGo(v any) any {
	switch v := v.(type) {
	case map[string]any:
		m := map[string]any{}
		for k, x := range v {
			m[k] = toGo(x)
		}
		return m
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			a[i] = toGo(x)
		}
		return a
	case numInt:
		return int64(v)
	case numFloat:
		return float64(v)
	}
	return v
}

// same compares an expected tree with what Go decoded (with UseNumber).
func same(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for k, x := range w {
			y, ok := g[k]
			if !ok || !same(x, y) {
				return false
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !same(w[i], g[i]) {
				return false
			}
		}
		return true
	case numInt:
		g, ok := got.(json.Number)
		return ok && string(g) == strconv.FormatInt(int64(w), 10)
	case numFloat:
		g, ok := got.(json.Number)
		if !ok {
			return false
		}
		f, err := strconv.ParseFloat(string(g), 64)
		return err == nil && math.Float64bits(f) == math.Float64bits(float64(w))
	}
	return want == got
}

// misuseDoc generates random calls, mostly ones a model of the encoder allows and now and then
// one it doesn't, and returns whether the model expects them to make a complete document.
func (g *jsonGen) misuseDoc() bool {
	type level struct{ obj, key bool }
	var stack []level
	done := false
	top := func() *level { return &stack[len(stack)-1] }
	valueOK := func() bool {
		switch {
		case len(stack) == 0:
			return !done
		case top().obj:
			return top().key
		}
		return true
	}
	allowed := func(op int) bool {
		switch op {
		case 0, 1, 3, 4: // {, [, an int, valid raw
			return valueOK()
		case 2: // End
			return len(stack) > 0 && !top().key
		case 5: // Key
			return len(stack) > 0 && top().obj && !top().key
		}
		return false // invalid raw, NaN, a second End at the top
	}
	emit := func(op int) {
		if op <= 4 && op != 2 && valueOK() {
			if len(stack) == 0 {
				done = true
			} else if top().obj {
				top().key = false
			}
		}
		switch op {
		case 0:
			g.cmd("{")
			stack = append(stack, level{obj: true})
		case 1:
			g.cmd("[")
			stack = append(stack, level{})
		case 2:
			g.cmd("]")
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case 3:
			g.cmd("i %d", g.r.Intn(100))
		case 4:
			g.cmd("r %x", `[1,{"a":null}]`)
		case 5:
			g.cmd("k %x", g.str(3, false))
			if len(stack) > 0 {
				top().key = true
			}
		case 6:
			g.cmd("r %x", []string{"", "{", "[1,]", "1 2", "tru"}[g.r.Intn(5)])
		case 7:
			g.cmd("f %d", math.Float64bits(math.Inf(g.r.Intn(3)-1)))
		}
	}
	for n := 1 + g.r.Intn(16); n > 0; n-- {
		if done && len(stack) == 0 && g.r.Intn(4) != 0 {
			break // complete; usually stop here rather than add a second top-level value
		}
		op := g.r.Intn(8)
		for tries := 0; !allowed(op) && g.r.Intn(12) != 0 && tries < 50; tries++ {
			op = g.r.Intn(8)
		}
		if !allowed(op) {
			emit(op)
			return false // the encoder keeps its first error: nothing after this matters
		}
		emit(op)
	}
	if g.r.Intn(4) != 0 { // usually complete the document
		for len(stack) > 0 {
			if top().obj && top().key {
				emit(3)
			}
			emit(2)
		}
	}
	return done && len(stack) == 0
}

// TestJSONEncoderRandom encodes random documents with std/json's Encoder (whole, streamed through
// a small buffer, and grown in an arena: all three must agree) and checks that Go's encoding/json
// parses the output back to the same values; encodes them again after SetIndent, which must give
// what Go's json.Indent makes of the compact output; and random call sequences, valid or not (half
// of them indented), must either make valid JSON or set an error, as a model of the encoder
// predicts.
func TestJSONEncoderRandom(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(jsonEncoderProg), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "enc")
	if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
		t.Fatal(err)
	}
	g := &jsonGen{r: rand.New(rand.NewSource(1))}
	const docs, misuses = 1500, 1500
	var wants []any
	var docCmds []string
	for i := 0; i < docs; i++ {
		g.size = 0
		start := g.cmds.Len()
		wants = append(wants, g.value(0, false))
		g.cmd(".")
		docCmds = append(docCmds, g.cmds.String()[start:])
	}
	// the same documents again, indented: the output must be what Go's json.Indent makes of the
	// compact one (prefixes and indents of up to 2 bytes, so a key's line fits the streaming buffer)
	prefixes := []string{"", " ", "\t", ">", "  ", "\r\n"}
	indents := []string{"", " ", "  ", "\t", "-", "\t "}
	var layouts [][2]string
	for i := 0; i < docs; i++ {
		l := [2]string{prefixes[g.r.Intn(len(prefixes))], indents[g.r.Intn(len(indents))]}
		layouts = append(layouts, l)
		g.cmd("I %x,%x", l[0], l[1])
		g.cmds.WriteString(docCmds[i])
	}
	var valid []bool
	for i := 0; i < misuses; i++ {
		if g.r.Intn(2) == 0 { // whitespace of a byte at most: misuse documents nest 16 deep
			g.cmd("I %x,%x", []string{"", " "}[g.r.Intn(2)], []string{"", " ", "\t"}[g.r.Intn(3)])
		}
		valid = append(valid, g.misuseDoc())
		g.cmd(".")
	}
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(g.cmds.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	// a result is "ok <length>\n" and the document (whose Raw values may hold newlines), or a line
	// "err <message>"
	br := bufio.NewReader(bytes.NewReader(out))
	var lines []string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSuffix(line, "\n")
		if n, ok := strings.CutPrefix(line, "ok "); ok {
			size, _ := strconv.Atoi(n)
			doc := make([]byte, size)
			if _, err := io.ReadFull(br, doc); err != nil {
				t.Fatal(err)
			}
			line = "ok " + string(doc)
		}
		lines = append(lines, line)
	}
	if len(lines) != 2*docs+misuses {
		t.Fatalf("%d lines of output for %d documents", len(lines), 2*docs+misuses)
	}
	bad := 0
	for i, want := range wants {
		line := lines[i]
		doc, ok := strings.CutPrefix(line, "ok ")
		var got any
		if ok {
			d := json.NewDecoder(strings.NewReader(doc))
			d.UseNumber()
			if err := d.Decode(&got); err != nil || d.More() {
				ok = false
			}
		}
		if !ok || !same(want, got) {
			bad++
			if bad <= 5 {
				t.Errorf("document %d: got %.300s\nwant %.300v", i, line, toGo(want))
			}
		}
	}
	for i, l := range layouts {
		plain, indented := lines[i], lines[docs+i]
		doc, ok := strings.CutPrefix(plain, "ok ")
		if !ok {
			if indented != plain {
				bad++
				t.Errorf("indented document %d: got %.300q, without SetIndent %.300q", i, indented, plain)
			}
			continue
		}
		var want bytes.Buffer
		if err := json.Indent(&want, []byte(doc), l[0], l[1]); err != nil {
			t.Fatal(err)
		}
		if indented != "ok "+want.String() {
			bad++
			if bad <= 5 {
				t.Errorf("indented document %d (prefix %q, indent %q): got\n%.600s\nwant\n%.600s", i, l[0], l[1], indented, want.String())
			}
		}
	}
	for i, v := range valid {
		line := lines[2*docs+i]
		doc, ok := strings.CutPrefix(line, "ok ")
		switch {
		case ok && !json.Valid([]byte(doc)):
			t.Errorf("call sequence %d: invalid JSON without an error: %s", i, doc)
		case ok != v || (!ok && !strings.HasPrefix(line, "err json: ")):
			bad++
			if bad <= 10 {
				t.Errorf("call sequence %d: got %q, the model expects valid=%v", i, line, v)
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d mismatches", bad)
	}
	nvalid, size := 0, 0
	for _, v := range valid {
		if v {
			nvalid++
		}
	}
	for _, l := range lines[:docs] {
		size += len(l)
	}
	t.Logf("%d documents (%d bytes in all), %d call sequences of which %d valid", docs, size, misuses, nvalid)
}
