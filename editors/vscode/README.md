# squark for VS Code

Syntax highlighting for [squark](../../docs/spec.md) (`.sq` files): Go's syntax plus squark's
keywords (`arena`, `extern`, `in`, `task`, `try`), size literals (`64K`, `8M`), vector types
(`[..N]T`), directives (`budget`, `pool`, `threads`, `workers`, `link`, `cflags`, `ldflags`),
the words that are keywords only in place (`shared var`, `thread var`, `select {`, `pool N` and
`stack N` after a signature, `blocking`, `preemptible`, `cap N` on map and channel storage, the
provenance words `from`, `static` and `into`) and `//squark:` pragmas. Comment toggling, bracket
matching and auto-indent come from `language-configuration.json`.

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
