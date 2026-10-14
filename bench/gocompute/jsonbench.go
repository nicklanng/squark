package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
)

type Item struct {
	SKU        string `json:"sku"`
	Name       string `json:"name"`
	Qty        int    `json:"qty"`
	PriceCents int64  `json:"price_cents"`
}

type Order struct {
	ID       int64  `json:"id"`
	Customer string `json:"customer"`
	Email    string `json:"email"`
	Paid     bool   `json:"paid"`
	Items    []Item `json:"items"`
}

// jsonBench encodes an Order with 10 items n times into a reused buffer and
// returns the total number of bytes produced (including the newline that
// json.Encoder appends after each value). id and paid change per iteration.
func jsonBench(n int) int64 {
	o := Order{
		Customer: "Ada Lovelace",
		Email:    "ada@example.com",
		Items:    make([]Item, 10),
	}
	for i := range o.Items {
		o.Items[i] = Item{
			SKU:        "SKU-" + strconv.Itoa(1000+i),
			Name:       "widget " + strconv.Itoa(i),
			Qty:        i + 1,
			PriceCents: int64(199 * (i + 1)),
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var total int64
	for i := 0; i < n; i++ {
		o.ID = int64(i)
		o.Paid = i%2 == 0
		buf.Reset()
		if err := enc.Encode(&o); err != nil {
			panic(err)
		}
		total += int64(buf.Len())
		buf.WriteTo(io.Discard)
	}
	return total
}
