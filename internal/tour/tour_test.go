package tour

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"squark"
)

func site(t *testing.T) *Site {
	t.Helper()
	sub, err := fs.Sub(squark.Tour, "tour")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(sub)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestLessons compiles and runs every lesson as the page would: its code must run cleanly (or end
// with the exit status it declares), and where it has a goal, its code must not meet the goal
// yet and its solution must. TOUR=chapter or TOUR=chapter/lesson runs some of them.
func TestLessons(t *testing.T) {
	s := site(t)
	only := os.Getenv("TOUR")
	for _, ch := range s.Chapters {
		for _, l := range ch.Lessons {
			id := ch.Slug + "/" + l.Slug
			if only != "" && id != only && ch.Slug != only {
				continue
			}
			t.Run(id, func(t *testing.T) {
				t.Parallel()
				r, err := Run(context.Background(), l.Code, l.Mode)
				if err != nil {
					t.Fatal(err)
				}
				if os.Getenv("TOUR_SHOW") != "" {
					t.Logf("stage %s exit %d:\n%s", r.Stage, r.Exit, r.Output)
				}
				if l.Goal != nil {
					if l.Goal.Met(r) {
						t.Errorf("the lesson's code already meets its goal (%s %q):\n%s", l.Goal.Kind, l.Goal.Text, r.Output)
					}
					sol, err := Run(context.Background(), l.Solution, l.Mode)
					if err != nil {
						t.Fatal(err)
					}
					if !l.Goal.Met(sol) {
						t.Errorf("the solution doesn't meet the goal (%s %q): stage %s, exit %d:\n%s", l.Goal.Kind, l.Goal.Text, sol.Stage, sol.Exit, sol.Output)
					}
					return
				}
				want := "run"
				if l.Mode == "build" {
					want = "built"
				}
				if r.Stage != want || r.Exit != l.Exit || r.TimedOut {
					t.Errorf("stage %s (want %s), exit %d (want %d), timed out %v:\n%s", r.Stage, want, r.Exit, l.Exit, r.TimedOut, r.Output)
				}
			})
		}
	}
}

func TestMarkdown(t *testing.T) {
	got := renderMarkdown(strings.Split("A `x * y` and **b** *c* [d](http://e) [[⇧⏎]].\n\n- one\n  more\n- two\n\n> note\n\n```squark\nfn f() {}\n```", "\n"))
	for _, want := range []string{
		"<p>A <code>x * y</code> and <strong>b</strong> <em>c</em> <a href=\"http://e\" target=\"_blank\" rel=\"noopener\">d</a> <kbd>⇧⏎</kbd>.</p>",
		"<li>one more</li>", "<li>two</li>", "<aside>note</aside>",
		`<pre class="snippet" data-lang="squark"><code>fn f() {}</code></pre>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ src, err string }{
		{"no title", "starts with"},
		{"# T\n## L\n", "no ```squark editor"},
		{"# T\n## L\ngoal: prints \"x\"\n\n```squark editor\nprogram\n```\n", "needs a ```squark solution"},
		{"# T\n## L\ngoal: makes coffee\n\n```squark editor\nprogram\n```\n", "bad goal"},
	} {
		if _, err := parseChapter("t", tc.src); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%q: got %v, want an error containing %q", tc.src, err, tc.err)
		}
	}
}

// TestHandlerGuards checks that only localhost requests with the tour's header run code.
func TestHandlerGuards(t *testing.T) {
	h := site(t).Handler()
	for _, tc := range []struct {
		host   string
		header bool
		want   int
	}{
		{"evil.example:3999", true, http.StatusForbidden},
		{"127.0.0.1:3999", false, http.StatusForbidden},
		{"localhost:3999", true, http.StatusOK},
		{"[::1]:3999", true, http.StatusOK},
	} {
		req := httptest.NewRequest("POST", "/api/check", strings.NewReader(`{"code":"program\nfn main() {}\n"}`))
		req.Host = tc.host
		if tc.header {
			req.Header.Set("X-Squark-Tour", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("host %s, header %v: status %d, want %d: %s", tc.host, tc.header, w.Code, tc.want, w.Body)
		}
	}
	req := httptest.NewRequest("GET", "/lessons.json", nil)
	req.Host = "localhost:3999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"live":true`) {
		t.Errorf("lessons.json: %d %.200s", w.Code, w.Body)
	}
}
