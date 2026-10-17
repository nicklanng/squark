// Scheduler internals shared by the runtime and the generated pool storage (pools.c).
#ifndef SQ_INTERNAL_H
#define SQ_INTERNAL_H
#include "sq_rt.h"

typedef struct {
#if defined(__aarch64__)
	uint64_t x[12]; // x19..x28, fp, lr
	uint64_t sp;
	uint64_t d[8]; // d8..d15
#elif defined(__x86_64__)
	uint64_t rbx, rbp, r12, r13, r14, r15, rsp;
#else
#error "unsupported architecture"
#endif
} sq_ctx;

enum { G_FREE, G_RUNNABLE, G_RUNNING, G_WAITING, G_DEAD };

struct sq_g {
	sq_ctx ctx;
	sq_pool *pool;
	void (*entry)(void *);
	void *args;
	uint8_t *stack_lo, *stack_hi;
	sq_g *next;  // run queue / free list / wait queue link
	int state;
	int64_t id;
	// channel operations
	void *elem;
	bool ok;
	// timers
	int64_t wake_at;
	int64_t heap_index; // -1 when not in the timer heap
	int64_t deadline;   // with deadline: when blocking I/O times out (sq_nanotime), or 0 for never
	// netpoll
	int wait_fd;        // fd this goroutine is parked on, or -1
	bool armed;         // a kqueue/epoll registration for this goroutine is outstanding
	int wake;           // why the goroutine was last woken (WAKE_*)
	bool guarded;       // guard page installed (done lazily on first use)
	bool release_stack; // return the whole stack to the OS when this goroutine exits
	void *tsan_fiber;   // ThreadSanitizer builds only: this goroutine's fiber
	void *shard;        // the shard (scheduler thread) that runs this goroutine
	void (*blk_fn)(void *); // a blocking extern call a helper thread runs for it (argument in elem)
	struct sq_waiter *sel_timer; // in a select with a timeout: the timer's waiter
};

enum { WAKE_NONE, WAKE_FD, WAKE_TIMER, WAKE_CLOSED };

#endif
