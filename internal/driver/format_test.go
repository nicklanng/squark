package driver

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"squark/internal/compiler"
)

// formatCorpus is every .sq file that parses: the standard library, the examples, the test
// programs, and doxasq's when it is next to this repository.
func formatCorpus(t *testing.T) map[string][]byte {
	files, err := SourceFiles([]string{"../../std/...", "../../examples/...", "../../testdata/..."})
	if err != nil {
		t.Fatal(err)
	}
	if more, err := SourceFiles([]string{"../../../doxasq/..."}); err == nil {
		files = append(files, more...)
	}
	out := map[string][]byte{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := compiler.ParseFile(f, src); err == nil {
			out[f] = src
		}
	}
	return out
}

// TestFormatIdempotent: squark fmt keeps every program's tokens (Format checks) and formatting
// its output again changes nothing.
func TestFormatIdempotent(t *testing.T) {
	for f, src := range formatCorpus(t) {
		once, err := compiler.Format(f, src)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		twice, err := compiler.Format(f, once)
		if err != nil {
			t.Errorf("%s, formatted: %v", f, err)
			continue
		}
		if string(once) != string(twice) {
			t.Errorf("%s: formatting twice differs:\n%s", f, firstDiff(string(once), string(twice)))
		}
	}
}

// TestFormatted: the standard library and the examples are formatted.
func TestFormatted(t *testing.T) {
	for f, src := range formatCorpus(t) {
		if !strings.Contains(f, "/std/") && !strings.Contains(f, "/examples/") {
			continue
		}
		out, err := compiler.Format(f, src)
		if err == nil && string(out) != string(src) {
			t.Errorf("%s is not formatted (squark fmt -w %s):\n%s", f, filepath.Clean(f), firstDiff(string(src), string(out)))
		}
	}
}

// firstDiff shows the first line where a and b differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + ":\n-" + x + "\n+" + y
		}
	}
	return ""
}

// TestFormatCases checks each rule on a small input.
func TestFormatCases(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"indentation from scratch", `program

fn main() {
x := 1
if x > 0 {
println(x)
}
}
`, `program

fn main() {
	x := 1
	if x > 0 {
		println(x)
	}
}
`},
		{"case and labels", `program

fn main() {
	x := 2
	outer: for i in 0..3 {
		switch x {
			case 1:
				break outer
			// the rest
			default:
				println(i)
		}
	}
}
`, `program

fn main() {
	x := 2
outer: for i in 0..3 {
		switch x {
		case 1:
			break outer
		// the rest
		default:
			println(i)
		}
	}
}
`},
		{"continued statements and signatures", `program

fn add(a int,
b int) -> int {
return a +
b
}

fn main() {
	if add(1, 2) == 3 and
	add(2, 2) == 4 {
	println("ok")
	}
}
`, `program

fn add(a int,
	b int) -> int {
	return a +
		b
}

fn main() {
	if add(1, 2) == 3 and
		add(2, 2) == 4 {
		println("ok")
	}
}
`},
		{"alignment", `module m

struct Point {
	x int // across
	label str // the screen
	pub z_order int
}

const (
	small = 1 // one
	larger = 22 // twenty-two
)

enum Kind {
	a // the first
	bbb
	cc // the third
}

const origin = Point{
	x: 0,
	label: "origin",
}

fn one() -> int { return 1 }
fn twenty() -> int { return 20 }
`, `module m

struct Point {
	x           int // across
	label       str // the screen
	pub z_order int
}

const (
	small  = 1  // one
	larger = 22 // twenty-two
)

enum Kind {
	a   // the first
	bbb
	cc  // the third
}

const origin = Point{
	x:     0,
	label: "origin",
}

fn one() -> int    { return 1 }
fn twenty() -> int { return 20 }
`},
		{"blank lines and semicolons", `

program


fn main() {

	x := 1;
	println(x);   // trailing


}

`, `program

fn main() {
	x := 1
	println(x) // trailing
}
`},
		{"spacing", `program

use bytes/. as b

extern fn sq_x(n int) -> int stack 1K c"sq_x"

fn f(p *int, xs ...int) -> int ! {
	y:=*p+ 1
	z := y*2 + 1
	s := xs[1 :]
	for i := 0;; i++ {
		if(i > 0) { break }
	}
	g( z , len( s ) )!
	return -y, nil
}
`, `program

use bytes/. as b

extern fn sq_x(n int) -> int stack 1K c"sq_x"

fn f(p *int, xs ...int) -> int ! {
	y := *p + 1
	z := y*2 + 1
	s := xs[1 :]
	for i := 0; ; i++ {
		if (i > 0) { break }
	}
	g(z, len(s))!
	return -y, nil
}
`},
		{"literals and comments kept", "program\n\nconst text = `line one\n  line two`\n\n/* a block\n   comment */\nfn main() {\n\tprintln(text) /* inline */\n}\n",
			"program\n\nconst text = `line one\n  line two`\n\n/* a block\n   comment */\nfn main() {\n\tprintln(text) /* inline */\n}\n"},
	} {
		out, err := compiler.Format("t.sq", []byte(tc.in))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if string(out) != tc.want {
			t.Errorf("%s:\n%s\nwant:\n%s\n%s", tc.name, out, tc.want, firstDiff(tc.want, string(out)))
		}
	}
	if _, err := compiler.Format("t.sq", []byte("program\n\nfn main( {\n")); err == nil {
		t.Errorf("a file that doesn't parse was formatted")
	}
}
