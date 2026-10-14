// C glue between squark and libsqlite3. squark strings are (pointer, length) pairs, so the shim
// passes lengths explicitly and NUL-terminates paths in a stack buffer.
#include <sqlite3.h>
#include <stdint.h>
#include <string.h>

typedef struct { const uint8_t *p; int64_t len; } sq_string;
typedef struct { uint8_t *p; int64_t len, cap; } sq_bytes;

int64_t sq_sqlite_open(sq_string path, void **db) {
	char buf[1024];
	if (path.len >= (int64_t)sizeof buf) return SQLITE_CANTOPEN;
	memcpy(buf, path.p, (size_t)path.len);
	buf[path.len] = 0;
	return sqlite3_open_v2(buf, (sqlite3 **)db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_NOMUTEX, NULL);
}

int64_t sq_sqlite_close(void *db) { return sqlite3_close_v2(db); }

int64_t sq_sqlite_exec(void *db, sq_string sql) {
	// sqlite3_exec needs NUL-terminated SQL; step through statements instead.
	const char *p = (const char *)sql.p, *end = p + sql.len;
	while (p < end) {
		sqlite3_stmt *st = NULL;
		const char *tail = NULL;
		int rc = sqlite3_prepare_v2(db, p, (int)(end - p), &st, &tail);
		if (rc != SQLITE_OK) return rc;
		if (!st) break; // whitespace or comment
		while ((rc = sqlite3_step(st)) == SQLITE_ROW) {
		}
		sqlite3_finalize(st);
		if (rc != SQLITE_DONE) return rc;
		p = tail;
	}
	return SQLITE_OK;
}

int64_t sq_sqlite_prepare(void *db, sq_string sql, void **stmt) {
	return sqlite3_prepare_v3(db, (const char *)sql.p, (int)sql.len, SQLITE_PREPARE_PERSISTENT, (sqlite3_stmt **)stmt, NULL);
}

int64_t sq_sqlite_bind_int(void *st, int64_t i, int64_t v) { return sqlite3_bind_int64(st, (int)i, v); }
int64_t sq_sqlite_bind_text(void *st, int64_t i, sq_string s) {
	return sqlite3_bind_text(st, (int)i, (const char *)s.p, (int)s.len, SQLITE_TRANSIENT);
}
int64_t sq_sqlite_bind_null(void *st, int64_t i) { return sqlite3_bind_null(st, (int)i); }
int64_t sq_sqlite_step(void *st) { return sqlite3_step(st); }
int64_t sq_sqlite_reset(void *st) { return sqlite3_reset(st); }
int64_t sq_sqlite_clear(void *st) { return sqlite3_clear_bindings(st); }
int64_t sq_sqlite_finalize(void *st) { return sqlite3_finalize(st); }
int64_t sq_sqlite_column_int(void *st, int64_t i) { return sqlite3_column_int64(st, (int)i); }
int64_t sq_sqlite_column_type(void *st, int64_t i) { return sqlite3_column_type(st, (int)i); }
int64_t sq_sqlite_column_count(void *st) { return sqlite3_column_count(st); }

// Copies column text into buf; returns the full length (callers detect truncation when > cap).
int64_t sq_sqlite_column_text(void *st, int64_t i, sq_bytes buf) {
	const unsigned char *t = sqlite3_column_text(st, (int)i);
	int64_t n = sqlite3_column_bytes(st, (int)i);
	int64_t k = n < buf.cap ? n : buf.cap;
	if (t && k > 0) memcpy(buf.p, t, (size_t)k);
	return n;
}

int64_t sq_sqlite_changes(void *db) { return sqlite3_changes64(db); }
int64_t sq_sqlite_last_insert_rowid(void *db) { return sqlite3_last_insert_rowid(db); }

// sqlite3_errstr returns static strings, so they can become squark error messages.
sq_string sq_sqlite_errstr(int64_t rc) {
	const char *s = sqlite3_errstr((int)rc);
	return (sq_string){(const uint8_t *)s, (int64_t)strlen(s)};
}
