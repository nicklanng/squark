package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// squarkBin builds this command once per test binary.
func squarkBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "squark")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func writeProgram(t *testing.T, dir, src string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sleeper = "program\n\nuse os, time\n\nfn main() {\n\tprintln(\"ready\", os.getpid())\n\ttime.sleep(time.hour)\n}\n"

// TestRunSignals: a signal to `squark run` reaches the program, the exit status says which signal
// (128+n, as a shell reports it), the program doesn't outlive squark, and the temporary binary is
// removed.
func TestRunSignals(t *testing.T) {
	squark := squarkBin(t)
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGTERM, 143}, {syscall.SIGINT, 130}, {syscall.SIGHUP, 129}, {syscall.SIGQUIT, 131}, {syscall.SIGALRM, 142}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			root := t.TempDir()
			prog := filepath.Join(root, "sleeper")
			writeProgram(t, prog, sleeper)
			tmp := filepath.Join(root, "tmp")
			os.Mkdir(tmp, 0o755)
			cmd := exec.Command(squark, "run", prog)
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.WaitDelay = 2 * time.Second // an orphaned program would hold stdout open
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			for i := 0; !strings.Contains(stdout.String(), "ready"); i++ {
				if i == 200 {
					cmd.Process.Kill()
					t.Fatal("program did not start")
				}
				time.Sleep(25 * time.Millisecond)
			}
			cmd.Process.Signal(tc.sig) // only squark, not the program
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				t.Fatal("squark run did not exit")
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != tc.code {
				t.Errorf("exit: %v, want status %d", err, tc.code)
			}
			if left, _ := os.ReadDir(tmp); len(left) > 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
			if f := strings.Fields(stdout.String()); len(f) == 2 {
				pid, _ := strconv.Atoi(f[1])
				if pid > 0 && syscall.Kill(pid, 0) == nil {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Errorf("the program (pid %d) outlived squark run", pid)
				}
			} else {
				t.Errorf("program output %q", stdout.String())
			}
		})
	}
}

// TestRunSignalDuringBuild: a signal while `squark run` is still compiling ends it with 128+n and
// removes its temporary directories (the build's too).
func TestRunSignalDuringBuild(t *testing.T) {
	squark := squarkBin(t)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			prog := filepath.Join(root, "sleeper")
			writeProgram(t, prog, sleeper)
			tmp := filepath.Join(root, "tmp")
			os.Mkdir(tmp, 0o755)
			// a C compiler that says when it starts, then takes its time
			marker := filepath.Join(root, "cc-started")
			slowcc := filepath.Join(root, "slowcc")
			script := "#!/bin/sh\ntouch " + marker + "\nsleep 1\nexec cc \"$@\"\n"
			if err := os.WriteFile(slowcc, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(squark, "run", prog)
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "CC="+slowcc)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			for i := 0; ; i++ {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if i == 400 {
					cmd.Process.Kill()
					t.Fatal("the build did not start")
				}
				time.Sleep(25 * time.Millisecond)
			}
			cmd.Process.Signal(sig)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				t.Fatal("squark run did not exit")
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 128+int(sig) {
				t.Errorf("exit: %v, want status %d", err, 128+int(sig))
			}
			time.Sleep(1500 * time.Millisecond) // the compiler, still running, finds its directory gone
			if left, _ := os.ReadDir(tmp); len(left) > 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
		})
	}
}

func TestCLIArgs(t *testing.T) {
	squark := squarkBin(t)
	root := t.TempDir()
	prog := filepath.Join(root, "hello")
	writeProgram(t, prog, "program\n\nfn main() { println(1) }\n")

	// flags after the directory are not silently ignored
	out, err := exec.Command(squark, "build", prog, "-o", filepath.Join(root, "x")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unexpected arguments") {
		t.Errorf("build dir -o x: %v %s", err, out)
	}

	// the default output name may not be the source directory itself
	cmd := exec.Command(squark, "build", "hello")
	cmd.Dir = root
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "already exists and is a directory") {
		t.Errorf("build hello from its parent: %v %s", err, out)
	}
}

// TestCLIFlagsPerCommand (V62): a flag the subcommand doesn't take is an error (exit 2), not
// silently ignored, and -manifest's value is checked before anything is built.
func TestCLIFlagsPerCommand(t *testing.T) {
	bin := squarkBin(t)
	dir := filepath.Join(t.TempDir(), "p")
	writeProgram(t, dir, "program\n\nfn main() { println(\"ok\") }\n")
	out := filepath.Join(t.TempDir(), "out")
	for _, args := range [][]string{
		{"run", "-o", out, dir},
		{"run", "-static", dir},
		{"check", "-cpu", "bogus", dir},
		{"doc", "-v", dir},
		{"build", "-manifest", "nomad", "-o", out, dir},
		{"test", "-o", out, dir},
		{"build", "-run", "x", dir},
		{"test", "-run", "(", dir},
		{"fmt", "-v", dir},
		{"lsp", "-v"},
		{"lsp", dir},
	} {
		cmd := exec.Command(bin, args...)
		msg, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("squark %s: %v, %s; want exit 2", strings.Join(args, " "), err, msg)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("a binary was built before -manifest was checked")
	}
	if msg, err := exec.Command(bin, "run", "-v", dir).CombinedOutput(); err != nil || !strings.HasSuffix(string(msg), "ok\n") {
		t.Errorf("squark run -v: %v\n%s", err, msg)
	}
}

// TestCLITest: squark test's exit status is 0 when every test passes and 1 when one fails or a
// package doesn't build; dir/... finds the packages.
func TestCLITest(t *testing.T) {
	bin := squarkBin(t)
	root := t.TempDir()
	writeProgram(t, filepath.Join(root, "a"), "program\n\nfn main() {}\n")
	os.WriteFile(filepath.Join(root, "a", "a_test.sq"), []byte("program\n\nfn test_one() { assert(1 + 1 == 2) }\n"), 0o644)
	writeProgram(t, filepath.Join(root, "b"), "module b\n")
	os.WriteFile(filepath.Join(root, "b", "b_test.sq"), []byte("module b\n\nfn test_two() { assert(1 + 1 == 3) }\n"), 0o644)
	for _, tc := range []struct {
		args   []string
		status int
		want   string
	}{
		{[]string{"test", filepath.Join(root, "a")}, 0, "ok   " + filepath.Join(root, "a") + "  1 test ("},
		{[]string{"test", root + "/..."}, 1, "--- FAIL test_two ("},
		{[]string{"test", "-run", "one", root + "/..."}, 0, "?    " + filepath.Join(root, "b") + "  no tests match -run one ("},
		{[]string{"test", filepath.Join(root, "missing")}, 1, "FAIL " + filepath.Join(root, "missing") + "  build failed"},
	} {
		msg, err := exec.Command(bin, tc.args...).CombinedOutput()
		status := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if status != tc.status || !strings.Contains(string(msg), tc.want) {
			t.Errorf("squark %s: status %d, want %d with %q:\n%s", strings.Join(tc.args, " "), status, tc.status, tc.want, msg)
		}
	}
}

// TestCLIFmt: squark fmt prints, lists (-l) or rewrites (-w), and a file that doesn't parse is
// reported and left alone (exit 1).
func TestCLIFmt(t *testing.T) {
	bin := squarkBin(t)
	dir := t.TempDir()
	messy := filepath.Join(dir, "messy.sq")
	tidy := filepath.Join(dir, "tidy.sq")
	broken := filepath.Join(dir, "sub", "broken.sq")
	os.WriteFile(messy, []byte("program\n\nfn main() {\nx:=1\nprintln(x)\n}\n"), 0o644)
	os.WriteFile(tidy, []byte("module tidy\n"), 0o644)
	os.MkdirAll(filepath.Dir(broken), 0o755)
	os.WriteFile(broken, []byte("program\n\nfn main( {\n"), 0o644)
	want := "program\n\nfn main() {\n\tx := 1\n\tprintln(x)\n}\n"
	if out, err := exec.Command(bin, "fmt", messy).Output(); err != nil || string(out) != want {
		t.Errorf("fmt file: %v %q", err, out)
	}
	if out, err := exec.Command(bin, "fmt", "-l", dir).Output(); err != nil || string(out) != messy+"\n" {
		t.Errorf("fmt -l dir: %v %q", err, out)
	}
	out, err := exec.Command(bin, "fmt", "-l", dir+"/...").CombinedOutput()
	if err == nil || !strings.Contains(string(out), messy+"\n") || !strings.Contains(string(out), "broken.sq:3") {
		t.Errorf("fmt -l dir/...: %v %q", err, out)
	}
	if out, err := exec.Command(bin, "fmt", "-w", messy, tidy).CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("fmt -w: %v %q", err, out)
	}
	if b, _ := os.ReadFile(messy); string(b) != want {
		t.Errorf("after -w: %q", b)
	}
	if b, _ := os.ReadFile(broken); string(b) != "program\n\nfn main( {\n" {
		t.Errorf("a file that doesn't parse was changed: %q", b)
	}
}
