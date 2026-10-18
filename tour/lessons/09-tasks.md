# Tasks and channels

A **task** is a function that runs concurrently, declared with its pool: the most copies of it
that may run at once. The compiler proves every task's stack, so a pool of N is N proven stacks,
and the memory map shows each pool. Tasks talk over channels.

## Tasks and pools

`task name(params) pool N { ... }` declares a task entry point, and `start name(args)` starts
one. Only tasks can be started, and they have no results. `start` on a full pool waits for a free
slot instead of growing the pool.

Run it and find the `task main.square` row in the memory map: 4 stacks, each proven. Then change
`pool 4` to `pool 4000` and run again.

```squark editor
program

thread results int chan[8] // channel storage, at package level

task square(n int) pool 4 {
	results <- n * n
}

fn main() {
	for i in 1..11 {
		start square(i) // waits for a free slot when all 4 are running
	}
	sum := 0
	for _ in 1..11 {
		sum += <-results
	}
	println("sum of squares:", sum)
}
```

## Channels

A channel's buffer is storage like any other: `int chan[4]` holds four ints, and `chan[0]` is
unbuffered. `ch <- v` sends, waiting while the buffer is full; `<-ch` receives, waiting while it
is empty; `close(ch)` ends it, and `for v in ch` receives until it is closed and empty.

Within a thread, tasks take turns. A task switches only when it blocks: on a channel, on
I/O, or on `time.sleep`. Two tasks on the same thread never run at the same time, so data they
share needs no locks, as long as nothing blocks in the middle of an update. That's why the output
below always comes out in the same order.

```squark editor
program

thread jobs int chan[2]
thread done bool chan[0]

task producer(n int) pool 1 {
	defer close(jobs)
	for i in 1..n+1 {
		jobs <- i
		println("sent", i)
	}
}

task consumer() pool 1 {
	total := 0
	for j in jobs {
		total += j
		println("    got", j)
	}
	println("total", total)
	done <- true
}

fn main() {
	start producer(5)
	start consumer()
	<-done
}
```

> If every task is blocked and nothing can wake them, the program stops with
> `fatal error: all tasks are asleep - deadlock!`. Delete `done <- true` to see it.

## Only static memory crosses
goal: rejects "to a new task"

A new task can outlive the function that started it, so its arguments, and anything sent on
a channel, may only refer to static memory: constants, package variables and package arenas. Plain
values such as numbers and structs of numbers can always be passed.

Build the name in a local buffer instead of passing the constant: `var buf u8[32]`, then pass
`str(strconv.append_int(append(buf[:0], "worker "...), 1))` to `greet`.

```squark editor
program

use strconv

thread done bool chan[0]

task greet(name str) pool 4 {
	println("hello,", name)
	done <- true
}

fn main() {
	start greet("worker 1")
	<-done
}
```

```squark solution
program

use strconv

thread done bool chan[0]

task greet(name str) pool 4 {
	println("hello,", name)
	done <- true
}

fn main() {
	var buf u8[32]
	name := strconv.append_int(append(buf[:0], "worker "...), 1)
	start greet(str(name))
	<-done
}
```

## Shedding load

Waiting for a free slot is right for a batch job and wrong for a server under a flood of
requests. `start f(x) else { ... }` doesn't wait: when the pool is full, it runs the block
instead and starts nothing. A server can then answer "busy" while it still has the memory to.

```squark editor
program

use time

thread finished int chan[8]

task slow_job(id int) pool 2 {
	time.sleep(10 * time.millisecond)
	finished <- id
}

fn main() {
	accepted := 0
	for id in 1..6 {
		start slow_job(id) else {
			println("job", id, "shed: the pool is full")
			continue
		}
		accepted++
	}
	for _ in 0..accepted {
		println("job", <-finished, "done")
	}
}
```

## select and timeouts

`select` waits on several channel operations at once and runs the first that can proceed. For a timeout, use
`case <-time.after(d):`. squark has no heap to allocate a fresh channel for each call, so
`time.after` is only allowed as a `select` case.

```squark editor
program

use time

thread replies int chan[1]

task answer(delay_ms int) pool 2 {
	time.sleep(time.Duration(delay_ms) * time.millisecond)
	replies <- delay_ms
}

fn ask(delay_ms int) {
	start answer(delay_ms)
	select {
	case v := <-replies:
		println("answered after", v, "ms")
	case <-time.after(50 * time.millisecond):
		println("gave up after 50 ms")
	}
}

fn main() {
	ask(10)
	ask(200)
}
```

> `with deadline 2 * time.second { ... }` does the same for I/O: every read, write, dial and DNS
> lookup in the block, and in everything it calls, gives up when the time runs out.
