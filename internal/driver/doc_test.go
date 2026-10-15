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
package main

const Long = ` + "`" + long + "`" + `
const F32 float32 = 0.1
const F64 = 0.1
const R = 'x'
const R2 rune = 'é'
const R3 = '\n'
const N int32 = 120

// Point is a point.
type Point struct {
	// X is across.
	X int
	Y int // trailing comments aren't doc comments

	// Z is up, and has
	// two lines.
	Z, W int
}

func main() {}
`
	os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
	out, err := Doc(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"// Package main is documented here.\n//\n// A second paragraph.\npackage main\n",
		"const Long = \"" + strings.Repeat("abcdefghij", 10) + `\"\\"` + "\n",
		"const F32 float32 = 0.1\n",
		"const F64 = 0.1\n",
		"const R = 'x'\n",
		"const R2 int32 = 'é'\n",
		`const R3 = '\n'` + "\n",
		"const N int32 = 120\n",
		"type Point struct {\n\t// X is across.\n\tX int\n\tY int\n\n\t// Z is up, and has\n\t// two lines.\n\tZ int\n\tW int\n}",
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
	if !strings.Contains(out, "// Package json writes and reads JSON") || !strings.Contains(out, "\npackage json\n") {
		t.Errorf("json's package comment is missing:\n%s", out[:min(len(out), 400)])
	}
}
