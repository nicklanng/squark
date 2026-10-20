# squark for VS Code

The compiler's errors as you type, hover, go to definition and formatting, through
`squark lsp` (below), and syntax highlighting for [squark](../../docs/spec.md) (`.sq` files): the file header (`program`,
`module NAME`, and the block of directives after it, one per line: `budget`, `pool`,
`workers`, `threads`, `link`, `cflags`, `ldflags`), imports (`use http, math/rand as r`,
`use ( ... )`), the keywords (`fn`, `task`, `extern`, `fault`, `pub`, `while`, `in`, `and`, `or`,
`not`, `arena`, ...), signatures (`fn m(x *T) -> int !` in a type's block, the result arrow `->` and the error
`!`, provenance `in s` and `in static`), postfix types (`u8[]`, `u8[..64]`, `int[str]`,
`Job chan[16]`, SIMD types like `f32x4`), the range operator (`0..n`), size literals (`64K`, `8M`),
the words that are keywords only in place (`start f(x)`, `loop {`, `select {`,
`with arena a 64K {`, `thread x int` and `shared t = f()` at package level, `pool N`, `stack N`,
`blocking` and `preemptible` after a signature, an extern's C symbol `c"sym"`), labels and
`//squark:` pragmas. Comment toggling, bracket matching and auto-indent come from
`language-configuration.json`.

## The language server

The extension starts `squark lsp`, which checks each package as you edit it (unsaved changes
included; a module is checked with its tests) and answers:

- **diagnostics**: the compiler's type and lifetime errors, on the word they point at;
- **hover**: a declaration's signature with its provenance, as `squark doc` shows it, and its doc
  comment; a field's type;
- **go to definition**: in your packages and in the standard library (opened read-only);
- **format document**: `squark fmt`.

It runs `squark.path` if set, else `bin/squark` in a workspace folder (a squark checkout), else
`squark` on the PATH. Build the compiler first: `go build -o bin/squark ./cmd/squark`. Other
editors can run `squark lsp` the same way: it speaks the Language Server Protocol over stdin and
stdout.

## Install

From a checkout, link the folder into VS Code's extensions and reload the window
(Command Palette → "Developer: Reload Window"):

```sh
ln -s "$PWD/editors/vscode" ~/.vscode/extensions/nicklanng.squark-0.2.0
```

Or build a package and install that:

```sh
cd editors/vscode
npx @vscode/vsce package          # writes squark-0.2.0.vsix
code --install-extension squark-0.2.0.vsix
```

## Test

The grammar is checked with VS Code's own TextMate engine: chosen tokens get the scopes they
should, and every `.sq` file in the repository tokenizes without invalid tokens or keywords read as
calls. The extension's client (`rpc.js`, no dependencies) is run against the real `squark lsp`
(it needs `bin/squark` built).

```sh
cd editors/vscode && npm install && npm test
```

When the language gains a keyword or a contextual word, add it to
`syntaxes/squark.tmLanguage.json` and a case to `test/grammar.test.mjs`.
