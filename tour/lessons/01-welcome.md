# Welcome

## Hello, squark

Welcome to the tour of **squark**, a language with no heap. Every byte a squark program can use is
known when it is compiled, and the compiler proves that no reference outlives the memory it points
to.

The program on the right is real squark, and the compiler on this machine builds it. Press **Run**,
or [[⇧⏎]] in the editor.

Three things happen as you work through the tour:

- **The compiler checks as you type.** Type and lifetime checking takes milliseconds, so the bar
  above the editor says whether the program is valid before you run it. Try deleting the closing
  `"` on line 4.
- **Every run draws a memory map.** It shows the program's worst case: code, static data, and every
  task's stack, each proven at compile time.
- **Some lessons set a goal.** Some goals ask you to fix a program and some ask you to break one.
  The contents (top left) keep track of your progress.

```squark editor
program

fn main() {
	println("hello, squark")
}
```

## The memory map

Run this program and look at the memory map under the editor. The **main task** row is the
stack of `main`, sized from the frame sizes clang reports for each function and a walk of the call
graph. It isn't a guess or a default. The line says how many bytes the proof found.

`buf` is 4 KiB of storage on that stack, and the program writes its output through it. Change `4K`
to `64K` and run again: the main task's row grows by 64 KiB, and the map marks the change. Nothing
in squark can allocate behind your back, so making a local variable bigger is the only way to make
the stack bigger.

> `4K`, `64K` and `1M` are size literals: integer constants multiplied by 1024, 1024² or 1024³.

```squark editor
program

use os, strconv

fn main() {
	var buf u8[4K] // on main's stack: declaring storage is the allocation
	line := append(buf[:0], "buf holds "...)
	line = strconv.append_int(line, len(buf))
	line = append(line, " bytes\n"...)
	os.stdout.write(line)
}
```

## Errors that name the fix
goal: prints "both"

squark's compiler errors tell you what to write instead. Habits from other languages trip you up
here: in squark the logical operators are the words `and`, `or` and `not`.

The bar above the editor already shows the error. Fix it, run the program, and the goal turns green.

```squark editor
program

fn main() {
	a := true
	b := true
	if a && b {
		println("both")
	}
}
```

```squark solution
program

fn main() {
	a := true
	b := true
	if a and b {
		println("both")
	}
}
```
