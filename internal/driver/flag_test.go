package driver

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// flagUsage is testdata/flag's usage message, as Go's flag package prints it.
const flagUsage = `Usage of PROG:
  -color
    	color the output (default true)
  -count int
    	how many
  -db file
    	a file to keep the notes in (default "notes.db")
  -idle duration
    	close a connection idle
    	for this long (default 30s)
  -name string
    	who to greet
  -port int
    	port to listen on (default 8080)
  -v	verbose
`

// TestFlag runs testdata/flag with command lines covering std/flag: values in each form, flags
// ending at "--" or a positional argument, and the messages and exit codes of -h and of bad flags
// (all as Go's flag package prints them). With threads every scheduler thread parses, and the
// usage must still be printed once.
func TestFlag(t *testing.T) {
	src, err := os.ReadFile("../../testdata/flag/main.sq")
	if err != nil {
		t.Fatal(err)
	}
	defaults := "port 8080 db notes.db v false idle 30s\nname  count 0 color true\n"
	cases := []struct {
		args   []string
		code   int
		stdout string // the whole of stdout
		stderr string // the first line of stderr, followed by the usage ("" for none)
	}{
		{nil, 0, defaults + "parsed false true nflag 0 narg 0\nout of range: true true\n", ""},
		{[]string{"-port", "9090", "-db=x.db", "--v", "--idle=1h2m3.5s", "-name", "a=b", "-count=0x1f", "-color=false"}, 0,
			"port 9090 db x.db v true idle 1h2m3.5s\nname a=b count 31 color false\nparsed false true nflag 7 narg 0\nout of range: true true\n", ""},
		{[]string{"-v", "a", "-port", "1"}, 0,
			"port 8080 db notes.db v true idle 30s\nname  count 0 color true\nparsed false true nflag 1 narg 3\narg 0 a a\narg 1 -port -port\narg 2 1 1\nout of range: true true\n", ""},
		{[]string{"-port=7", "--", "-v"}, 0,
			"port 7 db notes.db v false idle 30s\nname  count 0 color true\nparsed false true nflag 1 narg 1\narg 0 -v -v\nout of range: true true\n", ""},
		{[]string{"-", "x"}, 0, defaults + "parsed false true nflag 0 narg 2\narg 0 - -\narg 1 x x\nout of range: true true\n", ""},
		{[]string{"-count", "-1_000", "-count", "-9223372036854775808"}, 0,
			"port 8080 db notes.db v false idle 30s\nname  count -9223372036854775808 color true\nparsed false true nflag 1 narg 0\nout of range: true true\n", ""},
		{[]string{"-h"}, 0, "", "\n"},
		{[]string{"-help", "-x"}, 0, "", "\n"},
		{[]string{"-x"}, 2, "", "flag provided but not defined: -x\n"},
		{[]string{"-port", "abc"}, 2, "", "invalid value \"abc\" for flag -port: parse error\n"},
		{[]string{"-port=9223372036854775808"}, 2, "", "invalid value \"9223372036854775808\" for flag -port: value out of range\n"},
		{[]string{"-port", "1__0"}, 2, "", "invalid value \"1__0\" for flag -port: parse error\n"},
		{[]string{"-idle", "5"}, 2, "", "invalid value \"5\" for flag -idle: parse error\n"},
		{[]string{"-v=maybe"}, 2, "", "invalid boolean value \"maybe\" for -v: parse error\n"},
		{[]string{"-name"}, 2, "", "flag needs an argument: -name\n"},
		{[]string{"---x"}, 2, "", "bad flag syntax: ---x\n"},
		{[]string{"-name", "a\tb\"\x01"}, 0,
			"port 8080 db notes.db v false idle 30s\nname a\tb\"\x01 count 0 color true\nparsed false true nflag 1 narg 0\nout of range: true true\n", ""},
		{[]string{"-db", "x", "-port", "q\n"}, 2, "", "invalid value \"q\\n\" for flag -port: parse error\n"},
	}
	for _, threads := range []int{1, 4} {
		t.Run(fmt.Sprintf("threads=%d", threads), func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "main.sq"), src, 0o644)
			if threads > 1 {
				os.WriteFile(filepath.Join(dir, "threads.sq"), []byte(fmt.Sprintf("package main\n\nthreads %d\n", threads)), 0o644)
			}
			bin := filepath.Join(dir, "prog")
			if _, err := Build(Options{Dir: dir, Output: bin}); err != nil {
				t.Fatal(err)
			}
			for _, c := range cases {
				cmd := exec.Command(bin, c.args...)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				gotOut, wantOut := stdout.String(), c.stdout
				if threads > 1 && wantOut != "" {
					// each thread runs main, and their lines interleave
					wantOut = sortedLines(strings.Repeat(wantOut, threads))
					gotOut = sortedLines(gotOut)
				}
				wantErr := ""
				if c.stderr != "" {
					wantErr = strings.TrimPrefix(c.stderr, "\n") + flagUsage
				}
				gotErr := strings.ReplaceAll(stderr.String(), bin, "PROG")
				if code != c.code || gotOut != wantOut || gotErr != wantErr {
					t.Errorf("%q: exit %d, want %d\n--- stdout\n%s--- want\n%s--- stderr\n%s--- want\n%s",
						c.args, code, c.code, gotOut, wantOut, gotErr, wantErr)
				}
			}
		})
	}
}

func sortedLines(s string) string {
	lines := strings.SplitAfter(s, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "")
}
