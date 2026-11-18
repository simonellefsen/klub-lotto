package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
)

// kaedeordLodPoints is the score that earns the daily lod.
const kaedeordLodPoints = 50

// kaedeordClearBonus is what emptying the board adds (the game's rules page
// and puzzle both say 100).
const kaedeordClearBonus = 100

// runKaedeord plays Dagens Kædeord: open the parent page, enter the board, read
// it from the DOM, plan the whole game with the beam-search solver over the
// game's own word list, then trace each word with real mouse clicks. After
// every step the live board must match the simulated one; if it ever doesn't,
// the rest is re-planned from the live board. A board the plan doesn't clear is
// ended with Afslut → Bekræft so the score is submitted.
func runKaedeord(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kaedeord", flag.ContinueOnError)
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	dryRun := fs.Bool("dry-run", false, "read the board and print the plan; play nothing")
	beamFlag := fs.Int("beam", 200, "positions the solver keeps per depth")
	budgetFlag := fs.Duration("budget", time.Minute, "time limit for each solve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)
	d := &kaedeDriver{br: br, beam: *beamFlag, budget: *budgetFlag}

	fmt.Println("[1/4] opening Dagens Kædeord...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenKaedeord)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	fmt.Println("[2/4] entering the board...")
	enterCtx, enterCancel := context.WithTimeout(ctx, 80*time.Second)
	live, err := klublotto.EnterKaedeord(enterCtx, br, klublotto.OpenKaedeord, func(p klublotto.KaedeordPoint) error {
		return d.click(enterCtx, p)
	})
	enterCancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, klublotto.ErrAlreadyPlayed) {
			return err
		}
		return fmt.Errorf("kædeord did not load: %w — stopped before any move; rerun `make kaedeord`", err)
	}
	defer klublotto.LeaveFrame(br)
	fmt.Printf("       %d×%d board, %d tiles, score %d, %d delete(s), %d swap(s) left\n",
		live.Board.Width, live.Board.Height, len(live.Board.Tiles), live.Score, live.Deletes, live.Swaps)
	fmt.Print(indent(live.Board.String(), "       "))

	fmt.Println("[3/4] solving...")
	dict, file, err := klublotto.LoadKaedeordDict(ctx, live.WordList, cfg.DataDir)
	if err != nil {
		return err
	}
	d.dict = dict
	fmt.Printf("       word list: %d words (%s)\n", dict.Words, file)
	plan := d.solve(live)
	printKaedeordPlan(plan, live.Score)
	if *dryRun {
		fmt.Println("[4/4] dry run — playing nothing.")
		return nil
	}
	if plan.Score+max(live.Score, 0) < kaedeordLodPoints {
		return fmt.Errorf("best plan scores %d, under the %d points for the lod — leaving the game open", plan.Score+max(live.Score, 0), kaedeordLodPoints)
	}

	fmt.Println("[4/4] playing...")
	res, err := d.play(ctx, live, plan)
	if err != nil {
		return err
	}
	return logKaedeord(ctx, cfg, res)
}

// kaedeDriver holds the browser session and the solver settings, and turns
// plan steps into clicks.
type kaedeDriver struct {
	br     *browser.Client
	dict   *klublotto.KaedeordDict
	beam   int
	budget time.Duration
	origin klublotto.KaedeordPoint // the game iframe's top-left on the page
	avoid  map[string]bool         // words the game turned down this run
}

func (d *kaedeDriver) solve(live klublotto.KaedeordLive) klublotto.KaedeordPlan {
	start := time.Now()
	defer func() {
		fmt.Printf("       solved in %s (beam %d)\n", time.Since(start).Round(100*time.Millisecond), d.beam)
	}()
	return klublotto.SolveKaedeord(live.Board, d.dict, klublotto.KaedeordOptions{
		Beam:       d.beam,
		ClearBonus: kaedeordClearBonus,
		Deletes:    live.Deletes,
		Swaps:      live.Swaps,
		Deadline:   time.Now().Add(d.budget),
		Avoid:      d.avoid,
	})
}

// locate records where the game iframe sits on the page, so a point inside the
// frame can be clicked with real mouse events. It leaves the browser in the
// game frame.
func (d *kaedeDriver) locate(ctx context.Context) error {
	klublotto.LeaveFrame(d.br)
	raw, err := d.br.Eval(ctx, `JSON.stringify((()=>{const f=document.querySelector("iframe.kl-game__iframe");if(!f)return null;f.scrollIntoView({block:"nearest"});const r=f.getBoundingClientRect();return {x:r.x,y:r.y};})())`)
	if err == nil {
		var p *klublotto.KaedeordPoint
		if json.Unmarshal([]byte(raw), &p) == nil && p != nil {
			d.origin = *p
		} else {
			err = fmt.Errorf("game iframe not found on the page")
		}
	}
	if ferr := klublotto.EnterGameFrame(ctx, d.br); err == nil {
		err = ferr
	}
	return err
}

func (d *kaedeDriver) click(ctx context.Context, p klublotto.KaedeordPoint) error {
	if p == (klublotto.KaedeordPoint{}) {
		return fmt.Errorf("nothing to click")
	}
	if d.origin == (klublotto.KaedeordPoint{}) {
		if err := d.locate(ctx); err != nil {
			return err
		}
	}
	return d.br.MouseClick(ctx, int(d.origin.X+p.X+0.5), int(d.origin.Y+p.Y+0.5))
}

func (d *kaedeDriver) read(ctx context.Context) (klublotto.KaedeordLive, error) {
	return klublotto.ReadKaedeordLive(ctx, d.br)
}

// kaedeResult is what a run achieved.
type kaedeResult struct {
	words   []klublotto.KaedeordMove
	deletes int
	swaps   int
	points  int  // the live counter when the game was ended
	cleared bool // the board was emptied (+kaedeordClearBonus)
	ended   bool // the game took the result: its end screen showed
	endText string
	replans int
}

func (r kaedeResult) score() int {
	if r.cleared {
		return r.points + kaedeordClearBonus
	}
	return r.points
}

// kaedeMaxReplans caps how often a run re-plans after the live board stops
// matching the plan before it gives up and leaves the game open.
const kaedeMaxReplans = 4

func (d *kaedeDriver) play(ctx context.Context, live klublotto.KaedeordLive, plan klublotto.KaedeordPlan) (kaedeResult, error) {
	var res kaedeResult
	res.points = live.Score
	if err := d.locate(ctx); err != nil {
		return res, err
	}
	for n := 1; len(plan.Steps) > 0; n++ {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		step := plan.Steps[0]
		expect := live.Board.Apply(step)
		fmt.Printf("   [%d] %s\n", n, step)
		if err := d.do(ctx, live, step); err != nil {
			fmt.Printf("       could not play it: %v\n", err)
		}
		got, ok := d.settle(ctx, expect)
		if ok {
			plan.Steps = plan.Steps[1:]
			switch step.Kind {
			case klublotto.KaedeordWord:
				res.words = append(res.words, step.Move)
			case klublotto.KaedeordDelete:
				res.deletes++
			case klublotto.KaedeordSwap:
				res.swaps++
			}
			if len(got.Board.Tiles) == 0 || got.Screen != "board" {
				res.cleared = true
				res.points += step.Move.Points
				break
			}
			res.points = got.Score
			fmt.Printf("       ok — score %d, %d tiles left\n", got.Score, len(got.Board.Tiles))
			live = got
			continue
		}
		// The board isn't what the plan expected: play on from what is there. A
		// word that changed nothing was turned down, so don't plan it again.
		if step.Kind == klublotto.KaedeordWord && got.Board.Key() == live.Board.Key() {
			if d.avoid == nil {
				d.avoid = map[string]bool{}
			}
			d.avoid[step.Move.Word] = true
		}
		res.replans++
		if res.replans > kaedeMaxReplans {
			return res, fmt.Errorf("the live board kept diverging from the plan (%d re-plans) — game left open at score %d; rerun `make kaedeord` to resume", res.replans-1, res.points)
		}
		fmt.Printf("       board differs from the plan — re-planning from the live board (%d/%d)\n", res.replans, kaedeMaxReplans)
		if got.Screen != "board" {
			return res, fmt.Errorf("game left the board (screen %q) mid-plan", got.Screen)
		}
		if err := d.locate(ctx); err != nil {
			return res, err
		}
		live = got
		res.points = got.Score
		plan = d.solve(live)
		printKaedeordPlan(plan, live.Score)
	}

	if res.cleared {
		fmt.Println("       board cleared — waiting for the result screen")
	} else {
		if err := d.end(ctx); err != nil {
			return res, err
		}
	}
	end, ok := d.waitScreen(ctx, 15*time.Second, "done", "answered")
	res.ended = ok
	res.endText = end.Main
	if res.endText == "" {
		res.endText = end.Text
	}
	res.endText = strings.Join(strings.Fields(res.endText), " ")
	if ok {
		fmt.Printf("       result screen: %.160s\n", res.endText)
	} else {
		fmt.Printf("       warning: no result screen within 15s (last: %s)\n", end.Screen)
	}
	return res, nil
}

// do performs one step with clicks. The caller checks the outcome.
func (d *kaedeDriver) do(ctx context.Context, live klublotto.KaedeordLive, step klublotto.KaedeordStep) error {
	switch step.Kind {
	case klublotto.KaedeordDelete:
		return d.lifeline(ctx, live, "delete", step.Tiles[0], step.Tiles[0], step.Tiles[0])
	case klublotto.KaedeordSwap:
		return d.lifeline(ctx, live, "swap", step.Tiles[0], step.Tiles[1], step.Tiles[1])
	}
	return d.trace(ctx, live, step.Move.IDs)
}

// trace clicks a word's tiles in order, checking after each click that the
// game took it, then clicks the last tile again to submit. A click that didn't
// take is retried; a selection that broke is cleared and the word restarted.
func (d *kaedeDriver) trace(ctx context.Context, live klublotto.KaedeordLive, ids []int) error {
	baseline := live.Classes
	for attempt := 1; attempt <= 3; attempt++ {
		traced := 0
		for k, id := range ids {
			took := false
			for try := 0; try < 3 && !took; try++ {
				if try > 0 {
					time.Sleep(300 * time.Millisecond)
				}
				if err := d.click(ctx, live.At[id]); err != nil {
					return err
				}
				cur, err := d.read(ctx)
				if err != nil {
					continue
				}
				sel := cur.Selected(baseline)
				if containsAll(sel, ids[:k+1]) {
					took = true
				} else if k > 0 && !containsInt(sel, ids[0]) {
					break // the path was reset: start the word again
				}
			}
			if !took {
				break
			}
			traced++
		}
		if traced == len(ids) {
			return d.click(ctx, live.At[ids[len(ids)-1]])
		}
		fmt.Printf("       selection broke after %d/%d letters — retrying the word\n", traced, len(ids))
		d.clearSelection(ctx, live, ids[0])
	}
	return fmt.Errorf("could not trace %d tiles", len(ids))
}

// clearSelection empties a half-made selection: clicking the first selected
// tile clears it.
func (d *kaedeDriver) clearSelection(ctx context.Context, live klublotto.KaedeordLive, first int) {
	for i := 0; i < 3; i++ {
		cur, err := d.read(ctx)
		if err != nil || len(cur.Selected(live.Classes)) == 0 {
			return
		}
		_ = d.click(ctx, live.At[first])
		time.Sleep(200 * time.Millisecond)
	}
}

// lifeline switches to the delete or swap mode, then clicks the tiles: delete
// is the tile twice, swap is the first tile, the second, then the second again.
func (d *kaedeDriver) lifeline(ctx context.Context, live klublotto.KaedeordLive, button string, ids ...int) error {
	if err := d.click(ctx, live.Buttons[button]); err != nil {
		return fmt.Errorf("%s button: %w", button, err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, id := range ids {
		if err := d.click(ctx, live.At[id]); err != nil {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

// settle waits for the step's animations to finish and reports whether the
// board ended up as expected. An expected empty board is met by the game
// leaving the board for its result screen.
func (d *kaedeDriver) settle(ctx context.Context, expect klublotto.KaedeordBoard) (klublotto.KaedeordLive, bool) {
	want := expect.Key()
	deadline := time.Now().Add(6 * time.Second)
	var last klublotto.KaedeordLive
	for {
		time.Sleep(250 * time.Millisecond)
		if cur, err := d.read(ctx); err == nil {
			last = cur
			if len(expect.Tiles) == 0 && (cur.Screen != "board" || len(cur.Board.Tiles) == 0) {
				return cur, true
			}
			if cur.Screen == "board" && cur.Board.Key() == want {
				// The grid can still be easing to its new width; let it land so
				// the next click points are final.
				time.Sleep(450 * time.Millisecond)
				if again, err := d.read(ctx); err == nil && again.Board.Key() == want {
					return again, true
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return last, false
		}
	}
}

// end submits the result of a board that wasn't cleared: Afslut, then Bekræft
// on the "are you sure" dialog.
func (d *kaedeDriver) end(ctx context.Context) error {
	cur, err := d.read(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("       ending the game at %d points (Afslut → Bekræft)\n", cur.Score)
	if err := d.click(ctx, cur.Buttons["end"]); err != nil {
		return fmt.Errorf("Afslut: %w", err)
	}
	confirm, ok := d.waitScreen(ctx, 5*time.Second, "confirm")
	if !ok {
		return fmt.Errorf("no confirm dialog after Afslut (screen %q)", confirm.Screen)
	}
	time.Sleep(400 * time.Millisecond) // the dialog animates in
	if confirm, err = d.read(ctx); err != nil {
		return err
	}
	if err := d.click(ctx, confirm.Buttons["confirm"]); err != nil {
		return fmt.Errorf("Bekræft: %w", err)
	}
	return nil
}

func (d *kaedeDriver) waitScreen(ctx context.Context, budget time.Duration, screens ...string) (klublotto.KaedeordLive, bool) {
	deadline := time.Now().Add(budget)
	var last klublotto.KaedeordLive
	for {
		if cur, err := d.read(ctx); err == nil {
			last = cur
			for _, s := range screens {
				if cur.Screen == s {
					return cur, true
				}
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func printKaedeordPlan(p klublotto.KaedeordPlan, start int) {
	cleared := ""
	if p.Cleared {
		cleared = fmt.Sprintf(" + %d for clearing the board", kaedeordClearBonus)
	}
	fmt.Printf("       plan: %d steps, %d word points%s → final %d\n", len(p.Steps), p.Points, cleared, max(start, 0)+p.Score)
	for i, s := range p.Steps {
		fmt.Printf("       %2d. %s\n", i+1, s)
	}
}

// logKaedeord records the result in the daily ledger. Only a game that reached
// its result screen counts as submitted; the lod also needs 50 points.
func logKaedeord(ctx context.Context, cfg *config.Config, res kaedeResult) error {
	score := res.score()
	passed := res.ended && score >= kaedeordLodPoints
	var words []string
	for _, m := range res.words {
		words = append(words, fmt.Sprintf("%s (+%d)", m.Word, m.Points))
	}
	answer := fmt.Sprintf("Score %d", score)
	if res.cleared {
		answer += fmt.Sprintf(" · board cleared (+%d)", kaedeordClearBonus)
	}
	notes := fmt.Sprintf("Words: %s.", strings.Join(words, " · "))
	if res.deletes+res.swaps > 0 {
		notes += fmt.Sprintf(" Lifelines used: %d delete(s), %d swap(s).", res.deletes, res.swaps)
	}
	if res.replans > 0 {
		notes += fmt.Sprintf(" Re-planned %d time(s) from the live board.", res.replans)
	}
	if passed {
		notes += fmt.Sprintf(" Daily lod earned (%d+ points).", kaedeordLodPoints)
	} else if !res.ended {
		notes += " The game's result screen never showed — check the page."
	}
	notes += " Solved by the native Go beam search over the game's own word list."
	fmt.Printf("       ledger: %s\n", answer)
	return upsertDailyGame(ctx, cfg, "Kædeord",
		fmt.Sprintf("Chain words: trace words through a letter grid (%d points for the lod)", kaedeordLodPoints),
		answer, res.ended, passed, notes)
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func containsAll(xs, want []int) bool {
	for _, w := range want {
		if !containsInt(xs, w) {
			return false
		}
	}
	return true
}

func indent(s, pad string) string {
	var sb strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		sb.WriteString(pad + l + "\n")
	}
	return sb.String()
}
