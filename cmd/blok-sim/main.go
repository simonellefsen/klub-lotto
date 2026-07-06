// Command blok-sim runs the blok solver (BlokPlan) over many simulated games and
// prints the score distribution — an offline harness for tuning the heuristic
// without burning the single live game per day. Deterministic per --seed, so two
// builds can be compared on the exact same game sequence (paired A/B).
//
//	go run ./cmd/blok-sim -n 300 -seed 1
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/klublotto"
)

func main() {
	n := flag.Int("n", 300, "number of games to simulate")
	seed := flag.Int64("seed", 1, "base RNG seed (game i uses seed+i for reproducibility)")
	lod := flag.Int("lod", 200, "daily-lod threshold to report the pass rate for")
	// Heuristic weights (default to production; override to A/B a change).
	flag.IntVar(&klublotto.BlokWSurvival, "w-survival", klublotto.BlokWSurvival, "survival weight per placed piece")
	flag.IntVar(&klublotto.BlokWClear, "w-clear", klublotto.BlokWClear, "reward per line cleared")
	flag.IntVar(&klublotto.BlokWChain, "w-chain", klublotto.BlokWChain, "multiplier on real chain-bonus points in the lookahead")
	flag.IntVar(&klublotto.BlokWChainState, "w-chainstate", klublotto.BlokWChainState, "terminal value per live chain step")
	flag.IntVar(&klublotto.BlokWDead, "w-dead", klublotto.BlokWDead, "penalty per dead hole")
	flag.IntVar(&klublotto.BlokWTight, "w-tight", klublotto.BlokWTight, "penalty per tight gap")
	flag.IntVar(&klublotto.BlokWNear, "w-near", klublotto.BlokWNear, "reward per near-line unit")
	flag.Parse()
	fmt.Printf("weights: survival=%d clear=%d chain=%d chainstate=%d dead=%d tight=%d near=%d\n",
		klublotto.BlokWSurvival, klublotto.BlokWClear, klublotto.BlokWChain, klublotto.BlokWChainState,
		klublotto.BlokWDead, klublotto.BlokWTight, klublotto.BlokWNear)

	scores := make([]int, *n)
	cells := make([]int, *n)
	lodHits := 0
	start := time.Now()
	for i := 0; i < *n; i++ {
		rng := rand.New(rand.NewSource(*seed + int64(i)))
		res := klublotto.SimulateBlokGame(rng, klublotto.BlokPlan)
		scores[i] = res.Score
		cells[i] = res.Cells
		if res.Score >= *lod {
			lodHits++
		}
	}
	elapsed := time.Since(start)

	sort.Ints(scores)
	sort.Ints(cells)
	sum, cellSum := 0, 0
	for i := range scores {
		sum += scores[i]
		cellSum += cells[i]
	}
	mean := float64(sum) / float64(*n)
	pct := func(p float64) int { return scores[int(p*float64(*n-1))] }

	fmt.Printf("games=%d  seed=%d  (%.1fs, %.0f games/s)\n", *n, *seed, elapsed.Seconds(), float64(*n)/elapsed.Seconds())
	fmt.Printf("score:  mean=%.0f  min=%d  p25=%d  median=%d  p75=%d  p90=%d  max=%d\n",
		mean, scores[0], pct(0.25), pct(0.50), pct(0.75), pct(0.90), scores[*n-1])
	fmt.Printf("cells:  mean=%.0f  median=%d  (bonus share of score: %.0f%%)\n",
		float64(cellSum)/float64(*n), cells[*n/2], 100*(1-float64(cellSum)/float64(sum)))
	fmt.Printf("lod≥%d: %.0f%% of games (%d/%d)\n", *lod, 100*float64(lodHits)/float64(*n), lodHits, *n)
}
