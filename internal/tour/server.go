package tour

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Site is the tour: its web page and its lessons.
type Site struct {
	Static   fs.FS // index.html, tour.css, tour.js
	Chapters []*Chapter
}

// New loads the tour from fsys, which holds site/ and lessons/.
func New(fsys fs.FS) (*Site, error) {
	static, err := fs.Sub(fsys, "site")
	if err != nil {
		return nil, err
	}
	chs, err := Load(fsys, "lessons")
	if err != nil {
		return nil, err
	}
	return &Site{Static: static, Chapters: chs}, nil
}

// index is what the page fetches first: the lessons, and whether a compiler is behind it.
type index struct {
	Live     bool       `json:"live"`
	Chapters []*Chapter `json:"chapters"`
}

const maxCode = 256 << 10

// Handler serves the tour with a compiler behind it. It only answers requests from this machine
// (a loopback peer) addressed to localhost, and only runs code for requests whose X-Squark-Tour
// header is key: a page on another origin can't send the header without a preflight that this
// handler refuses, and another user on the machine doesn't know the key, which squark tour puts
// in the URL it opens (?k=key).
func (s *Site) Handler(key string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(s.Static))
	mux.HandleFunc("GET /lessons.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(index{Live: true, Chapters: s.Chapters})
	})
	run := func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Squark-Tour")), []byte(key)) != 1 || key == "" {
			http.Error(w, "missing or wrong X-Squark-Tour key: open the tour at the URL squark tour printed", http.StatusForbidden)
			return
		}
		var req struct {
			Code string `json:"code"`
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxCode)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var res *Result
		var err error
		if r.URL.Path == "/api/check" {
			res, err = Check(req.Code)
		} else {
			res, err = Run(r.Context(), req.Code, req.Mode)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}
	mux.HandleFunc("POST /api/run", run)
	mux.HandleFunc("POST /api/check", run)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) || !loopbackPeer(r.RemoteAddr) {
			http.Error(w, "the tour answers only requests from this machine to localhost", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// localHost reports whether a Host header names this machine by a loopback name, which keeps a
// DNS name that someone points at 127.0.0.1 from reaching the tour.
func localHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// loopbackPeer reports whether a request came from this machine (the Host header is the client's
// to choose; the peer address isn't).
func loopbackPeer(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Export writes the tour as static files into dir, for a web server with no compiler behind it.
// Every lesson's code and solution are compiled and run once, now, and the page shows those
// results; editing the code needs squark tour.
func (s *Site) Export(dir string, progress io.Writer) error {
	var lessons []*Lesson
	for _, ch := range s.Chapters {
		lessons = append(lessons, ch.Lessons...)
	}
	type job struct {
		l        *Lesson
		solution bool
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				code := j.l.Code
				if j.solution {
					code = j.l.Solution
				}
				res, err := Run(context.Background(), code, j.l.Mode)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s: %v", j.l.Title, err)
				}
				if j.solution {
					j.l.BakedSolution = res
				} else {
					j.l.Baked = res
				}
				if j.solution {
					fmt.Fprintf(progress, "baked %s (solution)\n", j.l.Title)
				} else {
					fmt.Fprintf(progress, "baked %s\n", j.l.Title)
				}
				mu.Unlock()
			}
		}()
	}
	for _, l := range lessons {
		jobs <- job{l, false}
		if l.Solution != "" {
			jobs <- job{l, true}
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	err := fs.WalkDir(s.Static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.Static, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		return err
	}
	b, err := json.Marshal(index{Live: false, Chapters: s.Chapters})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "lessons.json"), b, 0o644)
}
