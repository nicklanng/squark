package main

import (
	"fmt"
	"os"
	"sort"
)

// wordfreq lowercases ASCII letters, splits on anything that is not an ASCII
// letter, and prints the 10 most frequent words as "count word"
// (ties broken by word, ascending).
func wordfreq(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	counts := make(map[string]int)
	start := -1
	for i, c := range data {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
			data[i] = c
		}
		if 'a' <= c && c <= 'z' {
			if start < 0 {
				start = i
			}
		} else if start >= 0 {
			counts[string(data[start:i])]++
			start = -1
		}
	}
	if start >= 0 {
		counts[string(data[start:])]++
	}

	type wc struct {
		word  string
		count int
	}
	all := make([]wc, 0, len(counts))
	for w, c := range counts {
		all = append(all, wc{w, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].word < all[j].word
	})
	for i := 0; i < len(all) && i < 10; i++ {
		fmt.Printf("%d %s\n", all[i].count, all[i].word)
	}
	return nil
}
