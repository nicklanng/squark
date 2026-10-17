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
#if defined(__APPLE__)
#include <mach/mach.h> // thread_info: the watchdog reads threads' CPU time
#endif
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
	uint8_t outbuf[8192]; // println output, flushed at newlines, exit and before writes to fd 1
	size_t outlen;
	atomic_flag outlock;  // with several shards: lets an exiting thread write this one's buffer
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
	struct fd_slot *fds;  // per fd: who waits to read and write it (mmap'd, sized by RLIMIT_NOFILE)
	int64_t nfds;
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
	int64_t shared_parked;    // this shard's goroutines waiting for other threads (shared channels, locks, blocking calls)
	sq_g *detach_g;           // a goroutine detaching (sq_detach), handed to a helper once switched out
	uint64_t rand;            // select's random start
	sq_g *preempted;          // the goroutine that just yielded to preemption
	// read by the watchdog thread
	_Atomic int preempt;          // asks the running goroutine to yield (if it is preemptible)
	_Atomic uint64_t switches;    // goroutines run so far
	_Atomic(const char *) running; // the running goroutine's task (NULL: in the scheduler)
	_Atomic bool others;          // other goroutines were waiting when it started
#if defined(__APPLE__)
	mach_port_t mach_thread;      // for the watchdog: this thread's CPU time
#else
	clockid_t cpu_clock;
#endif
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
// Zero-initialized, so it costs no space in the binary (an initializer would store the whole
// struct, buffers included): main sets the few non-zero fields first.
static sq_shard sq_shard0;
static _Thread_local sq_shard *sq_self = &sq_shard0;
static atomic_int_fast64_t live_shards;

static void heap_remove(sq_g *g);
static void heap_up(int64_t i);
static int64_t deadline(int64_t ns);
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

// A shard's output buffer is its own, but a thread that ends the process (os.Exit, a panic, the
// last main returning) writes every shard's buffered partial line first. With several shards the
// buffer is therefore guarded by a spin lock that only the exiting thread ever contends for.
static void out_lock(sq_shard *s) {
	if (sq_shard_count > 1)
		while (atomic_flag_test_and_set_explicit(&s->outlock, memory_order_acquire)) {}
}

static void out_unlock(sq_shard *s) {
	if (sq_shard_count > 1) atomic_flag_clear_explicit(&s->outlock, memory_order_release);
}

static void flush_locked(sq_shard *s) {
	if (s->outlen) write_all(1, s->outbuf, s->outlen);
	s->outlen = 0;
}

void sq_flush(void) {
	if (!sq_self) return; // a helper thread: nothing buffered
	out_lock(sq_self);
	flush_locked(sq_self);
	out_unlock(sq_self);
}

static void out(const void *p, size_t n) {
	out_lock(sq_self);
	if (sq_self->outlen + n > sizeof sq_self->outbuf) flush_locked(sq_self);
	if (n > sizeof sq_self->outbuf) {
		write_all(1, p, n);
	} else {
		memcpy(sq_self->outbuf + sq_self->outlen, p, n);
		sq_self->outlen += n;
	}
	out_unlock(sq_self);
}

static sq_shard *_Atomic all_shards[SQ_MAX_SHARDS]; // (defined with the threads code)

// exit_flush writes every shard's buffered output (this thread's first) before the process exits.
// It may run in a signal handler that interrupted out(), so it never waits long for a lock: a
// buffer it can't lock is skipped. The locks it takes are kept, so nothing is added meanwhile.
static void exit_flush(void) {
	for (int64_t k = -1; k < sq_shard_count; k++) {
		sq_shard *s = k < 0 ? sq_self : atomic_load(&all_shards[k]);
		if (!s || (k >= 0 && s == sq_self)) continue;
		if (sq_shard_count <= 1) {
			if (s == sq_self) flush_locked(s);
			continue;
		}
		bool locked = false;
		for (int i = 0; i < 100 && !(locked = !atomic_flag_test_and_set_explicit(&s->outlock, memory_order_acquire)); i++) {
			struct timespec ts = {0, 10000};
			nanosleep(&ts, NULL);
		}
		if (locked) flush_locked(s);
	}
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
	if (!sq_self) { // a helper thread, in a blocking C call
		exit_flush();
		err_write("\n\nin a blocking C call (helper thread)\nexit status 2\n");
		_exit(2);
	}
	err_write("\n\ngoroutine ");
	if (sq_self->cur_g) {
		err_int(sq_self->cur_g->id);
		err_write(" [");
		err_write(sq_self->cur_g->pool->name);
		err_write("]");
	} else {
		err_write("0 [scheduler]");
	}
	err_write(":\n");
	if (file) { // a fault in C code has no source position
		err_write("\t");
		err_write(file);
		err_write(":");
		err_int(line);
		err_write("\n");
	}
	err_write("exit status 2\n");
	_exit(2);
}

void sq_panic_cstr(const char *msg, const char *file, int line) {
	exit_flush();
	err_write("panic: ");
	err_write(msg);
	die(file, line);
}

void sq_panic_str(sq_string msg, const char *file, int line) {
	exit_flush();
	err_write("panic: ");
	write_all(2, msg.p, (size_t)msg.len);
	die(file, line);
}

void sq_panic_int(int64_t v, const char *file, int line) {
	exit_flush();
	err_write("panic: ");
	err_int(v);
	die(file, line);
}

void sq_panic_index(int64_t i, int64_t n, const char *file, int line) {
	exit_flush();
	err_write("panic: runtime error: index out of range [");
	err_int(i);
	err_write("] with length ");
	err_int(n);
	die(file, line);
}

void sq_panic_slice(int64_t lo, int64_t hi, int64_t max, int64_t cap, const char *file, int line) {
	exit_flush();
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
	exit_flush();
	err_write("panic: runtime error: append exceeds capacity (need ");
	err_int(need);
	err_write(", capacity ");
	err_int(cap);
	err_write("): squark slices never grow onto the heap; append into an arena (append(...) in a) or give the slice more capacity");
	die(file, line);
}

void sq_panic_arena(sq_arena *a, int64_t need, const char *file, int line) {
	exit_flush();
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

void sq_panic_nil(const char *file, int line) {
	sq_panic_cstr("runtime error: invalid memory address or nil pointer dereference", file, line);
}

static uint8_t sq_sigstack[32768]; // not "sigstack": glibc declares a function of that name

static _Thread_local sq_g *helper_g; // a helper thread's detached goroutine (see sq_detach)

static void on_fault(int sig, siginfo_t *si, void *uctx) {
	(void)uctx;
	uintptr_t addr = (uintptr_t)si->si_addr;
	long pagesz = sysconf(_SC_PAGESIZE);
	sq_shard *home = sq_self ? sq_self : helper_g ? helper_g->shard : NULL; // a detached goroutine's
	for (int64_t i = 0; home && i < sq_npools; i++) {
		sq_pool *p = home->pools[i];
		uintptr_t lo = (uintptr_t)p->stacks, hi = lo + (uintptr_t)(p->count * p->slot_size);
		if (addr >= lo && addr < hi) {
			uintptr_t off = (addr - lo) % (uintptr_t)p->slot_size;
			if (off < (uintptr_t)pagesz) {
				exit_flush();
				err_write("fatal error: stack overflow in goroutine [");
				err_write(p->name);
				err_write("] (a C function used more stack than its //squark:stack budget)");
				die(NULL, 0);
			}
		}
	}
	exit_flush();
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

// park_shared parks the running goroutine until another thread wakes it (ready_shared); mu is
// the lock that guards the queue it waits in, released once the goroutine is marked waiting.
static void park_shared(pthread_mutex_t *mu) {
	sq_self->shared_parked++;
	sq_self->cur_g->state = G_WAITING; // before unlocking: a waker may queue it at once
	if (mu) pthread_mutex_unlock(mu);
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&sq_self->cur_g->ctx, &sq_self->sched_ctx);
}

// ---------------------------------------------------------------------------
// blocking calls: helper threads run a blocking extern while its goroutine waits

static pthread_mutex_t helper_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t helper_cv = PTHREAD_COND_INITIALIZER;
static sq_g *helper_head, *helper_tail;

// a helper running a detached goroutine: its own context, and the goroutine
static _Thread_local sq_ctx helper_ctx;
static _Thread_local void *helper_fiber;
static _Thread_local bool on_helper;

static void helper_push(sq_g *g) {
	pthread_mutex_lock(&helper_mu);
	q_push(&helper_head, &helper_tail, g);
	pthread_cond_signal(&helper_cv);
	pthread_mutex_unlock(&helper_mu);
}

static void *helper_thread(void *arg) {
	(void)arg;
	sq_self = NULL; // not a scheduler thread (sq_self starts out as shard 0 on every thread)
	on_helper = true;
#ifdef SQ_TSAN
	helper_fiber = __tsan_get_current_fiber();
#endif
	// a signal stack, so a stack overflow in a detached goroutine reports like any other
	static const size_t sigsz = 64 << 10;
	void *sig = mmap(NULL, sigsz, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	if (sig != MAP_FAILED) {
		stack_t ss = {.ss_sp = sig, .ss_size = sigsz, .ss_flags = 0};
		sigaltstack(&ss, NULL);
	}
	for (;;) {
		pthread_mutex_lock(&helper_mu);
		sq_g *g;
		while (!(g = q_pop(&helper_head, &helper_tail))) pthread_cond_wait(&helper_cv, &helper_mu);
		pthread_mutex_unlock(&helper_mu);
		if (g->blk_fn) {
			g->blk_fn(g->elem); // the goroutine's stack stays put while it waits: its arguments are safe
		} else {
			// a detached goroutine: carry on with its C call here until sq_reattach switches back
			helper_g = g;
			TSAN_SWITCH(g->tsan_fiber);
			sq_ctx_switch(&helper_ctx, &g->ctx);
			helper_g = NULL;
		}
		ready_shared(g);
	}
	return NULL;
}

bool sq_on_helper(void) { return on_helper; }

bool sq_detach(void) {
	if (!sq_self || sq_helpers_per_thread == 0) return false;
	sq_g *g = sq_self->cur_g;
	g->blk_fn = NULL;
	sq_self->detach_g = g; // the scheduler hands g to a helper once its context is saved
	park_shared(NULL);
	// now on a helper thread: nothing thread-local from before the switch is valid here
	return true;
}

// The thread-local accesses are made before the switch: after it, the function runs on the home
// thread, and a TLS address computed earlier would be the helper's.
void sq_reattach(void) {
	if (!on_helper) return;
	sq_g *g = helper_g;
	if (!g) return; // a blocking call's helper, not a detached goroutine
	sq_ctx *back = &helper_ctx;
	TSAN_SWITCH(helper_fiber);
	sq_ctx_switch(&g->ctx, back);
}

void sq_blocking(void (*fn)(void *), void *arg) {
	if (!sq_self) { // already on a helper (a detached goroutine): just call it
		fn(arg);
		return;
	}
	sq_g *g = sq_self->cur_g;
	g->blk_fn = fn;
	g->elem = arg;
	pthread_mutex_lock(&helper_mu);
	q_push(&helper_head, &helper_tail, g);
	pthread_cond_signal(&helper_cv);
	park_shared(&helper_mu); // only this thread's scheduler resumes g, after it has switched out
}

// start_helpers starts the helper threads, with every async signal blocked (the scheduler threads
// handle those) and the proven stack size.
static void start_helpers(int64_t n) {
	sigset_t all, old;
	sigfillset(&all);
	sigdelset(&all, SIGSEGV);
	sigdelset(&all, SIGBUS);
	sigdelset(&all, SIGFPE);
	sigdelset(&all, SIGILL);
	pthread_sigmask(SIG_BLOCK, &all, &old);
	for (int64_t i = 0; i < n; i++) {
		pthread_t t;
		pthread_attr_t attr;
		pthread_attr_init(&attr);
		pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
		long page = sysconf(_SC_PAGESIZE);
		size_t sz = (size_t)((sq_helper_stack + 16384 + page - 1) / page * page);
		if (sz < 65536) sz = 65536; // above PTHREAD_STACK_MIN everywhere
		pthread_attr_setstacksize(&attr, sz);
		if (pthread_create(&t, &attr, helper_thread, NULL) != 0) {
			err_write("fatal error: cannot start helper threads for blocking calls\n");
			_exit(2);
		}
		pthread_attr_destroy(&attr);
	}
	pthread_sigmask(SIG_SETMASK, &old, NULL);
}

// ---------------------------------------------------------------------------
// goroutine locks

// An uncontended lock or unlock is one compare-and-swap; the mutex and queue come in only when
// goroutines wait. A waiter sets contended before its last try, and unlock clears the owner
// before looking at contended (both sequentially consistent), so either the waiter's try
// succeeds or unlock sees it and hands the lock over.
void sq_gmutex_init(sq_gmutex *m) {
	pthread_mutex_init(&m->mu, NULL);
	atomic_init(&m->owner, NULL);
	atomic_init(&m->contended, false);
	m->wait_head = m->wait_tail = NULL;
}

void sq_gmutex_lock(sq_gmutex *m) {
	sq_g *g = sq_self->cur_g, *none = NULL;
	if (atomic_compare_exchange_strong(&m->owner, &none, g)) return;
	pthread_mutex_lock(&m->mu);
	atomic_store(&m->contended, true);
	none = NULL;
	if (atomic_compare_exchange_strong(&m->owner, &none, g)) {
		pthread_mutex_unlock(&m->mu);
		return;
	}
	q_push(&m->wait_head, &m->wait_tail, g);
	park_shared(&m->mu); // the unlocker hands the lock to us before waking us
}

void sq_gmutex_unlock(sq_gmutex *m) {
	atomic_store(&m->owner, NULL);
	if (!atomic_load(&m->contended)) return;
	pthread_mutex_lock(&m->mu);
	sq_g *next = NULL;
	if (m->wait_head) {
		sq_g *none = NULL;
		// else someone took it on the fast path meanwhile; their unlock hands it on
		if (atomic_compare_exchange_strong(&m->owner, &none, m->wait_head)) next = q_pop(&m->wait_head, &m->wait_tail);
	} else {
		atomic_store(&m->contended, false);
	}
	pthread_mutex_unlock(&m->mu);
	if (next) ready_shared(next);
}

// Only the owner can make the lock stop being its own, so a relaxed load answers "is it mine?"
// (the mutex orders everything else).
bool sq_gmutex_held(sq_gmutex *m) {
	return atomic_load_explicit(&m->owner, memory_order_relaxed) == sq_self->cur_g;
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

// ---------------------------------------------------------------------------
// channel operations. A waiting goroutine queues waiters on its own stack: one for a send or
// receive, one per case for select. A select's waiters can be claimed once (its fired index, by
// compare-and-swap), so whichever operation, close or timer gets there first completes the
// select; the others drop its waiters, and the selecting goroutine removes the rest when it wakes.
// Shared channels (shared var ... chan) are locked around every step; local ones belong to one
// scheduler thread and need no lock.

typedef struct sq_sel {
	_Atomic int64_t fired; // -1 until a case fires
} sq_sel;

static void chan_lock(sq_chan *c) {
	if (c->shared) pthread_mutex_lock(&c->mu);
}

static void chan_unlock(sq_chan *c) {
	if (c->shared) pthread_mutex_unlock(&c->mu);
}

static void wq_push(sq_waiter **head, sq_waiter **tail, sq_waiter *w) {
	w->next = NULL;
	if (*tail) (*tail)->next = w; else *head = w;
	*tail = w;
}

// claim reports whether w's operation may complete now (claiming its select if it has one).
static bool claim(sq_waiter *w) {
	if (!w->sel) return true;
	int64_t none = -1;
	return atomic_compare_exchange_strong(&((sq_sel *)w->sel)->fired, &none, w->index);
}

// wq_take pops the first waiter that can still complete; waiters of selects that already fired
// are dropped.
static sq_waiter *wq_take(sq_waiter **head, sq_waiter **tail) {
	sq_waiter *w;
	while ((w = *head)) {
		*head = w->next;
		if (!*head) *tail = NULL;
		w->next = NULL;
		if (claim(w)) return w;
	}
	return NULL;
}

static void wq_remove(sq_waiter **head, sq_waiter **tail, sq_waiter *w) {
	sq_waiter *prev = NULL;
	for (sq_waiter *p = *head; p; prev = p, p = p->next) {
		if (p != w) continue;
		if (prev) prev->next = p->next; else *head = p->next;
		if (*tail == p) *tail = prev;
		return;
	}
}

// wake makes a waiter's goroutine runnable once its operation is done. Called without channel
// locks; w is read before the goroutine can run again (only one waker claims it).
static void wake(sq_waiter *w) {
	sq_g *g = w->g;
	if (w->remote) {
		ready_shared(g);
		return;
	}
	sq_self->total_parked--;
	sq_ready(g);
}

// chan_try_send completes a send if it can without waiting; *wk is then a waiter to wake.
static bool chan_try_send(sq_chan *c, const void *elem, sq_waiter **wk) {
	sq_waiter *r = wq_take(&c->recvq_head, &c->recvq_tail);
	if (r) {
		memcpy(r->elem, elem, (size_t)c->elemsize);
		r->ok = true;
		*wk = r;
		return true;
	}
	if (c->len < c->cap) {
		int64_t tail = (c->head + c->len) % c->cap;
		memcpy(c->buf + tail * c->elemsize, elem, (size_t)c->elemsize);
		c->len++;
		return true;
	}
	return false;
}

// chan_try_recv completes a receive if it can without waiting (a closed, empty channel gives the
// zero value and *ok false); *wk is then a waiter to wake.
static bool chan_try_recv(sq_chan *c, void *out, bool *ok, sq_waiter **wk) {
	if (c->len > 0) {
		memcpy(out, c->buf + c->head * c->elemsize, (size_t)c->elemsize);
		c->head = (c->head + 1) % c->cap;
		c->len--;
		sq_waiter *s = wq_take(&c->sendq_head, &c->sendq_tail);
		if (s) { // a waiting sender's value takes the freed slot
			int64_t tail = (c->head + c->len) % c->cap;
			memcpy(c->buf + tail * c->elemsize, s->elem, (size_t)c->elemsize);
			c->len++;
			s->ok = true;
			*wk = s;
		}
		*ok = true;
		return true;
	}
	sq_waiter *s = wq_take(&c->sendq_head, &c->sendq_tail);
	if (s) {
		memcpy(out, s->elem, (size_t)c->elemsize);
		s->ok = true;
		*wk = s;
		*ok = true;
		return true;
	}
	if (c->closed) {
		memset(out, 0, (size_t)c->elemsize);
		*ok = false;
		return true;
	}
	return false;
}

// chan_remote reports whether waiters on c may be woken by another scheduler thread. With one
// thread a shared channel is a local one (under its lock): a goroutine blocked on it counts as
// parked, so a deadlock is reported instead of the thread sleeping forever.
static bool chan_remote(sq_chan *c) { return c->shared && sq_shard_count > 1; }

// chan_park parks the running goroutine after queueing w on c (c's lock held if shared).
static void chan_park(sq_chan *c) {
	if (chan_remote(c)) {
		park_shared(&c->mu);
		return;
	}
	sq_self->total_parked++;
	chan_unlock(c);
	sq_park();
}

static SQ_NORETURN void park_forever(void) { // a nil channel
	sq_self->total_parked++;
	sq_park();
	__builtin_unreachable();
}

void sq_chan_send(sq_chan *c, const void *elem, const char *file, int line) {
	if (!c) park_forever();
	chan_lock(c);
	if (c->closed) {
		chan_unlock(c);
		sq_panic_cstr("send on closed channel", file, line);
	}
	sq_waiter *wk = NULL;
	if (chan_try_send(c, elem, &wk)) {
		chan_unlock(c);
		if (wk) wake(wk);
		return;
	}
	sq_waiter w = {.g = sq_self->cur_g, .elem = (void *)elem, .remote = chan_remote(c)};
	wq_push(&c->sendq_head, &c->sendq_tail, &w);
	chan_park(c);
	if (!w.ok) sq_panic_cstr("send on closed channel", file, line);
}

bool sq_chan_recv(sq_chan *c, void *outp) {
	if (!c) park_forever();
	chan_lock(c);
	bool ok;
	sq_waiter *wk = NULL;
	if (chan_try_recv(c, outp, &ok, &wk)) {
		chan_unlock(c);
		if (wk) wake(wk);
		return ok;
	}
	sq_waiter w = {.g = sq_self->cur_g, .elem = outp, .remote = chan_remote(c)};
	wq_push(&c->recvq_head, &c->recvq_tail, &w);
	chan_park(c);
	if (!w.ok) memset(outp, 0, (size_t)c->elemsize);
	return w.ok;
}

void sq_chan_close(sq_chan *c, const char *file, int line) {
	if (!c) sq_panic_cstr("close of nil channel", file, line);
	chan_lock(c);
	if (c->closed) {
		chan_unlock(c);
		sq_panic_cstr("close of closed channel", file, line);
	}
	c->closed = true;
	sq_waiter *woken = NULL, *w;
	while ((w = wq_take(&c->recvq_head, &c->recvq_tail)) || (w = wq_take(&c->sendq_head, &c->sendq_tail))) {
		w->ok = false;
		w->next = woken;
		woken = w;
	}
	chan_unlock(c);
	while (woken) {
		w = woken;
		woken = w->next; // before waking: w is on the waiter's stack
		wake(w);
	}
}

static uint64_t sel_rand(void) { // xorshift, per shard: select's fairness
	uint64_t x = sq_self->rand;
	if (!x) x = 0x9e3779b97f4a7c15ull ^ (uint64_t)sq_nanotime() ^ ((uint64_t)sq_self->id << 32);
	x ^= x << 13;
	x ^= x >> 7;
	x ^= x << 17;
	sq_self->rand = x;
	return x;
}

int64_t sq_select(sq_scase *cs, sq_waiter *ws, int64_t n, bool block, int64_t timeout_ns, const char *file, int line) {
	// lock the shared channels once each, in address order (another select may lock them too)
	int64_t nl = 0;
	for (int64_t i = 0; i < n; i++) {
		sq_chan *c = cs[i].c;
		if (!c || !c->shared) continue;
		int64_t k = nl++;
		while (k > 0 && ws[k - 1].lock > c) {
			ws[k].lock = ws[k - 1].lock;
			k--;
		}
		ws[k].lock = c;
	}
	for (int64_t k = 0; k < nl; k++) {
		if (k == 0 || ws[k].lock != ws[k - 1].lock) pthread_mutex_lock(&ws[k].lock->mu);
	}
#define UNLOCK_ALL() \
	for (int64_t k = 0; k < nl; k++) \
		if (k == 0 || ws[k].lock != ws[k - 1].lock) pthread_mutex_unlock(&ws[k].lock->mu)

	// a case that is ready now, starting from a random one
	int64_t start = n > 0 ? (int64_t)(sel_rand() % (uint64_t)n) : 0;
	for (int64_t j = 0; j < n; j++) {
		int64_t i = (start + j) % n;
		sq_chan *c = cs[i].c;
		if (!c) continue; // a nil channel is never ready
		sq_waiter *wk = NULL;
		if (cs[i].send) {
			if (c->closed) {
				UNLOCK_ALL();
				sq_panic_cstr("send on closed channel", file, line);
			}
			if (!chan_try_send(c, cs[i].elem, &wk)) continue;
		} else if (!chan_try_recv(c, cs[i].elem, &cs[i].ok, &wk)) {
			continue;
		}
		UNLOCK_ALL();
		if (wk) wake(wk);
		return i;
	}
	if (!block) {
		UNLOCK_ALL();
		return -1;
	}

	// wait on every case (and the timer)
	sq_g *g = sq_self->cur_g;
	sq_sel sel;
	atomic_init(&sel.fired, -1);
	bool remote = nl > 0 && sq_shard_count > 1; // see chan_remote
	for (int64_t i = 0; i < n; i++) {
		sq_chan *c = cs[i].c;
		if (!c) continue;
		sq_chan *lk = ws[i].lock;
		ws[i] = (sq_waiter){.g = g, .elem = cs[i].elem, .sel = &sel, .index = i, .remote = remote, .lock = lk};
		if (cs[i].send) wq_push(&c->sendq_head, &c->sendq_tail, &ws[i]);
		else wq_push(&c->recvq_head, &c->recvq_tail, &ws[i]);
	}
	sq_waiter tw = {.g = g, .sel = &sel, .index = n, .remote = remote};
	if (timeout_ns >= 0) {
		g->sel_timer = &tw;
		g->wait_fd = -1;
		g->wake_at = deadline(timeout_ns);
		g->heap_index = sq_self->ntimers;
		sq_self->timer_heap[sq_self->ntimers++] = g;
		heap_up(g->heap_index);
	}
	if (remote) {
		sq_self->shared_parked++;
		g->state = G_WAITING; // before unlocking: a waker may queue it at once
		UNLOCK_ALL();
		TSAN_SWITCH(sq_self->tsan_sched);
		sq_ctx_switch(&g->ctx, &sq_self->sched_ctx);
	} else {
		sq_self->total_parked++;
		UNLOCK_ALL();
		sq_park();
	}
#undef UNLOCK_ALL

	// woken: one case fired; take the other waiters off their channels
	int64_t fired = atomic_load(&sel.fired);
	g->sel_timer = NULL;
	heap_remove(g);
	for (int64_t i = 0; i < n; i++) {
		sq_chan *c = cs[i].c;
		if (!c || i == fired) continue;
		chan_lock(c);
		if (cs[i].send) wq_remove(&c->sendq_head, &c->sendq_tail, &ws[i]);
		else wq_remove(&c->recvq_head, &c->recvq_tail, &ws[i]);
		chan_unlock(c);
	}
	if (fired == n) return n; // the timeout
	if (cs[fired].send) {
		if (!ws[fired].ok) sq_panic_cstr("send on closed channel", file, line);
	} else {
		cs[fired].ok = ws[fired].ok;
		if (!ws[fired].ok) memset(cs[fired].elem, 0, (size_t)cs[fired].c->elemsize);
	}
	return fired;
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
	if (!m) sq_panic_cstr("assignment to entry in nil map (a map needs storage: declare it V[K; N], or make it in an arena)", file, line);
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
#ifdef __APPLE__
	// macOS's CLOCK_MONOTONIC counts whole microseconds; the raw clock (mach_continuous_time, which
	// also runs during sleep) counts nanoseconds
	clock_gettime(CLOCK_MONOTONIC_RAW, &ts);
#else
	clock_gettime(CLOCK_MONOTONIC, &ts);
#endif
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

int64_t sq_unixnano(void) {
	struct timespec ts;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (int64_t)ts.tv_sec * 1000000000 + ts.tv_nsec;
}

// deadline is the monotonic time ns from now, saturating: a huge duration (time.Duration(math.MaxInt64))
// means "never", not a time in the past.
static int64_t deadline(int64_t ns) {
	int64_t now = sq_nanotime();
	return ns > INT64_MAX - now ? INT64_MAX : now + ns;
}

void sq_sleep(int64_t ns) {
	if (ns <= 0) {
		sq_gosched();
		return;
	}
	sq_g *g = sq_self->cur_g;
	g->wait_fd = -1;
	g->wake_at = deadline(ns);
	g->heap_index = sq_self->ntimers;
	sq_self->timer_heap[sq_self->ntimers++] = g;
	heap_up(g->heap_index);
	sq_park();
}


// The fd table records, per descriptor, the goroutine waiting to read it and the one waiting to
// write it, so that sq_close can wake them, and (epoll) one registration can serve both. It is
// mmap'd per shard, sized by the open-file limit; only the pages of descriptors in use are touched.
typedef struct fd_slot {
	sq_g *rd, *wr;
	uint8_t read_kind; // see read_needs_poll
} fd_slot;

static void fd_table_init(void) {
	struct rlimit rl;
	int64_t n = 1024;
	if (getrlimit(RLIMIT_NOFILE, &rl) == 0) {
		if (rl.rlim_cur == RLIM_INFINITY || rl.rlim_cur > (rlim_t)(1 << 20)) n = 1 << 20;
		else if ((int64_t)rl.rlim_cur > n) n = (int64_t)rl.rlim_cur;
	}
	void *m = mmap(NULL, (size_t)n * sizeof(fd_slot), PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANON, -1, 0);
	sq_self->fds = m == MAP_FAILED ? NULL : m;
	sq_self->nfds = m == MAP_FAILED ? 0 : n;
}

static fd_slot *slot(int fd) { return fd >= 0 && fd < sq_self->nfds ? &sq_self->fds[fd] : NULL; }

#if !SQ_KQUEUE
// epoll_arm registers fd for whichever directions have waiters (one registration per fd). Its
// data is fd<<2 | 2: neither a goroutine (aligned) nor an idle-set entry (bit 0).
static void epoll_arm(int fd, fd_slot *s) {
	struct epoll_event ev = {.events = (s->rd ? EPOLLIN : 0) | (s->wr ? EPOLLOUT : 0) | EPOLLONESHOT};
	ev.data.u64 = ((uint64_t)(uint32_t)fd << 2) | 2;
	if (epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, fd, &ev) < 0) epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, fd, &ev);
}
#endif

static void idle_event(int fd, int tag);
static void fd_event(sq_g *g, int fd);

#if SQ_KQUEUE
// kq_event delivers one kqueue event (or a failed registration's EV_ERROR, which wakes its waiter
// so that its syscall reports the error).
static void kq_event(const struct kevent *ev) {
	if ((uintptr_t)ev->udata & 1) { // idle set: udata is tag<<1 | 1 (goroutines are aligned)
		idle_event((int)ev->ident, (int)((uintptr_t)ev->udata >> 1));
		return;
	}
	fd_event(ev->udata, (int)ev->ident); // NULL udata: a wakeup or a withdrawn registration
}

// kq_flush sends the batched registrations without collecting events. Each change asks for a
// receipt (EV_RECEIPT), so every change is applied: without room for an error in the event list,
// kevent would stop at a failing change and drop the ones after it.
static void kq_flush(void) {
	struct kevent res[16];
	struct timespec zero = {0, 0};
	for (int i = 0; i < sq_self->nchanges; i += 16) {
		int k = sq_self->nchanges - i < 16 ? sq_self->nchanges - i : 16;
		for (int j = 0; j < k; j++) sq_self->changes[i + j].flags |= EV_RECEIPT;
		int n = kevent(sq_self->pollfd, sq_self->changes + i, k, res, k, &zero);
		for (int j = 0; j < n; j++)
			if (!(res[j].flags & EV_ERROR) || res[j].data != 0) kq_event(&res[j]); // not a plain receipt
	}
	sq_self->nchanges = 0;
}

// kq_change queues a registration change for the next kevent call.
static void kq_change(int fd, int16_t filter, uint16_t flags, void *udata) {
	if (sq_self->nchanges == (int)(sizeof sq_self->changes / sizeof sq_self->changes[0])) kq_flush();
	EV_SET(&sq_self->changes[sq_self->nchanges++], fd, filter, flags, 0, 0, udata);
}
#endif

// arm_fd registers the current goroutine to be woken when fd is readable (mode 'r') or writable
// ('w').
static void arm_fd(int fd, int mode) {
	sq_g *g = sq_self->cur_g;
	g->wait_fd = fd;
	g->wake = WAKE_NONE;
	fd_slot *s = slot(fd);
	if (s) {
		if (mode == 'r') s->rd = g;
		else s->wr = g;
	}
#if SQ_KQUEUE
	kq_change(fd, mode == 'r' ? EVFILT_READ : EVFILT_WRITE, EV_ADD | EV_ONESHOT, sq_self->cur_g);
#else
	if (s) {
		epoll_arm(fd, s);
	} else { // beyond the table: one waiter per fd
		struct epoll_event ev = {.events = (mode == 'r' ? EPOLLIN : EPOLLOUT) | EPOLLONESHOT, .data.ptr = sq_self->cur_g};
		if (epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, fd, &ev) < 0) epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, fd, &ev);
	}
#endif
	if (!g->armed) {
		g->armed = true;
		sq_self->fd_waiters++;
	}
}

// wait_fd parks the current goroutine until fd is readable or writable. It returns false if fd
// was closed meanwhile (by another goroutine): the caller must not use the descriptor number again,
// which may already name another file.
static bool wait_fd(int fd, int mode) {
	arm_fd(fd, mode);
	sq_park();
	return sq_self->cur_g->wake != WAKE_CLOSED;
}

// fd_event delivers a readiness event; stale events (for a goroutine no longer waiting on that
// fd) are dropped. A stale event must leave g's armed count alone: it belongs to g's current wait
// (another fd, say), and uncounting it would stop the scheduler polling for it. Stale events come
// from a kqueue registration that fails because the fd was closed before it was sent, and from
// an event that arrives after its goroutine was woken some other way (a timer).
static void fd_event(sq_g *g, int fd) {
	if (!g) return;
	fd_slot *s = slot(fd);
	if (s) {
		if (s->rd == g) s->rd = NULL;
		if (s->wr == g) s->wr = NULL;
	}
	if (g->state != G_WAITING || g->wait_fd != fd) return;
	if (g->armed) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
	g->wait_fd = -1;
	g->wake = WAKE_FD;
	heap_remove(g);
	sq_ready(g);
}

#if !SQ_KQUEUE
// fd_ready delivers an epoll event for a table fd to its reader and/or writer, and re-arms the
// registration for a waiter whose direction didn't fire (EPOLLONESHOT disarmed it).
static void fd_ready(int fd, uint32_t events) {
	fd_slot *s = slot(fd);
	if (!s) return;
	if (s->rd && (events & (EPOLLIN | EPOLLERR | EPOLLHUP | EPOLLRDHUP))) fd_event(s->rd, fd);
	if (s->wr && (events & (EPOLLOUT | EPOLLERR | EPOLLHUP))) fd_event(s->wr, fd);
	if (s->rd || s->wr) epoll_arm(fd, s);
}
#endif

// fd_closed wakes the goroutines waiting on fd, which another goroutine of this shard just closed;
// their wait_fd returns false.
static void fd_closed(int fd) {
	fd_slot *s = slot(fd);
	if (!s) return;
	s->read_kind = 0; // the number may be reused
	sq_g *ws[2] = {s->rd, s->wr};
	s->rd = s->wr = NULL;
	for (int i = 0; i < 2; i++) {
		sq_g *g = ws[i];
		if (!g || g->state != G_WAITING || g->wait_fd != fd) continue;
		if (g->armed) {
			g->armed = false;
			sq_self->fd_waiters--;
		}
		g->wait_fd = -1;
		g->wake = WAKE_CLOSED;
		heap_remove(g);
		sq_ready(g);
	}
}

static void netpoll(int64_t timeout_ns) {
#if SQ_KQUEUE
	struct kevent events[256];
	struct timespec ts, *tsp = NULL;
	if (timeout_ns >= 0) {
		ts.tv_sec = timeout_ns / 1000000000;
		ts.tv_nsec = timeout_ns % 1000000000;
		tsp = &ts;
	}
	// the event list has room for an error from every change (changes[] is no longer), so a
	// failing change doesn't stop the others from being applied
	int n = kevent(sq_self->pollfd, sq_self->changes, sq_self->nchanges, events, 256, tsp);
	sq_self->nchanges = 0;
	for (int i = 0; i < n; i++) kq_event(&events[i]);
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
		if (events[i].data.u64 & 2) { // a table fd: its reader and/or writer
			fd_ready((int)(events[i].data.u64 >> 2), events[i].events);
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
		if (g->sel_timer) { // a select's timeout: it fires unless a case already did
			sq_waiter *w = g->sel_timer;
			if (claim(w)) wake(w);
			continue;
		}
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
// On timeout the read registration is withdrawn; the caller either waits again or hands the fd to
// the idle set with sq_park_idle, both of which register it anew.
int64_t sq_wait_idle(int64_t fd, int64_t grace_ns) {
	sq_g *g = sq_self->cur_g;
	arm_fd((int)fd, 'r');
	g->wake_at = deadline(grace_ns);
	g->heap_index = sq_self->ntimers;
	sq_self->timer_heap[sq_self->ntimers++] = g;
	heap_up(g->heap_index);
	sq_park();
	if (g->wake == WAKE_FD || g->wake == WAKE_CLOSED) return 0; // closed: the read reports it
	// Timed out. Left armed, the registration would count g as waiting on fd after it moves on
	// (a goroutine blocked on a channel would then hang instead of being reported as a deadlock),
	// and its late event would be stale.
	fd_slot *s = slot((int)fd);
	bool mine = !s || s->rd == g; // else another goroutine has registered to read fd since
	if (s && s->rd == g) s->rd = NULL;
#if SQ_KQUEUE
	if (mine) kq_change((int)fd, EVFILT_READ, EV_DELETE, NULL); // fails harmlessly if it fired
#else
	if (s && (s->rd || s->wr)) {
		epoll_arm((int)fd, s); // keep the other direction's registration (another goroutine's)
	} else if (mine) {
		struct epoll_event ev = {.events = 0};
		epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, (int)fd, &ev);
	}
#endif
	if (g->armed) {
		g->armed = false;
		sq_self->fd_waiters--;
	}
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
	kq_change((int)fd, EVFILT_READ, EV_ADD | EV_ONESHOT, (void *)(((uintptr_t)tag << 1) | 1));
#else
	struct epoll_event ev = {.events = EPOLLIN | EPOLLONESHOT};
	ev.data.u64 = ((uint64_t)tag << 33) | ((uint64_t)(uint32_t)fd << 1) | 1;
	if (epoll_ctl(sq_self->pollfd, EPOLL_CTL_MOD, (int)fd, &ev) < 0) epoll_ctl(sq_self->pollfd, EPOLL_CTL_ADD, (int)fd, &ev);
#endif
	// the goroutine's own registration on fd (if still armed) was just replaced
	fd_slot *s = slot((int)fd);
	if (s && s->rd == g) s->rd = NULL;
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
	fd_slot *s = slot(fd);
	if (!s) return false;
	if (!s->read_kind) {
		struct stat st;
		int fl = fcntl(fd, F_GETFL);
		bool p = fl >= 0 && !(fl & O_NONBLOCK) && fstat(fd, &st) == 0 && (S_ISFIFO(st.st_mode) || isatty(fd));
		s->read_kind = p ? 2 : 1;
	}
	return s->read_kind == 2;
}

int64_t sq_read(int64_t fd, sq_bytes b) {
	if (sq_self->cur_g && read_needs_poll((int)fd)) {
		struct pollfd p = {(int)fd, POLLIN, 0};
		if (poll(&p, 1, 0) == 0 && !wait_fd((int)fd, 'r')) return -EBADF;
	}
	for (;;) {
		ssize_t r = read((int)fd, b.p, (size_t)b.len);
		if (r >= 0) return r;
		if (errno == EINTR) continue;
		if (errno == EAGAIN && sq_self->cur_g) {
			if (!wait_fd((int)fd, 'r')) return -EBADF;
			continue;
		}
		return -errno;
	}
}

// sq_read_timeout is sq_read that waits at most timeout_ns for input: it returns -SQ_ETIMEOUT if
// none arrived by then. (SQ_ETIMEOUT is above every errno, so the caller can tell it apart.)
#define SQ_ETIMEOUT 65536
int64_t sq_read_timeout(int64_t fd, sq_bytes b, int64_t timeout_ns) {
	int64_t deadline = sq_nanotime() + timeout_ns;
	if (sq_self->cur_g && read_needs_poll((int)fd)) {
		struct pollfd p = {(int)fd, POLLIN, 0};
		if (poll(&p, 1, 0) == 0 && (timeout_ns <= 0 || sq_wait_idle(fd, timeout_ns))) return -SQ_ETIMEOUT;
	}
	for (;;) {
		ssize_t r = read((int)fd, b.p, (size_t)b.len);
		if (r >= 0) return r;
		if (errno == EINTR) continue;
		if (errno == EAGAIN && sq_self->cur_g) {
			int64_t left = deadline - sq_nanotime();
			if (left <= 0 || sq_wait_idle(fd, left)) return -SQ_ETIMEOUT;
			continue; // readable, or closed meanwhile: the read reports which
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
			if (!wait_fd((int)fd, 'w')) return done > 0 ? done : -EBADF;
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
				if (!wait_fd((int)fd, 'w')) return done > 0 ? done : -EBADF;
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
	fd_closed((int)fd); // wakes this shard's goroutines waiting on it
	if (sq_self->nlisteners > 0) listener_closed((int)fd);
	return close((int)fd) < 0 ? -errno : 0;
}

int64_t sq_open(sq_string path, int64_t flags, int64_t mode) {
	char buf[1024];
	if (path.len >= (int64_t)sizeof buf) return -ENAMETOOLONG;
	if (path.len > 0 && memchr(path.p, 0, (size_t)path.len)) return -EINVAL; // C would stop at the NUL: a different file
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
	int shared_fd;      // macOS with threads: the shards' socket for this address (the table's own copy), or -1
	uint64_t shards[SQ_MAX_SHARDS / 64]; // which shards listen on it
} listen_table[SQ_MAX_LISTEN];
static int nlisten;

// sq_close_write shuts down the sending side of a connection (the peer reads EOF).
int64_t sq_close_write(int64_t fd) { return shutdown((int)fd, SHUT_WR) < 0 ? -errno : 0; }

// sq_local_port returns the port a socket is bound to (e.g. after listening on port 0).
int64_t sq_local_port(int64_t fd) {
	struct sockaddr_in sa;
	socklen_t n = sizeof sa;
	if (getsockname((int)fd, (struct sockaddr *)&sa, &n) < 0) return -errno;
	return ntohs(sa.sin_port);
}

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
	if (e >= 0 && listen_table[e].shared_fd >= 0) {
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
		listen_table[e].shared_fd = -1;
	}
#if SQ_KQUEUE
	// the table keeps its own copy for the other shards, closed when no shard listens any more
	if (fd >= 0 && e >= 0 && sq_shard_count > 1 && listen_table[e].shared_fd < 0) {
		listen_table[e].shared_fd = fcntl((int)fd, F_DUPFD_CLOEXEC, 0);
	}
#endif
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

// listener_closed forgets that this shard listens through fd, so it may listen there again. When
// no shard listens on the address any more, the table's copy of the socket is closed too, or the
// address would go on accepting connections nobody takes.
static void listener_closed(int fd) {
	for (int i = 0; i < sq_self->nlisteners; i++) {
		if (sq_self->listeners[i].fd != fd) continue;
		int e = sq_self->listeners[i].entry;
		int64_t me = sq_self->id;
		pthread_mutex_lock(&listen_mu);
		listen_table[e].shards[me / 64] &= ~((uint64_t)1 << (me % 64));
		bool any = false;
		for (size_t k = 0; k < sizeof listen_table[e].shards / sizeof listen_table[e].shards[0]; k++)
			if (listen_table[e].shards[k]) any = true;
		if (!any && listen_table[e].shared_fd >= 0) {
			close(listen_table[e].shared_fd);
			listen_table[e].shared_fd = -1;
		}
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
			if (!wait_fd((int)lfd, 'r')) return -EBADF; // the listener was closed
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
		(void)wait_fd(fd, 'w'); // fd is new and private: nothing else can close it
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
	exit_flush();
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

#define SQ_MAX_SLEEP_NS (3600 * (int64_t)1000000000) // an hour

static void schedule(void) {
	int64_t switches = 0;
	for (;;) {
		sq_g *g;
		while ((g = runq_pop())) {
			sq_self->cur_g = g;
			g->state = G_RUNNING;
			atomic_store_explicit(&sq_self->preempt, 0, memory_order_relaxed);
			atomic_store_explicit(&sq_self->switches, (uint64_t)++switches, memory_order_relaxed);
			atomic_store_explicit(&sq_self->others, sq_self->runq_head || sq_self->fd_waiters > 0 || sq_self->ntimers > 0 || sq_self->idle_parked > 0, memory_order_relaxed);
			atomic_store_explicit(&sq_self->running, g->pool->name, memory_order_relaxed);
			TSAN_SWITCH(g->tsan_fiber);
			sq_ctx_switch(&sq_self->sched_ctx, &g->ctx);
			atomic_store_explicit(&sq_self->running, NULL, memory_order_relaxed);
			sq_self->cur_g = NULL;
			if (sq_self->detach_g) { // g detached: its context is saved now, so a helper may resume it
				helper_push(sq_self->detach_g);
				sq_self->detach_g = NULL;
			}
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
					if (atomic_fetch_sub(&live_shards, 1) == 1) {
						exit_flush(); // other shards' goroutines may have printed since
						_exit(0);
					}
					sq_self->main_done = true;
					continue;
				}
				g_release(g);
			}
			if (g == sq_self->preempted) {
				// preempted: whatever became ready while it ran goes first, then it
				sq_self->preempted = NULL;
				fire_timers();
				if (sq_self->fd_waiters > 0 || sq_self->idle_parked > 0) netpoll(0);
				runq_push(g);
			} else if ((switches & 63) == 0 && (sq_self->fd_waiters > 0 || sq_self->ntimers > 0 || sq_self->idle_parked > 0)) {
				fire_timers();
				if (sq_self->fd_waiters > 0 || sq_self->idle_parked > 0) netpoll(0);
			}
		}
		// nothing runnable: block in the poller until I/O, a timer or another shard wakes us
		drain_remote();
		if (sq_self->runq_head) continue;
		if (sq_self->fd_waiters == 0 && sq_self->ntimers == 0 && sq_self->idle_parked == 0 && sq_self->shared_parked == 0) {
			if (sq_self->main_done) pthread_exit(NULL); // nothing left on this shard
			exit_flush();
			err_write("fatal error: all goroutines are asleep - deadlock!\n");
			_exit(2);
		}
		int64_t timeout = -1;
		if (sq_self->ntimers > 0) {
			timeout = sq_self->timer_heap[0]->wake_at - sq_nanotime();
			if (timeout < 0) timeout = 0;
			// a far-off timer: wake up now and then and look again (the pollers' timeouts are
			// limited: epoll takes an int of milliseconds)
			if (timeout > SQ_MAX_SLEEP_NS) timeout = SQ_MAX_SLEEP_NS;
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
	exit_flush();
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
static void on_child(int sig) { (void)sig; } // only interrupts sigsuspend

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
	// supervisor: pass stop signals on, wait for every worker, and stop all if one fails. The
	// signals are blocked except inside sigsuspend, so one that arrives between looking at
	// stop_signal and waiting isn't left unforwarded until a worker happens to exit.
	sigset_t watched, waitmask;
	sigemptyset(&watched);
	sigaddset(&watched, SIGINT);
	sigaddset(&watched, SIGTERM);
	sigaddset(&watched, SIGHUP);
	sigaddset(&watched, SIGCHLD);
	sigprocmask(SIG_BLOCK, &watched, &waitmask);
	sigdelset(&waitmask, SIGINT);
	sigdelset(&waitmask, SIGTERM);
	sigdelset(&waitmask, SIGHUP);
	sigdelset(&waitmask, SIGCHLD);
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = on_stop;
	sigaction(SIGINT, &sa, NULL);
	sigaction(SIGTERM, &sa, NULL);
	sigaction(SIGHUP, &sa, NULL);
	sa.sa_handler = on_child; // the default action ignores SIGCHLD, which wouldn't end sigsuspend
	sa.sa_flags = SA_NOCLDSTOP;
	sigaction(SIGCHLD, &sa, NULL);
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
		pid_t p = waitpid(-1, &st, WNOHANG);
		if (p == 0) {
			sigsuspend(&waitmask); // until a stop signal or a worker's exit
			continue;
		}
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
static sq_shard *_Atomic all_shards[SQ_MAX_SHARDS]; // for the watchdog and exit_flush
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

// ---------------------------------------------------------------------------
// preemption and the watchdog

_Thread_local _Atomic int *sq_preempt_flag = &sq_shard0.preempt;

// sq_preempt is called by a loop that found its thread's flag set: a preemptible task yields.
void sq_preempt(void) {
	atomic_store_explicit(&sq_self->preempt, 0, memory_order_relaxed);
	sq_g *g = sq_self->cur_g;
	if (!g->pool->preemptible) return;
	sq_self->preempted = g; // the scheduler queues it after polling
	TSAN_SWITCH(sq_self->tsan_sched);
	sq_ctx_switch(&g->ctx, &sq_self->sched_ctx);
}

static void watch_warn(const char *task, int64_t ms) {
	char buf[256];
	size_t n = 0;
	const char *parts[] = {"squark: goroutine [", task, "] has run for ", NULL};
	for (int i = 0; parts[i]; i++) {
		size_t k = strlen(parts[i]);
		if (n + k >= sizeof buf) k = sizeof buf - n - 1;
		memcpy(buf + n, parts[i], k);
		n += k;
	}
	char num[24];
	int k = 0;
	do num[k++] = (char)('0' + ms % 10); while ((ms /= 10) > 0 && k < 23);
	while (k > 0 && n < sizeof buf - 1) buf[n++] = num[--k];
	const char *tail = "ms of CPU without yielding while other goroutines wait on its thread: mark the "
	                   "task preemptible, or call runtime.gosched() in its loop\n";
	size_t t = strlen(tail);
	if (n + t >= sizeof buf) t = sizeof buf - n - 1;
	memcpy(buf + n, tail, t);
	n += t;
	write_all(2, buf, n);
}

// thread_cpu_ms is the CPU time a shard's thread has used (-1: unknown). The watchdog measures
// goroutines in CPU time, so a thread the OS didn't run (an overloaded machine) isn't blamed.
static int64_t thread_cpu_ms(sq_shard *s) {
#if defined(__APPLE__)
	thread_basic_info_data_t info;
	mach_msg_type_number_t n = THREAD_BASIC_INFO_COUNT;
	if (thread_info(s->mach_thread, THREAD_BASIC_INFO, (thread_info_t)&info, &n) != KERN_SUCCESS) return -1;
	return (int64_t)(info.user_time.seconds + info.system_time.seconds) * 1000 +
	       (int64_t)(info.user_time.microseconds + info.system_time.microseconds) / 1000;
#else
	struct timespec ts;
	if (s->cpu_clock == (clockid_t)-1 || clock_gettime(s->cpu_clock, &ts) != 0) return -1;
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
#endif
}

// watchdog asks a goroutine that has used its thread for a time slice (10ms of CPU) to yield, and
// warns once it has used 100ms while others waited. It reads only the shards' atomic fields and
// their threads' CPU clocks.
static void *watchdog(void *arg) {
	(void)arg;
	static uint64_t last[SQ_MAX_SHARDS];
	static int64_t start[SQ_MAX_SHARDS], held[SQ_MAX_SHARDS];
	static bool warned[SQ_MAX_SHARDS];
	const int64_t tick = sq_any_preemptible ? 5 : 50; // ms
	for (;;) {
		struct timespec ts = {0, tick * 1000000};
		nanosleep(&ts, NULL);
		for (int64_t i = 0; i < sq_shard_count; i++) {
			sq_shard *s = atomic_load(&all_shards[i]);
			if (!s) continue;
			uint64_t sw = atomic_load_explicit(&s->switches, memory_order_relaxed);
			const char *task = atomic_load_explicit(&s->running, memory_order_relaxed);
			int64_t cpu = thread_cpu_ms(s);
			if (sw != last[i] || !task) { // a new stint (or none): measure from here
				last[i] = sw;
				start[i] = cpu;
				held[i] = 0;
				warned[i] = false;
				continue;
			}
			held[i] = cpu >= 0 && start[i] >= 0 ? cpu - start[i] : held[i] + tick;
			if (held[i] >= 10) atomic_store_explicit(&s->preempt, 1, memory_order_relaxed);
			if (held[i] >= 100 && !warned[i] && atomic_load_explicit(&s->others, memory_order_relaxed)) {
				warned[i] = true;
				watch_warn(task, held[i]);
			}
		}
	}
	return NULL;
}

static void start_watchdog(void) {
	sigset_t all, old;
	sigfillset(&all);
	pthread_sigmask(SIG_BLOCK, &all, &old);
	pthread_t t;
	pthread_attr_t attr;
	pthread_attr_init(&attr);
	pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
	pthread_attr_setstacksize(&attr, 65536);
	pthread_create(&t, &attr, watchdog, NULL); // without it, nothing is preempted or reported
	pthread_attr_destroy(&attr);
	pthread_sigmask(SIG_SETMASK, &old, NULL);
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
	fd_table_init();
	sq_preempt_flag = &sq_self->preempt;
#if defined(__APPLE__)
	sq_self->mach_thread = pthread_mach_thread_np(pthread_self());
#else
	if (pthread_getcpuclockid(pthread_self(), &sq_self->cpu_clock) != 0) sq_self->cpu_clock = (clockid_t)-1;
#endif
	atomic_store(&all_shards[sq_self->id], sq_self); // publishes the fields above to the watchdog
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

// threads_from_env applies SQUARK_THREADS (like GOMAXPROCS) to a program written for threads.
static int64_t threads_from_env(int64_t n) {
	const char *e = getenv("SQUARK_THREADS");
	if (!e || !*e) return n;
	int64_t v = 0;
	for (const char *p = e; *p; p++) {
		if (*p < '0' || *p > '9' || v > SQ_MAX_SHARDS) {
			err_write("squark: ignoring SQUARK_THREADS (want a number from 1 to 1024)\n");
			return n;
		}
		v = v * 10 + (*p - '0');
	}
	if (v < 1 || v > SQ_MAX_SHARDS) {
		err_write("squark: ignoring SQUARK_THREADS (want a number from 1 to 1024)\n");
		return n;
	}
	if (sq_threads_max > 0 && v > sq_threads_max) {
		err_write("squark: SQUARK_THREADS is above the threads the memory budget was proven for; using ");
		err_num(sq_threads_max);
		err_write("\n");
		return sq_threads_max;
	}
	return v;
}

#if defined(__x86_64__) && defined(SQ_CPU)
// A program built for a newer x86-64 than the one it runs on (squark build -cpu) would die of an
// illegal instruction somewhere. This runs before anything else, compiled for the baseline CPU,
// and names what is missing instead.
__attribute__((constructor(101), target("arch=x86-64"))) static void sq_check_cpu(void) {
	__builtin_cpu_init();
	const char *missing = NULL;
#ifdef __AVX512F__
	if (!__builtin_cpu_supports("avx512f")) missing = "AVX-512";
#endif
#ifdef __AVX2__
	if (!__builtin_cpu_supports("avx2")) missing = "AVX2";
#endif
#ifdef __FMA__
	if (!__builtin_cpu_supports("fma")) missing = "FMA";
#endif
#ifdef __AVX__
	if (!__builtin_cpu_supports("avx")) missing = "AVX";
#endif
#ifdef __POPCNT__
	if (!__builtin_cpu_supports("popcnt")) missing = "POPCNT";
#endif
#ifdef __SSE4_2__
	if (!__builtin_cpu_supports("sse4.2")) missing = "SSE4.2";
#endif
	if (missing) {
		char msg[256];
		int n = snprintf(msg, sizeof msg, "fatal error: this program was built for x86-64 CPUs with %s (squark build -cpu %s), and this CPU doesn't have it\n", missing, SQ_CPU);
		if (write(2, msg, (size_t)n)) {}
		_exit(2);
	}
}
#endif

int main(int argc, char **argv) {
	sq_shard0.next_gid = 1;
	sq_shard0.pollfd = -1;
	sq_shard0.pools = sq_pools;
	sq_shard0.timer_heap = sq_timer_heap;
	sq_shard0.date_cached_at = -1;
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
	if (sq_threads_env) n = threads_from_env(n);
	if (n > SQ_MAX_SHARDS) n = SQ_MAX_SHARDS;
	sq_shard_count = n;
	atomic_store(&live_shards, n);
	if (sq_helpers_per_thread > 0) start_helpers(sq_helpers_per_thread * n);
	start_watchdog();
	sq_self->sigstack = sq_sigstack; // shard 0 runs on this thread, with the static copies
	sq_self->sigstack_size = sizeof sq_sigstack;
	shard_run(); // the other shards start once shard 0 has initialized (sq_start_shards)
	return 0;
}
