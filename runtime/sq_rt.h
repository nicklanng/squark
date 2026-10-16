// squark runtime interface. Included by every generated program.
// Nothing in the runtime allocates from the heap: goroutine stacks, channel buffers,
// map tables and timers all live in static memory or on goroutine stacks.
#ifndef SQ_RT_H
#define SQ_RT_H

#include <stdint.h>
#include <stddef.h>
#include <stdbool.h>
#include <string.h>
#include <pthread.h>
#include <stdatomic.h>

typedef struct { const uint8_t *p; int64_t len; } sq_string;
typedef struct { int64_t code; sq_string msg; } sq_error;
typedef struct { uint8_t *p; int64_t len, cap; } sq_bytes; // layout of a squark []byte

#define SQ_STR(lit, n) ((sq_string){(const uint8_t *)(lit), (n)})
#define SQ_NORETURN __attribute__((noreturn, cold))

// ---- SIMD vectors ----
// The SIMD types (spec §3) are GNU C vectors. The 256-bit ones are only 16-byte aligned: stacks and
// arenas guarantee 16, and unaligned vector loads are as fast as aligned ones on aligned data.
typedef int8_t sq_int8x16 __attribute__((vector_size(16)));
typedef uint8_t sq_uint8x16 __attribute__((vector_size(16)));
typedef int16_t sq_int16x8 __attribute__((vector_size(16)));
typedef uint16_t sq_uint16x8 __attribute__((vector_size(16)));
typedef int32_t sq_int32x4 __attribute__((vector_size(16)));
typedef uint32_t sq_uint32x4 __attribute__((vector_size(16)));
typedef int64_t sq_int64x2 __attribute__((vector_size(16)));
typedef uint64_t sq_uint64x2 __attribute__((vector_size(16)));
typedef float sq_float32x4 __attribute__((vector_size(16)));
typedef double sq_float64x2 __attribute__((vector_size(16)));
typedef int8_t sq_int8x32 __attribute__((vector_size(32), aligned(16)));
typedef uint8_t sq_uint8x32 __attribute__((vector_size(32), aligned(16)));
typedef int16_t sq_int16x16 __attribute__((vector_size(32), aligned(16)));
typedef uint16_t sq_uint16x16 __attribute__((vector_size(32), aligned(16)));
typedef int32_t sq_int32x8 __attribute__((vector_size(32), aligned(16)));
typedef uint32_t sq_uint32x8 __attribute__((vector_size(32), aligned(16)));
typedef int64_t sq_int64x4 __attribute__((vector_size(32), aligned(16)));
typedef uint64_t sq_uint64x4 __attribute__((vector_size(32), aligned(16)));
typedef float sq_float32x8 __attribute__((vector_size(32), aligned(16)));
typedef double sq_float64x4 __attribute__((vector_size(32), aligned(16)));

// sq_first16 and sq_last16 find the first and last set lane of a byte mask (lanes 0 or -1), or
// -1: each lane narrowed to 4 bits of a uint64, which arm64 does in one instruction (shrn).
typedef uint8_t sq_u8x8 __attribute__((vector_size(8)));
typedef uint16_t sq_u16x8 __attribute__((vector_size(16)));
static inline int64_t sq_first16(sq_int8x16 m) {
	sq_u8x8 n = __builtin_convertvector((sq_u16x8)m >> 4, sq_u8x8);
	uint64_t b;
	memcpy(&b, &n, 8);
	return b ? (int64_t)(__builtin_ctzll(b) >> 2) : -1;
}
static inline int64_t sq_last16(sq_int8x16 m) {
	sq_u8x8 n = __builtin_convertvector((sq_u16x8)m >> 4, sq_u8x8);
	uint64_t b;
	memcpy(&b, &n, 8);
	return b ? (int64_t)(15 - (__builtin_clzll(b) >> 2)) : -1;
}

// ---- panics ----
SQ_NORETURN void sq_panic_cstr(const char *msg, const char *file, int line);
SQ_NORETURN void sq_panic_str(sq_string msg, const char *file, int line);
SQ_NORETURN void sq_panic_int(int64_t v, const char *file, int line);
SQ_NORETURN void sq_panic_index(int64_t i, int64_t n, const char *file, int line);
SQ_NORETURN void sq_panic_slice(int64_t lo, int64_t hi, int64_t max, int64_t cap, const char *file, int line);
SQ_NORETURN void sq_panic_append(int64_t need, int64_t cap, const char *file, int line);
SQ_NORETURN void sq_panic_divide(const char *file, int line);
SQ_NORETURN void sq_panic_nil(const char *file, int line);

static inline int64_t sq_idx(int64_t i, int64_t n, const char *file, int line) {
	if (__builtin_expect((uint64_t)i >= (uint64_t)n, 0)) sq_panic_index(i, n, file, line);
	return i;
}
#define SQ_IDX(i, n) sq_idx((i), (n), __FILE__, __LINE__)

// sq_inrange reports whether i + off is an index into n elements for every i from lo up to hi
// (excluded), so a loop over them can run a copy without bounds checks. It says no when the sum
// overflows. sq_inrange_incl is the same with hi included; it also says no for hi = INT64_MAX,
// where the counter would wrap.
static inline bool sq_inrange(int64_t lo, int64_t hi, int64_t off, int64_t n) {
	int64_t a, b;
	if (lo >= hi) return true;
	if (__builtin_add_overflow(lo, off, &a) || __builtin_add_overflow(hi, off, &b)) return false;
	return a >= 0 && b <= n;
}
static inline bool sq_inrange_incl(int64_t lo, int64_t hi, int64_t off, int64_t n) {
	int64_t a, b;
	if (lo > hi) return true;
	if (hi == INT64_MAX || __builtin_add_overflow(lo, off, &a) || __builtin_add_overflow(hi, off, &b)) return false;
	return a >= 0 && b < n;
}
// The same for a counter counting down from hi: sq_inrange_down stops at lo, included (and says
// no for lo = INT64_MIN, where the counter would wrap), sq_inrange_above stops above lo.
static inline bool sq_inrange_down(int64_t lo, int64_t hi, int64_t off, int64_t n) {
	int64_t a, b;
	if (lo > hi) return true;
	if (lo == INT64_MIN || __builtin_add_overflow(lo, off, &a) || __builtin_add_overflow(hi, off, &b)) return false;
	return a >= 0 && b < n;
}
static inline bool sq_inrange_above(int64_t lo, int64_t hi, int64_t off, int64_t n) {
	if (lo >= hi) return true;
	return sq_inrange_down(lo + 1, hi, off, n);
}

// SQ_NN checks a pointer before it is dereferenced. A fault would catch most nil dereferences,
// but not a large offset from nil, and C lets the compiler assume a dereferenced pointer is
// non-null; the check also gives the panic a source position.
#define SQ_NN(p) ({ __auto_type sq__nn = (p); if (__builtin_expect(!sq__nn, 0)) sq_panic_nil(__FILE__, __LINE__); sq__nn; })

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

// Float to integer conversions saturate at the target's range; NaN gives 0. bits is the
// target's width; the caller casts the result to the target type.
static inline int64_t sq_f2i(double x, int bits) {
	double lim = bits == 64 ? 9223372036854775808.0 : (double)((int64_t)1 << (bits - 1));
	if (x != x) return 0;
	if (x >= lim) return bits == 64 ? INT64_MAX : (int64_t)lim - 1;
	if (x <= -lim) return bits == 64 ? INT64_MIN : -(int64_t)lim;
	return (int64_t)x;
}
static inline uint64_t sq_f2u(double x, int bits) {
	double lim = bits == 64 ? 18446744073709551616.0 : (double)((uint64_t)1 << bits);
	if (x != x || x <= -1.0) return 0;
	if (x >= lim) return bits == 64 ? UINT64_MAX : (uint64_t)lim - 1;
	return (uint64_t)x;
}

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
// sq_err_msg makes an error's message non-NULL: a NULL message pointer is what makes it nil.
static inline sq_string sq_err_msg(sq_string s) {
	if (!s.p) s.p = (const uint8_t *)"";
	return s;
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
	bool preemptible; // task ... preemptible: its loops yield when asked (sq_preempt)
} sq_pool;

// Preemption: loops in preemptible tasks (and the functions they call) check their thread's flag
// at every iteration; the watchdog sets it when a goroutine has run for a time slice.
extern _Thread_local _Atomic int *sq_preempt_flag;
void sq_preempt(void);
extern const bool sq_any_preemptible;

void sq_go(sq_pool *pool, void (*entry)(void *), const void *args, size_t argsize);
void sq_gosched(void);

// ---- channels ----
// A goroutine waiting on a channel queues a waiter: one for a send or receive, one per case of a
// select (the generated code provides them, on the goroutine's stack).
typedef struct sq_waiter {
	sq_g *g;
	void *elem;              // the value to send, or where a received value goes
	struct sq_waiter *next;
	void *sel;               // the select this waiter belongs to (NULL: a plain send or receive)
	int64_t index;           // its case
	bool ok;                 // the operation completed (false: the channel was closed)
	bool remote;             // woken through its shard's remote queue (shared channels involved)
	struct sq_chan *lock;    // scratch: select's lock order
} sq_waiter;

// One case of a select: a send of *elem, or a receive into *elem (ok: not closed).
typedef struct sq_scase {
	struct sq_chan *c;
	void *elem;
	bool send;
	bool ok;
} sq_scase;

typedef struct sq_chan {
	uint8_t *buf;
	int64_t elemsize, cap, len, head;
	bool closed;
	sq_waiter *recvq_head, *recvq_tail, *sendq_head, *sendq_tail;
	bool shared;        // shared var ... chan: used by every scheduler thread, under mu
	pthread_mutex_t mu;
} sq_chan;

sq_chan *sq_chan_init(sq_chan *c, void *buf, int64_t elemsize, int64_t cap);
void sq_chan_send(sq_chan *c, const void *elem, const char *file, int line);
bool sq_chan_recv(sq_chan *c, void *out);
void sq_chan_close(sq_chan *c, const char *file, int line);
int64_t sq_chan_len_locked(sq_chan *c);
// sq_select runs a select over n channel cases (ws: n waiters for it). It returns the case that
// fired, -1 for default (block false: nothing was ready), or n when timeout_ns (>= 0) passed first.
int64_t sq_select(sq_scase *cs, sq_waiter *ws, int64_t n, bool block, int64_t timeout_ns, const char *file, int line);

// Blocking externs (extern ... blocking): the call runs on a helper thread while the goroutine
// waits, so the other goroutines of its scheduler thread keep running.
extern const int64_t sq_helpers_per_thread; // 0: the program makes no blocking calls
extern const int64_t sq_helper_stack;       // their stack size, proven like goroutine stacks
void sq_blocking(void (*fn)(void *), void *arg);

// sq_detach moves the running goroutine, in the middle of a C call that turned out to be slow, to a
// helper thread: its scheduler thread goes on with its other goroutines, and the C code carries on
// (on the same stack) on the helper. It returns true once running there (false if it can't: no
// helpers, or already on one). sq_reattach, called before the C function returns to squark code,
// moves the goroutine back to its own thread (it does nothing if the goroutine wasn't moved).
// In between, the C code runs on another OS thread: it must not touch the squark runtime or rely on
// thread-local state or thread-owned locks from before sq_detach.
bool sq_detach(void);
void sq_reattach(void);
bool sq_on_helper(void); // running on a helper thread (a blocking call, or detached)
int64_t sq_nanotime(void);

// sq_gmutex: a lock held by a goroutine; others that want it wait (park) instead of blocking their
// thread. Usable from any scheduler thread. Zero-initialized storage needs sq_gmutex_init.
typedef struct sq_gmutex {
	pthread_mutex_t mu;         // guards the wait queue (only used when goroutines wait)
	struct sq_g *_Atomic owner;
	_Atomic bool contended;     // goroutines may be waiting: unlock must look
	struct sq_g *wait_head, *wait_tail;
} sq_gmutex;
void sq_gmutex_init(sq_gmutex *m);
void sq_gmutex_lock(sq_gmutex *m);
void sq_gmutex_unlock(sq_gmutex *m);
bool sq_gmutex_held(sq_gmutex *m); // by the running goroutine
static inline int64_t sq_chan_len(sq_chan *c) { return c ? (c->shared ? sq_chan_len_locked(c) : c->len) : 0; }
sq_chan *sq_chan_init_shared(sq_chan *c, void *buf, int64_t elemsize, int64_t cap);
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
static inline int64_t sq_map_cap(sq_map *m) { return m ? m->maxlen : 0; } // entries it can hold
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
extern const bool sq_threads_env;  // SQUARK_THREADS applies (the program has a threads directive)
extern const int64_t sq_threads_max; // with a budget: the count it was proven for (0: no limit)
extern const size_t sq_globals_size; // generated: one shard's package variables
void sq_globals_set(void *p);        // generated: point this thread at a shard's package variables
int64_t sq_shard_id(void);
void sq_start_shards(void);          // shard 0, once its package variables (and shared vars) are set
void sq_program_main(void *args);

#endif
