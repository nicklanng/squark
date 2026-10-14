// squark runtime: M:1 goroutine scheduler, kqueue/epoll netpoll, timers, channels, maps, panics.
// This file must never call malloc/free (checked by `squark build`, which inspects the
// undefined symbols of every object it produces).
#define _GNU_SOURCE
#include "sq_rt.h"
#include "sq_internal.h"

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/socket.h>
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
#endif

// ---------------------------------------------------------------------------
// context switching


void sq_ctx_switch(sq_ctx *from, sq_ctx *to);

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


static sq_ctx sched_ctx;
static sq_g *cur_g;
static sq_g *runq_head, *runq_tail;
static int64_t next_gid = 1;
static int64_t total_parked; // goroutines blocked on channels/slots (not fds/timers)
static int64_t fd_waiters;

static void runq_push(sq_g *g) {
	g->state = G_RUNNABLE;
	g->next = NULL;
	if (runq_tail) runq_tail->next = g; else runq_head = g;
	runq_tail = g;
}

static sq_g *runq_pop(void) {
	sq_g *g = runq_head;
	if (g) {
		runq_head = g->next;
		if (!runq_head) runq_tail = NULL;
		g->next = NULL;
	}
	return g;
}

static void sq_park(void) {
	sq_g *g = cur_g;
	g->state = G_WAITING;
	sq_ctx_switch(&g->ctx, &sched_ctx);
}

static void sq_ready(sq_g *g) { runq_push(g); }

void sq_gosched(void) {
	sq_g *g = cur_g;
	runq_push(g);
	sq_ctx_switch(&g->ctx, &sched_ctx);
}

static void g_entry(void) {
	sq_g *g = cur_g;
	g->entry(g->args);
	g->state = G_DEAD;
	sq_ctx_switch(&g->ctx, &sched_ctx);
	__builtin_unreachable();
}

static void g_prepare(sq_g *g, void (*entry)(void *), const void *args, size_t argsize) {
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
	g->id = next_gid++;
}

static void pool_init(sq_pool *p) {
	p->free = NULL;
	for (int64_t i = p->count - 1; i >= 0; i--) {
		sq_g *g = &p->gs[i];
		uint8_t *slot = p->stacks + i * p->slot_size;
		long pagesz = sysconf(_SC_PAGESIZE);
		// guard page at the low end of every stack
		mprotect(slot, (size_t)pagesz, PROT_NONE);
		g->stack_lo = slot + pagesz;
		g->stack_hi = slot + p->slot_size;
		g->pool = p;
		g->state = G_FREE;
		g->next = p->free;
		p->free = g;
	}
}

void sq_go(sq_pool *p, void (*entry)(void *), const void *args, size_t argsize) {
	while (!p->free) {
		// all slots busy: wait for one (backpressure instead of unbounded growth)
		sq_g *self = cur_g;
		self->next = NULL;
		if (p->waitq_tail) p->waitq_tail->next = self; else p->waitq_head = self;
		p->waitq_tail = self;
		total_parked++;
		sq_park();
	}
	sq_g *g = p->free;
	p->free = g->next;
	g_prepare(g, entry, args, argsize);
	p->live++;
	p->started++;
	if (p->live > p->peak) p->peak = p->live;
	runq_push(g);
}

static void g_release(sq_g *g) {
	sq_pool *p = g->pool;
	g->state = G_FREE;
	g->next = p->free;
	p->free = g;
	p->live--;
	if (p->waitq_head) {
		sq_g *w = p->waitq_head;
		p->waitq_head = w->next;
		if (!p->waitq_head) p->waitq_tail = NULL;
		total_parked--;
		sq_ready(w);
	}
}

// ---------------------------------------------------------------------------
// output

static uint8_t outbuf[8192];
static size_t outlen;

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
	if (outlen) write_all(1, outbuf, outlen);
	outlen = 0;
}

static void out(const void *p, size_t n) {
	if (outlen + n > sizeof outbuf) sq_flush();
	if (n > sizeof outbuf) {
		write_all(1, p, n);
		return;
	}
	memcpy(outbuf + outlen, p, n);
	outlen += n;
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
	if (cur_g) {
		err_int(cur_g->id);
		err_write(" [");
		err_write(cur_g->pool->name);
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

void sq_panic_divide(const char *file, int line) {
	sq_panic_cstr("runtime error: integer divide by zero", file, line);
}

static uint8_t sigstack[32768];

static void on_fault(int sig, siginfo_t *si, void *uctx) {
	(void)uctx;
	uintptr_t addr = (uintptr_t)si->si_addr;
	long pagesz = sysconf(_SC_PAGESIZE);
	for (int64_t i = 0; i < sq_npools; i++) {
		sq_pool *p = sq_pools[i];
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

void sq_chan_send(sq_chan *c, const void *elem, const char *file, int line) {
	if (!c) {
		total_parked++;
		sq_park(); // send on nil channel blocks forever
	}
	if (c->closed) sq_panic_cstr("send on closed channel", file, line);
	sq_g *r = q_pop(&c->recvq_head, &c->recvq_tail);
	if (r) {
		memcpy(r->elem, elem, (size_t)c->elemsize);
		r->ok = true;
		total_parked--;
		sq_ready(r);
		return;
	}
	if (c->len < c->cap) {
		int64_t tail = (c->head + c->len) % c->cap;
		memcpy(c->buf + tail * c->elemsize, elem, (size_t)c->elemsize);
		c->len++;
		return;
	}
	sq_g *g = cur_g;
	g->elem = (void *)elem;
	g->ok = false;
	q_push(&c->sendq_head, &c->sendq_tail, g);
	total_parked++;
	sq_park();
	if (!g->ok) sq_panic_cstr("send on closed channel", file, line);
}

bool sq_chan_recv(sq_chan *c, void *outp) {
	if (!c) {
		total_parked++;
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
			total_parked--;
			sq_ready(s);
		}
		return true;
	}
	sq_g *s = q_pop(&c->sendq_head, &c->sendq_tail);
	if (s) {
		memcpy(outp, s->elem, (size_t)c->elemsize);
		s->ok = true;
		total_parked--;
		sq_ready(s);
		return true;
	}
	if (c->closed) {
		memset(outp, 0, (size_t)c->elemsize);
		return false;
	}
	sq_g *g = cur_g;
	g->elem = outp;
	g->ok = false;
	q_push(&c->recvq_head, &c->recvq_tail, g);
	total_parked++;
	sq_park();
	if (!g->ok) memset(outp, 0, (size_t)c->elemsize);
	return g->ok;
}

void sq_chan_close(sq_chan *c, const char *file, int line) {
	if (!c) sq_panic_cstr("close of nil channel", file, line);
	if (c->closed) sq_panic_cstr("close of closed channel", file, line);
	c->closed = true;
	sq_g *g;
	while ((g = q_pop(&c->recvq_head, &c->recvq_tail))) {
		g->ok = false;
		total_parked--;
		sq_ready(g);
	}
	while ((g = q_pop(&c->sendq_head, &c->sendq_tail))) {
		g->ok = false;
		total_parked--;
		sq_ready(g);
	}
}

// ---------------------------------------------------------------------------
// maps (linear probing with backward-shift deletion; ctrl byte 1 = occupied)

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
	m->maxlen = maxlen;
	m->keysize = keysize;
	m->valsize = valsize;
	m->kind = kind;
	memset(ctrl, 0, (size_t)slots);
	return m;
}

static int64_t map_find(sq_map *m, const void *key) {
	int64_t i = (int64_t)(map_hash(m, key) & (uint64_t)m->mask);
	for (;;) {
		if (!m->ctrl[i]) return -1 - i;
		if (map_keyeq(m, m->keys + i * m->keysize, key)) return i;
		i = (i + 1) & m->mask;
	}
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
	i = -1 - i;
	m->ctrl[i] = 1;
	memcpy(m->keys + i * m->keysize, key, (size_t)m->keysize);
	memset(m->vals + i * m->valsize, 0, (size_t)m->valsize);
	m->len++;
	return m->vals + i * m->valsize;
}

void sq_map_delete(sq_map *m, const void *key) {
	if (!m || m->len == 0) return;
	int64_t i = map_find(m, key);
	if (i < 0) return;
	// backward-shift deletion keeps probe sequences intact without tombstones
	int64_t j = i;
	for (;;) {
		m->ctrl[i] = 0;
		for (;;) {
			j = (j + 1) & m->mask;
			if (!m->ctrl[j]) {
				m->len--;
				return;
			}
			int64_t k = (int64_t)(map_hash(m, m->keys + j * m->keysize) & (uint64_t)m->mask);
			// can the entry at j move to i?
			if ((i <= j) ? (i < k && k <= j) : (i < k || k <= j)) continue;
			break;
		}
		m->ctrl[i] = 1;
		memcpy(m->keys + i * m->keysize, m->keys + j * m->keysize, (size_t)m->keysize);
		memcpy(m->vals + i * m->valsize, m->vals + j * m->valsize, (size_t)m->valsize);
		i = j;
	}
}

int64_t sq_map_next(sq_map *m, int64_t i) {
	if (!m) return -1;
	for (; i < m->slots; i++)
		if (m->ctrl[i]) return i;
	return -1;
}

// ---------------------------------------------------------------------------
// netpoll and timers

static int pollfd = -1;
extern sq_g *sq_timer_heap[];
static int64_t ntimers;

static void heap_swap(int64_t a, int64_t b) {
	sq_g *t = sq_timer_heap[a];
	sq_timer_heap[a] = sq_timer_heap[b];
	sq_timer_heap[b] = t;
	sq_timer_heap[a]->heap_index = a;
	sq_timer_heap[b]->heap_index = b;
}

static void heap_up(int64_t i) {
	while (i > 0) {
		int64_t p = (i - 1) / 2;
		if (sq_timer_heap[p]->wake_at <= sq_timer_heap[i]->wake_at) break;
		heap_swap(i, p);
		i = p;
	}
}

static void heap_down(int64_t i) {
	for (;;) {
		int64_t l = 2 * i + 1, r = l + 1, m = i;
		if (l < ntimers && sq_timer_heap[l]->wake_at < sq_timer_heap[m]->wake_at) m = l;
		if (r < ntimers && sq_timer_heap[r]->wake_at < sq_timer_heap[m]->wake_at) m = r;
		if (m == i) return;
		heap_swap(i, m);
		i = m;
	}
}

static sq_g *heap_pop(void) {
	sq_g *g = sq_timer_heap[0];
	ntimers--;
	if (ntimers > 0) {
		sq_timer_heap[0] = sq_timer_heap[ntimers];
		sq_timer_heap[0]->heap_index = 0;
		heap_down(0);
	}
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
	sq_g *g = cur_g;
	g->wake_at = sq_nanotime() + ns;
	g->heap_index = ntimers;
	sq_timer_heap[ntimers++] = g;
	heap_up(g->heap_index);
	sq_park();
}

#if SQ_KQUEUE
static struct kevent changes[256];
static int nchanges;
#endif

// wait_fd parks the current goroutine until fd is readable (mode 'r') or writable ('w').
static void wait_fd(int fd, int mode) {
#if SQ_KQUEUE
	if (nchanges == (int)(sizeof changes / sizeof changes[0])) {
		kevent(pollfd, changes, nchanges, NULL, 0, NULL);
		nchanges = 0;
	}
	EV_SET(&changes[nchanges++], fd, mode == 'r' ? EVFILT_READ : EVFILT_WRITE, EV_ADD | EV_ONESHOT, 0, 0, cur_g);
#else
	struct epoll_event ev = {.events = (mode == 'r' ? EPOLLIN : EPOLLOUT) | EPOLLONESHOT, .data.ptr = cur_g};
	if (epoll_ctl(pollfd, EPOLL_CTL_MOD, fd, &ev) < 0) epoll_ctl(pollfd, EPOLL_CTL_ADD, fd, &ev);
#endif
	fd_waiters++;
	sq_park();
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
	int n = kevent(pollfd, changes, nchanges, events, 256, tsp);
	nchanges = 0;
	for (int i = 0; i < n; i++) {
		if (events[i].flags & EV_ERROR) {
			// registration failed (e.g. fd closed): wake the waiter so its syscall reports the error
			if (events[i].udata) {
				fd_waiters--;
				sq_ready(events[i].udata);
			}
			continue;
		}
		fd_waiters--;
		sq_ready(events[i].udata);
	}
#else
	struct epoll_event events[256];
	int ms = timeout_ns < 0 ? -1 : (int)((timeout_ns + 999999) / 1000000);
	int n = epoll_wait(pollfd, events, 256, ms);
	for (int i = 0; i < n; i++) {
		fd_waiters--;
		sq_ready(events[i].data.ptr);
	}
#endif
}

static void fire_timers(void) {
	if (!ntimers) return;
	int64_t now = sq_nanotime();
	while (ntimers > 0 && sq_timer_heap[0]->wake_at <= now) sq_ready(heap_pop());
}

// ---------------------------------------------------------------------------
// system interface used by the standard library (all return -errno on failure)

static int set_nonblock(int fd) {
	int fl = fcntl(fd, F_GETFL);
	return fcntl(fd, F_SETFL, fl | O_NONBLOCK);
}

int64_t sq_read(int64_t fd, sq_bytes b) {
	for (;;) {
		ssize_t r = read((int)fd, b.p, (size_t)b.len);
		if (r >= 0) return r;
		if (errno == EINTR) continue;
		if (errno == EAGAIN && cur_g) {
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
		if (errno == EAGAIN && cur_g) {
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
			if (errno == EAGAIN && cur_g) {
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

// sq_httpdate writes an RFC 1123 date ("Mon, 02 Jan 2006 15:04:05 GMT", 29 bytes), cached per second.
int64_t sq_httpdate(sq_bytes dst) {
	static char cache[32];
	static time_t cached_at = -1;
	time_t now = time(NULL);
	if (now != cached_at) {
		struct tm tm;
		gmtime_r(&now, &tm);
		static const char days[] = "SunMonTueWedThuFriSat";
		static const char months[] = "JanFebMarAprMayJunJulAugSepOctNovDec";
		char *p = cache;
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
		cached_at = now;
	}
	int64_t n = dst.len < 29 ? dst.len : 29;
	memcpy(dst.p, cache, (size_t)n);
	return n;
}

int64_t sq_close(int64_t fd) { return close((int)fd) < 0 ? -errno : 0; }

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

int64_t sq_listen_tcp(sq_string host, int64_t port, int64_t backlog) {
	int fd = socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return -errno;
	int one = 1;
	setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
	struct sockaddr_in sa;
	memset(&sa, 0, sizeof sa);
	sa.sin_family = AF_INET;
	sa.sin_port = htons((uint16_t)port);
	char hb[64];
	if (host.len == 0) {
		sa.sin_addr.s_addr = htonl(INADDR_ANY);
	} else {
		if (host.len >= (int64_t)sizeof hb) { close(fd); return -EINVAL; }
		memcpy(hb, host.p, (size_t)host.len);
		hb[host.len] = 0;
		if (inet_pton(AF_INET, hb, &sa.sin_addr) != 1) { close(fd); return -EINVAL; }
	}
	if (bind(fd, (struct sockaddr *)&sa, sizeof sa) < 0 || listen(fd, (int)backlog) < 0) {
		int e = errno;
		close(fd);
		return -e;
	}
	set_nonblock(fd);
	return fd;
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
		if (errno == EAGAIN && cur_g) {
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

sq_string sq_strerror(int64_t err) {
	const char *s = strerror((int)err);
	return (sq_string){(const uint8_t *)s, (int64_t)strlen(s)};
}

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

int64_t sq_pool_count(void) { return sq_npools; }
sq_string sq_pool_name(int64_t i) {
	const char *n = sq_pools[i]->name;
	return (sq_string){(const uint8_t *)n, (int64_t)strlen(n)};
}
int64_t sq_pool_stat(int64_t i, int64_t which) {
	sq_pool *p = sq_pools[i];
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
			cur_g = g;
			g->state = G_RUNNING;
			sq_ctx_switch(&sched_ctx, &g->ctx);
			cur_g = NULL;
			if (g->state == G_DEAD) {
				if (g->pool == &sq_main_pool) {
					sq_flush();
					_exit(0); // returning from main ends the program, like Go
				}
				g_release(g);
			}
			if ((++switches & 63) == 0 && (fd_waiters > 0 || ntimers > 0)) {
				fire_timers();
				if (fd_waiters > 0) netpoll(0);
			}
		}
		// nothing runnable: block in the poller until I/O or a timer fires
		if (fd_waiters == 0 && ntimers == 0) {
			sq_flush();
			err_write("fatal error: all goroutines are asleep - deadlock!\n");
			_exit(2);
		}
		int64_t timeout = -1;
		if (ntimers > 0) {
			timeout = sq_timer_heap[0]->wake_at - sq_nanotime();
			if (timeout < 0) timeout = 0;
		}
		if (fd_waiters > 0) {
			netpoll(timeout);
		} else if (timeout > 0) {
			struct timespec ts = {timeout / 1000000000, timeout % 1000000000};
			nanosleep(&ts, NULL);
		}
		fire_timers();
	}
}

int main(int argc, char **argv) {
	g_argc = argc;
	g_argv = argv;
	signal(SIGPIPE, SIG_IGN);
	stack_t ss = {.ss_sp = sigstack, .ss_size = sizeof sigstack, .ss_flags = 0};
	sigaltstack(&ss, NULL);
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = on_fault;
	sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
	sigaction(SIGSEGV, &sa, NULL);
	sigaction(SIGBUS, &sa, NULL);
#if SQ_KQUEUE
	pollfd = kqueue();
#else
	pollfd = epoll_create1(0);
#endif
	for (int64_t i = 0; i < sq_npools; i++) pool_init(sq_pools[i]);
	sq_go(&sq_main_pool, sq_program_main, NULL, 0);
	schedule();
	return 0;
}
