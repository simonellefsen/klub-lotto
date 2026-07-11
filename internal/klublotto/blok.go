package klublotto

import (
	"context"
	"errors"
	"image/png"
	"os"
	"sort"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// Blok for Blok perception + solver. This is a faithful Go port of the original
// Python tooling (tools/blok/blok.py + the solver half of blok_play.py): same
// colour thresholds, projection profiles, band segmentation and full-trio
// lookahead. Keeping the magic numbers identical is deliberate — they were tuned
// empirically against the live WebGL canvas, and a parity test
// (TestBlokPerceptionMatchesFixture) pins the perception output to a saved
// screenshot so the port can't silently drift.
//
// The game is an 8x8 "Block Blast" style board: drop a tray of three pieces onto
// the grid, clearing full rows/columns for points. The board state can only be
// read from pixels (cross-origin WebGL canvas — no DOM game state), so we sample
// a screenshot rather than scrape the DOM.

// blokImage is a decoded screenshot with cheap per-pixel 8-bit RGB access.
type blokImage struct {
	w, h int
	pix  []uint8 // length 3*w*h, row-major RGB
}

func loadBlokImage(path string) (*blokImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	im := &blokImage{w: w, h: h, pix: make([]uint8, 3*w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := (y*w + x) * 3
			im.pix[i] = uint8(r >> 8)
			im.pix[i+1] = uint8(g >> 8)
			im.pix[i+2] = uint8(bb >> 8)
		}
	}
	return im, nil
}

func (im *blokImage) at(x, y int) (int, int, int) {
	i := (y*im.w + x) * 3
	return int(im.pix[i]), int(im.pix[i+1]), int(im.pix[i+2])
}

// BlokGeom is the detected board geometry in IMAGE pixels (the screenshot is 2x
// the CSS viewport, so the driver halves these when computing click points).
type BlokGeom struct {
	X0, Y0 int
	Cell   float64
}

// BlokPiece is one tray piece: its filled-cell shape plus a pickup point in CSS
// viewport coordinates (image pixels / 2 for the retina screenshot).
type BlokPiece struct {
	Shape        [][]int
	H, W, Cells  int
	PickX, PickY int
}

func blokIsDark(r, g, b int) bool { return r < 150 && r > 50 && g < 80 && b < 80 }

// detectBoard locates the 8x8 board by thresholding the dark-maroon grid and
// taking the bounding box from per-column / per-row projection profiles. ok is
// false when no board is present (e.g. the win/game-over screen replaced it) —
// the Python tool raised here; we signal it so the driver treats it as the board
// being gone rather than indexing off the image.
func detectBoard(im *blokImage) (x0, y0 int, cell float64, ok bool) {
	H, W := im.h, im.w
	r0 := int(0.10 * float64(H))
	r1 := int(0.50 * float64(H))
	col := make([]int, W)
	for x := 0; x < W; x++ {
		s := 0
		for y := r0; y < r1; y++ {
			if r, g, b := im.at(x, y); blokIsDark(r, g, b) {
				s++
			}
		}
		col[x] = s
	}
	colThr := float64(maxIntSlice(col)) * 0.5
	xmin, xmax := -1, -1
	for x := 0; x < W; x++ {
		if float64(col[x]) > colThr {
			if xmin < 0 {
				xmin = x
			}
			xmax = x
		}
	}
	if xmin < 0 || xmax <= xmin {
		return 0, 0, 0, false // no board columns
	}
	x0, x1 := xmin, xmax

	rr := make([]int, H)
	for y := 0; y < H; y++ {
		s := 0
		for x := x0; x <= x1; x++ {
			if r, g, b := im.at(x, y); blokIsDark(r, g, b) {
				s++
			}
		}
		rr[y] = s
	}
	rowThr := float64(maxIntSlice(rr)) * 0.5
	y0 = -1
	for y := 0; y < H; y++ {
		if float64(rr[y]) > rowThr {
			y0 = y
			break
		}
	}
	if y0 < 0 {
		return 0, 0, 0, false // no board rows
	}
	cell = float64(x1-x0) / 8.0
	return x0, y0, cell, true
}

// readBoard samples each of the 64 cell centres and classifies filled vs empty
// by the mean RGB of a small patch (empty cells are dark maroon).
func readBoard(im *blokImage) ([8][8]int, BlokGeom, bool) {
	x0, y0, cell, ok := detectBoard(im)
	var g [8][8]int
	if !ok {
		return g, BlokGeom{}, false
	}
	h := int(cell * 0.16)
	if h < 2 {
		h = 2
	}
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			cx := float64(x0) + (float64(c)+0.5)*cell
			cy := float64(y0) + (float64(r)+0.5)*cell
			cxi, cyi := int(cx), int(cy)
			var sr, sg, sb, n int
			for yy := cyi - h; yy < cyi+h; yy++ {
				if yy < 0 || yy >= im.h {
					continue
				}
				for xx := cxi - h; xx < cxi+h; xx++ {
					if xx < 0 || xx >= im.w {
						continue
					}
					pr, pg, pb := im.at(xx, yy)
					sr += pr
					sg += pg
					sb += pb
					n++
				}
			}
			mr := int(float64(sr) / float64(n))
			mg := int(float64(sg) / float64(n))
			mb := int(float64(sb) / float64(n))
			if mr < 155 && mg < 60 && mb < 55 { // empty
				g[r][c] = 0
			} else {
				g[r][c] = 1
			}
		}
	}
	return g, BlokGeom{X0: x0, Y0: y0, Cell: cell}, true
}

func blokPieceMaskHit(r, g, b int) bool { return b > 120 || g > 180 }

// blokBands returns runs of indices where proj exceeds 18% of its max — the
// tile boundaries within a piece's bounding box.
func blokBands(proj []int) [][2]int {
	mx := maxIntSlice(proj)
	if mx == 0 {
		return nil
	}
	thr := float64(mx) * 0.18
	var out [][2]int
	s := -1
	for i, v := range proj {
		on := float64(v) > thr
		if on && s < 0 {
			s = i
		} else if !on && s >= 0 {
			out = append(out, [2]int{s, i - 1})
			s = -1
		}
	}
	if s >= 0 {
		out = append(out, [2]int{s, len(proj) - 1})
	}
	return out
}

// readPieces segments the tray (a band below the board) into individual pieces
// and reconstructs each shape grid from its tile bands.
func readPieces(im *blokImage, geom BlokGeom) []BlokPiece {
	x0, y0, cell := geom.X0, geom.Y0, geom.Cell
	_ = x0
	ty0 := int(float64(y0) + 8.3*cell)
	ty1 := int(float64(y0) + 11.5*cell)
	if ty0 < 0 {
		ty0 = 0
	}
	if ty1 > im.h {
		ty1 = im.h
	}
	// m(x,y): piece-coloured pixel within the tray band.
	m := func(x, y int) bool {
		if y < ty0 || y >= ty1 {
			return false
		}
		return blokPieceMaskHit(im.at(x, y))
	}

	W := im.w
	var colsList []int
	for x := 0; x < W; x++ {
		for y := ty0; y < ty1; y++ {
			if m(x, y) {
				colsList = append(colsList, x)
				break
			}
		}
	}
	if len(colsList) == 0 {
		return nil
	}
	// Group columns whose gap is within 0.4 cell into one piece.
	var groups [][]int
	cur := []int{colsList[0]}
	for _, x := range colsList[1:] {
		if float64(x-cur[len(cur)-1]) <= 0.4*cell {
			cur = append(cur, x)
		} else {
			groups = append(groups, cur)
			cur = []int{x}
		}
	}
	groups = append(groups, cur)

	var pieces []BlokPiece
	for _, gp := range groups {
		gx0, gx1 := gp[0], gp[len(gp)-1]
		gy0, gy1 := -1, -1
		for y := ty0; y < ty1; y++ {
			any := false
			for x := gx0; x <= gx1; x++ {
				if m(x, y) {
					any = true
					break
				}
			}
			if any {
				if gy0 < 0 {
					gy0 = y
				}
				gy1 = y
			}
		}
		bh, bw := gy1-gy0+1, gx1-gx0+1
		box := make([][]bool, bh)
		colSum := make([]int, bw)
		rowSum := make([]int, bh)
		for i := 0; i < bh; i++ {
			box[i] = make([]bool, bw)
			for j := 0; j < bw; j++ {
				v := m(gx0+j, gy0+i)
				box[i][j] = v
				if v {
					colSum[j]++
					rowSum[i]++
				}
			}
		}
		cb := blokBands(colSum)
		rb := blokBands(rowSum)
		nr, nc := len(rb), len(cb)
		shape := make([][]int, nr)
		cells := 0
		for i := 0; i < nr; i++ {
			shape[i] = make([]int, nc)
			rs, re := rb[i][0], rb[i][1]
			for j := 0; j < nc; j++ {
				cs, ce := cb[j][0], cb[j][1]
				tot, cnt := 0, 0
				for ii := rs; ii <= re; ii++ {
					for jj := cs; jj <= ce; jj++ {
						if box[ii][jj] {
							tot++
						}
						cnt++
					}
				}
				if float64(tot)/float64(cnt) > 0.35 {
					shape[i][j] = 1
					cells++
				}
			}
		}
		cx := (gx0 + gx1) / 2
		cy := (gy0 + gy1) / 2
		pieces = append(pieces, BlokPiece{
			Shape: shape, H: nr, W: nc, Cells: cells,
			PickX: cx / 2, PickY: cy / 2, // image px → CSS viewport (2x retina)
		})
	}
	return pieces
}

// ReadBlokScreenshot decodes a saved screenshot and returns the perceived board,
// its geometry, and the tray pieces — the one call the driver needs.
func ReadBlokScreenshot(path string) ([8][8]int, BlokGeom, []BlokPiece, error) {
	var z [8][8]int
	im, err := loadBlokImage(path)
	if err != nil {
		return z, BlokGeom{}, nil, err
	}
	board, geom, ok := readBoard(im)
	if !ok {
		return z, BlokGeom{}, nil, errBlokNoBoard
	}
	pieces := readPieces(im, geom)
	return board, geom, pieces, nil
}

// errBlokNoBoard means no 8x8 board was found in the screenshot — at the end of
// a healthy run this is the win/game-over screen, not a fault.
var errBlokNoBoard = errors.New("blok: no board detected (win/game-over or blank canvas)")

// OpenBlok navigates to the Blok for Blok parent page (capped networkidle wait).
func OpenBlok(ctx context.Context, br *browser.Client) error {
	return openParentGame(ctx, br, BlokURL)
}

// --- solver -----------------------------------------------------------------

// BlokScoredMove is a candidate first-move with its full-trio lookahead score.
type BlokScoredMove struct {
	Score int
	Pi    int
	R, C  int
}

// blokValid returns every (row,col) origin where shape s fits on board b.
func blokValid(b [8][8]int, s [][]int) [][2]int {
	h, w := len(s), len(s[0])
	var o [][2]int
	for r := 0; r <= 8-h; r++ {
		for c := 0; c <= 8-w; c++ {
			ok := true
			for i := 0; i < h && ok; i++ {
				for j := 0; j < w; j++ {
					if s[i][j] == 1 && b[r+i][c+j] == 1 {
						ok = false
						break
					}
				}
			}
			if ok {
				o = append(o, [2]int{r, c})
			}
		}
	}
	return o
}

// BlokApply places s at (r,c), clears full rows/cols, and returns the resulting
// board plus the number of lines cleared.
func BlokApply(b [8][8]int, s [][]int, r, c int) ([8][8]int, int) {
	g := b
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(s[0]); j++ {
			if s[i][j] == 1 {
				g[r+i][c+j] = 1
			}
		}
	}
	var fr, fc []int
	for i := 0; i < 8; i++ {
		full := true
		for j := 0; j < 8; j++ {
			if g[i][j] == 0 {
				full = false
				break
			}
		}
		if full {
			fr = append(fr, i)
		}
	}
	for j := 0; j < 8; j++ {
		full := true
		for i := 0; i < 8; i++ {
			if g[i][j] == 0 {
				full = false
				break
			}
		}
		if full {
			fc = append(fc, j)
		}
	}
	for _, i := range fr {
		for j := 0; j < 8; j++ {
			g[i][j] = 0
		}
	}
	for _, j := range fc {
		for i := 0; i < 8; i++ {
			g[i][j] = 0
		}
	}
	return g, len(fr) + len(fc)
}

// Tunable heuristic weights. These are the production defaults; the offline
// simulator/harness (cmd/blok-sim) overrides them to A/B different settings so we
// can tune the solver against thousands of games instead of the one live game/day.
var (
	BlokWSurvival   = 10000 // per trio piece placed — dominates (never strand a piece)
	BlokWClear      = 120   // per line cleared (survival proxy: frees space)
	BlokWChain      = 1     // multiplier on REAL chain-bonus points earned in the branch
	BlokWChainState = 30    // terminal value per live chain step (future-payout proxy)
	BlokWDead       = 45    // penalty per dead 1x1 hole (unfillable)
	BlokWTight      = 4     // penalty per tight single-neighbour empty cell
	BlokWNear       = 10    // reward per near-complete-line unit (6/8→+1, 7/8→+2); tuned via cmd/blok-sim
	BlokW3x3        = 150   // penalty when no 3x3 region fits anywhere (boxed-in-soon signal); tuned via cmd/blok-sim
)

// blok3x3 is a solid 3x3 block used only to PROBE the board — it is not a real
// tray piece. The biggest common pieces are 3x3 (the rectangle) and the 3x3
// L-pentomino's bounding box, so a board with no open 3x3 region is a strong
// leading signal that the next trio's big piece will strand the game (the
// classic boxed-in death). Checked via the existing blokValid machinery.
var blok3x3 = [][]int{{1, 1, 1}, {1, 1, 1}, {1, 1, 1}}

// blokAny3x3Fits reports whether an all-filled 3x3 block fits anywhere on b.
func blokAny3x3Fits(b [8][8]int) bool {
	return len(blokValid(b, blok3x3)) > 0
}

// BlokChain is the live combo-chain state, carried ACROSS trios (confirmed from
// live traces: the escalation ran unbroken through ~8 trios on 2026-07-05).
type BlokChain struct {
	Len        int // clearing placements in the current chain (0 = no chain)
	SinceClear int // placements since the last clear (3 ⇒ chain expired)
}

// Advance transitions the chain across one placement and returns the REAL bonus
// points that placement pays. Payout schedule confirmed from a full continuous
// chain in live per-step score deltas (2026-07-07, Δscore − Δcells): the k-th
// clearing placement of a chain pays 10×(k−1) — only the FIRST clear is free,
// then +10 each — and a placement clearing multiple lines at once pays NOTHING
// extra (one chain step, same bonus). Validated live up to chain 41 (+400) on
// 2026-07-11. (An earlier fit from a 2026-07-05 trace read "first TWO free";
// that trace was a broken-and-restarted chain — the continuous 07-07 data is
// authoritative.)
//
// GAP RULE: the chain survives at most TWO consecutive non-clearing placements;
// the third kills it. Confirmed 2026-07-11 across a 193-move game: 89/89 clears
// after gaps of 0-2 continued the chain, 3/3 clears after a gap of exactly 3
// restarted it at +0 (the only mispredictions of the whole game, under the old
// "gap of 3 survives" model). This is the single source of truth used by the
// simulator, the planner, and the driver.
func (ch BlokChain) Advance(cleared bool) (BlokChain, int) {
	if !cleared {
		ch.SinceClear++
		if ch.SinceClear > 2 {
			ch.Len = 0
		}
		return ch, 0
	}
	if ch.Len > 0 && ch.SinceClear <= 2 {
		ch.Len++
	} else {
		ch.Len = 1 // start (or restart) the chain
	}
	ch.SinceClear = 0
	return ch, 10 * (ch.Len - 1) // 1st clear +0, 2nd +10, 3rd +20…
}

// blokQuality rewards open space, heavily penalises dead 1x1 holes (no piece is a
// single cell) and tight single-neighbour gaps, and lightly rewards near-complete
// rows/cols so fills consolidate toward clearable lines instead of scattering.
func blokQuality(b [8][8]int) int {
	empty, dead, tight := 0, 0, 0
	dirs := [4][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for r := 0; r < 8; r++ {
		for c := 0; c < 8; c++ {
			if b[r][c] != 0 {
				continue
			}
			empty++
			en := 0
			for _, d := range dirs {
				nr, nc := r+d[0], c+d[1]
				if nr >= 0 && nr < 8 && nc >= 0 && nc < 8 && b[nr][nc] == 0 {
					en++
				}
			}
			if en == 0 {
				dead++
			} else if en == 1 {
				tight++
			}
		}
	}
	// Line-completion pressure: reward rows/cols that are one or two cells from
	// clearing (6-7 of 8 filled), so the solver concentrates fills into completable
	// lines rather than smearing them across the board (which is what leads to a
	// boxed-in game-over). This turned out to be one of the strongest levers — the
	// offline sim (cmd/blok-sim) showed the mean score roughly doubling as BlokWNear
	// went 2→10 across seeds — hence the healthy default weight. It still stays below
	// the per-line clear reward, so it never lures the solver into leaving a line
	// uncleared when it could clear it.
	near := 0
	for i := 0; i < 8; i++ {
		rf, cf := 0, 0
		for j := 0; j < 8; j++ {
			rf += b[i][j]
			cf += b[j][i]
		}
		if rf == 6 || rf == 7 {
			near += rf - 5 // 6→+1, 7→+2
		}
		if cf == 6 || cf == 7 {
			near += cf - 5
		}
	}
	quality := empty - BlokWDead*dead - BlokWTight*tight + BlokWNear*near
	if !blokAny3x3Fits(b) {
		quality -= BlokW3x3
	}
	return quality
}

// BlokPlan does the full-trio lookahead: it tries every ordering/placement of the
// remaining pieces and ranks each possible FIRST move by the best score reachable
// through the whole trio.
//
// Scoring model (from the game's rules): +1 point per cell placed, so SURVIVING
// is the dominant point source — and, critically, failing to place a trio piece is
// GAME OVER. So the score is dominated by how many of the trio's pieces the branch
// manages to PLACE (survivalWeight): a first move that clears a line but then
// strands the 3x3 (fatal) must rank below one that places all three, even if the
// fatal line scores more clears. Under that: the 120/line survival proxy for
// clearing (frees space for future trios), the REAL chain payout for each clearing
// placement (BlokChain.Advance — makes the lookahead sequence clears one line per
// placement instead of clearing two at once, which the game pays nothing extra
// for), and a terminal chain-state value (a live chain is future income the next
// trio can harvest). chain is the LIVE chain state carried across trios by the
// caller. Returns moves best-first, tie-broken descending (score, pi, r, c).
func BlokPlan(board [8][8]int, shapes [][][]int, chain BlokChain) []BlokScoredMove {
	type key struct{ Pi, R, C int }
	results := map[key]int{}
	nShapes := len(shapes)
	// BlokWSurvival must dominate every other term so placing one more piece always
	// wins: max reachable clear+chain+quality is well under it, so "place all 3"
	// strictly beats any "place 2 with big clears" (fatal) line.
	// cl = total lines cleared in the trio (survival proxy); bonus = accumulated
	// REAL chain payout in this branch; ch = chain state after the branch so far.
	var rec func(bd [8][8]int, rem []int, first *key, cl, bonus int, ch BlokChain)
	rec = func(bd [8][8]int, rem []int, first *key, cl, bonus int, ch BlokChain) {
		if first != nil {
			placed := nShapes - len(rem)
			// The live chain's terminal value is scaled by its remaining gap
			// slack: with SinceClear=s the next trio has 2−s free non-clearing
			// placements before the chain dies, so a fresh chain (s=0) is worth
			// full value and one at s=2 (must clear IMMEDIATELY) a third.
			chainState := BlokWChainState * ch.Len * (3 - ch.SinceClear) / 3
			sc := placed*BlokWSurvival + cl*BlokWClear + BlokWChain*bonus +
				chainState + blokQuality(bd)
			if old, ok := results[*first]; !ok || sc > old {
				results[*first] = sc
			}
		}
		for k := 0; k < len(rem); k++ {
			pi := rem[k]
			sh := shapes[pi]
			for _, rc := range blokValid(bd, sh) {
				nb, n := BlokApply(bd, sh, rc[0], rc[1])
				f := first
				if f == nil {
					mv := key{pi, rc[0], rc[1]}
					f = &mv
				}
				nch, pay := ch.Advance(n > 0)
				rest := make([]int, 0, len(rem)-1)
				rest = append(rest, rem[:k]...)
				rest = append(rest, rem[k+1:]...)
				rec(nb, rest, f, cl+n, bonus+pay, nch)
			}
		}
	}
	rem := make([]int, len(shapes))
	for i := range rem {
		rem[i] = i
	}
	rec(board, rem, nil, 0, 0, chain)

	out := make([]BlokScoredMove, 0, len(results))
	for mv, sc := range results {
		out = append(out, BlokScoredMove{Score: sc, Pi: mv.Pi, R: mv.R, C: mv.C})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Pi != b.Pi {
			return a.Pi > b.Pi
		}
		if a.R != b.R {
			return a.R > b.R
		}
		return a.C > b.C
	})
	return out
}

func maxIntSlice(a []int) int {
	m := 0
	for _, v := range a {
		if v > m {
			m = v
		}
	}
	return m
}
