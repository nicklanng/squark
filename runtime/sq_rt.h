// squark runtime interface. Included by every generated program.
// Nothing in the runtime allocates from the heap: goroutine stacks, channel buffers,
// map tables and timers all live in static memory or on goroutine stacks.
#ifndef SQ_RT_H
#define SQ_RT_H

#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>
#include <string.h>

typedef struct { const uint8_t *p; int64_t len; } sq_string;
typedef struct { int64_t code; sq_string msg; } sq_error;
typedef struct { uint8_t *p; int64_t len, cap; } sq_bytes; // layout of a squark []byte

#define SQ_STR(lit, n) ((sq_string){(const uint8_t *)(lit), (n)})
#define SQ_NORETURN __attribute__((noreturn, cold))

// ---- panics ----
SQ_NORETURN void sq_panic_cstr(const char *msg, const char *file, int line);
SQ_NORETURN void sq_panic_str(sq_string msg, const char *file, int line);
SQ_NORETURN void sq_panic_int(int64_t v, const char *file, int line);
SQ_NORETURN void sq_panic_index(int64_t i, int64_t n, const char *file, int line);
SQ_NORETURN void sq_panic_slice(int64_t lo, int64_t hi, int64_t max, int64_t cap, const char *file, int line);
SQ_NORETURN void sq_panic_append(int64_t need, int64_t cap, const char *file, int line);
SQ_NORETURN void sq_panic_divide(const char *file, int line);

static inline int64_t sq_idx(int64_t i, int64_t n, const char *file, int line) {
	if (__builtin_expect((uint64_t)i >= (uint64_t)n, 0)) sq_panic_index(i, n, file, line);
	return i;
}
#define SQ_IDX(i, n) sq_idx((i), (n), __FILE__, __LINE__)

static inline void sq_slice_check(int64_t lo, int64_t hi, int64_t max, int64_t cap, const char *file, int line) {
	if (__builtin_expect(!(0 <= lo && lo <= hi && hi <= max && (uint64_t)max <= (uint64_t)cap), 0))
		sq_panic_slice(lo, hi, max, cap, file, line);
}
#define SQ_SLICE_CHECK(lo, hi, max, cap) sq_slice_check((lo), (hi), (max), (cap), __FILE__, __LINE__)

// Go integer semantics: division by zero panics, MinInt/-1 wraps, shifts >= width are defined.
#define SQ_DEFINE_DIV(T, NAME, MIN)                                                         \
	static inline T sq_div_##NAME(T a, T b, const char *f, int l) {                        \
		if (__builtin_expect(b == 0, 0)) sq_panic_divide(f, l);                             \
		if (MIN != 0 && b == (T)-1) return (T)(0 - (uint64_t)a);                            \
		return a / b;                                                                       \
	}                                                                                       \
	static inline T sq_rem_##NAME(T a, T b, const char *f, int l) {                        \
		if (__builtin_expect(b == 0, 0)) sq_panic_divide(f, l);                             \
		if (MIN != 0 && b == (T)-1) return 0;                                               \
		return a % b;                                                                       \
	}
SQ_DEFINE_DIV(int64_t, i64, INT64_MIN)
SQ_DEFINE_DIV(int32_t, i32, INT32_MIN)
SQ_DEFINE_DIV(int16_t, i16, INT16_MIN)
SQ_DEFINE_DIV(int8_t, i8, INT8_MIN)
SQ_DEFINE_DIV(uint64_t, u64, 0)
SQ_DEFINE_DIV(uint32_t, u32, 0)
SQ_DEFINE_DIV(uint16_t, u16, 0)
SQ_DEFINE_DIV(uint8_t, u8, 0)
#define SQ_DIV(NAME, a, b) sq_div_##NAME((a), (b), __FILE__, __LINE__)
#define SQ_REM(NAME, a, b) sq_rem_##NAME((a), (b), __FILE__, __LINE__)

static inline uint64_t sq_shl(uint64_t x, uint64_t n, int width) { return n >= (uint64_t)width ? 0 : x << n; }
static inline uint64_t sq_shru(uint64_t x, uint64_t n, int width) { return n >= (uint64_t)width ? 0 : x >> n; }
static inline int64_t sq_shrs(int64_t x, uint64_t n, int width) { return n >= (uint64_t)width ? (x < 0 ? -1 : 0) : x >> n; }

// ---- strings ----
static inline bool sq_str_eq(sq_string a, sq_string b) {
	return a.len == b.len && (a.p == b.p || a.len == 0 || memcmp(a.p, b.p, (size_t)a.len) == 0);
}
static inline int sq_str_cmp(sq_string a, sq_string b) {
	int64_t n = a.len < b.len ? a.len : b.len;
	int c = n > 0 ? memcmp(a.p, b.p, (size_t)n) : 0;
	if (c != 0) return c;
	return a.len < b.len ? -1 : a.len > b.len ? 1 : 0;
}
static inline bool sq_err_eq(sq_error a, sq_error b) {
	return a.code == b.code && (a.msg.p == b.msg.p || (a.msg.p && b.msg.p && sq_str_eq(a.msg, b.msg)));
}
int32_t sq_decode_rune(sq_string s, int64_t i, int64_t *width);

static inline int64_t sq_copy(void *dst, int64_t dlen, const void *src, int64_t slen, int64_t esize) {
	int64_t n = dlen < slen ? dlen : slen;
	if (n > 0) memmove(dst, src, (size_t)(n * esize));
	return n;
}

// ---- arenas: bump allocation in a fixed block (stack, static or passed in) ----
typedef struct sq_arena {
	uint8_t *base;
	int64_t used, cap;
	const char *name;
} sq_arena;

SQ_NORETURN void sq_panic_arena(sq_arena *a, int64_t need, const char *file, int line);

// sq_arena_raw returns size bytes (not zeroed) aligned to align.
static inline void *sq_arena_raw(sq_arena *a, int64_t size, int64_t align, const char *file, int line) {
	int64_t off = (a->used + align - 1) & ~(align - 1);
	if (__builtin_expect(size < 0 || size > a->cap - off, 0)) sq_panic_arena(a, size, file, line);
	a->used = off + size;
	return a->base + off;
}
static inline void *sq_arena_zero(sq_arena *a, int64_t size, int64_t align, const char *file, int line) {
	void *p = sq_arena_raw(a, size, align, file, line);
	if (size) memset(p, 0, (size_t)size);
	return p;
}
// sq_arena_bytes computes n*esize, panicking on overflow.
static inline int64_t sq_arena_bytes(sq_arena *a, int64_t n, int64_t esize, const char *file, int line) {
	if (__builtin_expect(n < 0 || (esize > 0 && n > INT64_MAX / esize), 0)) sq_panic_arena(a, INT64_MAX, file, line);
	return n * esize;
}
// sq_arena_grow resizes the block p (old_bytes, of which used_bytes are live) to new_bytes,
// extending in place when p is the most recent allocation.
void *sq_arena_grow(sq_arena *a, void *p, int64_t used_bytes, int64_t old_bytes, int64_t new_bytes, int64_t align, const char *file, int line);

// ---- printing (builtin print/println, to stdout) ----
void sq_print_str(sq_string s);
void sq_print_int(int64_t v);
void sq_print_uint(uint64_t v);
void sq_print_float(double v);
void sq_print_bool(bool v);
void sq_print_ptr(const void *p);
void sq_print_err(sq_error e);
void sq_print_byte(uint8_t c);
void sq_flush(void);

// ---- goroutines ----
typedef struct sq_g sq_g;
typedef struct sq_pool {
	const char *name;      // entry function
	int64_t count;         // max live goroutines
	int64_t stack_size;    // usable bytes per stack (proven by the compiler)
	int64_t slot_size;     // stack_size rounded to pages + guard page
	uint8_t *stacks;       // count * slot_size, statically allocated
	sq_g *gs;              // count goroutine descriptors
	sq_g *free;
	sq_g *waitq_head, *waitq_tail; // goroutines waiting for a free slot
	int64_t live, peak, started;
	int64_t observed; // deepest stack use seen (only with SQ_STACKCHECK=1)
	int64_t index;    // position in sq_pools; each shard has its own copy of every pool
} sq_pool;

void sq_go(sq_pool *pool, void (*entry)(void *), const void *args, size_t argsize);
void sq_gosched(void);

// ---- channels ----
typedef struct sq_chan {
	uint8_t *buf;
	int64_t elemsize, cap, len, head;
	bool closed;
	sq_g *recvq_head, *recvq_tail, *sendq_head, *sendq_tail;
} sq_chan;

sq_chan *sq_chan_init(sq_chan *c, void *buf, int64_t elemsize, int64_t cap);
void sq_chan_send(sq_chan *c, const void *elem, const char *file, int line);
bool sq_chan_recv(sq_chan *c, void *out);
void sq_chan_close(sq_chan *c, const char *file, int line);
static inline int64_t sq_chan_len(sq_chan *c) { return c ? c->len : 0; }
static inline int64_t sq_chan_cap(sq_chan *c) { return c ? c->cap : 0; }

// ---- maps: fixed-capacity open addressing ----
enum { SQ_KEY_BYTES = 0, SQ_KEY_STRING = 1 };
enum { SQ_SLOT_EMPTY = 0, SQ_SLOT_FULL = 1, SQ_SLOT_DELETED = 2, SQ_SLOT_MOVING = 3 };
typedef struct sq_map {
	uint8_t *keys, *vals, *ctrl;
	int64_t slots, mask, len, maxlen, deleted;
	int32_t keysize, valsize, kind;
} sq_map;

sq_map *sq_map_init(sq_map *m, void *keys, void *vals, uint8_t *ctrl, int64_t slots, int64_t maxlen,
                    int32_t keysize, int32_t valsize, int32_t kind);
sq_map *sq_map_new_in(sq_arena *a, int64_t cap, int32_t keysize, int32_t valsize, int32_t kind, const char *file, int line);
void *sq_map_get(sq_map *m, const void *key);
void *sq_map_put(sq_map *m, const void *key, const char *file, int line);
void sq_map_delete(sq_map *m, const void *key);
int64_t sq_map_next(sq_map *m, int64_t i);
static inline int64_t sq_map_len(sq_map *m) { return m ? m->len : 0; }
static inline void *sq_map_key(sq_map *m, int64_t i) { return m->keys + i * m->keysize; }
static inline void *sq_map_val(sq_map *m, int64_t i) { return m->vals + i * m->valsize; }

// ---- program entry (generated) ----
extern sq_pool *const sq_pools[];
extern const int64_t sq_npools;
extern sq_pool sq_main_pool;
extern const int64_t sq_max_goroutines;
extern const int64_t sq_workers; // workers N (0: one per core)
extern int64_t sq_worker_count;    // processes actually running
extern int64_t sq_shard_count;     // scheduler threads in this process
extern const int64_t sq_threads;   // threads N (0: one per core)
extern const size_t sq_globals_size; // generated: one shard's package variables
void sq_globals_set(void *p);        // generated: point this thread at a shard's package variables
void sq_program_main(void *args);

#endif
