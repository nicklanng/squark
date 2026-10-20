package compiler

import (
	"fmt"
	"path/filepath"
	"strings"
)

// TestEntry is the function a test build runs instead of main: it runs the test its first
// argument names.
const TestEntry = "squark_test_main"

// TestFunc is a test: fn test_NAME() or fn test_NAME() -> ! in one of the package's _test.sq files.
type TestFunc struct {
	Name string
	Err  bool // it returns an error
	Pos  Pos
}

// findTests collects the tests of a package's _test.sq files and keeps each out of line, so its
// proven stack is its own.
func findTests(files []*File) ([]TestFunc, ErrorList) {
	var tests []TestFunc
	var errs ErrorList
	for _, f := range files {
		if !strings.HasSuffix(f.Path, "_test.sq") {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*FuncDecl)
			if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "test_") {
				continue
			}
			res := fd.Type.Results
			isErr := len(res) == 1 && len(res[0].Names) == 0 && isIdent(res[0].Type, "error")
			if fd.Kind != KindFunc || fd.Body == nil || len(fd.Type.Params) > 0 || len(res) > 0 && !isErr {
				errs = append(errs, &Error{fd.P, fmt.Sprintf("test %s must be fn %s() or fn %s() -> ! (no parameters; nothing or an error as its result)", fd.Name.Name, fd.Name.Name, fd.Name.Name)})
				continue
			}
			fd.Pragmas = append(fd.Pragmas, "noinline")
			tests = append(tests, TestFunc{Name: fd.Name.Name, Err: isErr, Pos: fd.P})
		}
	}
	return tests, errs
}

func isIdent(x Expr, name string) bool {
	id, ok := x.(*Ident)
	return ok && id.Name == name
}

// testMain is the source of the file a test build adds to the package (named pkgName): its
// entry runs the test named by its first argument. A test that returns an error prints it and
// exits with status 1; a panic exits with 2, as always.
func testMain(dir, pkgName string, tests []TestFunc) (*File, error) {
	var b strings.Builder
	b.WriteString(headerName(pkgName) + "\n\nuse os as squark_test_os\n\n")
	fmt.Fprintf(&b, "fn %s() {\n\tif squark_test_os.n_args() < 2 {\n\t\treturn\n\t}\n\tswitch squark_test_os.arg(1) {\n", TestEntry)
	for _, t := range tests {
		fmt.Fprintf(&b, "\tcase %q:\n", t.Name)
		if t.Err {
			fmt.Fprintf(&b, "\t\tsquark_test_check(%s())\n", t.Name)
		} else {
			fmt.Fprintf(&b, "\t\t%s()\n", t.Name)
		}
	}
	b.WriteString("\t}\n}\n\n")
	b.WriteString(`fn squark_test_check(err error) {
	if err == nil {
		return
	}
	if err.code() != 0 {
		print("error: ", err.message(), " (code ", err.code(), ")\n")
	} else {
		print("error: ", err.message(), "\n")
	}
	squark_test_os.exit(1)
}
`)
	return ParseFile(filepath.Join(dir, "squark_test_main.sq"), []byte(b.String()))
}
