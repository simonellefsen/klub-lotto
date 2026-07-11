// Command blok-sim runs the blok solver (BlokPlan) over many simulated games and
// prints the score distribution — an offline harness for tuning the heuristic
// without burning the single live game per day. Deterministic per --seed, so two
// builds can be compared on the exact same game sequence (paired A/B). Games
// within a batch run concurrently (they only read the shared weight vars, never
// write them), so -n scales with CPU count.
//
//	go run ./cmd/blok-sim -n 300 -seed 1
//	go run ./cmd/blok-sim -n 300 -seed 1 -compare -w-3x3 300   # production defaults vs this override, paired seeds
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/klublotto"
)

// weightSet is a snapshot of every tunable BlokW* value, so -compare can swap
// between "production defaults" and "candidate (CLI flags)" between batches.
type weightSet struct {
	survival, clear, chain, chainstate, dead, tight, near, w3x3 int
}

func currentWeights() weightSet {
	return weightSet{
		survival:   klublotto.BlokWSurvival,
		clear:      klublotto.BlokWClear,
		chain:      klublotto.BlokWChain,
		chainstate: klublotto.BlokWChainState,
		dead:       klublotto.BlokWDead,
		tight:      klublotto.BlokWTight,
		near:       klublotto.BlokWNear,
		w3x3:       klublotto.BlokW3x3,
	}
}

func applyWeights(w weightSet) {
	klublotto.BlokWSurvival = w.survival
	klublotto.BlokWClear = w.clear
	klublotto.BlokWChain = w.chain
	klublotto.BlokWChainState = w.chainstate
	klublotto.BlokWDead = w.dead
	klublotto.BlokWTight = w.tight
	klublotto.BlokWNear = w.near
	klublotto.BlokW3x3 = w.w3x3
}

func (w weightSet) String() string {
	return fmt.Sprintf("survival=%d clear=%d chain=%d chainstate=%d dead=%d tight=%d near=%d 3x3=%d",
		w.survival, w.clear, w.chain, w.chainstate, w.dead, w.tight, w.near, w.w3x3)
}

// batchResult is one seed's game outcome, kept paired by index for -compare.
type batchResult struct {
	score, cells int
}

// runBatch plays n games (seed+0 .. seed+n-1) under the CURRENT klublotto.BlokW*
// values, fanned out across GOMAXPROCS workers. The weight vars are read-only
// for the duration of the batch (never mutated by a game), so concurrent reads
// are safe.
func runBatch(n int, seed int64) []batchResult {
	out := make([]batchResult, n)
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= n {
					return
				}
				rng := rand.New(rand.NewSource(seed + int64(i)))
				res := klublotto.SimulateBlokGame(rng, klublotto.BlokPlan)
				out[i] = batchResult{score: res.Score, cells: res.Cells}
			}
		}()
	}
	wg.Wait()
	return out
}

type stats struct {
	mean, median            float64
	p25, p75, p90, min, max int
	lodHits, n              int
}

func summarize(results []batchResult, lod int) stats {
	n := len(results)
	scores := make([]int, n)
	sum := 0
	lodHits := 0
	for i, r := range results {
		scores[i] = r.score
		sum += r.score
		if r.score >= lod {
			lodHits++
		}
	}
	sort.Ints(scores)
	pct := func(p float64) int { return scores[int(p*float64(n-1))] }
	return stats{
		mean: float64(sum) / float64(n), median: float64(pct(0.5)),
		p25: pct(0.25), p75: pct(0.75), p90: pct(0.9),
		min: scores[0], max: scores[n-1],
		lodHits: lodHits, n: n,
	}
}

func (s stats) String() string {
	return fmt.Sprintf("mean=%.0f  min=%d  p25=%d  median=%.0f  p75=%d  p90=%d  max=%d  lod-rate=%.0f%% (%d/%d)",
		s.mean, s.min, s.p25, s.median, s.p75, s.p90, s.max, 100*float64(s.lodHits)/float64(s.n), s.lodHits, s.n)
}

// signTestPValue is the classical two-sided sign test (normal approximation
// with continuity correction) for "is B systematically better than A" from
// paired per-seed deltas: pos = #(B>A), neg = #(B<A), ties excluded. Small
// n or extreme skew are handled fine by the correction; this is a rough
// significance flag, not a rigorous stat — good enough to stop eyeballing
// two runs and start asking "is this within noise".
func signTestPValue(pos, neg int) float64 {
	n := pos + neg
	if n == 0 {
		return 1
	}
	mean := float64(n) / 2
	diff := math.Abs(float64(pos)-mean) - 0.5
	if diff < 0 {
		diff = 0
	}
	sd := math.Sqrt(float64(n)) / 2
	z := diff / sd
	p := 2 * (1 - 0.5*(1+math.Erf(z/math.Sqrt2)))
	if p > 1 {
		p = 1
	}
	return p
}

func main() {
	n := flag.Int("n", 300, "number of games to simulate")
	seed := flag.Int64("seed", 1, "base RNG seed (game i uses seed+i for reproducibility)")
	lod := flag.Int("lod", 200, "daily-lod threshold to report the pass rate for")
	compare := flag.Bool("compare", false, "A/B the production defaults (A) against the flag-overridden weights (B) on the same paired seeds, with a sign test")
	// Heuristic weights (default to production; override to A/B a change).
	flag.IntVar(&klublotto.BlokWSurvival, "w-survival", klublotto.BlokWSurvival, "survival weight per placed piece")
	flag.IntVar(&klublotto.BlokWClear, "w-clear", klublotto.BlokWClear, "reward per line cleared")
	flag.IntVar(&klublotto.BlokWChain, "w-chain", klublotto.BlokWChain, "multiplier on real chain-bonus points in the lookahead")
	flag.IntVar(&klublotto.BlokWChainState, "w-chainstate", klublotto.BlokWChainState, "terminal value per live chain step")
	flag.IntVar(&klublotto.BlokWDead, "w-dead", klublotto.BlokWDead, "penalty per dead hole")
	flag.IntVar(&klublotto.BlokWTight, "w-tight", klublotto.BlokWTight, "penalty per tight gap")
	flag.IntVar(&klublotto.BlokWNear, "w-near", klublotto.BlokWNear, "reward per near-line unit")
	flag.IntVar(&klublotto.BlokW3x3, "w-3x3", klublotto.BlokW3x3, "penalty when no 3x3 region fits anywhere (boxed-in-soon signal)")
	flag.Parse()

	// Capture BEFORE any batch runs: at this point the vars hold either the
	// production defaults (flag not passed) or the CLI override (flag passed).
	// In -compare mode this candidate set is B; A is reconstructed by resetting
	// each var to klublotto's zero-flag default, which flag.IntVar already
	// baked in as each flag's default value — so we snapshot defaults from the
	// flag package rather than re-declaring them by hand.
	candidate := currentWeights()

	if !*compare {
		fmt.Printf("weights: %s\n", candidate)
		start := time.Now()
		results := runBatch(*n, *seed)
		elapsed := time.Since(start)
		s := summarize(results, *lod)
		cellSum := 0
		for _, r := range results {
			cellSum += r.cells
		}
		fmt.Printf("games=%d  seed=%d  (%.1fs, %.0f games/s)\n", *n, *seed, elapsed.Seconds(), float64(*n)/elapsed.Seconds())
		fmt.Printf("score:  %s\n", s)
		fmt.Printf("cells:  mean=%.0f  (bonus share of score: %.0f%%)\n",
			float64(cellSum)/float64(*n), 100*(1-float64(cellSum)/(s.mean*float64(*n))))
		return
	}

	// -compare: rebuild the "A" (production-default) set from each flag's
	// registered default, run A, then swap in the candidate (B) and run B on
	// the identical seeds.
	defaults := weightSet{}
	flag.VisitAll(func(f *flag.Flag) {
		switch f.Name {
		case "w-survival":
			fmt.Sscanf(f.DefValue, "%d", &defaults.survival)
		case "w-clear":
			fmt.Sscanf(f.DefValue, "%d", &defaults.clear)
		case "w-chain":
			fmt.Sscanf(f.DefValue, "%d", &defaults.chain)
		case "w-chainstate":
			fmt.Sscanf(f.DefValue, "%d", &defaults.chainstate)
		case "w-dead":
			fmt.Sscanf(f.DefValue, "%d", &defaults.dead)
		case "w-tight":
			fmt.Sscanf(f.DefValue, "%d", &defaults.tight)
		case "w-near":
			fmt.Sscanf(f.DefValue, "%d", &defaults.near)
		case "w-3x3":
			fmt.Sscanf(f.DefValue, "%d", &defaults.w3x3)
		}
	})

	fmt.Printf("A (defaults):  %s\n", defaults)
	fmt.Printf("B (candidate): %s\n", candidate)

	applyWeights(defaults)
	startA := time.Now()
	resultsA := runBatch(*n, *seed)
	elapsedA := time.Since(startA)

	applyWeights(candidate)
	startB := time.Now()
	resultsB := runBatch(*n, *seed)
	elapsedB := time.Since(startB)

	sA := summarize(resultsA, *lod)
	sB := summarize(resultsB, *lod)
	fmt.Printf("games=%d  seed=%d  (A %.1fs, B %.1fs)\n", *n, *seed, elapsedA.Seconds(), elapsedB.Seconds())
	fmt.Printf("A: %s\n", sA)
	fmt.Printf("B: %s\n", sB)

	deltas := make([]int, *n)
	pos, neg, zero := 0, 0, 0
	deltaSum := 0
	for i := 0; i < *n; i++ {
		d := resultsB[i].score - resultsA[i].score
		deltas[i] = d
		deltaSum += d
		switch {
		case d > 0:
			pos++
		case d < 0:
			neg++
		default:
			zero++
		}
	}
	sort.Ints(deltas)
	meanDelta := float64(deltaSum) / float64(*n)
	medianDelta := deltas[*n/2]
	p := signTestPValue(pos, neg)
	verdict := "not distinguishable from noise"
	if p < 0.05 {
		if meanDelta > 0 {
			verdict = "B is significantly BETTER than A"
		} else {
			verdict = "B is significantly WORSE than A"
		}
	}
	fmt.Printf("delta (B-A):  mean=%+.0f  median=%+d\n", meanDelta, medianDelta)
	fmt.Printf("sign test:  %d/%d improved, %d worse, %d tied  (p=%.4f) — %s\n",
		pos, *n, neg, zero, p, verdict)
}
