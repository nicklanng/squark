package main

import (
	"bufio"
	"os"
)

// genText writes mb MiB of deterministic pseudo-text for the wordfreq benchmark.
// Words are built from a small syllable table, and a skewed LCG choice gives a
// Zipf-like frequency distribution over a vocabulary of a few thousand words.
func genText(path string, mb int) error {
	syllables := []string{
		"ka", "lo", "mi", "ra", "te", "nu", "si", "po", "ve", "da",
		"an", "or", "el", "us", "ith", "ber", "con", "tal", "mar", "fen",
		"qu", "zo", "pri", "gle", "ston", "wen", "hal", "dro", "mex", "ty",
	}
	const vocab = 4096
	words := make([]string, vocab)
	for i := range words {
		n := i
		w := ""
		for k := 0; k < 1+i%3; k++ {
			w += syllables[n%len(syllables)]
			n /= len(syllables)
			n += k * 7
		}
		if i%11 == 0 {
			w = string(w[0]-'a'+'A') + w[1:] // some capitalised words exercise lowercasing
		}
		words[i] = w
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	var state uint64 = 42
	next := func() uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state >> 33
	}
	limit := int64(mb) << 20
	var written int64
	col := 0
	for written < limit {
		// skew: squaring a uniform value favours small indexes
		r := next() % vocab
		idx := (r * r) / vocab
		word := words[idx]
		sep := " "
		switch next() % 16 {
		case 0:
			sep = ", "
		case 1:
			sep = ". "
		}
		col += len(word) + len(sep)
		if col > 72 {
			sep = "\n"
			col = 0
		}
		n1, _ := w.WriteString(word)
		n2, _ := w.WriteString(sep)
		written += int64(n1 + n2)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}
