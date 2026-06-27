package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/config"
	"github.com/simonellefsen/klub-lotto/internal/klublotto"
)

// runBlok plays Blok for Blok end to end: open the parent page, click "Start
// spil", then loop — perceive the board+tray from a screenshot, plan the best
// move with full-trio lookahead, drag the piece, verify, and read the live score
// from the DOM — until the board can't take another piece (game over) or an
// optional --goal score is reached. This is the Go port of the former Python
// tools/blok (perception + solver live in internal/klublotto/blok.go).
func runBlok(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("blok", flag.ContinueOnError)
	headlessFlag := fs.Bool("headless", false, "force headless browser")
	goalFlag := fs.Int("goal", 0, "stop once the live score reaches this (0 = play to game-over for max score)")
	maxStepsFlag := fs.Int("max-steps", 2000, "safety cap on the number of placement attempts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// BLOK_GOAL env mirrors the old Makefile knob (GOAL=...).
	goal := *goalFlag
	if goal == 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("BLOK_GOAL"))); err == nil {
			goal = v
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	br := gameBrowser(cfg, *headlessFlag)
	restartHeadedSession(ctx, br)
	d := &blokDriver{br: br, shotDir: cfg.DataDir}

	fmt.Println("[1/3] opening Dagens Blok for Blok...")
	openCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	err = openGameWithLogin(openCtx, br, cfg, klublotto.OpenBlok)
	curURL, _ := br.URL(openCtx)
	cancel()
	if err != nil {
		return err
	}
	fmt.Println("       at:", curURL)

	fmt.Println("[2/3] starting game (Start spil)...")
	d.startGame(ctx)

	if goal > 0 {
		fmt.Printf("[3/3] playing — stop at score >= %d (shots in %s)\n", goal, d.shotDir)
	} else {
		fmt.Printf("[3/3] playing to game-over for max score (shots in %s)\n", d.shotDir)
	}
	res, err := d.play(ctx, goal, *maxStepsFlag)
	if err != nil {
		return err
	}
	return logBlokScore(ctx, cfg, res)
}

// blokDailyThreshold is the score that earns the daily lod in Blok for Blok.
const blokDailyThreshold = 200

// logBlokScore records the final Blok score in the daily ledger, mirroring how
// the other games upsert their result. The lod is awarded at blokDailyThreshold,
// so we mark it registered once the best score passes it.
func logBlokScore(ctx context.Context, cfg *config.Config, res blokResult) error {
	passed := res.best >= blokDailyThreshold
	recPath := filepath.Join(cfg.DataDir, "blok-scores.csv")

	answer := fmt.Sprintf("Score %d · high score %d", res.current, res.best)
	lod := fmt.Sprintf("daily lod earned (passed %d)", blokDailyThreshold)
	if !passed {
		lod = fmt.Sprintf("did not reach the %d-point daily lod", blokDailyThreshold)
	}
	notes := fmt.Sprintf("Played to game-over with the native Go solver; %s. Day's final score %d (high score %d) over %d placements. Per-move score record: `%s`.",
		lod, res.current, res.best, res.placed, recPath)

	fmt.Printf("       ledger: %s — %s\n", answer, lod)
	return upsertDailyGame(ctx, cfg, "Blok for Blok",
		fmt.Sprintf("Reach %d points (1010!-style block puzzle)", blokDailyThreshold),
		answer, res.scored, passed, notes)
}

// blokDriver bundles the browser session + screenshot dir and the small set of
// canvas-driving primitives the loop needs.
type blokDriver struct {
	br      *browser.Client
	shotDir string
}

// abTimeout bounds each browser op so a hung agent-browser command (e.g. a frame
// switch into the game iframe after it's been replaced by the game-over screen)
// can never freeze the whole run.
const abTimeout = 20 * time.Second

func (d *blokDriver) op(ctx context.Context, fn func(context.Context) error) {
	c, cancel := context.WithTimeout(ctx, abTimeout)
	defer cancel()
	_ = fn(c)
}

func (d *blokDriver) eval(ctx context.Context, js string) string {
	c, cancel := context.WithTimeout(ctx, abTimeout)
	defer cancel()
	out, _ := d.br.Eval(c, js)
	return out
}

// resizeFix recovers the cross-origin canvas, which collapses to ~30px on
// (re)embed; an in-frame resize event restores it to full size. Harmless when
// not collapsed. Leaves the frame on main.
func (d *blokDriver) resizeFix(ctx context.Context) {
	d.op(ctx, func(c context.Context) error { return klublotto.EnterGameFrame(c, d.br) })
	d.eval(ctx, `(function(){window.dispatchEvent(new Event("resize"));return 1;})()`)
	d.op(ctx, func(c context.Context) error { return d.br.Frame(c, "main") })
}

func (d *blokDriver) shot(ctx context.Context, name string) string {
	p := filepath.Join(d.shotDir, name)
	d.op(ctx, func(c context.Context) error { return d.br.Screenshot(c, p) })
	return p
}

// neutral is an off-board, in-viewport spot to release a stuck pickup.
var blokNeutral = [2]int{40, 380}

func (d *blokDriver) mouseReleaseSafe(ctx context.Context) {
	d.op(ctx, func(c context.Context) error { return d.br.MouseMove(c, blokNeutral[0], blokNeutral[1]) })
	time.Sleep(100 * time.Millisecond)
	d.op(ctx, func(c context.Context) error { return d.br.MouseUp(c) })
	time.Sleep(300 * time.Millisecond)
}

func (d *blokDriver) startGame(ctx context.Context) {
	d.op(ctx, func(c context.Context) error { return klublotto.EnterGameFrame(c, d.br) })
	out := d.eval(ctx, `(function(){var els=[...document.querySelectorAll("div,button,span")]`+
		`.filter(e=>/start spil/i.test(e.textContent)&&e.offsetParent);`+
		`if(els.length){els[els.length-1].click();return "started";}`+
		`return "no start button (already in play?)";})()`)
	fmt.Println("       " + strings.Trim(strings.TrimSpace(out), `"`))
	time.Sleep(1500 * time.Millisecond)
	d.eval(ctx, `(function(){window.dispatchEvent(new Event("resize"));return 1})()`)
	d.op(ctx, func(c context.Context) error { return d.br.Frame(c, "main") })
	time.Sleep(time.Second)
}

// drag does a smooth stepped pointer drag: Phaser only lifts the piece for many
// small continuous steps; big jumps are rejected. Pick up, hold, glide, release.
func (d *blokDriver) drag(ctx context.Context, px, py, qx, qy int) {
	const steps = 30
	d.op(ctx, func(c context.Context) error { return d.br.MouseMove(c, px, py) })
	time.Sleep(200 * time.Millisecond)
	d.op(ctx, func(c context.Context) error { return d.br.MouseDown(c) })
	time.Sleep(320 * time.Millisecond)
	for i := 1; i <= steps; i++ {
		x := px + (qx-px)*i/steps
		y := py + (qy-py)*i/steps
		d.op(ctx, func(c context.Context) error { return d.br.MouseMove(c, x, y) })
		time.Sleep(35 * time.Millisecond)
	}
	time.Sleep(180 * time.Millisecond)
	d.op(ctx, func(c context.Context) error { return d.br.MouseUp(c) })
	time.Sleep(700 * time.Millisecond)
}

// readScores reads the live (current, best) score from the iframe DOM
// (.game-current-score-value / .game-best-score-value). Plain text nodes, so the
// read is exact and immune to the stale-canvas problem. ok=false for a value
// that couldn't be read (e.g. the win/game-over screen replaced the board).
func (d *blokDriver) readScores(ctx context.Context) (cur, best int, ok bool) {
	d.op(ctx, func(c context.Context) error { return klublotto.EnterGameFrame(c, d.br) })
	out := d.eval(ctx, `(function(){function n(s){var e=document.querySelector(s);`+
		`if(!e)return "";return (e.textContent||"").replace(/[^0-9]/g,"");}`+
		`return n(".game-current-score-value")+"|"+n(".game-best-score-value");})()`)
	d.op(ctx, func(c context.Context) error { return d.br.Frame(c, "main") })
	out = strings.Trim(strings.TrimSpace(out), `"`)
	parts := strings.Split(out, "|")
	if len(parts) != 2 {
		return 0, 0, false
	}
	cur, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	best, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return cur, best, true
}

type blokReading struct {
	board  [8][8]int
	geom   klublotto.BlokGeom
	pieces []klublotto.BlokPiece
}

func blokKey(b [8][8]int, pieces []klublotto.BlokPiece) string {
	var sb strings.Builder
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			sb.WriteByte(byte('0' + b[r][c]))
		}
	}
	sb.WriteByte('|')
	for _, p := range pieces {
		fmt.Fprintf(&sb, "%dx%d:", p.H, p.W)
		for _, row := range p.Shape {
			for _, v := range row {
				sb.WriteByte(byte('0' + v))
			}
		}
		sb.WriteByte(';')
	}
	return sb.String()
}

// readSettled returns a reading once two consecutive perceptions agree (guards
// against catching a mid-animation frame), retrying through blank/collapsed
// canvas frames via resizeFix. ok=false means the board could not be read
// (canvas blank/collapsed — at the end of a healthy run this is the game-over
// screen replacing the board).
func (d *blokDriver) readSettled(ctx context.Context, tries int) (blokReading, bool) {
	var prev string
	var last blokReading
	haveLast := false
	for i := 0; i < tries; i++ {
		d.resizeFix(ctx)
		time.Sleep(350 * time.Millisecond)
		p := d.shot(ctx, "rd.png")
		board, geom, pieces, err := klublotto.ReadBlokScreenshot(p)
		if err != nil {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		last = blokReading{board: board, geom: geom, pieces: pieces}
		haveLast = true
		key := blokKey(board, pieces)
		if key == prev && len(pieces) > 0 {
			return last, true
		}
		prev = key
		time.Sleep(300 * time.Millisecond)
	}
	return last, haveLast
}

// releaseVP returns the viewport pixel to release at so the piece's top-left
// lands on board cell (r,c): the footprint centre, clamped inside the 8x8 board.
func releaseVP(geom klublotto.BlokGeom, shape [][]int, r, c int) (int, int) {
	bxv := float64(geom.X0) / 2
	byv := float64(geom.Y0) / 2
	cv := geom.Cell / 2
	h, w := len(shape), len(shape[0])
	rx := bxv + (float64(c)+float64(w)/2)*cv
	ry := byv + (float64(r)+float64(h)/2)*cv
	rx = clampF(rx, bxv+cv*0.35, bxv+8*cv-cv*0.35)
	ry = clampF(ry, byv+cv*0.35, byv+8*cv-cv*0.35)
	return int(rx), int(ry)
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// placeAndVerify drags the piece and confirms it landed by comparing the new
// read against the PREDICTED board (so a placement that immediately clears a
// line — which can leave the board identical or emptier — still reads as
// success). Returns ok=false only when nothing changed (a rejected drag).
func (d *blokDriver) placeAndVerify(ctx context.Context, board [8][8]int, geom klublotto.BlokGeom, piece klublotto.BlokPiece, r, c int) (bool, [8][8]int) {
	predicted, _ := klublotto.BlokApply(board, piece.Shape, r, c)
	rx, ry := releaseVP(geom, piece.Shape, r, c)
	d.drag(ctx, piece.PickX, piece.PickY, rx, ry)
	read, ok := d.readSettled(ctx, 6)
	if !ok {
		return true, predicted // board gone (game over) — let the caller's score read decide
	}
	if read.board == predicted {
		return true, read.board
	}
	if read.board == board {
		d.mouseReleaseSafe(ctx) // nothing changed → ensure nothing is held
		return false, read.board
	}
	// Unexpected board: the drag did something, or perception drifted. Soft-success
	// — the next receding-horizon read re-plans from reality.
	return true, read.board
}

func blokCells(b [8][8]int) int {
	n := 0
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			n += b[r][c]
		}
	}
	return n
}

func pieceCells(s [][]int) int {
	n := 0
	for _, row := range s {
		for _, v := range row {
			n += v
		}
	}
	return n
}

// blokResult is the final state of a play loop, surfaced so runBlok can log the
// score to the daily ledger. current/best are the last live scores read off the
// iframe before the game-over screen replaced the board; scored is false if we
// never managed to read a score (e.g. immediate game-over).
type blokResult struct {
	current int
	best    int
	steps   int
	placed  int
	scored  bool
}

func (d *blokDriver) play(ctx context.Context, goal, maxSteps int) (blokResult, error) {
	var res blokResult
	recPath := filepath.Join(d.shotDir, "blok-scores.csv")
	rec, err := os.Create(recPath)
	if err != nil {
		return res, err
	}
	defer rec.Close()
	fmt.Fprintln(rec, "step,piece,row,col,current_score,best_score,placed_cells,board_filled")

	placed, steps, stuck := 0, 0, 0
	bad := map[string]bool{} // (piece-shape,r,c) moves to avoid this trio
	lastSig := ""

	badKey := func(shape [][]int, r, c int) string {
		var sb strings.Builder
		for _, row := range shape {
			for _, v := range row {
				sb.WriteByte(byte('0' + v))
			}
			sb.WriteByte('/')
		}
		fmt.Fprintf(&sb, "@%d,%d", r, c)
		return sb.String()
	}

	for steps < maxSteps {
		select {
		case <-ctx.Done():
			res.steps, res.placed = steps, placed
			return res, ctx.Err()
		default:
		}
		steps++
		read, ok := d.readSettled(ctx, 6)
		if !ok {
			d.shot(ctx, "blok_final.png")
			fmt.Printf("[%d] board gone — likely game over (win/game-over screen). placed~%d. See blok_final.png\n", steps, placed)
			fmt.Printf("score record written to %s\n", recPath)
			res.steps, res.placed = steps, placed
			return res, nil
		}
		sig := blokKey(read.board, read.pieces)
		if sig != lastSig { // board/tray changed → fresh trio context
			bad = map[string]bool{}
			lastSig = sig
		}
		if len(read.pieces) == 0 {
			fmt.Printf("[%d] tray empty; waiting for refill\n", steps)
			time.Sleep(1200 * time.Millisecond)
			continue
		}
		shapes := make([][][]int, len(read.pieces))
		for i, p := range read.pieces {
			shapes[i] = p.Shape
		}
		ranked := klublotto.BlokPlan(read.board, shapes)
		// Drop moves already known to fail for this trio.
		var mv *klublotto.BlokScoredMove
		for i := range ranked {
			m := ranked[i]
			if !bad[badKey(shapes[m.Pi], m.R, m.C)] {
				mv = &ranked[i]
				break
			}
		}
		if mv == nil {
			stuck++
			fmt.Printf("[%d] no non-failed move available (stuck=%d)\n", steps, stuck)
			if stuck >= 3 {
				fmt.Println("GAME OVER / unsolvable from here")
				break
			}
			time.Sleep(800 * time.Millisecond)
			continue
		}
		piece := read.pieces[mv.Pi]
		placedOK, board2 := d.placeAndVerify(ctx, read.board, read.geom, piece, mv.R, mv.C)
		if !placedOK {
			bad[badKey(piece.Shape, mv.R, mv.C)] = true
			fmt.Printf("[%d] FAILED %dx%d@(%d,%d) — will try next-best\n", steps, piece.H, piece.W, mv.R, mv.C)
			continue
		}
		stuck = 0
		placed += pieceCells(piece.Shape)
		lastSig = blokKey(board2, nil) // force fresh trio detection next loop

		cur, best, scoreOK := d.readScores(ctx)
		curS, bestS := "", ""
		if scoreOK {
			curS, bestS = strconv.Itoa(cur), strconv.Itoa(best)
			res.current, res.best, res.scored = cur, best, true
		}
		fmt.Fprintf(rec, "%d,%dx%d,%d,%d,%s,%s,%d,%d\n",
			steps, piece.H, piece.W, mv.R, mv.C, curS, bestS, placed, blokCells(board2))
		fmt.Printf("[%d] placed %dx%d@(%d,%d)  score=%s  best=%s  placed~%d  board_filled=%d\n",
			steps, piece.H, piece.W, mv.R, mv.C, orQ(curS), orQ(bestS), placed, blokCells(board2))

		if !scoreOK {
			// Score element gone → board replaced by the win/game-over screen.
			// Confirm once (guard a transient mid-animation miss), then finish.
			time.Sleep(800 * time.Millisecond)
			if _, _, ok2 := d.readScores(ctx); !ok2 {
				fmt.Printf("[%d] score element gone — game over / completed. Finishing.\n", steps)
				break
			}
		}
		if goal > 0 && scoreOK && cur >= goal {
			fmt.Printf("[%d] GOAL REACHED: current score %d >= %d\n", steps, cur, goal)
			break
		}
	}
	d.shot(ctx, "blok_final.png")
	fmt.Printf("score record written to %s\n", recPath)
	fmt.Printf("DONE: ~%d cells placed in %d steps\n", placed, steps)
	res.steps, res.placed = steps, placed
	return res, nil
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}
