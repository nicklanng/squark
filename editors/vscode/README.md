# squark for VS Code

Syntax highlighting for [squark](../../docs/spec.md) (`.sq` files): the file header (`program`,
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

## Install

From a checkout, link the folder into VS Code's extensions and reload the window
(Command Palette → "Developer: Reload Window"):

```sh
ln -s "$PWD/editors/vscode" ~/.vscode/extensions/nicklanng.squark-0.1.0
```

Or build a package and install that:

```sh
cd editors/vscode
npx @vscode/vsce package          # writes squark-0.1.0.vsix
code --install-extension squark-0.1.0.vsix
```

## Test

The grammar is checked with VS Code's own TextMate engine: chosen tokens get the scopes they
should, and every `.sq` file in the repository tokenizes without invalid tokens or keywords read as
calls.

```sh
cd editors/vscode && npm install && npm test
```

When the language gains a keyword or a contextual word, add it to
`syntaxes/squark.tmLanguage.json` and a case to `test/grammar.test.mjs`.
