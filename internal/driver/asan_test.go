package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// needClangASan skips a test under gcc: gcc's AddressSanitizer loses track of the fake stack across
// squark's task stack switches ("False positive error reports may follow"), clang's doesn't.
func needClangASan(t *testing.T) {
	if strings.Contains(os.Getenv("CC"), "gcc") {
		t.Skip("the ASan harness needs clang (CC=" + os.Getenv("CC") + ")")
	}
}

// TestRunASan runs every TestRun program built with AddressSanitizer. A squark program that the
// checker accepts must never touch memory after its lifetime ends; ASan turns a lifetime-checker
// hole into a failing test instead of silently wrong output.
func TestRunASan(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: rebuilds every program with AddressSanitizer")
	}
	needClangASan(t)
	runPrograms(t, "address")
}

// danglingProg keeps a pointer into a returned frame. rawptr bypasses the lifetime checker, so it
// compiles; BODY runs in main or in a task, to cover the main stack and a task's.
const danglingProg = `program

use runtime

//squark:noinline
fn leak() -> *int {
	x := 1234
	return (*int)(rawptr(&x))
}

//squark:noinline
fn clobber() -> int {
	var junk int[64]
	for i in 0..64 {
		junk[i] = -1
	}
	return junk[7]
}

task t() pool 1 {
	p := leak()
	println(clobber(), *p)
}

fn main() {
MAIN}
`

// TestASanCatchesDangling makes sure the harness sees the bug class at all.
func TestASanCatchesDangling(t *testing.T) {
	needClangASan(t)
	for where, body := range map[string]string{
		"main": "\tp := leak()\n\tprintln(clobber(), *p)\n\truntime.gosched()\n",
		"task": "\tstart t()\n\truntime.gosched()\n",
	} {
		t.Run(where, func(t *testing.T) {
			dir := t.TempDir()
			src := strings.Replace(danglingProg, "MAIN", body, 1)
			if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "prog")
			if _, err := Build(Options{Dir: dir, Output: bin, Sanitize: "address"}); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin)
			cmd.Env = append(os.Environ(), "ASAN_OPTIONS="+asanOptions)
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), "stack-use-after-return") {
				t.Errorf("ASan did not report the dangling pointer:\n%s", out)
			}
		})
	}
}
