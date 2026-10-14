// A C function that blocks its thread, like a slow query or a read from a slow disk.
#include <stdint.h>
#include <unistd.h>

int64_t slow_ms(int64_t ms) {
	usleep((useconds_t)(ms * 1000));
	return ms;
}

void slow_fill(uint8_t *p, int64_t n, int64_t v) {
	usleep(1000);
	for (int64_t i = 0; i < n; i++) p[i] = (uint8_t)v;
}
