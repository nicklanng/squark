// Used by TestNoHeapAtRuntime (internal/driver). Injected with DYLD_INSERT_LIBRARIES, it counts
// heap allocations and writes the running totals to $HEAPCOUNT_FILE every 100ms, formatting them
// without libc (which may allocate).
#include <stdlib.h>
#include <stdio.h>
#include <pthread.h>
#include <unistd.h>
#include <fcntl.h>
#include <stdatomic.h>
#include <malloc/malloc.h>

static atomic_long n_malloc, n_calloc, n_realloc, n_other;

static void *my_malloc(size_t s) { atomic_fetch_add(&n_malloc, 1); return malloc(s); }
static void *my_calloc(size_t a, size_t b) { atomic_fetch_add(&n_calloc, 1); return calloc(a, b); }
static void *my_realloc(void *p, size_t s) { atomic_fetch_add(&n_realloc, 1); return realloc(p, s); }
static void *my_valloc(size_t s) { atomic_fetch_add(&n_other, 1); return valloc(s); }
static int my_posix_memalign(void **p, size_t a, size_t s) { atomic_fetch_add(&n_other, 1); return posix_memalign(p, a, s); }

#define INTERPOSE(n, o) __attribute__((used)) static struct { const void *r, *orig; } interpose_##o \
	__attribute__((section("__DATA,__interpose"))) = {(const void *)n, (const void *)o}
INTERPOSE(my_malloc, malloc);
INTERPOSE(my_calloc, calloc);
INTERPOSE(my_realloc, realloc);
INTERPOSE(my_valloc, valloc);
INTERPOSE(my_posix_memalign, posix_memalign);

static int put(char *b, long v) { // decimal without libc formatting (which may allocate)
	char t[24]; int n = 0, k = 0;
	do { t[n++] = (char)('0' + v % 10); v /= 10; } while (v);
	while (n) b[k++] = t[--n];
	return k;
}

static void *reporter(void *arg) {
	int fd = (int)(long)arg;
	for (;;) {
		usleep(100000);
		char b[128]; int k = 0;
		long v[4] = {n_malloc, n_calloc, n_realloc, n_other};
		for (int i = 0; i < 4; i++) { k += put(b + k, v[i]); b[k++] = i < 3 ? ' ' : '\n'; }
		pwrite(fd, b, k, 0);
		ftruncate(fd, k);
	}
	return NULL;
}

__attribute__((constructor)) static void start(void) {
	const char *path = getenv("HEAPCOUNT_FILE");
	if (!path) return;
	int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
	pthread_t t;
	if (pthread_create(&t, NULL, reporter, (void *)(long)fd) != 0) write(fd, "no thread\n", 10);
}
