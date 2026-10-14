package main

// Minimal hand-written cgo binding to the Homebrew libsqlite3.
// Only the calls the notes API needs are bound.

/*
#cgo CFLAGS: -I/opt/homebrew/opt/sqlite/include
#cgo LDFLAGS: -L/opt/homebrew/opt/sqlite/lib -lsqlite3
#include <stdlib.h>
#include <sqlite3.h>

// SQLITE_TRANSIENT is a function-pointer macro cgo cannot express directly.
// Go string memory is passed without copying; SQLITE_TRANSIENT makes sqlite
// copy it before returning. An empty Go string may have a nil data pointer,
// which sqlite would bind as NULL, so substitute "" in that case.
static int bind_text_transient(sqlite3_stmt *s, int i, _GoString_ v) {
	size_t n = _GoStringLen(v);
	const char *p = n ? _GoStringPtr(v) : "";
	return sqlite3_bind_text(s, i, p, (int)n, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"errors"
	"strconv"
	"unsafe"
)

const (
	sqliteOK   = C.SQLITE_OK
	sqliteRow  = C.SQLITE_ROW
	sqliteDone = C.SQLITE_DONE
)

type DB struct{ db *C.sqlite3 }

type Stmt struct {
	s  *C.sqlite3_stmt
	db *DB
}

func (d *DB) err(rc C.int) error {
	return errors.New("sqlite: " + C.GoString(C.sqlite3_errmsg(d.db)) + " (rc=" + strconv.Itoa(int(rc)) + ")")
}

func Open(path string) (*DB, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var h *C.sqlite3
	// The app serialises access with its own mutex, so NOMUTEX is safe.
	flags := C.int(C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_CREATE | C.SQLITE_OPEN_NOMUTEX)
	rc := C.sqlite3_open_v2(cpath, &h, flags, nil)
	d := &DB{db: h}
	if rc != sqliteOK {
		err := d.err(rc)
		C.sqlite3_close(h)
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() { C.sqlite3_close(d.db) }

func (d *DB) Exec(sql string) error {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	if rc := C.sqlite3_exec(d.db, csql, nil, nil, nil); rc != sqliteOK {
		return d.err(rc)
	}
	return nil
}

func (d *DB) Prepare(sql string) (*Stmt, error) {
	csql := C.CString(sql)
	defer C.free(unsafe.Pointer(csql))
	var s *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(d.db, csql, -1, &s, nil); rc != sqliteOK {
		return nil, d.err(rc)
	}
	return &Stmt{s: s, db: d}, nil
}

func (d *DB) Changes() int64        { return int64(C.sqlite3_changes(d.db)) }
func (d *DB) LastInsertRowID() int64 { return int64(C.sqlite3_last_insert_rowid(d.db)) }

func (s *Stmt) BindText(i int, v string) error {
	if rc := C.bind_text_transient(s.s, C.int(i), v); rc != sqliteOK {
		return s.db.err(rc)
	}
	return nil
}

func (s *Stmt) BindInt64(i int, v int64) error {
	if rc := C.sqlite3_bind_int64(s.s, C.int(i), C.sqlite3_int64(v)); rc != sqliteOK {
		return s.db.err(rc)
	}
	return nil
}

// Step returns (true, nil) for a row, (false, nil) when done.
func (s *Stmt) Step() (bool, error) {
	switch rc := C.sqlite3_step(s.s); rc {
	case sqliteRow:
		return true, nil
	case sqliteDone:
		return false, nil
	default:
		return false, s.db.err(rc)
	}
}

func (s *Stmt) ColumnInt64(i int) int64 { return int64(C.sqlite3_column_int64(s.s, C.int(i))) }

func (s *Stmt) ColumnText(i int) string {
	p := C.sqlite3_column_text(s.s, C.int(i)) // must precede column_bytes
	n := C.sqlite3_column_bytes(s.s, C.int(i))
	if p == nil || n == 0 {
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(p)), n)
}

// Reset readies the statement for reuse and drops its bindings.
func (s *Stmt) Reset() {
	C.sqlite3_reset(s.s)
	C.sqlite3_clear_bindings(s.s)
}

func (s *Stmt) Finalize() { C.sqlite3_finalize(s.s) }
