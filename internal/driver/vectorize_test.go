package driver

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestVectorize builds testdata/vectorize with clang's loop-vectorization remarks and checks
// that every function marked "want: vectorized" got a vectorized loop. Benchmarks show the speed
// (bench/run.sh with ONLY=kernels); this catches a codegen change that stops clang vectorizing.
func TestVectorize(t *testing.T) {
	if strings.Contains(os.Getenv("CC"), "gcc") {
		t.Skip("vectorization remarks need clang (CC=" + os.Getenv("CC") + ")")
	}
	dir := "../../testdata/vectorize"
	src, err := os.ReadFile(filepath.Join(dir, "main.sq"))
	if err != nil {
		t.Fatal(err)
	}
	// each function's first line, and which ones want a vectorized loop
	type fn struct {
		name string
		line int
		want bool
	}
	var fns []fn
	funcRE := regexp.MustCompile(`^func (\w+)\(`)
	want := false
	sc := bufio.NewScanner(bytes.NewReader(src))
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if strings.TrimSpace(text) == "// want: vectorized" {
			want = true
		}
		if m := funcRE.FindStringSubmatch(text); m != nil {
			fns = append(fns, fn{m[1], line, want})
			want = false
		}
	}
	var remarks bytes.Buffer
	if _, err := Build(Options{Dir: dir, Output: filepath.Join(t.TempDir(), "vec"), Remarks: &remarks}); err != nil {
		t.Fatal(err)
	}
	// clang places a loop's remark on a line of the function it's in
	got := map[string]bool{}
	remarkRE := regexp.MustCompile(`vectorize/main\.sq:(\d+):\d+: remark: vectorized loop`)
	for _, m := range remarkRE.FindAllStringSubmatch(remarks.String(), -1) {
		line, _ := strconv.Atoi(m[1])
		i := sort.Search(len(fns), func(i int) bool { return fns[i].line > line }) - 1
		if i >= 0 {
			got[fns[i].name] = true
		}
	}
	for _, f := range fns {
		if f.want && !got[f.name] {
			t.Errorf("%s (main.sq:%d): loop not vectorized", f.name, f.line)
		}
	}
	if t.Failed() {
		t.Logf("clang remarks:\n%s", remarks.String())
	}
}
