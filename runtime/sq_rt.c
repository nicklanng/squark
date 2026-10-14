// squark runtime: M:1 goroutine scheduler, kqueue/epoll netpoll, timers, channels, maps, panics.
// This file must never call malloc/free (checked by `squark build`, which inspects the
// undefined symbols of every object it produces).
#define _GNU_SOURCE
#include "sq_rt.h"
#include "sq_internal.h"

#include <errno.h>
#include <poll.h>
#include <sys/stat.h>
#include <stdio.h>
#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <pthread.h>
#include <stdatomic.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <time.h>
#include <unistd.h>
#if defined(__APPLE__) || defined(__FreeBSD__)
#include <sys/event.h>
#define SQ_KQUEUE 1
#else
#include <sys/epoll.h>
#include <sys/eventfd.h>
#include <sys/prctl.h>
#endif

// ---------------------------------------------------------------------------
// context switching


void sq_ctx_switch(sq_ctx *from, sq_ctx *to);

// ThreadSanitizer has to be told when the runtime switches stacks (goroutine <-> scheduler), or it
// sees one thread jumping between unrelated stacks. Nothing of this exists in normal builds.
#if defined(__has_feature)
#if __has_feature(thread_sanitizer)
#define SQ_TSAN 1
#endif
#endif
#if defined(__SANITIZE_THREAD__) && !defined(SQ_TSAN)
#define SQ_TSAN 1
#endif
#ifdef SQ_TSAN
void *__tsan_get_current_fiber(void);
void *__tsan_create_fiber(unsigned flags);
void __tsan_switch_to_fiber(void *fiber, unsigned flags);
#define TSAN_SWITCH(fiber) __tsan_switch_to_fiber((fiber), 0)
#else
#define TSAN_SWITCH(fiber) ((void)0)
#endif

#if defined(__aarch64__)
#if defined(__APPLE__)
#define SQ_SYM(x) "_" #x
#else
#define SQ_SYM(x) #x
#endif
__asm__(
	".text\n"
	".globl " SQ_SYM(sq_ctx_switch) "\n"
	".p2align 2\n"
	SQ_SYM(sq_ctx_switch) ":\n"
	"  stp x19, x20, [x0, #0]\n"
	"  stp x21, x22, [x0, #16]\n"
	"  stp x23, x24, [x0, #32]\n"
	"  stp x25, x26, [x0, #48]\n"
	"  stp x27, x28, [x0, #64]\n"
	"  stp x29, x30, [x0, #80]\n"
	"  mov x9, sp\n"
	"  str x9, [x0, #96]\n"
	"  stp d8, d9, [x0, #104]\n"
	"  stp d10, d11, [x0, #120]\n"
	"  stp d12, d13, [x0, #136]\n"
	"  stp d14, d15, [x0, #152]\n"
	"  ldp x19, x20, [x1, #0]\n"
	"  ldp x21, x22, [x1, #16]\n"
	"  ldp x23, x24, [x1, #32]\n"
	"  ldp x25, x26, [x1, #48]\n"
	"  ldp x27, x28, [x1, #64]\n"
	"  ldp x29, x30, [x1, #80]\n"
	"  ldr x9, [x1, #96]\n"
	"  mov sp, x9\n"
	"  ldp d8, d9, [x1, #104]\n"
	"  ldp d10, d11, [x1, #120]\n"
	"  ldp d12, d13, [x1, #136]\n"
	"  ldp d14, d15, [x1, #152]\n"
	"  ret\n");
#elif defined(__x86_64__)
#if defined(__APPLE__)
#define SQ_SYM(x) "_" #x
#else
#define SQ_SYM(x) #x
#endif
__asm__(
	".text\n"
	".globl " SQ_SYM(sq_ctx_switch) "\n"
	SQ_SYM(sq_ctx_switch) ":\n"
	"  movq %rbx, 0(%rdi)\n"
	"  movq %rbp, 8(%rdi)\n"
	"  movq %r12, 16(%rdi)\n"
	"  movq %r13, 24(%rdi)\n"
	"  movq %r14, 32(%rdi)\n"
	"  movq %r15, 40(%rdi)\n"
	"  movq %rsp, 48(%rdi)\n"
	"  movq 0(%rsi), %rbx\n"
	"  movq 8(%rsi), %rbp\n"
	"  movq 16(%rsi), %r12\n"
	"  movq 24(%rsi), %r13\n"
	"  movq 32(%rsi), %r14\n"
	"  movq 40(%rsi), %r15\n"
	"  movq 48(%rsi), %rsp\n"
	"  ret\n");
#endif

// ---------------------------------------------------------------------------
// goroutines


// Everything a scheduler owns lives in its shard. One shard per scheduler thread; code reaches the
// current one through sq_self.
#define SQ_MAX_SHARDS 1024 // threads N

typedef struct sq_shard {
	sq_ctx sched_ctx;
	sq_g *cur_g;
	sq_g *runq_head, *runq_tail;
	int64_t next_gid;
	int64_t total_parked; // goroutines blocked on channels/slots (not fds/timers)
	int64_t fd_waiters;   // outstanding netpoll registrations
	uint8_t outbuf[8192]; // println output, flushed at exit and before writes to fd 1
	size_t outlen;
	int pollfd; // kqueue or epoll
	sq_pool *const *pools; // this shard's copy of every pool, by sq_pool.index
	sq_g **timer_heap;
	int64_t ntimers;
#if SQ_KQUEUE
	struct kevent changes[256]; // registrations batched into the next kevent call
	int nchanges;
#endif
	int32_t idle_ready[1 << 16]; // see "The idle set" below
	int64_t idle_head, idle_len, idle_parked;
	sq_g *idle_waiter;
	uint8_t fd_read_kind[1024]; // see read_needs_poll
	char date_cache[32];        // sq_httpdate
	time_t date_cached_at;
	int64_t id;        // 0 .. sq_shard_count-1
	bool main_done;    // this shard's main has returned; it keeps serving its other goroutines
	bool start_pending; // shard 0: start the other shards from the scheduler's stack
	// goroutines woken by other shards (shared channels): queued here, then this shard's poller
	// is woken (EVFILT_USER on macOS, an eventfd on Linux)
	pthread_mutex_t remote_mu;
	sq_g *remote_head, *remote_tail;
	atomic_bool remote_pending;
	int wake_fd;              // Linux: eventfd
	int64_t shared_parked;    // this shard's goroutines waiting on shared channels
	struct {
		int fd;
		int entry; // in listen_table
	} listeners[32];   // this shard's listening sockets
	int nlisteners;
	void *globals;     // this shard's package variables (shard 0: the generated static copy)
	void *tsan_sched;  // ThreadSanitizer builds only: the scheduler's own fiber
	uint8_t *sigstack; // this thread's signal stack (for stack-overflow reports)
	size_t sigstack_size;
} sq_shard;

extern sq_g *sq_timer_heap[];
static sq_shard sq_shard0 = {.next_gid = 1, .pollfd = -1, .pools = sq_pools, .timer_heap = sq_timer_heap, .date_cached_at = -1};
static _Thread_local sq_shard *sq_self = &sq_shard0;
static atomic_int_fast64_t live_shards;

static void heap_remove(sq_g *g);
static void err_write(const char *s);
static void stack_report(void);
static bool stackcheck;
static void release(uint8_t *p, int64_t n);

static void runq_push(sq_g *g) {
	g->state = G_RUNNABLE;
	g->next = NULL;
	if (sq_self->runq_tail) sq_self->runq_tail->next = g; else sq_self->runq_head = g;
	sq_self->runq_tail = g;
}

static sq_g *runq_pop(void) {
	sq_g *g = sq_self->runq_head;
	if (g) {
		sq_self->runq_head = g->next;
		if (!sq_self->runq_head) sq_self->runq_tail = NULL;
		g->next = NULL;
	}
	return g;
}

static void sq_park(void) {
	sq_g *g = sq_self->cur_g;
	g->state = G_WAITING;
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&g->ctx, &sq_self->sched_ctx);
}

static void sq_ready(sq_g *g) { runq_push(g); }

void sq_gosched(void) {
	sq_g *g = sq_self->cur_g;
	runq_push(g);
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&g->ctx, &sq_self->sched_ctx);
}

static void g_entry(void) {
	sq_g *g = sq_self->cur_g;
	g->entry(g->args);
	g->state = G_DEAD;
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&g->ctx, &sq_self->sched_ctx);
	__builtin_unreachable();
}

static void g_prepare(sq_g *g, void (*entry)(void *), const void *args, size_t argsize) {
#ifdef SQ_TSAN
	if (!g->tsan_fiber) g->tsan_fiber = __tsan_create_fiber(0); // kept for the slot's next goroutine
#endif
	uintptr_t top = (uintptr_t)g->stack_hi;
	top -= (argsize + 15) & ~(uintptr_t)15;
	if (argsize) memcpy((void *)top, args, argsize);
	g->args = (void *)top;
	g->entry = entry;
	memset(&g->ctx, 0, sizeof g->ctx);
#if defined(__aarch64__)
	top &= ~(uintptr_t)15;
	g->ctx.sp = top;
	g->ctx.x[11] = (uint64_t)(uintptr_t)g_entry; // lr
#elif defined(__x86_64__)
	top &= ~(uintptr_t)15;
	top -= 16;
	*(uint64_t *)top = (uint64_t)(uintptr_t)g_entry;
	g->ctx.rsp = top;
#endif
	g->id = sq_self->next_gid++;
}

static void pool_init(sq_pool *p) {
	p->free = NULL;
	size_t total = (size_t)(p->count * p->slot_size);
	void *mem = mmap(NULL, total, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	if (mem == MAP_FAILED) {
		err_write("fatal error: cannot reserve goroutine stacks for pool ");
		err_write(p->name);
		err_write("\n");
		_exit(2);
	}
	p->stacks = mem;
	for (int64_t i = p->count - 1; i >= 0; i--) {
		sq_g *g = &p->gs[i];
		uint8_t *slot = p->stacks + i * p->slot_size;
		long pagesz = sysconf(_SC_PAGESIZE);
		// the guard page at the low end of every stack is installed on first use (see sq_go)
		g->stack_lo = slot + pagesz;
		g->heap_index = -1;
		g->wait_fd = -1;
		g->stack_hi = slot + p->slot_size;
		g->pool = p;
		g->shard = sq_self;
		g->state = G_FREE;
		g->next = p->free;
		p->free = g;
	}
}

void sq_go(sq_pool *p, void (*entry)(void *), const void *args, size_t argsize) {
	p = sq_self->pools[p->index]; // generated code names the template; use this shard's copy
	while (!p->free) {
		// all slots busy: wait for one (backpressure instead of unbounded growth)
		sq_g *self = sq_self->cur_g;
		self->next = NULL;
		if (p->waitq_tail) p->waitq_tail->next = self; else p->waitq_head = self;
		p->waitq_tail = self;
		sq_self->total_parked++;
		sq_park();
	}
	sq_g *g = p->free;
	p->free = g->next;
	if (!g->guarded) {
		long pagesz = sysconf(_SC_PAGESIZE);
		mprotect(g->stack_lo - pagesz, (size_t)pagesz, PROT_NONE);
		g->guarded = true;
	}
	if (stackcheck) memset(g->stack_lo, 0xA5, (size_t)(g->stack_hi - g->stack_lo));
	g_prepare(g, entry, args, argsize);
	p->live++;
	p->started++;
	if (p->live > p->peak) p->peak = p->live;
	runq_push(g);
}

// stack_used measures how deep a painted stack was used.
static int64_t stack_used(sq_g *g) {
	uint8_t *q = g->stack_lo;
	while (q < g->stack_hi && *q == 0xA5) q++;
	return g->stack_hi - q;
}

static void g_release(sq_g *g) {
	sq_pool *p = g->pool;
	if (stackcheck) {
		int64_t u = stack_used(g);
		if (u > p->observed) p->observed = u;
	}
	if (g->armed) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
	g->wait_fd = -1;
	heap_remove(g);
	if (g->release_stack) {
		g->release_stack = false;
		release(g->stack_lo, g->stack_hi - g->stack_lo);
	}
	g->state = G_FREE;
	g->next = p->free;
	p->free = g;
	p->live--;
	if (p->waitq_head) {
		sq_g *w = p->waitq_head;
		p->waitq_head = w->next;
		if (!p->waitq_head) p->waitq_tail = NULL;
		sq_self->total_parked--;
		sq_ready(w);
	}
}

// ---------------------------------------------------------------------------
// output


static void write_all(int fd, const void *p, size_t n) {
	const uint8_t *b = p;
	while (n > 0) {
		ssize_t w = write(fd, b, n);
		if (w < 0) {
			if (errno == EINTR) continue;
			if (errno == EAGAIN) {
				struct timespec ts = {0, 1000000};
				nanosleep(&ts, NULL);
				continue;
			}
			return;
		}
		b += w;
		n -= (size_t)w;
	}
}

void sq_flush(void) {
	if (sq_self->outlen) write_all(1, sq_self->outbuf, sq_self->outlen);
	sq_self->outlen = 0;
}

static void out(const void *p, size_t n) {
	if (sq_self->outlen + n > sizeof sq_self->outbuf) sq_flush();
	if (n > sizeof sq_self->outbuf) {
		write_all(1, p, n);
		return;
	}
	memcpy(sq_self->outbuf + sq_self->outlen, p, n);
	sq_self->outlen += n;
}

void sq_print_byte(uint8_t c) {
	out(&c, 1);
	if (c == '\n') sq_flush();
}
void sq_print_str(sq_string s) { out(s.p, (size_t)s.len); }

static size_t fmt_uint(char *buf, uint64_t v) {
	char tmp[24];
	size_t n = 0;
	do {
		tmp[n++] = (char)('0' + v % 10);
		v /= 10;
	} while (v);
	for (size_t i = 0; i < n; i++) buf[i] = tmp[n - 1 - i];
	return n;
}

void sq_print_uint(uint64_t v) {
	char buf[24];
	out(buf, fmt_uint(buf, v));
}

void sq_print_int(int64_t v) {
	char buf[24];
	size_t n = 0;
	if (v < 0) {
		buf[n++] = '-';
		n += fmt_uint(buf + 1, (uint64_t)0 - (uint64_t)v);
	} else {
		n = fmt_uint(buf, (uint64_t)v);
	}
	out(buf, n);
}

void sq_print_float(double v) {
	// Go's print uses %e-style formatting; keep it simple and exact enough.
	char buf[40];
	if (v != v) { out("NaN", 3); return; }
	if (v > 1.7976931348623157e308) { out("+Inf", 4); return; }
	if (v < -1.7976931348623157e308) { out("-Inf", 4); return; }
	int neg = v < 0;
	if (neg) v = -v;
	int e = 0;
	if (v != 0) {
		while (v >= 10) { v /= 10; e++; }
		while (v < 1) { v *= 10; e--; }
	}
	size_t n = 0;
	buf[n++] = neg ? '-' : '+';
	uint64_t digits = (uint64_t)(v * 1e8 + 0.5);
	if (digits >= 1000000000ULL) { digits /= 10; e++; }
	char d[12];
	size_t nd = fmt_uint(d, digits);
	if (nd < 9) { // 0 (or a rounding edge): always nine significant digits
		memmove(d + 9 - nd, d, nd);
		memset(d, '0', 9 - nd);
		nd = 9;
	}
	buf[n++] = d[0];
	buf[n++] = '.';
	for (size_t i = 1; i < nd; i++) buf[n++] = d[i];
	buf[n++] = 'e';
	buf[n++] = e < 0 ? '-' : '+';
	if (e < 0) e = -e;
	buf[n++] = (char)('0' + e / 100);
	buf[n++] = (char)('0' + (e / 10) % 10);
	buf[n++] = (char)('0' + e % 10);
	out(buf, n);
}

void sq_print_bool(bool v) {
	if (v) out("true", 4); else out("false", 5);
}

void sq_print_ptr(const void *p) {
	static const char hex[] = "0123456789abcdef";
	char buf[20];
	uintptr_t v = (uintptr_t)p;
	size_t n = 0;
	buf[n++] = '0';
	buf[n++] = 'x';
	char tmp[16];
	size_t k = 0;
	do {
		tmp[k++] = hex[v & 15];
		v >>= 4;
	} while (v);
	while (k) buf[n++] = tmp[--k];
	out(buf, n);
}

void sq_print_err(sq_error e) {
	if (!e.msg.p) { out("<nil>", 5); return; }
	sq_print_str(e.msg);
}

// ---------------------------------------------------------------------------
// panics

static void err_write(const char *s) { write_all(2, s, strlen(s)); }

static void err_int(int64_t v) {
	char buf[24];
	size_t n = 0;
	if (v < 0) {
		buf[n++] = '-';
		n += fmt_uint(buf + 1, (uint64_t)0 - (uint64_t)v);
	} else {
		n = fmt_uint(buf, (uint64_t)v);
	}
	write_all(2, buf, n);
}

static SQ_NORETURN void die(const char *file, int line) {
	err_write("\n\ngoroutine ");
	if (sq_self->cur_g) {
		err_int(sq_self->cur_g->id);
		err_write(" [");
		err_write(sq_self->cur_g->pool->name);
		err_write("]");
	} else {
		err_write("0 [scheduler]");
	}
	err_write(":\n\t");
	if (file) {
		err_write(file);
		err_write(":");
		err_int(line);
	}
	err_write("\nexit status 2\n");
	_exit(2);
}

void sq_panic_cstr(const char *msg, const char *file, int line) {
	sq_flush();
	err_write("panic: ");
	err_write(msg);
	die(file, line);
}

void sq_panic_str(sq_string msg, const char *file, int line) {
	sq_flush();
	err_write("panic: ");
	write_all(2, msg.p, (size_t)msg.len);
	die(file, line);
}

void sq_panic_int(int64_t v, const char *file, int line) {
	sq_flush();
	err_write("panic: ");
	err_int(v);
	die(file, line);
}

void sq_panic_index(int64_t i, int64_t n, const char *file, int line) {
	sq_flush();
	err_write("panic: runtime error: index out of range [");
	err_int(i);
	err_write("] with length ");
	err_int(n);
	die(file, line);
}

void sq_panic_slice(int64_t lo, int64_t hi, int64_t max, int64_t cap, const char *file, int line) {
	sq_flush();
	err_write("panic: runtime error: slice bounds out of range [");
	err_int(lo);
	err_write(":");
	err_int(hi);
	err_write(":");
	err_int(max);
	err_write("] with capacity ");
	err_int(cap);
	die(file, line);
}

void sq_panic_append(int64_t need, int64_t cap, const char *file, int line) {
	sq_flush();
	err_write("panic: runtime error: append exceeds capacity (need ");
	err_int(need);
	err_write(", capacity ");
	err_int(cap);
	err_write("); squark slices never grow onto the heap");
	die(file, line);
}

void sq_panic_arena(sq_arena *a, int64_t need, const char *file, int line) {
	sq_flush();
	err_write("panic: arena ");
	err_write(a->name);
	err_write(" is full (capacity ");
	err_int(a->cap);
	err_write(", used ");
	err_int(a->used);
	err_write(", need ");
	err_int(need);
	err_write(" more)");
	die(file, line);
}

void *sq_arena_grow(sq_arena *a, void *p, int64_t used_bytes, int64_t old_bytes, int64_t new_bytes, int64_t align, const char *file, int line) {
	uint8_t *b = p;
	if (b && b + old_bytes == a->base + a->used && b >= a->base && new_bytes - old_bytes <= a->cap - a->used) {
		a->used += new_bytes - old_bytes; // the block is on top of the arena: extend in place
		return p;
	}
	void *np = sq_arena_raw(a, new_bytes, align, file, line);
	if (used_bytes > 0) memcpy(np, p, (size_t)used_bytes);
	return np;
}

void sq_panic_divide(const char *file, int line) {
	sq_panic_cstr("runtime error: integer divide by zero", file, line);
}

static uint8_t sq_sigstack[32768]; // not "sigstack": glibc declares a function of that name

static void on_fault(int sig, siginfo_t *si, void *uctx) {
	(void)uctx;
	uintptr_t addr = (uintptr_t)si->si_addr;
	long pagesz = sysconf(_SC_PAGESIZE);
	for (int64_t i = 0; i < sq_npools; i++) {
		sq_pool *p = sq_self->pools[i];
		uintptr_t lo = (uintptr_t)p->stacks, hi = lo + (uintptr_t)(p->count * p->slot_size);
		if (addr >= lo && addr < hi) {
			uintptr_t off = (addr - lo) % (uintptr_t)p->slot_size;
			if (off < (uintptr_t)pagesz) {
				sq_flush();
				err_write("fatal error: stack overflow in goroutine [");
				err_write(p->name);
				err_write("] (a C function used more stack than its //squark:stack budget)");
				die(NULL, 0);
			}
		}
	}
	sq_flush();
	if (addr < 65536) {
		err_write("panic: runtime error: invalid memory address or nil pointer dereference");
	} else {
		err_write("fatal error: unexpected signal ");
		err_int(sig);
		err_write(" at address ");
		char buf[20];
		static const char hex[] = "0123456789abcdef";
		size_t n = 0;
		for (int s = 60; s >= 0; s -= 4) buf[n++] = hex[(addr >> s) & 15];
		write_all(2, buf, n);
	}
	die(NULL, 0);
}

// ---------------------------------------------------------------------------
// strings

int32_t sq_decode_rune(sq_string s, int64_t i, int64_t *width) {
	const uint8_t *p = s.p + i;
	int64_t n = s.len - i;
	uint8_t c = p[0];
	if (c < 0x80) { *width = 1; return c; }
	if (c >= 0xC2 && c < 0xE0 && n >= 2 && (p[1] & 0xC0) == 0x80) {
		*width = 2;
		return ((int32_t)(c & 0x1F) << 6) | (p[1] & 0x3F);
	}
	if (c >= 0xE0 && c < 0xF0 && n >= 3 && (p[1] & 0xC0) == 0x80 && (p[2] & 0xC0) == 0x80) {
		int32_t r = ((int32_t)(c & 0x0F) << 12) | ((int32_t)(p[1] & 0x3F) << 6) | (p[2] & 0x3F);
		if (r >= 0x800 && (r < 0xD800 || r > 0xDFFF)) { *width = 3; return r; }
	}
	if (c >= 0xF0 && c < 0xF5 && n >= 4 && (p[1] & 0xC0) == 0x80 && (p[2] & 0xC0) == 0x80 && (p[3] & 0xC0) == 0x80) {
		int32_t r = ((int32_t)(c & 0x07) << 18) | ((int32_t)(p[1] & 0x3F) << 12) | ((int32_t)(p[2] & 0x3F) << 6) | (p[3] & 0x3F);
		if (r >= 0x10000 && r <= 0x10FFFF) { *width = 4; return r; }
	}
	*width = 1;
	return 0xFFFD;
}

// ---------------------------------------------------------------------------
// channels

sq_chan *sq_chan_init(sq_chan *c, void *buf, int64_t elemsize, int64_t cap) {
	memset(c, 0, sizeof *c);
	c->buf = buf;
	c->elemsize = elemsize;
	c->cap = cap;
	return c;
}

static void q_push(sq_g **head, sq_g **tail, sq_g *g) {
	g->next = NULL;
	if (*tail) (*tail)->next = g; else *head = g;
	*tail = g;
}

static sq_g *q_pop(sq_g **head, sq_g **tail) {
	sq_g *g = *head;
	if (g) {
		*head = g->next;
		if (!*head) *tail = NULL;
	}
	return g;
}

static void chan_send_shared(sq_chan *c, const void *elem, const char *file, int line);
static bool chan_recv_shared(sq_chan *c, void *outp);
static void chan_close_shared(sq_chan *c, const char *file, int line);

void sq_chan_send(sq_chan *c, const void *elem, const char *file, int line) {
	if (c && c->shared) {
		chan_send_shared(c, elem, file, line);
		return;
	}
	if (!c) {
		sq_self->total_parked++;
		sq_park(); // send on nil channel blocks forever
	}
	if (c->closed) sq_panic_cstr("send on closed channel", file, line);
	sq_g *r = q_pop(&c->recvq_head, &c->recvq_tail);
	if (r) {
		memcpy(r->elem, elem, (size_t)c->elemsize);
		r->ok = true;
		sq_self->total_parked--;
		sq_ready(r);
		return;
	}
	if (c->len < c->cap) {
		int64_t tail = (c->head + c->len) % c->cap;
		memcpy(c->buf + tail * c->elemsize, elem, (size_t)c->elemsize);
		c->len++;
		return;
	}
	sq_g *g = sq_self->cur_g;
	g->elem = (void *)elem;
	g->ok = false;
	q_push(&c->sendq_head, &c->sendq_tail, g);
	sq_self->total_parked++;
	sq_park();
	if (!g->ok) sq_panic_cstr("send on closed channel", file, line);
}

bool sq_chan_recv(sq_chan *c, void *outp) {
	if (c && c->shared) return chan_recv_shared(c, outp);
	if (!c) {
		sq_self->total_parked++;
		sq_park();
	}
	if (c->len > 0) {
		memcpy(outp, c->buf + c->head * c->elemsize, (size_t)c->elemsize);
		c->head = (c->head + 1) % c->cap;
		c->len--;
		sq_g *s = q_pop(&c->sendq_head, &c->sendq_tail);
		if (s) {
			int64_t tail = (c->head + c->len) % c->cap;
			memcpy(c->buf + tail * c->elemsize, s->elem, (size_t)c->elemsize);
			c->len++;
			s->ok = true;
			sq_self->total_parked--;
			sq_ready(s);
		}
		return true;
	}
	sq_g *s = q_pop(&c->sendq_head, &c->sendq_tail);
	if (s) {
		memcpy(outp, s->elem, (size_t)c->elemsize);
		s->ok = true;
		sq_self->total_parked--;
		sq_ready(s);
		return true;
	}
	if (c->closed) {
		memset(outp, 0, (size_t)c->elemsize);
		return false;
	}
	sq_g *g = sq_self->cur_g;
	g->elem = outp;
	g->ok = false;
	q_push(&c->recvq_head, &c->recvq_tail, g);
	sq_self->total_parked++;
	sq_park();
	if (!g->ok) memset(outp, 0, (size_t)c->elemsize);
	return g->ok;
}

void sq_chan_close(sq_chan *c, const char *file, int line) {
	if (c && c->shared) {
		chan_close_shared(c, file, line);
		return;
	}
	if (!c) sq_panic_cstr("close of nil channel", file, line);
	if (c->closed) sq_panic_cstr("close of closed channel", file, line);
	c->closed = true;
	sq_g *g;
	while ((g = q_pop(&c->recvq_head, &c->recvq_tail))) {
		g->ok = false;
		sq_self->total_parked--;
		sq_ready(g);
	}
	while ((g = q_pop(&c->sendq_head, &c->sendq_tail))) {
		g->ok = false;
		sq_self->total_parked--;
		sq_ready(g);
	}
}

// Shared channels (shared var ... chan): every scheduler thread may use them, so each operation
// holds the channel's mutex, and a waiter that belongs to another shard is handed to that shard
// (ready_shared) instead of this one's run queue. A waiter's stack stays put while it is parked,
// so copying into it from another thread is safe; it runs again only on its own shard.

static void wake_shard(sq_shard *s) {
#if SQ_KQUEUE
	struct kevent ev;
	EV_SET(&ev, 1, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL);
	kevent(s->pollfd, &ev, 1, NULL, 0, NULL);
#else
	uint64_t one = 1;
	ssize_t r = write(s->wake_fd, &one, sizeof one);
	(void)r;
#endif
}

// ready_shared makes runnable a goroutine that was parked on a shared channel.
static void ready_shared(sq_g *g) {
	sq_shard *s = g->shard;
	if (s == sq_self) {
		s->shared_parked--;
		runq_push(g);
		return;
	}
	pthread_mutex_lock(&s->remote_mu);
	g->next = NULL;
	if (s->remote_tail) s->remote_tail->next = g; else s->remote_head = g;
	s->remote_tail = g;
	pthread_mutex_unlock(&s->remote_mu);
	if (!atomic_exchange(&s->remote_pending, true)) wake_shard(s);
}

// drain_remote moves goroutines other shards have woken onto this shard's run queue.
static void drain_remote(void) {
	if (!atomic_load_explicit(&sq_self->remote_pending, memory_order_acquire)) return;
	atomic_store(&sq_self->remote_pending, false);
#if !SQ_KQUEUE
	uint64_t v;
	ssize_t r = read(sq_self->wake_fd, &v, sizeof v);
	(void)r;
#endif
	pthread_mutex_lock(&sq_self->remote_mu);
	sq_g *g = sq_self->remote_head;
	sq_self->remote_head = sq_self->remote_tail = NULL;
	pthread_mutex_unlock(&sq_self->remote_mu);
	while (g) {
		sq_g *next = g->next;
		sq_self->shared_parked--;
		runq_push(g);
		g = next;
	}
}

static void park_shared(sq_chan *c) {
	sq_self->shared_parked++;
	sq_self->cur_g->state = G_WAITING; // before unlocking: a waker may queue it at once
	pthread_mutex_unlock(&c->mu);
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&sq_self->cur_g->ctx, &sq_self->sched_ctx);
}

sq_chan *sq_chan_init_shared(sq_chan *c, void *buf, int64_t elemsize, int64_t cap) {
	sq_chan_init(c, buf, elemsize, cap);
	c->shared = true;
	pthread_mutex_init(&c->mu, NULL);
	return c;
}

int64_t sq_chan_len_locked(sq_chan *c) {
	pthread_mutex_lock(&c->mu);
	int64_t n = c->len;
	pthread_mutex_unlock(&c->mu);
	return n;
}

static void chan_send_shared(sq_chan *c, const void *elem, const char *file, int line) {
	pthread_mutex_lock(&c->mu);
	if (c->closed) {
		pthread_mutex_unlock(&c->mu);
		sq_panic_cstr("send on closed channel", file, line);
	}
	sq_g *r = q_pop(&c->recvq_head, &c->recvq_tail);
	if (r) {
		memcpy(r->elem, elem, (size_t)c->elemsize);
		r->ok = true;
		pthread_mutex_unlock(&c->mu);
		ready_shared(r);
		return;
	}
	if (c->len < c->cap) {
		int64_t tail = (c->head + c->len) % c->cap;
		memcpy(c->buf + tail * c->elemsize, elem, (size_t)c->elemsize);
		c->len++;
		pthread_mutex_unlock(&c->mu);
		return;
	}
	sq_g *g = sq_self->cur_g;
	g->elem = (void *)elem;
	g->ok = false;
	q_push(&c->sendq_head, &c->sendq_tail, g);
	park_shared(c);
	if (!g->ok) sq_panic_cstr("send on closed channel", file, line);
}

static bool chan_recv_shared(sq_chan *c, void *outp) {
	pthread_mutex_lock(&c->mu);
	if (c->len > 0) {
		memcpy(outp, c->buf + c->head * c->elemsize, (size_t)c->elemsize);
		c->head = (c->head + 1) % c->cap;
		c->len--;
		sq_g *s = q_pop(&c->sendq_head, &c->sendq_tail);
		if (s) {
			int64_t tail = (c->head + c->len) % c->cap;
			memcpy(c->buf + tail * c->elemsize, s->elem, (size_t)c->elemsize);
			c->len++;
			s->ok = true;
		}
		pthread_mutex_unlock(&c->mu);
		if (s) ready_shared(s);
		return true;
	}
	sq_g *s = q_pop(&c->sendq_head, &c->sendq_tail);
	if (s) {
		memcpy(outp, s->elem, (size_t)c->elemsize);
		s->ok = true;
		pthread_mutex_unlock(&c->mu);
		ready_shared(s);
		return true;
	}
	if (c->closed) {
		pthread_mutex_unlock(&c->mu);
		memset(outp, 0, (size_t)c->elemsize);
		return false;
	}
	sq_g *g = sq_self->cur_g;
	g->elem = outp;
	g->ok = false;
	q_push(&c->recvq_head, &c->recvq_tail, g);
	park_shared(c);
	if (!g->ok) memset(outp, 0, (size_t)c->elemsize);
	return g->ok;
}

static void chan_close_shared(sq_chan *c, const char *file, int line) {
	pthread_mutex_lock(&c->mu);
	if (c->closed) {
		pthread_mutex_unlock(&c->mu);
		sq_panic_cstr("close of closed channel", file, line);
	}
	c->closed = true;
	sq_g *waiters = NULL, *g;
	while ((g = q_pop(&c->recvq_head, &c->recvq_tail)) || (g = q_pop(&c->sendq_head, &c->sendq_tail))) {
		g->ok = false;
		g->next = waiters; // q_pop cleared nothing; relink into a private list
		waiters = g;
	}
	pthread_mutex_unlock(&c->mu);
	while (waiters) {
		g = waiters;
		waiters = g->next;
		ready_shared(g);
	}
}

// ---------------------------------------------------------------------------
// maps: linear probing. delete leaves a tombstone (SQ_SLOT_DELETED) instead of moving entries, so a
// range loop that deletes still visits every other entry exactly once. Tombstones are compacted
// away only when a new key is inserted (see map_compact).

static inline uint64_t mix64(uint64_t x) {
	x ^= x >> 30;
	x *= 0xbf58476d1ce4e5b9ULL;
	x ^= x >> 27;
	x *= 0x94d049bb133111ebULL;
	x ^= x >> 31;
	return x;
}

static uint64_t map_hash(sq_map *m, const void *key) {
	if (m->kind == SQ_KEY_STRING) {
		const sq_string *s = key;
		uint64_t h = 0xcbf29ce484222325ULL ^ (uint64_t)s->len;
		int64_t i = 0;
		for (; i + 8 <= s->len; i += 8) {
			uint64_t w;
			memcpy(&w, s->p + i, 8);
			h = (h ^ w) * 0x100000001b3ULL;
			h ^= h >> 29;
		}
		for (; i < s->len; i++) h = (h ^ s->p[i]) * 0x100000001b3ULL;
		return mix64(h);
	}
	uint64_t v = 0;
	memcpy(&v, key, m->keysize < 8 ? (size_t)m->keysize : 8);
	return mix64(v);
}

static inline bool map_keyeq(sq_map *m, const void *a, const void *b) {
	if (m->kind == SQ_KEY_STRING) return sq_str_eq(*(const sq_string *)a, *(const sq_string *)b);
	return memcmp(a, b, (size_t)m->keysize) == 0;
}

sq_map *sq_map_init(sq_map *m, void *keys, void *vals, uint8_t *ctrl, int64_t slots, int64_t maxlen,
                    int32_t keysize, int32_t valsize, int32_t kind) {
	m->keys = keys;
	m->vals = vals;
	m->ctrl = ctrl;
	m->slots = slots;
	m->mask = slots - 1;
	m->len = 0;
	m->deleted = 0;
	m->maxlen = maxlen;
	m->keysize = keysize;
	m->valsize = valsize;
	m->kind = kind;
	memset(ctrl, 0, (size_t)slots);
	return m;
}

sq_map *sq_map_new_in(sq_arena *a, int64_t cap, int32_t keysize, int32_t valsize, int32_t kind, const char *file, int line) {
	if (cap < 0) sq_panic_cstr("negative map capacity", file, line);
	// slots must reach cap*4/3+1 without overflowing; anything this large can't fit an arena anyway
	if (cap > INT64_MAX / 4) sq_panic_arena(a, INT64_MAX, file, line);
	int64_t slots = 1;
	while (slots < cap + cap / 3 + 1) slots <<= 1;
	sq_map *m = sq_arena_raw(a, sizeof(sq_map), 8, file, line);
	void *keys = sq_arena_raw(a, sq_arena_bytes(a, slots, keysize, file, line), 8, file, line);
	void *vals = sq_arena_raw(a, sq_arena_bytes(a, slots, valsize, file, line), 8, file, line);
	uint8_t *ctrl = sq_arena_raw(a, slots, 1, file, line);
	return sq_map_init(m, keys, vals, ctrl, slots, cap, keysize, valsize, kind);
}

static inline int64_t map_home(sq_map *m, int64_t i) {
	return (int64_t)(map_hash(m, m->keys + i * m->keysize) & (uint64_t)m->mask);
}

// map_find returns the slot holding key, or -1 - (the slot to insert it at).
static int64_t map_find(sq_map *m, const void *key) {
	int64_t i = (int64_t)(map_hash(m, key) & (uint64_t)m->mask);
	int64_t free = -1;
	for (int64_t n = 0; n < m->slots; n++, i = (i + 1) & m->mask) {
		uint8_t c = m->ctrl[i];
		if (c == SQ_SLOT_EMPTY) return -1 - (free >= 0 ? free : i);
		if (c == SQ_SLOT_DELETED) {
			if (free < 0) free = i;
		} else if (map_keyeq(m, m->keys + i * m->keysize, key)) {
			return i;
		}
	}
	return -1 - free; // no empty slot left: every free slot is a tombstone, and len < slots
}

static void map_swap(uint8_t *a, uint8_t *b, int32_t n) {
	for (int32_t k = 0; k < n; k++) {
		uint8_t t = a[k];
		a[k] = b[k];
		b[k] = t;
	}
}

// map_compact rehashes in place to drop tombstones. It moves entries, so it runs only from
// sq_map_put inserting a new key (never from delete, which range loops rely on).
static void map_compact(sq_map *m) {
	for (int64_t i = 0; i < m->slots; i++)
		m->ctrl[i] = m->ctrl[i] == SQ_SLOT_FULL ? SQ_SLOT_MOVING : SQ_SLOT_EMPTY;
	for (int64_t i = 0; i < m->slots; i++) {
		while (m->ctrl[i] == SQ_SLOT_MOVING) {
			// place the entry at i at the first slot of its probe sequence not yet holding a placed entry
			int64_t j = map_home(m, i);
			while (m->ctrl[j] == SQ_SLOT_FULL) j = (j + 1) & m->mask;
			if (j == i) {
				m->ctrl[i] = SQ_SLOT_FULL;
			} else if (m->ctrl[j] == SQ_SLOT_EMPTY) {
				memcpy(m->keys + j * m->keysize, m->keys + i * m->keysize, (size_t)m->keysize);
				memcpy(m->vals + j * m->valsize, m->vals + i * m->valsize, (size_t)m->valsize);
				m->ctrl[j] = SQ_SLOT_FULL;
				m->ctrl[i] = SQ_SLOT_EMPTY;
			} else { // j holds an entry still to be placed: swap, then place that one
				map_swap(m->keys + j * m->keysize, m->keys + i * m->keysize, m->keysize);
				map_swap(m->vals + j * m->valsize, m->vals + i * m->valsize, m->valsize);
				m->ctrl[j] = SQ_SLOT_FULL;
			}
		}
	}
	m->deleted = 0;
}

void *sq_map_get(sq_map *m, const void *key) {
	if (!m || m->len == 0) return NULL;
	int64_t i = map_find(m, key);
	return i >= 0 ? m->vals + i * m->valsize : NULL;
}

void *sq_map_put(sq_map *m, const void *key, const char *file, int line) {
	if (!m) sq_panic_cstr("assignment to entry in nil map", file, line);
	int64_t i = map_find(m, key);
	if (i >= 0) return m->vals + i * m->valsize;
	if (m->len >= m->maxlen) sq_panic_cstr("map capacity exceeded (squark maps have a fixed capacity set by make)", file, line);
	if (m->deleted > m->slots / 8) {
		map_compact(m);
		i = map_find(m, key);
	}
	i = -1 - i;
	if (m->ctrl[i] == SQ_SLOT_DELETED) m->deleted--;
	m->ctrl[i] = SQ_SLOT_FULL;
	memcpy(m->keys + i * m->keysize, key, (size_t)m->keysize);
	memset(m->vals + i * m->valsize, 0, (size_t)m->valsize);
	m->len++;
	return m->vals + i * m->valsize;
}

void sq_map_delete(sq_map *m, const void *key) {
	if (!m || m->len == 0) return;
	int64_t i = map_find(m, key);
	if (i < 0) return;
	m->len--;
	if (m->ctrl[(i + 1) & m->mask] != SQ_SLOT_EMPTY) {
		m->ctrl[i] = SQ_SLOT_DELETED;
		m->deleted++;
		return;
	}
	// no probe sequence continues past an empty slot, so this slot and any tombstones right before
	// it can become empty again
	m->ctrl[i] = SQ_SLOT_EMPTY;
	for (i = (i - 1) & m->mask; m->ctrl[i] == SQ_SLOT_DELETED; i = (i - 1) & m->mask) {
		m->ctrl[i] = SQ_SLOT_EMPTY;
		m->deleted--;
	}
}

int64_t sq_map_next(sq_map *m, int64_t i) {
	if (!m) return -1;
	for (; i < m->slots; i++)
		if (m->ctrl[i] == SQ_SLOT_FULL) return i;
	return -1;
}

// ---------------------------------------------------------------------------
// netpoll and timers


static void heap_swap(int64_t a, int64_t b) {
	sq_g *t = sq_self->timer_heap[a];
	sq_self->timer_heap[a] = sq_self->timer_heap[b];
	sq_self->timer_heap[b] = t;
	sq_self->timer_heap[a]->heap_index = a;
	sq_self->timer_heap[b]->heap_index = b;
}

static void heap_up(int64_t i) {
	while (i > 0) {
		int64_t p = (i - 1) / 2;
		if (sq_self->timer_heap[p]->wake_at <= sq_self->timer_heap[i]->wake_at) break;
		heap_swap(i, p);
		i = p;
	}
}

static void heap_down(int64_t i) {
	for (;;) {
		int64_t l = 2 * i + 1, r = l + 1, m = i;
		if (l < sq_self->ntimers && sq_self->timer_heap[l]->wake_at < sq_self->timer_heap[m]->wake_at) m = l;
		if (r < sq_self->ntimers && sq_self->timer_heap[r]->wake_at < sq_self->timer_heap[m]->wake_at) m = r;
		if (m == i) return;
		heap_swap(i, m);
		i = m;
	}
}

static void heap_remove(sq_g *g) {
	int64_t i = g->heap_index;
	if (i < 0) return;
	sq_self->ntimers--;
	if (i != sq_self->ntimers) {
		sq_self->timer_heap[i] = sq_self->timer_heap[sq_self->ntimers];
		sq_self->timer_heap[i]->heap_index = i;
		heap_down(i);
		heap_up(i);
	}
	g->heap_index = -1;
}

static sq_g *heap_pop(void) {
	sq_g *g = sq_self->timer_heap[0];
	sq_self->ntimers--;
	if (sq_self->ntimers > 0) {
		sq_self->timer_heap[0] = sq_self->timer_heap[sq_self->ntimers];
		sq_self->timer_heap[0]->heap_index = 0;
		heap_down(0);
	}
	g->heap_index = -1;
	return g;
}

int64_t sq_nanotime(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

int64_t sq_unixnano(void) {
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

void sq_sleep(int64_t ns) {
	if (ns <= 0) {
		sq_gosched();
		return;
	}
	sq_g *g = sq_self->cur_g;
	g->wait_fd = -1;
	g->wake_at = sq_nanotime() + ns;
	g->heap_index = sq_self->ntimers;
	sq_self->timer_heap[sq_self->ntimers++] = g;
	heap_up(g->heap_index);
	sq_park();
}


// wait_fd parks the current goroutine until fd is readable (mode 'r') or writable ('w').
static void arm_fd(int fd, int mode) {
	sq_g *g = sq_self->cur_g;
	g->wait_fd = fd;
	g->wake = WAKE_NONE;
#if SQ_KQUEUE
	if (sq_self->nchanges == (int)(sizeof sq_self->changes / sizeof sq_self->changes[0])) {
		kevent(sq_self->pollfd, sq_self->changes, sq_self->nchanges, NULL, 0, NULL);
		sq_self->nchanges = 0;
	}
	EV_SET(&sq_self->changes[sq_self->nchanges++], fd, mode == 'r' ? EVFILT_READ : EVFILT_WRITE, EV_ADD | EV_ONESHOT, 0, 0, sq_self->cur_g);
#else
	struct epoll_event ev = {.events = (mode == 'r' ? EPOLLIN : EPOLLOUT) | EPOLLONESHOT, .data.ptr = sq_self->cur_g};
	if (epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, fd, &ev) < 0) epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, fd, &ev);
#endif
	if (!g->armed) {
		g->armed = true;
		sq_self->fd_waiters++;
	}
}

static void wait_fd(int fd, int mode) {
	arm_fd(fd, mode);
	sq_park();
}

// fd_event delivers a readiness event; stale events (for a goroutine no longer waiting on that
// fd) are dropped.
static void fd_event(sq_g *g, int fd) {
	if (!g) return;
	if (g->armed) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
	if (g->state != G_WAITING || g->wait_fd != fd) return;
	g->wait_fd = -1;
	g->wake = WAKE_FD;
	heap_remove(g);
	sq_ready(g);
}

static void idle_event(int fd, int tag);

static void netpoll(int64_t timeout_ns) {
#if SQ_KQUEUE
	struct kevent events[256];
	struct timespec ts, *tsp = NULL;
	if (timeout_ns >= 0) {
		ts.tv_sec = timeout_ns / 1000000000;
		ts.tv_nsec = timeout_ns % 1000000000;
		tsp = &ts;
	}
	int n = kevent(sq_self->pollfd, sq_self->changes, sq_self->nchanges, events, 256, tsp);
	sq_self->nchanges = 0;
	for (int i = 0; i < n; i++) {
		if ((uintptr_t)events[i].udata & 1) { // idle set: udata is tag<<1 | 1 (goroutines are aligned)
			idle_event((int)events[i].ident, (int)((uintptr_t)events[i].udata >> 1));
			continue;
		}
		// EV_ERROR (e.g. fd closed) also wakes the waiter so its syscall reports the error
		fd_event(events[i].udata, (int)events[i].ident);
	}
#else
	struct epoll_event events[256];
	int ms = timeout_ns < 0 ? -1 : (int)((timeout_ns + 999999) / 1000000);
	int n = epoll_wait(sq_self->pollfd, events, 256, ms);
	for (int i = 0; i < n; i++) {
		if (events[i].data.u64 & 1) { // idle set: tag<<33 | fd<<1 | 1
			uint64_t d = events[i].data.u64;
			idle_event((int)((d >> 1) & 0xffffffff), (int)(d >> 33));
			continue;
		}
		sq_g *g = events[i].data.ptr;
		fd_event(g, g ? g->wait_fd : -1);
	}
#endif
}

static void fire_timers(void) {
	if (!sq_self->ntimers) return;
	int64_t now = sq_nanotime();
	while (sq_self->ntimers > 0 && sq_self->timer_heap[0]->wake_at <= now) {
		sq_g *g = heap_pop();
		g->wake = WAKE_TIMER;
		g->wait_fd = -1;
		sq_ready(g);
	}
}

// release returns the whole pages inside [p, p+n) to the OS; they read as zero afterwards.
static void release(uint8_t *p, int64_t n) {
	uintptr_t page = (uintptr_t)sysconf(_SC_PAGESIZE);
	uintptr_t lo = ((uintptr_t)p + page - 1) & ~(page - 1);
	uintptr_t hi = ((uintptr_t)p + (uintptr_t)n) & ~(page - 1);
	if (hi <= lo) return;
#if defined(__APPLE__)
	// on macOS only replacing the mapping drops RSS immediately
	mmap((void *)lo, hi - lo, PROT_READ | PROT_WRITE, MAP_FIXED | MAP_PRIVATE | MAP_ANON, -1, 0);
#else
	madvise((void *)lo, hi - lo, MADV_DONTNEED);
#endif
}

// sq_wait_idle parks until fd is readable (returns 0) or until grace_ns passes first (returns 1).
// On timeout the read registration stays armed; the caller either waits again or hands the fd to
// the idle set with sq_park_idle.
int64_t sq_wait_idle(int64_t fd, int64_t grace_ns) {
	sq_g *g = sq_self->cur_g;
	arm_fd((int)fd, 'r');
	g->wake_at = sq_nanotime() + grace_ns;
	g->heap_index = sq_self->ntimers;
	sq_self->timer_heap[sq_self->ntimers++] = g;
	heap_up(g->heap_index);
	sq_park();
	if (g->wake == WAKE_FD) return 0;
#if !SQ_KQUEUE
	// The read registration would otherwise stay armed for g after it moves on. epoll events carry
	// only g, not the fd, so fd_event can't tell such a stale event from a wait g starts later (a
	// time.Sleep, or another fd). Disarm it; waiting again or sq_park_idle re-registers the fd.
	// (kqueue events carry the fd, which fd_event checks.)
	struct epoll_event ev = {.events = 0};
	epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, (int)fd, &ev);
	if (g->armed) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
#endif
	return 1;
}

// The idle set: connections with nothing to do are registered with the poller without any
// goroutine. When one becomes readable its fd is queued for the (single) idle consumer, which
// typically starts a fresh goroutine for it. An idle connection therefore costs no stack at all.
// Each parked fd carries a small tag (e.g. which server it belongs to) that comes back with it;
// queue entries hold tag<<24 | fd.

static void idle_event(int fd, int tag) {
	sq_self->idle_parked--;
	if (sq_self->idle_len == (int64_t)(sizeof sq_self->idle_ready / sizeof sq_self->idle_ready[0])) {
		sq_panic_cstr("idle connection queue overflow", __FILE__, __LINE__);
	}
	sq_self->idle_ready[(sq_self->idle_head + sq_self->idle_len) % (int64_t)(sizeof sq_self->idle_ready / sizeof sq_self->idle_ready[0])] = (int32_t)((uint32_t)tag << 24 | (uint32_t)fd);
	sq_self->idle_len++;
	if (sq_self->idle_waiter) {
		sq_g *w = sq_self->idle_waiter;
		sq_self->idle_waiter = NULL;
		fd_event(w, -2);
	}
}

// sq_park_idle moves fd into the idle set with tag (0..255) and marks the calling goroutine's stack
// for release when it exits.
void sq_park_idle(int64_t fd, int64_t tag) {
	sq_g *g = sq_self->cur_g;
	if (tag < 0 || tag > 255) sq_panic_cstr("idle tag out of range", __FILE__, __LINE__);
	if (fd >= 1 << 24) sq_panic_cstr("fd too large for the idle set", __FILE__, __LINE__);
#if SQ_KQUEUE
	if (sq_self->nchanges == (int)(sizeof sq_self->changes / sizeof sq_self->changes[0])) {
		kevent(sq_self->pollfd, sq_self->changes, sq_self->nchanges, NULL, 0, NULL);
		sq_self->nchanges = 0;
	}
	EV_SET(&sq_self->changes[sq_self->nchanges++], (int)fd, EVFILT_READ, EV_ADD | EV_ONESHOT, 0, 0, (void *)(((uintptr_t)tag << 1) | 1));
#else
	struct epoll_event ev = {.events = EPOLLIN | EPOLLONESHOT};
	ev.data.u64 = ((uint64_t)tag << 33) | ((uint64_t)(uint32_t)fd << 1) | 1;
	if (epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, (int)fd, &ev) < 0) epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, (int)fd, &ev);
#endif
	// the goroutine's own registration on fd (if still armed) was just replaced
	if (g->armed && g->wait_fd == (int)fd) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
	g->wait_fd = -1;
	g->release_stack = true;
	sq_self->idle_parked++;
}

// sq_next_idle returns the next idle connection that has become readable, parking until one does,
// as tag<<32 | fd.
int64_t sq_next_idle(void) {
	while (sq_self->idle_len == 0) {
		if (sq_self->idle_waiter) sq_panic_cstr("only one goroutine may wait for idle connections", __FILE__, __LINE__);
		sq_self->idle_waiter = sq_self->cur_g;
		sq_self->cur_g->wait_fd = -2;
		if (!sq_self->cur_g->armed) {
			sq_self->cur_g->armed = true;
			sq_self->fd_waiters++;
		}
		sq_park();
	}
	uint32_t e = (uint32_t)sq_self->idle_ready[sq_self->idle_head];
	int64_t v = (int64_t)(e >> 24) << 32 | (e & 0xffffff);
	sq_self->idle_head = (sq_self->idle_head + 1) % (int64_t)(sizeof sq_self->idle_ready / sizeof sq_self->idle_ready[0]);
	sq_self->idle_len--;
	return v;
}

int64_t sq_idle_count(void) { return sq_self->idle_parked; }

// sq_float64bits is math.Float64bits.
uint64_t sq_float64bits(double f) {
	uint64_t u;
	memcpy(&u, &f, sizeof u);
	return u;
}

// sq_float64frombits is math.Float64frombits.
double sq_float64frombits(uint64_t u) {
	double f;
	memcpy(&f, &u, sizeof f);
	return f;
}

// ---------------------------------------------------------------------------
// system interface used by the standard library (all return -errno on failure)

static int set_nonblock(int fd) {
	int fl = fcntl(fd, F_GETFL);
	return fcntl(fd, F_SETFL, fl | O_NONBLOCK);
}

// A read from a blocking pipe or terminal (stdin, typically) would stop every goroutine, and making
// it non-blocking would change the descriptor for the parent shell too. So for those, wait for
// readability through the poller first, parking only this goroutine. Sockets are non-blocking
// already and regular files are always readable: they are read directly.
// sq_self->fd_read_kind[fd]: 0 unknown, 1 read directly, 2 poll first

static bool read_needs_poll(int fd) {
	if (fd < 0 || fd >= (int)sizeof sq_self->fd_read_kind) return false;
	if (!sq_self->fd_read_kind[fd]) {
		struct stat st;
		int fl = fcntl(fd, F_GETFL);
		bool p = fl >= 0 && !(fl & O_NONBLOCK) && fstat(fd, &st) == 0 && (S_ISFIFO(st.st_mode) || isatty(fd));
		sq_self->fd_read_kind[fd] = p ? 2 : 1;
	}
	return sq_self->fd_read_kind[fd] == 2;
}

int64_t sq_read(int64_t fd, sq_bytes b) {
	if (sq_self->cur_g && read_needs_poll((int)fd)) {
		struct pollfd p = {(int)fd, POLLIN, 0};
		if (poll(&p, 1, 0) == 0) wait_fd((int)fd, 'r');
	}
	for (;;) {
		ssize_t r = read((int)fd, b.p, (size_t)b.len);
		if (r >= 0) return r;
		if (errno == EINTR) continue;
		if (errno == EAGAIN && sq_self->cur_g) {
			wait_fd((int)fd, 'r');
			continue;
		}
		return -errno;
	}
}

static int64_t write_fd(int64_t fd, const uint8_t *p, int64_t n) {
	int64_t done = 0;
	if (fd == 1) sq_flush();
	while (done < n) {
		ssize_t w = write((int)fd, p + done, (size_t)(n - done));
		if (w >= 0) {
			done += w;
			continue;
		}
		if (errno == EINTR) continue;
		if (errno == EAGAIN && sq_self->cur_g) {
			wait_fd((int)fd, 'w');
			continue;
		}
		return done > 0 ? done : -errno;
	}
	return done;
}

int64_t sq_write(int64_t fd, sq_bytes b) { return write_fd(fd, b.p, b.len); }
int64_t sq_write_str(int64_t fd, sq_string s) { return write_fd(fd, s.p, s.len); }

// sq_writev2 writes a then b with as few syscalls as possible (headers + body).
int64_t sq_writev2(int64_t fd, sq_bytes a, sq_bytes b) {
	struct iovec iov[2] = {{a.p, (size_t)a.len}, {b.p, (size_t)b.len}};
	int64_t total = a.len + b.len, done = 0;
	int idx = 0;
	while (done < total) {
		while (idx < 2 && iov[idx].iov_len == 0) idx++;
		ssize_t w = writev((int)fd, iov + idx, 2 - idx);
		if (w < 0) {
			if (errno == EINTR) continue;
			if (errno == EAGAIN && sq_self->cur_g) {
				wait_fd((int)fd, 'w');
				continue;
			}
			return done > 0 ? done : -errno;
		}
		done += w;
		size_t left = (size_t)w;
		while (left > 0 && idx < 2) {
			size_t k = left < iov[idx].iov_len ? left : iov[idx].iov_len;
			iov[idx].iov_base = (uint8_t *)iov[idx].iov_base + k;
			iov[idx].iov_len -= k;
			left -= k;
			if (iov[idx].iov_len == 0) idx++;
		}
	}
	return done;
}

// utc_date fills the date and time fields of tm for Unix time t (UTC, proleptic Gregorian),
// using Howard Hinnant's civil_from_days.
static void utc_date(time_t t, struct tm *tm) {
	int64_t days = (int64_t)t / 86400, secs = (int64_t)t % 86400;
	if (secs < 0) {
		secs += 86400;
		days--;
	}
	tm->tm_hour = (int)(secs / 3600);
	tm->tm_min = (int)(secs / 60 % 60);
	tm->tm_sec = (int)(secs % 60);
	tm->tm_wday = (int)((days % 7 + 11) % 7); // 1970-01-01 was a Thursday
	int64_t z = days + 719468;
	int64_t era = (z >= 0 ? z : z - 146096) / 146097;
	int64_t doe = z - era * 146097;
	int64_t yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
	int64_t doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
	int64_t mp = (5 * doy + 2) / 153;
	int64_t d = doy - (153 * mp + 2) / 5 + 1;
	int64_t m = mp < 10 ? mp + 3 : mp - 9;
	tm->tm_mday = (int)d;
	tm->tm_mon = (int)(m - 1);
	tm->tm_year = (int)(yoe + era * 400 + (m <= 2) - 1900);
}

// sq_httpdate writes an RFC 1123 date ("Mon, 02 Jan 2006 15:04:05 GMT", 29 bytes), cached per second.
int64_t sq_httpdate(sq_bytes dst) {
	time_t now = time(NULL);
	if (now != sq_self->date_cached_at) {
		// UTC by arithmetic, not gmtime_r: on macOS its first call sets up time-zone state and
		// notifications with ~50 heap allocations, and UTC needs none of that
		struct tm tm;
		utc_date(now, &tm);
		static const char days[] = "SunMonTueWedThuFriSat";
		static const char months[] = "JanFebMarAprMayJunJulAugSepOctNovDec";
		char *p = sq_self->date_cache;
		memcpy(p, days + tm.tm_wday * 3, 3); p += 3;
		*p++ = ','; *p++ = ' ';
		*p++ = (char)('0' + tm.tm_mday / 10); *p++ = (char)('0' + tm.tm_mday % 10); *p++ = ' ';
		memcpy(p, months + tm.tm_mon * 3, 3); p += 3; *p++ = ' ';
		int y = tm.tm_year + 1900;
		*p++ = (char)('0' + y / 1000); *p++ = (char)('0' + y / 100 % 10); *p++ = (char)('0' + y / 10 % 10); *p++ = (char)('0' + y % 10); *p++ = ' ';
		*p++ = (char)('0' + tm.tm_hour / 10); *p++ = (char)('0' + tm.tm_hour % 10); *p++ = ':';
		*p++ = (char)('0' + tm.tm_min / 10); *p++ = (char)('0' + tm.tm_min % 10); *p++ = ':';
		*p++ = (char)('0' + tm.tm_sec / 10); *p++ = (char)('0' + tm.tm_sec % 10);
		memcpy(p, " GMT", 4);
		sq_self->date_cached_at = now;
	}
	int64_t n = dst.len < 29 ? dst.len : 29;
	memcpy(dst.p, sq_self->date_cache, (size_t)n);
	return n;
}

static void listener_closed(int fd);
static void start_shards(void);
static void *shard_thread(void *arg);

int64_t sq_close(int64_t fd) {
	if (fd >= 0 && fd < (int64_t)sizeof sq_self->fd_read_kind) sq_self->fd_read_kind[fd] = 0; // the number may be reused
	if (sq_self->nlisteners > 0) listener_closed((int)fd);
	return close((int)fd) < 0 ? -errno : 0;
}

int64_t sq_open(sq_string path, int64_t flags, int64_t mode) {
	char buf[1024];
	if (path.len >= (int64_t)sizeof buf) return -ENAMETOOLONG;
	memcpy(buf, path.p, (size_t)path.len);
	buf[path.len] = 0;
	int f = 0;
	switch (flags & 3) {
	case 0: f = O_RDONLY; break;
	case 1: f = O_WRONLY; break;
	default: f = O_RDWR; break;
	}
	if (flags & 0x40) f |= O_CREAT;
	if (flags & 0x200) f |= O_TRUNC;
	if (flags & 0x400) f |= O_APPEND;
	int fd = open(buf, f | O_CLOEXEC, (int)mode);
	return fd < 0 ? -errno : fd;
}

// parse_addr fills sa from a host (empty: any) and port.
static int parse_addr(sq_string host, int64_t port, struct sockaddr_in *sa) {
	memset(sa, 0, sizeof *sa);
	sa->sin_family = AF_INET;
	sa->sin_port = htons((uint16_t)port);
	if (host.len == 0) {
		sa->sin_addr.s_addr = htonl(INADDR_ANY);
		return 0;
	}
	char hb[64];
	if (host.len >= (int64_t)sizeof hb) return -EINVAL;
	memcpy(hb, host.p, (size_t)host.len);
	hb[host.len] = 0;
	return inet_pton(AF_INET, hb, &sa->sin_addr) == 1 ? 0 : -EINVAL;
}

static int64_t listen_new(const struct sockaddr_in *sa, int64_t backlog, bool reuseport) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return -errno;
	int one = 1;
	setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
	if (reuseport) setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &one, sizeof one);
	if (bind(fd, (const struct sockaddr *)sa, sizeof *sa) < 0 || listen(fd, (int)backlog) < 0) {
		int e = errno;
		close(fd);
		return -e;
	}
	set_nonblock(fd);
	return fd;
}

// Listening when several shards (threads N) or processes (workers N) serve one address. Each
// shard listens on an address once; a second Listen of the same address from the same shard fails
// with "address in use", as it would in a single-threaded program (otherwise, on Linux, the extra
// socket would join the SO_REUSEPORT group and the kernel would route connections to it that
// nobody accepts). Shards of one process share the address: on Linux each has its own socket in
// the group and the kernel spreads connections; macOS doesn't spread them, so there the shards
// share one socket and whichever is free accepts. Port 0 (any free port) is never shared: each
// Listen gets its own port, as in Go.
#define SQ_MAX_LISTEN 64
static pthread_mutex_t listen_mu = PTHREAD_MUTEX_INITIALIZER;
static struct {
	uint32_t addr;
	uint16_t port;      // network order
	int shared_fd;      // macOS: the process's socket for this address (the table's own copy)
	uint64_t shards[SQ_MAX_SHARDS / 64]; // which shards listen on it
} listen_table[SQ_MAX_LISTEN];
static int nlisten;

int64_t sq_listen_tcp(sq_string host, int64_t port, int64_t backlog) {
	struct sockaddr_in sa;
	if (parse_addr(host, port, &sa) < 0) return -EINVAL;
	bool shared = sq_worker_count > 1 || sq_shard_count > 1;
	if (!shared || port == 0) return listen_new(&sa, backlog, false);
	pthread_mutex_lock(&listen_mu);
	int e = -1;
	for (int i = 0; i < nlisten; i++)
		if (listen_table[i].addr == sa.sin_addr.s_addr && listen_table[i].port == sa.sin_port) e = i;
	int64_t me = sq_self->id;
	if (e >= 0 && (listen_table[e].shards[me / 64] >> (me % 64) & 1)) {
		pthread_mutex_unlock(&listen_mu);
		return -EADDRINUSE;
	}
	int64_t fd;
#if SQ_KQUEUE
	if (e >= 0 && sq_shard_count > 1) {
		fd = dup(listen_table[e].shared_fd);
		if (fd < 0) fd = -errno;
	} else
#endif
	fd = listen_new(&sa, backlog, true);
	if (fd >= 0 && e < 0 && nlisten < SQ_MAX_LISTEN) {
		e = nlisten++;
		memset(&listen_table[e], 0, sizeof listen_table[e]);
		listen_table[e].addr = sa.sin_addr.s_addr;
		listen_table[e].port = sa.sin_port;
		listen_table[e].shared_fd = dup((int)fd);
	}
	if (fd >= 0 && e >= 0) {
		listen_table[e].shards[me / 64] |= (uint64_t)1 << (me % 64);
		if (sq_self->nlisteners < (int)(sizeof sq_self->listeners / sizeof sq_self->listeners[0])) {
			sq_self->listeners[sq_self->nlisteners].fd = (int)fd;
			sq_self->listeners[sq_self->nlisteners].entry = e;
			sq_self->nlisteners++;
		}
	}
	pthread_mutex_unlock(&listen_mu);
	return fd;
}

// listener_closed forgets that this shard listens through fd, so it may listen there again.
static void listener_closed(int fd) {
	for (int i = 0; i < sq_self->nlisteners; i++) {
		if (sq_self->listeners[i].fd != fd) continue;
		int e = sq_self->listeners[i].entry;
		int64_t me = sq_self->id;
		pthread_mutex_lock(&listen_mu);
		listen_table[e].shards[me / 64] &= ~((uint64_t)1 << (me % 64));
		pthread_mutex_unlock(&listen_mu);
		sq_self->listeners[i] = sq_self->listeners[--sq_self->nlisteners];
		return;
	}
}

int64_t sq_accept(int64_t lfd) {
	for (;;) {
		int fd = accept((int)lfd, NULL, NULL);
		if (fd >= 0) {
			set_nonblock(fd);
			int one = 1;
			setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
#ifdef SO_NOSIGPIPE
			setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof one);
#endif
			return fd;
		}
		if (errno == EINTR || errno == ECONNABORTED) continue;
		if (errno == EAGAIN && sq_self->cur_g) {
			wait_fd((int)lfd, 'r');
			continue;
		}
		return -errno;
	}
}

int64_t sq_dial_tcp(sq_string host, int64_t port) {
	char hb[64];
	if (host.len >= (int64_t)sizeof hb) return -EINVAL;
	memcpy(hb, host.p, (size_t)host.len);
	hb[host.len] = 0;
	struct sockaddr_in sa;
	memset(&sa, 0, sizeof sa);
	sa.sin_family = AF_INET;
	sa.sin_port = htons((uint16_t)port);
	if (inet_pton(AF_INET, hb, &sa.sin_addr) != 1) return -EINVAL;
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return -errno;
	set_nonblock(fd);
	int one = 1;
	setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
#ifdef SO_NOSIGPIPE
	setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof one);
#endif
	if (connect(fd, (struct sockaddr *)&sa, sizeof sa) < 0) {
		if (errno != EINPROGRESS) {
			int e = errno;
			close(fd);
			return -e;
		}
		wait_fd(fd, 'w');
		int err = 0;
		socklen_t len = sizeof err;
		getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &len);
		if (err) {
			close(fd);
			return -err;
		}
	}
	return fd;
}

static int g_argc;
static char **g_argv;

int64_t sq_argc(void) { return g_argc; }
sq_string sq_argv(int64_t i) {
	if (i < 0 || i >= g_argc) return (sq_string){NULL, 0};
	return (sq_string){(const uint8_t *)g_argv[i], (int64_t)strlen(g_argv[i])};
}

sq_string sq_getenv(sq_string name) {
	char buf[256];
	if (name.len >= (int64_t)sizeof buf) return (sq_string){NULL, 0};
	memcpy(buf, name.p, (size_t)name.len);
	buf[name.len] = 0;
	const char *v = getenv(buf);
	if (!v) return (sq_string){NULL, 0};
	return (sq_string){(const uint8_t *)v, (int64_t)strlen(v)};
}

#if defined(__GLIBC__) && (__GLIBC__ > 2 || (__GLIBC__ == 2 && __GLIBC_MINOR__ >= 32))
const char *strerrordesc_np(int errnum);
#endif

sq_string sq_strerror(int64_t err) {
#if defined(__GLIBC__) && (__GLIBC__ > 2 || (__GLIBC__ == 2 && __GLIBC_MINOR__ >= 32))
	// glibc's strerror looks up a translation, which heap-allocates on every call;
	// strerrordesc_np returns the same untranslated text as a static string
	const char *s = strerrordesc_np((int)err);
	if (!s) s = "unknown error";
#else
	// macOS: a static string for every errno it knows; for others strerror formats "Unknown
	// error: N" into a buffer it allocates on first use
#if defined(ELAST)
	if (err < 0 || err > ELAST) {
		static const char unknown[] = "unknown error";
		return (sq_string){(const uint8_t *)unknown, (int64_t)sizeof unknown - 1};
	}
#endif
	const char *s = strerror((int)err);
#endif
	return (sq_string){(const uint8_t *)s, (int64_t)strlen(s)};
}

int64_t sq_getpid(void) { return (int64_t)getpid(); }

void sq_exit(int64_t code) {
	sq_flush();
	_exit((int)code);
}

// Resource introspection for benchmarks and the stdlib's runtime package.
int64_t sq_maxrss(void) {
	struct rusage ru;
	getrusage(RUSAGE_SELF, &ru);
#if defined(__APPLE__)
	return ru.ru_maxrss; // bytes
#else
	return ru.ru_maxrss * 1024;
#endif
}

int64_t sq_cputime(void) {
	struct rusage ru;
	getrusage(RUSAGE_SELF, &ru);
	return ((int64_t)ru.ru_utime.tv_sec + ru.ru_stime.tv_sec) * 1000000000 +
	       ((int64_t)ru.ru_utime.tv_usec + ru.ru_stime.tv_usec) * 1000;
}

int64_t sq_shard_id(void) { return sq_self->id; }
int64_t sq_shard_total(void) { return sq_shard_count; }

int64_t sq_pool_count(void) { return sq_npools; }
sq_string sq_pool_name(int64_t i) {
	const char *n = sq_self->pools[i]->name;
	return (sq_string){(const uint8_t *)n, (int64_t)strlen(n)};
}
int64_t sq_pool_stat(int64_t i, int64_t which) {
	sq_pool *p = sq_self->pools[i]; // this shard's
	switch (which) {
	case 0: return p->count;
	case 1: return p->stack_size;
	case 2: return p->live;
	case 3: return p->peak;
	case 4: return p->started;
	}
	return 0;
}

// ---------------------------------------------------------------------------
// scheduler

static void schedule(void) {
	int64_t switches = 0;
	for (;;) {
		sq_g *g;
		while ((g = runq_pop())) {
			sq_self->cur_g = g;
			g->state = G_RUNNING;
			TSAN_SWITCH(g->tsan_fiber);
			sq_ctx_switch(&sq_self->sched_ctx, &g->ctx);
			sq_self->cur_g = NULL;
			drain_remote();
			if (sq_self->start_pending) {
				sq_self->start_pending = false;
				start_shards();
			}
			if (g->state == G_DEAD) {
				if (g->pool->index == sq_main_pool.index) {
					sq_flush();
					g_release(g);
					if (stackcheck) stack_report();
					// returning from main ends the program, like Go; with threads N, once every
					// shard's main has returned. Until then this shard keeps running its other
					// goroutines: other shards' connections may be queued on its listening socket.
					if (atomic_fetch_sub(&live_shards, 1) == 1) _exit(0);
					sq_self->main_done = true;
					continue;
				}
				g_release(g);
			}
			if ((++switches & 63) == 0 && (sq_self->fd_waiters > 0 || sq_self->ntimers > 0 || sq_self->idle_parked > 0)) {
				fire_timers();
				if (sq_self->fd_waiters > 0 || sq_self->idle_parked > 0) netpoll(0);
			}
		}
		// nothing runnable: block in the poller until I/O, a timer or another shard wakes us
		drain_remote();
		if (sq_self->runq_head) continue;
		if (sq_self->fd_waiters == 0 && sq_self->ntimers == 0 && sq_self->idle_parked == 0 && sq_self->shared_parked == 0) {
			if (sq_self->main_done) pthread_exit(NULL); // nothing left on this shard
			sq_flush();
			err_write("fatal error: all goroutines are asleep - deadlock!\n");
			_exit(2);
		}
		int64_t timeout = -1;
		if (sq_self->ntimers > 0) {
			timeout = sq_self->timer_heap[0]->wake_at - sq_nanotime();
			if (timeout < 0) timeout = 0;
		}
		if (sq_self->fd_waiters > 0 || sq_self->idle_parked > 0 || sq_self->shared_parked > 0) {
			netpoll(timeout);
		} else if (timeout > 0) {
			struct timespec ts = {timeout / 1000000000, timeout % 1000000000};
			nanosleep(&ts, NULL);
		}
		fire_timers();
	}
}

static void stack_report(void) {
	err_write("stack check: pool, proven bytes, deepest observed bytes\n");
	for (int64_t i = 0; i < sq_npools; i++) {
		sq_pool *p = sq_self->pools[i];
		for (int64_t k = 0; k < p->count; k++) {
			sq_g *g = &p->gs[k];
			if (g->state != G_FREE) {
				int64_t u = stack_used(g);
				if (u > p->observed) p->observed = u;
			}
		}
		err_write("  ");
		err_write(p->name);
		err_write(" ");
		err_int(p->stack_size);
		err_write(" ");
		err_int(p->observed);
		err_write("\n");
	}
}

static void on_term(int sig) {
	(void)sig;
	sq_flush();
	if (stackcheck) stack_report();
	_exit(0);
}

// ---------------------------------------------------------------------------
// workers N: a process per core. The parent forks the workers before any squark code runs, so
// each initializes its own package variables, goroutine pools and connections, then supervises.

#define SQ_MAX_WORKERS 1024
int64_t sq_worker_count = 1; // processes running this program
static pid_t worker_pids[SQ_MAX_WORKERS];
static volatile sig_atomic_t stop_signal;

static void on_stop(int sig) { stop_signal = sig; }

static void err_num(int64_t v) {
	char b[24];
	int n = 0, k = 0;
	char t[24];
	if (v < 0) { b[k++] = '-'; v = -v; }
	do { t[n++] = (char)('0' + v % 10); v /= 10; } while (v);
	while (n) b[k++] = t[--n];
	b[k] = 0;
	err_write(b);
}

static void run_workers(void) {
	int64_t n = sq_workers;
	if (n == 0) { // workers auto
		long c = sysconf(_SC_NPROCESSORS_ONLN);
		n = c > 0 ? c : 1;
	}
	if (n > SQ_MAX_WORKERS) n = SQ_MAX_WORKERS;
	sq_worker_count = n;
	if (n <= 1) return;
	pid_t parent = getpid();
	for (int64_t i = 0; i < n; i++) {
		pid_t pid = fork();
		if (pid == 0) {
#if !SQ_KQUEUE
			prctl(PR_SET_PDEATHSIG, SIGTERM); // the supervisor is gone: stop too (Linux only)
			if (getppid() != parent) _exit(1);
#endif
			return;
		}
		if (pid < 0) {
			err_write("fatal error: cannot start worker processes\n");
			for (int64_t j = 0; j < i; j++) kill(worker_pids[j], SIGTERM);
			_exit(2);
		}
		worker_pids[i] = pid;
	}
	// supervisor: pass stop signals on, wait for every worker, and stop all if one fails
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = on_stop; // no SA_RESTART: waitpid returns EINTR so the signal is forwarded
	sigaction(SIGINT, &sa, NULL);
	sigaction(SIGTERM, &sa, NULL);
	sigaction(SIGHUP, &sa, NULL);
	int64_t alive = n;
	int status = 0;
	int forwarded = 0;
	while (alive > 0) {
		if (stop_signal && stop_signal != forwarded) {
			forwarded = stop_signal;
			for (int64_t i = 0; i < n; i++)
				if (worker_pids[i] > 0) kill(worker_pids[i], forwarded);
		}
		int st;
		pid_t p = waitpid(-1, &st, 0);
		if (p < 0) {
			if (errno == EINTR) continue;
			break;
		}
		int64_t idx = -1;
		for (int64_t i = 0; i < n; i++)
			if (worker_pids[i] == p) idx = i;
		if (idx < 0) continue;
		worker_pids[idx] = 0;
		alive--;
		int code = WIFEXITED(st) ? WEXITSTATUS(st) : 128 + WTERMSIG(st);
		if (code != 0 && status == 0) {
			status = code;
			if (!stop_signal) {
				err_write("squark: worker ");
				err_num(idx);
				err_write(" exited with status ");
				err_num(code);
				err_write("; stopping the other workers\n");
				for (int64_t i = 0; i < n; i++)
					if (worker_pids[i] > 0) kill(worker_pids[i], SIGTERM);
			}
		}
	}
	_exit(status); // the first failing worker's status, or a stop signal's 128+n, or 0
}

// ---------------------------------------------------------------------------
// threads N: a scheduler thread per core, each a shard with its own run queue, poller, timers,
// goroutine pools and package variables. Goroutines never move between shards.

#define SQ_SIGSTACK_SIZE 32768
int64_t sq_shard_count = 1;
// live_shards (declared with sq_shard): shards whose main hasn't returned

static size_t align16(size_t n) { return (n + 15) & ~(size_t)15; }

// shard_new lays out a shard and everything it owns in one mapping: the shard, its copies of the
// pools with their goroutine tables, its timer heap, its package variables and its signal stack.
// Stacks are reserved when the shard starts (pool_init).
static sq_shard *shard_new(void) {
	size_t sz = align16(sizeof(sq_shard)) + align16((size_t)sq_npools * sizeof(sq_pool *)) +
	            align16((size_t)sq_npools * sizeof(sq_pool));
	for (int64_t i = 0; i < sq_npools; i++) sz += align16((size_t)sq_pools[i]->count * sizeof(sq_g));
	sz += align16((size_t)sq_max_goroutines * sizeof(sq_g *)) + align16(sq_globals_size) + SQ_SIGSTACK_SIZE;
	uint8_t *m = mmap(NULL, sz, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	if (m == MAP_FAILED) {
		err_write("fatal error: cannot reserve memory for a scheduler thread\n");
		_exit(2);
	}
	sq_shard *s = (sq_shard *)m;
	m += align16(sizeof(sq_shard));
	sq_pool **pools = (sq_pool **)m;
	m += align16((size_t)sq_npools * sizeof(sq_pool *));
	sq_pool *copies = (sq_pool *)m;
	m += align16((size_t)sq_npools * sizeof(sq_pool));
	for (int64_t i = 0; i < sq_npools; i++) {
		const sq_pool *t = sq_pools[i];
		sq_pool *c = &copies[i];
		c->name = t->name;
		c->count = t->count;
		c->stack_size = t->stack_size;
		c->slot_size = t->slot_size;
		c->index = i;
		c->gs = (sq_g *)m;
		m += align16((size_t)t->count * sizeof(sq_g));
		pools[i] = c;
	}
	s->pools = pools;
	s->timer_heap = (sq_g **)m;
	m += align16((size_t)sq_max_goroutines * sizeof(sq_g *));
	s->globals = m; // zeroed by mmap: Go's zero values
	m += align16(sq_globals_size);
	s->sigstack = m;
	s->sigstack_size = SQ_SIGSTACK_SIZE;
	s->next_gid = 1;
	s->pollfd = -1;
	s->date_cached_at = -1;
	return s;
}

// shard_run runs the current thread's shard: signal stack, poller, pools, then main.
static void shard_run(void) {
#ifdef SQ_TSAN
	sq_self->tsan_sched = __tsan_get_current_fiber();
#endif
	stack_t ss = {.ss_sp = sq_self->sigstack, .ss_size = sq_self->sigstack_size, .ss_flags = 0};
	sigaltstack(&ss, NULL);
#if SQ_KQUEUE
	sq_self->pollfd = kqueue();
	struct kevent wake; // other shards trigger this to hand us goroutines (shared channels)
	EV_SET(&wake, 1, EVFILT_USER, EV_ADD | EV_CLEAR, 0, 0, NULL);
	kevent(sq_self->pollfd, &wake, 1, NULL, 0, NULL);
#else
	sq_self->pollfd = epoll_create1(0);
	sq_self->wake_fd = eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
	struct epoll_event wake = {.events = EPOLLIN | EPOLLET}; // data 0: no goroutine, just a wakeup
	epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, sq_self->wake_fd, &wake);
#endif
	pthread_mutex_init(&sq_self->remote_mu, NULL);
	for (int64_t i = 0; i < sq_npools; i++) pool_init(sq_self->pools[i]);
	sq_go(&sq_main_pool, sq_program_main, NULL, 0);
	schedule();
}

// start_shards creates shards 1..n-1 (on shard 0's scheduler stack).
static void start_shards(void) {
	for (int64_t i = 1; i < sq_shard_count; i++) {
		pthread_t t;
		pthread_attr_t attr;
		pthread_attr_init(&attr);
		pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
		sq_shard *s = shard_new();
		s->id = i;
		if (pthread_create(&t, &attr, shard_thread, s) != 0) {
			err_write("fatal error: cannot start scheduler threads\n");
			_exit(2);
		}
		pthread_attr_destroy(&attr);
	}
}

// sq_start_shards is called by shard 0's main goroutine once package variables and shared vars
// are initialized: everything it wrote is visible to the new threads (pthread_create orders it).
void sq_start_shards(void) {
	if (sq_shard_count <= 1) return;
	sq_self->start_pending = true;
	sq_gosched(); // the scheduler creates the threads on its own stack, then resumes us
}

static void *shard_thread(void *arg) {
	sq_self = arg;
	sq_globals_set(sq_self->globals);
	shard_run();
	return NULL;
}

int main(int argc, char **argv) {
	g_argc = argc;
	g_argv = argv;
	run_workers(); // returns in each worker process (or at once, for a single worker)
	signal(SIGPIPE, SIG_IGN);
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = on_fault;
	sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
	sigaction(SIGSEGV, &sa, NULL);
	sigaction(SIGBUS, &sa, NULL);
	stackcheck = getenv("SQ_STACKCHECK") != NULL;
	if (stackcheck) {
		signal(SIGTERM, on_term);
		signal(SIGINT, on_term);
	}
	int64_t n = sq_threads;
	if (n == 0) { // threads auto
		long c = sysconf(_SC_NPROCESSORS_ONLN);
		n = c > 0 ? c : 1;
	}
	if (n > SQ_MAX_SHARDS) n = SQ_MAX_SHARDS;
	sq_shard_count = n;
	atomic_store(&live_shards, n);
	sq_self->sigstack = sq_sigstack; // shard 0 runs on this thread, with the static copies
	sq_self->sigstack_size = sizeof sq_sigstack;
	shard_run(); // the other shards start once shard 0 has initialized (sq_start_shards)
	return 0;
}
