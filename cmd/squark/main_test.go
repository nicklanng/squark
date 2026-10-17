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
