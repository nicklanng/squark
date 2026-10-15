package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// asanOptions makes AddressSanitizer catch what the lifetime checker exists to prevent: a reference
// used after its frame returned (detect_stack_use_after_return) or its block ended. The runtime
// installs its own (static) signal stack, so ASan must not manage one: it would munmap ours when a
// scheduler thread exits.
const asanOptions = "detect_stack_use_after_return=1:detect_stack_use_after_scope=1:use_sigaltstack=0:exitcode=86"

// needClangASan skips a test under gcc: gcc's AddressSanitizer loses track of the fake stack across
// squark's goroutine stack switches ("False positive error reports may follow"), clang's doesn't.
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
// compiles; BODY runs in main or in a task, to cover the main stack and a goroutine's.
const danglingProg = `package main

import "runtime"

//squark:noinline
func leak() *int {
	x := 1234
	return (*int)(rawptr(&x))
}

//squark:noinline
func clobber() int {
	var junk [64]int
	for i := range 64 {
		junk[i] = -1
	}
	return junk[7]
}

task t() pool 1 {
	p := leak()
	println(clobber(), *p)
}

func main() {
MAIN}
`

// TestASanCatchesDangling makes sure the harness sees the bug class at all.
func TestASanCatchesDangling(t *testing.T) {
	needClangASan(t)
	for where, body := range map[string]string{
		"main": "\tp := leak()\n\tprintln(clobber(), *p)\n\truntime.Gosched()\n",
		"task": "\tgo t()\n\truntime.Gosched()\n",
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
