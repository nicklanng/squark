package driver

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// writePkg writes files (name to source) into a new directory under dir.
func writePkg(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const testedModule = `module mathx

pub fn add(a, b int) -> int { return a + b }

fault Negative 7 "negative"

fn check(x int) -> ! {
	if x < 0 {
		return Negative
	}
	return nil
}

thread hits int
`

const testedModuleTests = `module mathx

use testing, time

fn test_add() {
	assert(add(2, 2) == 4)
}

fn test_assert_values() {
	println("output before")
	s := "a\tb"
	assert(s == "a b")
}

fn test_assert_left() {
	assert(add(2, 2) == 5, "two and two")
}

fn test_error() -> ! {
	check(-1)!
	return nil
}

fn test_nil_error() -> ! {
	check(1)!
	return nil
}

fn test_expected_panic() {
	testing.expect_panic("vector is full")
	var v int[..1]
	v.push(1)
	v.push(2)
}

fn test_wrong_panic() {
	testing.expect_panic("boom")
	panic("bang")
}

fn test_no_panic() {
	testing.expect_panic("boom")
}

fn test_skipped() {
	testing.skip("needs a database")
}

fn test_fresh_a() {
	assert(hits == 0)
	hits++
}

fn test_fresh_b() {
	assert(hits == 0)
	hits++
}

fn test_hangs() {
	time.sleep(time.minute)
}

fn helper_not_a_test() {
	panic("not run")
}
`

// TestSquarkTest runs squark test's runner on a module whose tests pass, fail (by an error, a
// panic, an assert, a wrong or missing expected panic and a timeout) and skip.
func TestSquarkTest(t *testing.T) {
	dir := writePkg(t, filepath.Join(t.TempDir(), "mathx"), map[string]string{"mathx.sq": testedModule, "mathx_test.sq": testedModuleTests})
	var out bytes.Buffer
	sum, err := Test(TestOptions{Dir: dir, Out: &out, Timeout: 2 * time.Second, Parallel: 4})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log(got)
	if sum != (TestSummary{Passed: 5, Failed: 6, Skipped: 1}) {
		t.Errorf("summary %+v, want 5 passed, 6 failed, 1 skipped", sum)
	}
	for _, want := range []string{
		"--- FAIL test_assert_values (",
		"    output before\n    panic: assertion failed: s == \"a b\"\n    \tleft:  \"a\\tb\"\n",
		"    panic: assertion failed: add(2, 2) == 5: two and two\n    \tleft:  4\n\n",
		"mathx_test.sq:16",
		"--- FAIL test_error (",
		"    error: negative (code 7)\n",
		"--- FAIL test_wrong_panic (",
		"panicked, but not with \"boom\"",
		"--- FAIL test_no_panic (",
		"did not panic (expected a panic with \"boom\")",
		"--- FAIL test_hangs (",
		"timed out after 2s",
		"FAIL " + dir + "  6 of 12 failed, 1 skipped (",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, unwanted := range []string{"test_add (", "test_nil_error", "test_expected_panic", "test_fresh", "helper_not_a_test", "right:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output has %q", unwanted)
		}
	}
	// failures come in the tests' order, whatever order they ran in
	if a, b := strings.Index(got, "test_assert_values"), strings.Index(got, "test_hangs"); a < 0 || b < a {
		t.Errorf("failures out of order")
	}

	out.Reset()
	sum, err = Test(TestOptions{Dir: dir, Out: &out, Verbose: true, Run: regexp.MustCompile(`^test_(add|skipped|fresh_a)$`)})
	if err != nil {
		t.Fatal(err)
	}
	got = out.String()
	if sum != (TestSummary{Passed: 2, Skipped: 1}) {
		t.Errorf("-run: summary %+v, want 2 passed and 1 skipped\n%s", sum, got)
	}
	for _, want := range []string{"--- ok test_add (", ", stack ", "--- skip test_skipped (", "    needs a database\n", "ok   " + dir + "  3 tests, 1 skipped ("} {
		if !strings.Contains(got, want) {
			t.Errorf("-v -run: output lacks %q\n%s", want, got)
		}
	}

	out.Reset()
	if _, err := Test(TestOptions{Dir: dir, Out: &out, Run: regexp.MustCompile(`nothing`)}); err != nil {
		t.Fatal(err)
	}
	if want := "?    " + dir + "  no tests match -run nothing ("; !strings.HasPrefix(out.String(), want) {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

// TestSquarkTestProgram tests a program: its main doesn't run, and its threads and budget don't
// apply (the test build has one thread and its own memory).
func TestSquarkTestProgram(t *testing.T) {
	dir := writePkg(t, filepath.Join(t.TempDir(), "prog"), map[string]string{
		"main.sq": `program {
	budget 1M
	threads 4
}

use runtime

task worker(n int, done int chan) pool 4 {
	done <- n * 2
}

fn double_all(done int chan) -> int {
	for i in 0..4 {
		start worker(i, done)
	}
	sum := 0
	for _ in 0..4 {
		sum += <-done
	}
	return sum
}

fn main() {
	panic("main ran")
}
`,
		"main_test.sq": `program

use os, runtime

thread done int chan[4]

fn test_tasks() {
	assert(double_all(done) == 12)
	assert(runtime.shards() == 1)
}

fn test_in_package_dir() -> ! {
	f := os.open("main.sq")!
	return f.close()
}
`})
	var out bytes.Buffer
	sum, err := Test(TestOptions{Dir: dir, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if sum != (TestSummary{Passed: 2}) {
		t.Errorf("summary %+v, want 2 passed\n%s", sum, out.String())
	}
	// the program still builds as it did, its tests left out
	if _, err := Build(Options{Dir: dir, Output: filepath.Join(t.TempDir(), "prog")}); err == nil || !strings.Contains(err.Error(), "memory budget exceeded") {
		t.Errorf("the program's own build: %v, want its budget exceeded", err)
	}
}

func TestSquarkTestErrors(t *testing.T) {
	for _, tc := range []struct{ test, want string }{
		{"fn test_args(x int) {}", "main_test.sq:3:1: test test_args must be fn test_args() or fn test_args() -> !"},
		{"fn test_result() -> int { return 1 }", "test test_result must be"},
		{"task test_task() pool 1 {}", "test test_task must be"},
		{"fn test_x() { assert(1) }", "assert needs a condition (have 1 of type int)"},
		{"fn test_x() { assert(true, \"why\", 3) }", "assert takes a condition and an optional message"},
		{"fn test_x() { assert(true, 3) }", "assert's message must be a string (have 3 of type int)"},
	} {
		dir := writePkg(t, filepath.Join(t.TempDir(), "p"), map[string]string{
			"main.sq":      "program\n\nfn main() {}\n",
			"main_test.sq": "program\n\n" + tc.test + "\n",
		})
		_, err := Test(TestOptions{Dir: dir, Out: new(bytes.Buffer)})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.test, err, tc.want)
		}
	}
}

func TestTestDirs(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "a/b", "c", "a/testdata/x", ".hidden"} {
		files := map[string]string{"x.sq": "module x\n"}
		if d != "c" {
			files["x_test.sq"] = "module x\n"
		}
		writePkg(t, filepath.Join(root, d), files)
	}
	dirs, err := TestDirs([]string{root + "/...", "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "a"), filepath.Join(root, "a/b"), "explicit"}
	if strings.Join(dirs, " ") != strings.Join(want, " ") {
		t.Errorf("got %q, want %q", dirs, want)
	}
}

// TestAssertMessages checks what a failed assert prints for each kind of operand.
func TestAssertMessages(t *testing.T) {
	cases := []struct{ body, want string }{
		{`x := 3; assert(x == 4)`, "assertion failed: x == 4\n\tleft:  3\n"},
		{`x := 3; assert(4 == x)`, "assertion failed: 4 == x\n\tright: 3\n"},
		{`a, b := 1, 2; assert(a > b)`, "assertion failed: a > b\n\tleft:  1\n\tright: 2\n"},
		{`f := 1.5; assert(f < 1.0)`, "assertion failed: f < 1.0\n\tleft:  +1.50000000e+000\n"},
		{`s := "x\n\"y\x01"; assert(s == "")`, "assertion failed: s == \"\"\n\tleft:  \"x\\n\\\"y\\x01\"\n"},
		{`d := Dir.east; assert(d == .north)`, "assertion failed: d == .north\n\tleft:  1\n"},
		{`var e error = Gone; assert(e == nil)`, "assertion failed: e == nil\n\n"},
		{`var e error = Gone; assert(e != Gone)`, "assertion failed: e != Gone\n\tleft:  gone\n"},
		{`var e error; assert(e == Gone)`, "assertion failed: e == Gone\n\tleft:  <nil>\n"},
		{`b := true; assert(not b)`, "assertion failed: not b\n\n"},
		{`assert(P{1, 2} == P{1, 3})`, "assertion failed: P{1, 2} == P{1, 3}\n\n"},
		{"x := 1; assert(x ==  // the text as written, on one line\n\t\t/* without comments */ 2)", "assertion failed: x == 2\n\tleft:  1\n"},
		{`s := "a"; assert(s == "//  x" + "/*" or s == "\"//")`, `assertion failed: s == "//  x" + "/*" or s == "\"//"` + "\n\n"},
		{`n := 2; assert(n < 1, "n is small")`, "assertion failed: n < 1: n is small\n\tleft:  2\n"},
		{`assert(next() == 5)`, "assertion failed: next() == 5\n\tleft:  1\n"},
	}
	var src strings.Builder
	src.WriteString("module m\n\nenum Dir { north, east }\n\nstruct P {\n\tx, y int\n}\n\nfault Gone 404 \"gone\"\n\nthread calls int\n\nfn next() -> int {\n\tcalls++\n\treturn calls\n}\n")
	for i, c := range cases {
		fmt.Fprintf(&src, "\nfn test_%d() {\n\t%s\n}\n", i, strings.ReplaceAll(c.body, "; ", "\n\t"))
	}
	dir := writePkg(t, filepath.Join(t.TempDir(), "m"), map[string]string{"m.sq": "module m\n", "m_test.sq": src.String()})
	var out bytes.Buffer
	sum, err := Test(TestOptions{Dir: dir, Out: &out, Parallel: 4})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != len(cases) {
		t.Errorf("%d failed, want %d", sum.Failed, len(cases))
	}
	// each failure's report, unindented
	reports := map[string]string{}
	for _, part := range strings.Split(out.String(), "--- FAIL ")[1:] {
		name, rest, _ := strings.Cut(part, " (")
		_, rest, _ = strings.Cut(rest, "\n")
		reports[name] = strings.ReplaceAll(rest, "\n    ", "\n")
	}
	for i, c := range cases {
		got := reports[fmt.Sprintf("test_%d", i)]
		if !strings.HasPrefix(got, "    panic: "+c.want) {
			t.Errorf("%s: got\n%s\nwant panic: %q", c.body, got, c.want)
		}
	}
}

// TestExampleTests runs the tests of the examples that have them.
func TestExampleTests(t *testing.T) {
	dirs, err := TestDirs([]string{"../../examples/..."})
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no example tests: %v", err)
	}
	for _, d := range dirs {
		var out bytes.Buffer
		sum, err := Test(TestOptions{Dir: d, Out: &out})
		if err != nil || sum.Failed > 0 || sum.Passed == 0 {
			t.Errorf("%s: %v\n%s", d, err, out.String())
		}
	}
}
