// A C function that computes for a while: after 1ms it moves its task to a helper thread
// (sq_detach), finishes there, and moves back (sq_reattach) before returning.
#include <stdint.h>
#include "sq_rt.h"

int64_t crunch(int64_t ms) {
	int64_t start = sq_nanotime(), x = 0;
	int moved = 0;
	while (sq_nanotime() - start < ms * 1000000) {
		for (int i = 0; i < 1000; i++) x += i ^ x;
		if (!moved && sq_nanotime() - start > 1000000) moved = sq_detach();
	}
	int on_helper = sq_on_helper();
	sq_reattach();
	return moved && on_helper && !sq_on_helper() ? 1 : 0; // moved, ran there, came back
}
