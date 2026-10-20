# Memory budgets

Everything so far adds up to a number: the worst case. Code, static data, every pool's stacks, and
optionally kernel socket buffers and C heaps, all known at build time. A program can state the
most it may use, and the build fails if the worst case could ever exceed it.

## A budget

Directives go in a block after `program`. `budget 1M` says this program may never need more than
1 MiB. The build checks it, and the memory map draws the budget as a line.

The binary knows its worst case too. On Linux it checks its cgroup when it starts: if the container
it runs in allows less memory than the worst case, it exits at once with a message, instead of
being killed later at its busiest.

```squark editor
program {
	budget 1M
}

fn main() {
	println("well within budget")
}
```

## Over budget
goal: builds

This program has room for 1000 workers. Each one's stack is reserved, and together they need more
than the 8 MiB budget. Run it: the build fails, and the memory map shows which row is to blame.
Make it fit without raising the budget.

> A pool counts once something starts its task. A task that no code path starts takes no memory.

```squark editor
program {
	budget 8M
}

thread jobs int chan[64]
thread squares int chan[64]

task worker(id int) pool 1000 {
	for j in jobs {
		squares <- j * j
	}
}

fn main() {
	for i in 0..4 {
		start worker(i)
	}
	for j in 1..6 {
		jobs <- j
	}
	close(jobs)
	sum := 0
	for _ in 1..6 {
		sum += <-squares
	}
	println("sum of squares:", sum)
}
```

```squark solution
program {
	budget 8M
}

thread jobs int chan[64]
thread squares int chan[64]

task worker(id int) pool 16 {
	for j in jobs {
		squares <- j * j
	}
}

fn main() {
	for i in 0..4 {
		start worker(i)
	}
	for j in 1..6 {
		jobs <- j
	}
	close(jobs)
	sum := 0
	for _ in 1..6 {
		sum += <-squares
	}
	println("sum of squares:", sum)
}
```

> A program can also resize a library's pool without editing the library: `pool http.serve_conn 512`
> in the header.

## A server's worst case
mode: build

This is a complete HTTP server. It would run until stopped, so this lesson builds it without
running it. Press **Build** and read the map:

- **task http.serve_conn**: one proven stack per connection served at once, 512 of them
- **sockets**: with `connections` and `sockbuf`, the kernel's socket buffers are counted too, since
  Linux charges them to the container
- **static data** includes the poller, the timers and the idle-connection tables

Each request gets `r.arena`, which lives on its connection's stack and is gone once the response is
written. Try `pool http.serve_conn 2048` and watch the server go over its budget.

```squark editor
program {
	budget 160M
	pool http.serve_conn 512 // at most 512 connections served at once
	connections 1024 // at most 1024 sockets open
	sockbuf 16K // each socket's kernel buffers
}

use http

// GET /hello?name=world
fn hello(w *http.ResponseWriter, r *http.Request) {
	w.text(200, "hello, " + r.query_param("name") + "\n" in r.arena)
}

fn main() {
	http.handle_func("GET /hello", hello)
	if err := http.listen_and_serve(":8080", nil); err != nil {
		panic(err)
	}
}
```

## Threads

By default a program runs on one thread. `threads 4` runs four scheduler threads in one process,
and **each runs its own copy of `main`**, with its own package variables. Threads share nothing
mutable, so nothing needs a lock. A package variable says which kind it is:

- `thread x T`: one copy per thread
- `shared x = ...`: one copy for all threads, read-only once the program has started
- `shared ch T chan[N]`: a channel between threads

Static data and stacks are per thread, so the map counts them four times. `threads auto` runs one
thread per CPU the process may use, and follows a container's CPU limit.

```squark editor
program {
	threads 4
}

use runtime

shared primes = first_primes() // computed once, before the threads start
thread found int // each thread's own

fn first_primes() -> int[8] {
	var ps int[8]
	n := 0
	for c := 2; n < 8; c++ {
		prime := true
		for i in 0..n {
			if c%ps[i] == 0 {
				prime = false
			}
		}
		if prime {
			ps[n] = c
			n++
		}
	}
	return ps
}

fn main() {
	for p in primes {
		if p%(runtime.shard()+2) == 1 {
			found++
		}
	}
	println("thread", runtime.shard(), "of", runtime.shards(), "found", found)
}
```
