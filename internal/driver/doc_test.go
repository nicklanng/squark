package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocValuesAndComments: squark doc shows constants as written (a long string in full and
// quoted, a float32 at float32 precision, a rune as a rune literal) and prints the package doc
// comment and struct fields' doc comments.
func TestDocValuesAndComments(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("abcdefghij", 10) + `"\`
	src := `// Package main is documented here.
//
// A second paragraph.
program

const long = ` + "`" + long + "`" + `
const single f32 = 0.1
const double = 0.1
const r = 'x'
const r2 rune = 'é'
const r3 = '\n'
const n i32 = 120

// Point is a point.
struct Point {
	// x is across.
	x int
	y int // trailing comments aren't doc comments

	// z is up, and has
	// two lines.
	z, w int
}

fn main() {}
`
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	out, err := Doc(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"// Package main is documented here.\n//\n// A second paragraph.\nprogram\n",
		"const long = \"" + strings.Repeat("abcdefghij", 10) + `\"\\"` + "\n",
		"const single f32 = 0.1\n",
		"const double = 0.1\n",
		"const r = 'x'\n",
		"const r2 i32 = 'é'\n",
		`const r3 = '\n'` + "\n",
		"const n i32 = 120\n",
		"struct Point {\n\t// x is across.\n\tx int\n\ty int\n\n\t// z is up, and has\n\t// two lines.\n\tz int\n\tw int\n}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doc lacks %q:\n%s", want, out)
		}
	}
}

// TestDocStdPackageComments: the standard library reference carries package doc comments.
func TestDocStdPackageComments(t *testing.T) {
	out, err := Doc("std", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "// Package json writes and reads JSON") || !strings.Contains(out, "\nmodule json\n") {
		t.Errorf("json's package comment is missing:\n%s", out[:min(len(out), 400)])
	}
}

// TestDocTemplates (Q9): a function with anytype parameters is documented by its template, which
// is never analysed: without a declared contract its result shows no provenance (it was shown
// as in static), only a declared one.
func TestDocTemplates(t *testing.T) {
	dir := t.TempDir()
	src := `program

struct Entity { name str }

pub fn first(s anytype) -> str { return s[0] }
pub fn matching(a arena, v anytype) -> Entity[] { return make(Entity[], 0, 4) in a }
pub fn pick(a arena, v anytype) -> str in a { return "x" }

fn main() {
	println(first(str[]{"a"}))
	with arena x 1K { _ = matching(x, 1); _ = pick(x, 1) }
}
`
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	out, err := Doc(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fn first(s anytype) -> str // provenance inferred for each instance\n",
		"fn matching(a arena, v anytype) -> Entity[] // provenance inferred for each instance\n",
		"fn pick(a arena, v anytype) -> str in a\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doc lacks %q:\n%s", want, out)
		}
	}
}
