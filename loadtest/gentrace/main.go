// Command gentrace writes a synthetic request trace: N requests drawn from a
// Zipf distribution over M distinct prompts (a few prompts are very common,
// most are rare). It also prints the best hit rate any cache could reach on
// the trace, so a measured hit rate can be judged against it.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
)

func main() {
	n := flag.Int("n", 10000, "number of requests")
	m := flag.Int("m", 1000, "number of distinct prompts")
	s := flag.Float64("s", 1.1, "Zipf exponent (must be > 1; higher = more skewed)")
	seed := flag.Int64("seed", 42, "random seed")
	model := flag.String("model", "gpt-4.1-mini", "model name in each request")
	out := flag.String("out", "loadtest/trace.jsonl", "output file (JSON Lines)")
	flag.Parse()

	if *s <= 1 || *m < 2 || *n < 1 {
		log.Fatal("need s > 1, m >= 2, n >= 1")
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)

	rng := rand.New(rand.NewSource(*seed))
	zipf := rand.NewZipf(rng, *s, 1, uint64(*m-1))
	seen := map[uint64]struct{}{}

	for i := 0; i < *n; i++ {
		k := zipf.Uint64()
		seen[k] = struct{}{}
		req := map[string]any{
			"model":       *model,
			"temperature": 0,
			"messages": []map[string]string{
				{"role": "user", "content": fmt.Sprintf("synthetic prompt #%d", k)},
			},
		}
		if err := enc.Encode(req); err != nil {
			log.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}

	best := 100 * float64(*n-len(seen)) / float64(*n)
	fmt.Fprintf(os.Stderr, "wrote %s: N=%d M=%d s=%.2f seed=%d; %d distinct prompts used; best possible hit rate %.1f%%\n",
		*out, *n, *m, *s, *seed, len(seen), best)
}
