// Used by TestNoHeapAtRuntime (internal/driver). The glibc variant of count_darwin.c, injected with
// LD_PRELOAD: counts heap allocations and writes the totals to $HEAPCOUNT_FILE every 100ms.
#define _GNU_SOURCE
#include <stddef.h>
#include <unistd.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>

extern void *__libc_malloc(size_t);
extern void *__libc_calloc(size_t, size_t);
extern void *__libc_realloc(void *, size_t);
extern void *__libc_memalign(size_t, size_t);
static atomic_long nm, nc, nr;

void *malloc(size_t s) { atomic_fetch_add(&nm, 1); return __libc_malloc(s); }
void *calloc(size_t a, size_t b) { atomic_fetch_add(&nc, 1); return __libc_calloc(a, b); }
void *realloc(void *p, size_t s) { atomic_fetch_add(&nr, 1); return __libc_realloc(p, s); }
int posix_memalign(void **p, size_t a, size_t s) { atomic_fetch_add(&nm, 1); *p = __libc_memalign(a, s); return *p ? 0 : 12; }

static int put(char *b, long v) {
	char t[24]; int n = 0, k = 0;
	do { t[n++] = (char)('0' + v % 10); v /= 10; } while (v);
	while (n) b[k++] = t[--n];
	return k;
}
static void *reporter(void *arg) {
	int fd = (int)(long)arg;
	for (;;) {
		usleep(100000);
		char b[96]; int k = 0;
		long v[3] = {nm, nc, nr};
		for (int i = 0; i < 3; i++) { k += put(b + k, v[i]); b[k++] = i < 2 ? ' ' : '\n'; }
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
