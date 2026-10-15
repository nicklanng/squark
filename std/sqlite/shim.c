// C glue between squark and libsqlite3. squark strings are (pointer, length) pairs, so the shim
// passes lengths explicitly and NUL-terminates paths in a stack buffer.
//
// A slow statement must not stop its scheduler thread. Statements start inline (fast ones finish
// there); one that runs longer than SLOW_NS (the progress handler checks every 1000 VM steps) or
// must wait for another connection's lock (the busy handler) moves its goroutine to a helper
// thread mid-call with sq_detach, and carries on there; sq_reattach brings it back before the
// call returns. Open, Prepare, Finalize and Close always run on a helper (blocking externs).
//
// A goroutine parked in a blocking call lets another reach the same connection, so each connection
// has a lock (a goroutine lock: waiters park). A statement takes it at its first Bind or Step and
// gives it back at Reset, so Bind, Step, Column, Changes and LastInsertRowID see one goroutine's
// work; the lock calls run on the scheduler thread, before the blocking call they protect.
#include <sqlite3.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>
#include "sq_rt.h"

typedef struct conn {
	sqlite3 *db;
	sq_gmutex mu;
	int64_t active; // the holder's active statements plus calls in progress
	int64_t start;  // when the current Step or Exec started (sq_nanotime)
	int64_t waited; // microseconds the busy handler has slept for the current lock
} conn;

#define SLOW_NS 100000 // 100µs: longer than two thread handoffs

// progress runs every 1000 VM steps: a statement running long moves to a helper thread.
static int progress(void *arg) {
	conn *c = arg;
	if (!sq_on_helper() && sq_nanotime() - c->start > SLOW_NS) sq_detach();
	return 0; // carry on
}

// busy is the connection's busy handler: it waits for another connection's lock, on a helper
// thread. Write transactions here are short (tens of microseconds), so it retries at once after
// moving and then backs off from 50µs, doubling up to 20ms; it gives up after 5 seconds in all,
// like sqlite3_busy_timeout(5000).
static int busy(void *arg, int count) {
	conn *c = arg;
	if (count == 0) c->waited = 0;
	if (!sq_on_helper() && sq_detach()) return 1; // moved: the lock may well be free by now
	if (c->waited >= 5000000) return 0;
	int64_t us = 50;
	for (int i = 0; i < count && us < 20000; i++) us *= 2;
	if (us > 20000) us = 20000;
	if (c->waited + us > 5000000) us = 5000000 - c->waited;
	c->waited += us;
	usleep((useconds_t)us);
	return 1;
}

typedef struct stmt {
	sqlite3_stmt *st;
	conn *c;
	int active; // between the first Bind or Step and Reset (holds c's lock)
} stmt;

static void acquire(conn *c) {
	if (!sq_gmutex_held(&c->mu)) sq_gmutex_lock(&c->mu); // the last holder left active at 0
	c->active++;
}

static void release(conn *c) {
	if (--c->active == 0) sq_gmutex_unlock(&c->mu);
}

// owned reports whether the running goroutine is using st (so its columns are its own).
static int owned(stmt *s) { return s->active && sq_gmutex_held(&s->c->mu); }

// lock calls, made on the scheduler thread around blocking ones
void sq_sqlite_db_use(conn *c) { acquire(c); }
void sq_sqlite_db_done(conn *c) { release(c); }
void sq_sqlite_stmt_use(stmt *s) {
	if (!sq_gmutex_held(&s->c->mu)) {
		sq_gmutex_lock(&s->c->mu);
	}
	if (!s->active) {
		s->active = 1;
		s->c->active++;
	}
}

// sq_sqlite_open opens a connection (blocking: on a helper thread). The wrapper is allocated by
// SQLite, which manages its own memory.
int64_t sq_sqlite_open(sq_string path, conn **out) {
	char buf[1024];
	*out = NULL;
	if (path.len >= (int64_t)sizeof buf) return SQLITE_CANTOPEN;
	if (path.len > 0 && memchr(path.p, 0, (size_t)path.len)) return SQLITE_CANTOPEN; // Open reports EINVAL first
	memcpy(buf, path.p, (size_t)path.len);
	buf[path.len] = 0;
	conn *c = sqlite3_malloc(sizeof *c);
	if (!c) return SQLITE_NOMEM;
	memset(c, 0, sizeof *c);
	sq_gmutex_init(&c->mu);
	// SQLITE_OPEN_NOMUTEX: the connection lock above keeps one thread in it at a time
	int rc = sqlite3_open_v2(buf, &c->db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_NOMUTEX, NULL);
	// wait for another connection's lock (other threads or worker processes) instead of failing
	// at once; the wait happens on a helper thread
	if (rc == SQLITE_OK) {
		sqlite3_busy_handler(c->db, busy, c);
		sqlite3_progress_handler(c->db, 1000, progress, c);
	}
	*out = c;
	return rc;
}

// sq_sqlite_close closes the connection (blocking; its caller holds the lock).
int64_t sq_sqlite_close(conn *c) {
	if (!c) return SQLITE_MISUSE;
	int rc = sqlite3_close_v2(c->db);
	sq_gmutex_unlock(&c->mu);
	sqlite3_free(c);
	return rc;
}

static int64_t exec(sqlite3 *db, sq_string sql);
// sq_sqlite_exec runs SQL (inline, moving to a helper if slow; its caller holds the lock).
int64_t sq_sqlite_exec(conn *c, sq_string sql) {
	c->start = sq_nanotime();
	int64_t rc = exec(c->db, sql);
	sq_reattach();
	return rc;
}

static int64_t exec(sqlite3 *db, sq_string sql) {
	// sqlite3_exec needs NUL-terminated SQL; step through statements instead.
	const char *p = (const char *)sql.p, *end = p + sql.len;
	while (p < end) {
		sqlite3_stmt *st = NULL;
		const char *tail = NULL;
		int rc = sqlite3_prepare_v2(db, p, (int)(end - p), &st, &tail);
		if (rc != SQLITE_OK) return rc;
		if (!st) break; // whitespace or comment
		// SQLite skips the busy handler where waiting could deadlock (two connections setting up
		// a new database at once, as workers N do). Outside a transaction the failed statement
		// was rolled back, so retrying it is safe: up to the same 5 seconds as the busy timeout.
		for (int tries = 0;; tries++) {
			while ((rc = sqlite3_step(st)) == SQLITE_ROW) {
			}
			if ((rc & 0xff) != SQLITE_BUSY || !sqlite3_get_autocommit(db) || tries == 500) break;
			sqlite3_reset(st);
			if (!sq_on_helper()) sq_detach();
			sqlite3_sleep(10);
		}
		sqlite3_finalize(st);
		if (rc != SQLITE_DONE) return rc;
		p = tail;
	}
	return SQLITE_OK;
}

// sq_sqlite_prepare compiles a statement (blocking; its caller holds the lock).
int64_t sq_sqlite_prepare(conn *c, sq_string sql, stmt **out) {
	*out = NULL;
	stmt *s = sqlite3_malloc(sizeof *s);
	if (!s) return SQLITE_NOMEM;
	memset(s, 0, sizeof *s);
	s->c = c;
	int rc = sqlite3_prepare_v3(c->db, (const char *)sql.p, (int)sql.len, SQLITE_PREPARE_PERSISTENT, &s->st, NULL);
	if (rc != SQLITE_OK) {
		sqlite3_free(s);
		return rc;
	}
	*out = s;
	return rc;
}

// Bind calls take the connection for the statement (until Reset).
int64_t sq_sqlite_bind_int(stmt *s, int64_t i, int64_t v) {
	sq_sqlite_stmt_use(s);
	return sqlite3_bind_int64(s->st, (int)i, v);
}
int64_t sq_sqlite_bind_text(stmt *s, int64_t i, sq_string v) {
	sq_sqlite_stmt_use(s);
	return sqlite3_bind_text(s->st, (int)i, (const char *)v.p, (int)v.len, SQLITE_TRANSIENT);
}
int64_t sq_sqlite_bind_null(stmt *s, int64_t i) {
	sq_sqlite_stmt_use(s);
	return sqlite3_bind_null(s->st, (int)i);
}

// sq_sqlite_step runs the statement to its next row (after sq_sqlite_stmt_use): inline, moving to
// a helper thread if it runs long or waits for a lock.
int64_t sq_sqlite_step(stmt *s) {
	s->c->start = sq_nanotime();
	int rc = sqlite3_step(s->st);
	sq_reattach();
	return rc;
}

// sq_sqlite_reset readies the statement to run again, clears its bindings and gives back the
// connection.
int64_t sq_sqlite_reset(stmt *s) {
	conn *c = s->c;
	acquire(c);
	int rc = sqlite3_reset(s->st);
	sqlite3_clear_bindings(s->st);
	if (s->active) {
		s->active = 0;
		c->active--;
	}
	release(c);
	return rc;
}

// sq_sqlite_finalize frees the statement (blocking; its caller holds the lock).
int64_t sq_sqlite_finalize(stmt *s) {
	if (!s) return SQLITE_OK;
	int rc = sqlite3_finalize(s->st);
	if (s->active) s->c->active--;
	sqlite3_free(s);
	return rc;
}

// Column calls read the row the running goroutine stepped to; on a statement it isn't using
// (after Reset, or another goroutine's) they return nothing rather than touch it.
int64_t sq_sqlite_column_int(stmt *s, int64_t i) { return owned(s) ? sqlite3_column_int64(s->st, (int)i) : 0; }
int64_t sq_sqlite_column_type(stmt *s, int64_t i) { return owned(s) ? sqlite3_column_type(s->st, (int)i) : SQLITE_NULL; }
int64_t sq_sqlite_column_count(stmt *s) { return sqlite3_column_count(s->st); } // fixed at prepare

// Copies column text into buf; returns the full length (callers detect truncation when > cap).
int64_t sq_sqlite_column_text(stmt *s, int64_t i, sq_bytes buf) {
	if (!owned(s)) return 0;
	const unsigned char *t = sqlite3_column_text(s->st, (int)i);
	int64_t n = sqlite3_column_bytes(s->st, (int)i);
	int64_t k = n < buf.cap ? n : buf.cap;
	if (t && k > 0) memcpy(buf.p, t, (size_t)k);
	return n;
}

// Changes and LastInsertRowID report the holder's last statement: call them before Reset.
int64_t sq_sqlite_changes(conn *c) {
	acquire(c);
	int64_t n = sqlite3_changes64(c->db);
	release(c);
	return n;
}
int64_t sq_sqlite_last_insert_rowid(conn *c) {
	acquire(c);
	int64_t n = sqlite3_last_insert_rowid(c->db);
	release(c);
	return n;
}

// sqlite3_errstr returns static strings, so they can become squark error messages.
sq_string sq_sqlite_errstr(int64_t rc) {
	const char *s = sqlite3_errstr((int)rc);
	return (sq_string){(const uint8_t *)s, (int64_t)strlen(s)};
}
