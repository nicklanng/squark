// gonotes is the Go baseline for the squark notes REST API benchmark:
// net/http + encoding/json + a minimal cgo binding to libsqlite3.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	maxTitle   = 200
	maxBody    = 4000
	maxRequest = 64 << 10 // request bodies larger than this are rejected with 400
	listLimit  = 100
)

type Note struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Store struct {
	mu     sync.Mutex
	db     *DB
	insert *Stmt
	get    *Stmt
	list   *Stmt
	del    *Stmt
}

func OpenStore(path string) (*Store, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	if err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;`); err != nil {
		return nil, err
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS notes(id INTEGER PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL)`); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	for _, p := range []struct {
		dst **Stmt
		sql string
	}{
		{&s.insert, `INSERT INTO notes(title, body) VALUES(?, ?)`},
		{&s.get, `SELECT id, title, body FROM notes WHERE id = ?`},
		{&s.list, `SELECT id, title, body FROM notes ORDER BY id DESC LIMIT ` + strconv.Itoa(listLimit)},
		{&s.del, `DELETE FROM notes WHERE id = ?`},
	} {
		if *p.dst, err = db.Prepare(p.sql); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Insert(title, body string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.insert.Reset()
	if err := s.insert.BindText(1, title); err != nil {
		return 0, err
	}
	if err := s.insert.BindText(2, body); err != nil {
		return 0, err
	}
	if _, err := s.insert.Step(); err != nil {
		return 0, err
	}
	return s.db.LastInsertRowID(), nil
}

func (s *Store) Get(id int64) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.get.Reset()
	if err := s.get.BindInt64(1, id); err != nil {
		return Note{}, false, err
	}
	row, err := s.get.Step()
	if err != nil || !row {
		return Note{}, false, err
	}
	return Note{ID: s.get.ColumnInt64(0), Title: s.get.ColumnText(1), Body: s.get.ColumnText(2)}, true, nil
}

func (s *Store) List() ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.list.Reset()
	notes := make([]Note, 0, listLimit)
	for {
		row, err := s.list.Step()
		if err != nil {
			return nil, err
		}
		if !row {
			return notes, nil
		}
		notes = append(notes, Note{ID: s.list.ColumnInt64(0), Title: s.list.ColumnText(1), Body: s.list.ColumnText(2)})
	}
}

func (s *Store) Delete(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.del.Reset()
	if err := s.del.BindInt64(1, id); err != nil {
		return false, err
	}
	if _, err := s.del.Step(); err != nil {
		return false, err
	}
	return s.db.Changes() > 0, nil
}

// ---- HTTP ----

type errorBody struct {
	Error string `json:"error"`
}

var (
	errNotFound   = errorBody{"not found"}
	errBadRequest = errorBody{"bad request"}
	errMethod     = errorBody{"method not allowed"}
	errInternal   = errorBody{"internal error"}
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

// parseID accepts only a non-empty run of ASCII digits that fits in int64.
func parseID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

type server struct{ store *Store }

func (sv *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/health":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, errMethod)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	case path == "/notes":
		switch r.Method {
		case http.MethodGet:
			sv.list(w)
		case http.MethodPost:
			sv.create(w, r)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errMethod)
		}
	case strings.HasPrefix(path, "/notes/"):
		id, ok := parseID(path[len("/notes/"):])
		if !ok {
			writeJSON(w, http.StatusNotFound, errNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			sv.get(w, id)
		case http.MethodDelete:
			sv.delete(w, id)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, errMethod)
		}
	default:
		writeJSON(w, http.StatusNotFound, errNotFound)
	}
}

func (sv *server) list(w http.ResponseWriter) {
	notes, err := sv.store.List()
	if err != nil {
		log.Print(err)
		writeJSON(w, http.StatusInternalServerError, errInternal)
		return
	}
	writeJSON(w, http.StatusOK, notes)
}

func (sv *server) get(w http.ResponseWriter, id int64) {
	n, ok, err := sv.store.Get(id)
	switch {
	case err != nil:
		log.Print(err)
		writeJSON(w, http.StatusInternalServerError, errInternal)
	case !ok:
		writeJSON(w, http.StatusNotFound, errNotFound)
	default:
		writeJSON(w, http.StatusOK, n)
	}
}

func (sv *server) create(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequest+1))
	if err != nil || len(data) > maxRequest {
		writeJSON(w, http.StatusBadRequest, errBadRequest)
		return
	}
	var in struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.Unmarshal(data, &in); err != nil || in.Title == nil || *in.Title == "" {
		writeJSON(w, http.StatusBadRequest, errBadRequest)
		return
	}
	body := ""
	if in.Body != nil {
		body = *in.Body
	}
	if len(*in.Title) > maxTitle || len(body) > maxBody {
		writeJSON(w, http.StatusBadRequest, errBadRequest)
		return
	}
	id, err := sv.store.Insert(*in.Title, body)
	if err != nil {
		log.Print(err)
		writeJSON(w, http.StatusInternalServerError, errInternal)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		ID int64 `json:"id"`
	}{id})
}

func (sv *server) delete(w http.ResponseWriter, id int64) {
	ok, err := sv.store.Delete(id)
	switch {
	case err != nil:
		log.Print(err)
		writeJSON(w, http.StatusInternalServerError, errInternal)
	case !ok:
		writeJSON(w, http.StatusNotFound, errNotFound)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func main() {
	port := flag.Int("port", 8080, "listen port")
	dbPath := flag.String("db", "notes.db", "sqlite database path")
	flag.Parse()

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	addr := ":" + strconv.Itoa(*port)
	log.Printf("gonotes listening on %s (db %s)", addr, *dbPath)
	log.Fatal(http.ListenAndServe(addr, &server{store: store}))
}
