// A stand-in for a slow I/O call (a disk read, a query): it waits, then fills the caller's buffer
// with bytes that depend only on the record id. Declared `blocking` in main.sq, so it runs on a
// helper thread and the scheduler thread keeps running other tasks meanwhile.
#include <stdint.h>
#include <unistd.h>

// squark's []byte, passed by value
typedef struct {
	uint8_t *p;
	int64_t len, cap;
} jobs_bytes;

int64_t jobs_fetch(int64_t id, jobs_bytes buf, int64_t ms) {
	usleep((useconds_t)(ms * 1000));
	uint32_t x = (uint32_t)id * 2654435761u + 1;
	for (int64_t i = 0; i < buf.len; i++) {
		x = x * 1103515245u + 12345u;
		buf.p[i] = (uint8_t)(x >> 16);
	}
	return buf.len;
}
