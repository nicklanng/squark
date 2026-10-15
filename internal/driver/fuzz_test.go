package driver

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFuzzSoundness mutates the test programs and runs every mutant the checker accepts under
// AddressSanitizer. It looks for the bugs this compiler has had most: a reference the lifetime
// checker lets outlive its memory (an ASan report), a Go panic in the compiler, and C the compiler
// generates but clang rejects. Off by default:
//
//	SQUARK_FUZZ=2000 go test ./internal/driver -run TestFuzzSoundness -v -timeout 1h
//
// SQUARK_FUZZ_SEED picks the random seed (default: the time). Findings are written to a directory
// the log names, one per distinct report, each with the mutant's main.sq and what went wrong.
func TestFuzzSoundness(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("SQUARK_FUZZ"))
	if n <= 0 {
		t.Skip("set SQUARK_FUZZ=<mutants> to fuzz")
	}
	needClangASan(t)
	seed := time.Now().UnixNano()
	if s, err := strconv.ParseInt(os.Getenv("SQUARK_FUZZ_SEED"), 10, 64); err == nil {
		seed = s
	}
	corpus := fuzzCorpus(t)
	outDir, err := os.MkdirTemp("", fmt.Sprintf("squark-fuzz-%d-", seed))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed %d, %d seed programs, findings in %s", seed, len(corpus), outDir)

	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		findings int
		stats    = map[string]int{}
	)
	record := func(kind, src, report string) {
		key := kind + ": " + reportKey(report)
		mu.Lock()
		defer mu.Unlock()
		stats[kind]++
		if seen[key] {
			return
		}
		seen[key] = true
		findings++
		dir := filepath.Join(outDir, fmt.Sprintf("%03d-%s", findings, kind))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644)
		os.WriteFile(filepath.Join(dir, "report.txt"), []byte(report), 0o644)
		t.Errorf("%s: %s (%s)", kind, key, dir)
	}

	jobs := make(chan int64)
	var wg sync.WaitGroup
	for w := 0; w < max(runtime.NumCPU()/2, 1); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				r := rand.New(rand.NewSource(s))
				base := corpus[r.Intn(len(corpus))]
				src := mutate(r, base)
				kind, report := tryMutant(t, src)
				mu.Lock()
				stats["tried"]++
				if kind == "rejected" || kind == "accepted" {
					stats[kind]++
				}
				mu.Unlock()
				if kind != "rejected" && kind != "accepted" {
					record(kind, src, report)
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- seed + int64(i)
	}
	close(jobs)
	wg.Wait()
	t.Logf("stats: %v", stats)
}

// fuzzCorpus is every program in testdata/run and testdata/errors. The error tests are good seeds:
// deleting the line with the error often leaves a pointer-heavy program that the checker accepts.
func fuzzCorpus(t *testing.T) []string {
	var corpus []string
	for _, pat := range []string{"../../testdata/run/*.sq", "../../testdata/errors/*.sq"} {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src := string(b)
			// servers and timing-dependent programs make poor mutants: they block or time out
			if strings.Contains(src, "net.Listen") || strings.Contains(src, "threads ") || strings.Contains(src, "workers ") {
				continue
			}
			corpus = append(corpus, src)
		}
	}
	return corpus
}

var identRe = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\b`)

var keywords = map[string]bool{"package": true, "import": true, "func": true, "var": true, "const": true,
	"type": true, "struct": true, "map": true, "chan": true, "return": true, "if": true, "else": true,
	"for": true, "range": true, "switch": true, "case": true, "default": true, "break": true,
	"continue": true, "go": true, "defer": true, "select": true, "task": true, "pool": true, "arena": true,
	"in": true, "try": true, "cap": true, "extern": true, "shared": true, "thread": true, "main": true}

// mutate applies one to three line or token edits. They are chosen to move references around:
// dropping a statement (often the one an error test expects to be rejected), duplicating or
// swapping statements, renaming an identifier to another in the file, and adding or removing &.
func mutate(r *rand.Rand, src string) string {
	lines := strings.Split(src, "\n")
	for i := len(lines) - 1; i >= 0; i-- { // error tests' annotations are noise to the mutant
		if j := strings.Index(lines[i], "// ERROR"); j >= 0 {
			lines[i] = strings.TrimRight(lines[i][:j], " \t")
		}
	}
	stmt := func() int {
		var c []int
		for i, l := range lines {
			tl := strings.TrimSpace(l)
			if strings.HasPrefix(l, "\t") && tl != "" && tl != "}" && !strings.HasSuffix(tl, "{") && !strings.HasPrefix(tl, "//") && !strings.HasPrefix(tl, "case ") && tl != "default:" {
				c = append(c, i)
			}
		}
		if len(c) == 0 {
			return -1
		}
		return c[r.Intn(len(c))]
	}
	idents := identRe.FindAllString(strings.Join(lines, "\n"), -1)
	var names []string
	for _, id := range idents {
		if !keywords[id] {
			names = append(names, id)
		}
	}
	for k := 0; k < 1+r.Intn(3); k++ {
		i := stmt()
		if i < 0 {
			break
		}
		switch r.Intn(6) {
		case 0: // delete
			lines = append(lines[:i], lines[i+1:]...)
		case 1: // duplicate after another statement
			j := stmt()
			lines = append(lines[:j+1], append([]string{lines[i]}, lines[j+1:]...)...)
		case 2: // swap
			j := stmt()
			lines[i], lines[j] = lines[j], lines[i]
		case 3: // rename one identifier occurrence
			locs := identRe.FindAllStringIndex(lines[i], -1)
			if len(locs) == 0 || len(names) == 0 {
				continue
			}
			l := locs[r.Intn(len(locs))]
			if keywords[lines[i][l[0]:l[1]]] {
				continue
			}
			lines[i] = lines[i][:l[0]] + names[r.Intn(len(names))] + lines[i][l[1]:]
		case 4: // take an address
			locs := identRe.FindAllStringIndex(lines[i], -1)
			if len(locs) == 0 {
				continue
			}
			l := locs[r.Intn(len(locs))]
			lines[i] = lines[i][:l[0]] + "&" + lines[i][l[0]:]
		case 5: // drop an address
			if j := strings.Index(lines[i], "&"); j >= 0 && !strings.HasPrefix(lines[i][j:], "&&") {
				lines[i] = lines[i][:j] + lines[i][j+1:]
			}
		}
	}
	return strings.Join(lines, "\n")
}

// tryMutant checks src, and if it is accepted builds it with ASan and runs it. It returns
// "rejected", "accepted" (ran without anything wrong), or a finding: "compiler-crash" (a panic or
// fatal error in the compiler), "cc-failure", "asan" or "crash" (a signal other than a panic).
// The compiler runs in a child process (TestFuzzChild): a Go stack overflow can't be recovered.
func tryMutant(t *testing.T, src string) (kind, report string) {
	dir, err := os.MkdirTemp("", "squark-fuzz-mutant-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "main.sq"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFuzzChild$")
	child.Env = append(os.Environ(), "SQUARK_FUZZ_CHILD="+dir)
	out, _ := child.CombinedOutput()
	result, rest, _ := strings.Cut(string(out), "\n")
	switch {
	case ctx.Err() != nil:
		return "compiler-hang", "the compiler ran for over 60s"
	case result == "RESULT rejected":
		return "rejected", ""
	case result == "RESULT cc-failure":
		return "cc-failure", rest
	case result != "RESULT built":
		return "compiler-crash", string(out)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	cmd := exec.CommandContext(ctx2, filepath.Join(dir, "prog"))
	cmd.Env = append(os.Environ(), "ASAN_OPTIONS="+asanOptions)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Run()
	switch e := stderr.String(); {
	case strings.Contains(e, "AddressSanitizer"):
		return "asan", e
	case strings.Contains(e, "unexpected signal"):
		return "crash", e
	}
	return "accepted", ""
}

// TestFuzzChild is the compiler half of tryMutant, run in a child process: it checks and builds
// the mutant in $SQUARK_FUZZ_CHILD and prints the outcome on its first line.
func TestFuzzChild(t *testing.T) {
	dir := os.Getenv("SQUARK_FUZZ_CHILD")
	if dir == "" {
		t.Skip("only run by TestFuzzSoundness")
	}
	if err := Check(dir); err != nil {
		fmt.Println("RESULT rejected")
		return
	}
	if _, err := Build(Options{Dir: dir, Output: filepath.Join(dir, "prog"), Sanitize: "address"}); err != nil {
		if strings.Contains(err.Error(), "squark compiler bug") {
			fmt.Printf("RESULT cc-failure\n%v\n", err)
			return
		}
		fmt.Println("RESULT rejected") // e.g. a stack proof error after checking
		return
	}
	fmt.Println("RESULT built")
}

// reportKey reduces a report to what identifies the bug: the ASan error kind and the first frame
// in the program, or the first line of anything else.
func reportKey(report string) string {
	if m := regexp.MustCompile(`AddressSanitizer: ([a-z-]+)`).FindStringSubmatch(report); m != nil {
		if f := regexp.MustCompile(`#\d+ 0x[0-9a-f]+ in (\S+)`).FindStringSubmatch(report); f != nil {
			return m[1] + " in " + f[1]
		}
		return m[1]
	}
	line, _, _ := strings.Cut(strings.TrimSpace(report), "\n")
	return regexp.MustCompile(`/[^ :]*/`).ReplaceAllString(line, "") // drop temp paths
}
