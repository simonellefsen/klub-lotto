package klublotto

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/llm"
)

type KrydsordData struct {
	SolutionSecret string `json:"solution_secret"`
	SolutionUser   string `json:"solution_user"`
	CellCountX     int    `json:"cell_count_x"`
	CellCountY     int    `json:"cell_count_y"`
	OffsetX        int    `json:"offset_x"`
	OffsetY        int    `json:"offset_y"`
	Title          string `json:"title"`
	Image          string `json:"image"`
	IframeURL      string `json:"iframe_url,omitempty"`
	PuzzleID       string `json:"puzzle_id,omitempty"`
	CrosswordID    string `json:"crossword_id,omitempty"`
}

type KrydsordCell struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

type KrydsordSlot struct {
	ID        string         `json:"id"`
	Direction string         `json:"direction"`
	Row       int            `json:"row"`
	Col       int            `json:"col"`
	Length    int            `json:"length"`
	Cells     []KrydsordCell `json:"cells"`
}

type KrydsordArtifacts struct {
	APIPath   string
	ImagePath string
	MaskPath  string
	SlotsPath string
}

type KrydsordGridCheck struct {
	OK       bool
	Errors   []string
	AnswerN  int
	FilledN  int
	Expected int
}

// KrydsordClue maps a visible clue (read via vision from board image) to a slot.
type KrydsordClue struct {
	SlotID    string `json:"slot_id"`
	Direction string `json:"direction"`
	Clue      string `json:"clue"`
	Length    int    `json:"length"`
	// IsImage marks a clue whose cell is a picture/icon (the Clue text is then an
	// English description of the depicted object, e.g. "grill", "t-shirt").
	IsImage bool `json:"is_image,omitempty"`
}

func OpenKrydsord(ctx context.Context, br *browser.Client) error {
	return openParentGame(ctx, br, KrydsordURL)
}

func ExtractKrydsordData(ctx context.Context, br *browser.Client) (KrydsordData, error) {
	var data KrydsordData
	iframe, _ := br.Eval(ctx, `(() => Array.from(document.querySelectorAll('iframe')).map(f => f.src).find(s => /iframes\.krydsord\.dk/i.test(s)) || '')()`)
	iframeSrc := unwrapAgentBrowserString(iframe)
	if iframeSrc == "" {
		return data, fmt.Errorf("could not find Krydsord iframe on parent page")
	}

	// Run the same-origin API fetch INSIDE the embedded OOPIF (agent-browser can now
	// enter it). This avoids navigating the top tab to the standalone iframe URL,
	// which carries a single-use launcher token and just hangs on the red spinner.
	// If the in-frame fetch doesn't yield a usable payload (older daemon, frame not
	// yet attached, vendor quirk), fall back to the legacy navigate-and-fetch path.
	raw, inFrameOK := fetchKrydsordAPIInFrame(ctx, br)
	if d, err := parseKrydsordEnvelope(raw, iframeSrc); inFrameOK && err == nil {
		return d, nil
	}

	// Fallback: navigate the top-level tab to the iframe URL and fetch there.
	if err := br.Open(ctx, iframeSrc); err != nil {
		return data, fmt.Errorf("open Krydsord iframe: %w", err)
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(800 * time.Millisecond)
	raw, err := br.Eval(ctx, krydsordFetchJS)
	if err != nil {
		return data, fmt.Errorf("fetch Krydsord iframe API: %w", err)
	}
	return parseKrydsordEnvelope(raw, iframeSrc)
}

// fetchKrydsordAPIInFrame switches into the embedded game iframe, runs the
// same-origin API fetch there, and switches back to the main frame. Returns the
// raw envelope JSON and whether the frame switch + eval succeeded.
func fetchKrydsordAPIInFrame(ctx context.Context, br *browser.Client) (string, bool) {
	entered := false
	for _, sel := range []string{"iframe.kl-game__iframe", "iframe[src*='krydsord']"} {
		if br.Frame(ctx, sel) == nil {
			// Confirm we're actually inside the game doc (not the parent fallback).
			n, _ := br.Eval(ctx, `String(document.querySelectorAll('.cell').length)`)
			if cnt, _ := strconv.Atoi(strings.TrimSpace(n)); cnt > 0 {
				entered = true
				break
			}
		}
	}
	if !entered {
		_ = br.Frame(context.Background(), "")
		return "", false
	}
	raw, err := br.Eval(ctx, krydsordFetchJS)
	_ = br.Frame(context.Background(), "")
	if err != nil {
		return "", false
	}
	return raw, true
}

// parseKrydsordEnvelope decodes the {status,puzzle,text,error} envelope returned
// by krydsordFetchJS into KrydsordData. A non-empty error, non-2xx status, or
// unparseable payload yields an error (so the caller can fall back).
func parseKrydsordEnvelope(raw, iframeSrc string) (KrydsordData, error) {
	var data KrydsordData
	if strings.TrimSpace(raw) == "" {
		return data, fmt.Errorf("empty Krydsord API response")
	}
	var envelope struct {
		Status int `json:"status"`
		Puzzle struct {
			ID          string `json:"id"`
			CrosswordID string `json:"crossword_id"`
			Title       string `json:"title"`
		} `json:"puzzle"`
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return data, fmt.Errorf("parse Krydsord API envelope: %w (raw=%s)", err, raw)
	}
	if envelope.Error != "" {
		return data, fmt.Errorf("%s", envelope.Error)
	}
	if envelope.Status < 200 || envelope.Status >= 300 {
		return data, fmt.Errorf("Krydsord API returned HTTP %d: %s", envelope.Status, envelope.Text)
	}
	if err := json.Unmarshal([]byte(envelope.Text), &data); err != nil {
		return data, fmt.Errorf("parse Krydsord API payload: %w", err)
	}
	data.IframeURL = iframeSrc
	data.PuzzleID = envelope.Puzzle.ID
	data.CrosswordID = envelope.Puzzle.CrosswordID
	return data, ValidateKrydsordData(data)
}

func ValidateKrydsordData(data KrydsordData) error {
	if data.CellCountX <= 0 || data.CellCountY <= 0 {
		return fmt.Errorf("invalid Krydsord dimensions %dx%d", data.CellCountX, data.CellCountY)
	}
	want := data.CellCountX * data.CellCountY
	if len([]rune(data.SolutionSecret)) != want {
		return fmt.Errorf("solution_secret has %d cells, expected %d", len([]rune(data.SolutionSecret)), want)
	}
	return nil
}

func FormatKrydsordMask(data KrydsordData) string {
	rows := krydsordRows(data.SolutionSecret, data.CellCountX, data.CellCountY)
	for r := range rows {
		for c, ch := range rows[r] {
			if ch == ' ' {
				rows[r][c] = '.'
			} else {
				rows[r][c] = '#'
			}
		}
	}
	return joinRuneRows(rows)
}

func FormatKrydsordUserGrid(data KrydsordData) string {
	rows := krydsordRows(data.SolutionSecret, data.CellCountX, data.CellCountY)
	user := []rune(data.SolutionUser)
	for r := range rows {
		for c, ch := range rows[r] {
			idx := r*data.CellCountX + c
			if ch == ' ' {
				rows[r][c] = '.'
				continue
			}
			if idx < len(user) && user[idx] != ' ' {
				rows[r][c] = user[idx]
			} else {
				rows[r][c] = '_'
			}
		}
	}
	return joinRuneRows(rows)
}

func ValidateKrydsordAnswerGrid(data KrydsordData, grid []string) KrydsordGridCheck {
	return validateKrydsordGrid(data, grid, false)
}

func ValidateKrydsordPartialGrid(data KrydsordData, grid []string) KrydsordGridCheck {
	return validateKrydsordGrid(data, grid, true)
}

func ParseKrydsordGrid(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty Krydsord grid")
	}
	var obj struct {
		Grid []string `json:"grid"`
	}
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && len(obj.Grid) > 0 {
		return obj.Grid, nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}
	var rows []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if fields := strings.Fields(line); len(fields) >= 2 && strings.HasPrefix(strings.ToUpper(fields[0]), "R") {
			line = fields[len(fields)-1]
		}
		rows = append(rows, line)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no Krydsord grid rows found")
	}
	return rows, nil
}

func validateKrydsordGrid(data KrydsordData, grid []string, allowUnknown bool) KrydsordGridCheck {
	check := KrydsordGridCheck{OK: true, Expected: data.CellCountX * data.CellCountY}
	if len(grid) != data.CellCountY {
		check.OK = false
		check.Errors = append(check.Errors, fmt.Sprintf("grid has %d rows, expected %d", len(grid), data.CellCountY))
	}
	maskRows := krydsordRows(data.SolutionSecret, data.CellCountX, data.CellCountY)
	for r := 0; r < data.CellCountY; r++ {
		if r >= len(grid) {
			continue
		}
		row := []rune(grid[r])
		if len(row) != data.CellCountX {
			check.OK = false
			check.Errors = append(check.Errors, fmt.Sprintf("row %d has %d columns, expected %d", r+1, len(row), data.CellCountX))
			continue
		}
		for c := 0; c < data.CellCountX; c++ {
			wantAnswer := maskRows[r][c] != ' '
			ch := row[c]
			if !wantAnswer {
				if ch != '.' {
					check.OK = false
					check.Errors = append(check.Errors, fmt.Sprintf("R%dC%d is a clue/non-answer cell but grid has %q", r+1, c+1, string(ch)))
				}
				continue
			}
			check.AnswerN++
			if ch == '.' || ch == '_' || ch == ' ' {
				if allowUnknown && (ch == '_' || ch == ' ') {
					continue
				}
				check.OK = false
				check.Errors = append(check.Errors, fmt.Sprintf("R%dC%d is an answer cell but is blank/non-answer", r+1, c+1))
				continue
			}
			if !isDanishUpperLetter(ch) {
				check.OK = false
				check.Errors = append(check.Errors, fmt.Sprintf("R%dC%d has invalid answer character %q", r+1, c+1, string(ch)))
				continue
			}
			check.FilledN++
		}
	}
	return check
}

func isDanishUpperLetter(ch rune) bool {
	return (ch >= 'A' && ch <= 'Z') || ch == 'Æ' || ch == 'Ø' || ch == 'Å'
}

func BuildKrydsordSlots(data KrydsordData) []KrydsordSlot {
	grid := krydsordRows(data.SolutionSecret, data.CellCountX, data.CellCountY)
	var slots []KrydsordSlot
	acrossN, downN := 1, 1
	for r := 0; r < data.CellCountY; r++ {
		c := 0
		for c < data.CellCountX {
			if grid[r][c] == ' ' {
				c++
				continue
			}
			start := c
			var cells []KrydsordCell
			for c < data.CellCountX && grid[r][c] != ' ' {
				cells = append(cells, KrydsordCell{Row: r + 1, Col: c + 1})
				c++
			}
			if len(cells) >= 2 {
				slots = append(slots, KrydsordSlot{
					ID:        fmt.Sprintf("A%d", acrossN),
					Direction: "across",
					Row:       r + 1,
					Col:       start + 1,
					Length:    len(cells),
					Cells:     cells,
				})
				acrossN++
			}
		}
	}
	for c := 0; c < data.CellCountX; c++ {
		r := 0
		for r < data.CellCountY {
			if grid[r][c] == ' ' {
				r++
				continue
			}
			start := r
			var cells []KrydsordCell
			for r < data.CellCountY && grid[r][c] != ' ' {
				cells = append(cells, KrydsordCell{Row: r + 1, Col: c + 1})
				r++
			}
			if len(cells) >= 2 {
				slots = append(slots, KrydsordSlot{
					ID:        fmt.Sprintf("D%d", downN),
					Direction: "down",
					Row:       start + 1,
					Col:       c + 1,
					Length:    len(cells),
					Cells:     cells,
				})
				downN++
			}
		}
	}

	// Emit length-1 slots so single-letter clue answers are not dropped. Two
	// kinds, both common in these boards:
	//   - A cell that is part of a >=2 word in ONE direction but length-1 in the
	//     OTHER, governed by a clue in the short direction (KAMMERTONEN->A,
	//     TON->T, REX->R, RØNTGEN->R, SPANIEN->E). Without a slot the clue is
	//     lost and the crossing letter is unconstrained by it.
	//   - A truly isolated single cell (no neighbour either way).
	// Track across- and down-coverage by the >=2 runs separately.
	acrossCovered := map[[2]int]bool{}
	downCovered := map[[2]int]bool{}
	for _, s := range slots {
		for _, cell := range s.Cells {
			key := [2]int{cell.Row, cell.Col}
			if s.Direction == "across" {
				acrossCovered[key] = true
			} else {
				downCovered[key] = true
			}
		}
	}
	for r := 0; r < data.CellCountY; r++ {
		for c := 0; c < data.CellCountX; c++ {
			if grid[r][c] == ' ' {
				continue
			}
			key := [2]int{r + 1, c + 1}
			inA, inD := acrossCovered[key], downCovered[key]
			switch {
			case inA && inD:
				// normal crossing of two real words — nothing to add
			case inA && !inD:
				// part of an across word, length-1 down: a clue directly above
				// it governs a 1-letter down answer.
				if r > 0 && grid[r-1][c] == ' ' {
					slots = append(slots, KrydsordSlot{
						ID: fmt.Sprintf("D%d", downN), Direction: "down",
						Row: r + 1, Col: c + 1, Length: 1,
						Cells: []KrydsordCell{{Row: r + 1, Col: c + 1}},
					})
					downN++
				}
			case inD && !inA:
				// part of a down word, length-1 across: a clue to its left
				// governs a 1-letter across answer.
				if c > 0 && grid[r][c-1] == ' ' {
					slots = append(slots, KrydsordSlot{
						ID: fmt.Sprintf("A%d", acrossN), Direction: "across",
						Row: r + 1, Col: c + 1, Length: 1,
						Cells: []KrydsordCell{{Row: r + 1, Col: c + 1}},
					})
					acrossN++
				}
			default:
				// isolated single cell — one slot, prefer "down" if a clue cell
				// sits directly above.
				dir := "across"
				id := fmt.Sprintf("A%d", acrossN)
				acrossN++
				if r > 0 && grid[r-1][c] == ' ' {
					dir = "down"
					id = fmt.Sprintf("D%d", downN)
					downN++
				}
				slots = append(slots, KrydsordSlot{
					ID: id, Direction: dir, Row: r + 1, Col: c + 1, Length: 1,
					Cells: []KrydsordCell{{Row: r + 1, Col: c + 1}},
				})
			}
		}
	}
	return slots
}

func SaveKrydsordArtifacts(dataDir string, data KrydsordData, slots []KrydsordSlot) (KrydsordArtifacts, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return KrydsordArtifacts{}, err
	}
	ts := time.Now().UTC().Format("20060102-150405")
	art := KrydsordArtifacts{
		APIPath:   filepath.Join(dataDir, "krydsord-api-"+ts+".json"),
		ImagePath: filepath.Join(dataDir, "krydsord-board-"+ts+".jpg"),
		MaskPath:  filepath.Join(dataDir, "krydsord-mask-"+ts+".txt"),
		SlotsPath: filepath.Join(dataDir, "krydsord-slots-"+ts+".json"),
	}
	apiBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return art, err
	}
	if err := os.WriteFile(art.APIPath, apiBytes, 0o644); err != nil {
		return art, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "krydsord-api.json"), apiBytes, 0o644); err != nil {
		return art, err
	}
	mask := FormatKrydsordMask(data) + "\n"
	if err := os.WriteFile(art.MaskPath, []byte(mask), 0o644); err != nil {
		return art, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "krydsord-mask.txt"), []byte(mask), 0o644); err != nil {
		return art, err
	}
	slotBytes, err := json.MarshalIndent(slots, "", "  ")
	if err != nil {
		return art, err
	}
	if err := os.WriteFile(art.SlotsPath, slotBytes, 0o644); err != nil {
		return art, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "krydsord-slots.json"), slotBytes, 0o644); err != nil {
		return art, err
	}
	if data.Image != "" {
		image := data.Image
		if i := strings.Index(image, ","); strings.HasPrefix(image, "data:") && i >= 0 {
			image = image[i+1:]
		}
		img, err := base64.StdEncoding.DecodeString(image)
		if err != nil {
			return art, fmt.Errorf("decode Krydsord board image: %w", err)
		}
		if err := os.WriteFile(art.ImagePath, img, 0o644); err != nil {
			return art, err
		}
		if err := os.WriteFile(filepath.Join(dataDir, "krydsord-board.jpg"), img, 0o644); err != nil {
			return art, err
		}
	}
	return art, nil
}

func krydsordRows(s string, width, height int) [][]rune {
	cells := []rune(s)
	rows := make([][]rune, height)
	for r := 0; r < height; r++ {
		rows[r] = make([]rune, width)
		for c := 0; c < width; c++ {
			idx := r*width + c
			if idx < len(cells) {
				rows[r][c] = cells[idx]
			}
		}
	}
	return rows
}

func joinRuneRows(rows [][]rune) string {
	var b strings.Builder
	for r, row := range rows {
		if r > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(string(row))
	}
	return b.String()
}

const krydsordFetchJS = `(async () => {
  let list = window.puzzle_list || (typeof puzzle_list !== 'undefined' ? puzzle_list : null);
  if (!list || !Array.isArray(list.puzzles)) {
    const html = document.documentElement ? document.documentElement.outerHTML : '';
    const marker = 'new PuzzleList(';
    const start = html.indexOf(marker);
    if (start >= 0) {
      const rest = html.slice(start + marker.length);
      let end = rest.indexOf(');(function');
      if (end < 0) end = rest.indexOf(');');
      if (end >= 0) list = JSON.parse(rest.slice(0, end));
    }
  }
  const puzzle = (list && Array.isArray(list.puzzles) && list.puzzles[0]) || {};
  const crosswordID = puzzle.id || puzzle.crossword_id || '';
  if (!crosswordID) return JSON.stringify({error:'puzzle id not found'});
  const body = new URLSearchParams({cmd:'get_data_and_image', crossword_id:String(crosswordID)});
  const res = await fetch(location.href, {method:'POST', credentials:'include', body});
  const text = await res.text();
  return JSON.stringify({status:res.status, puzzle, text});
})()`

// ExtractKrydsordClues uses Anthropic vision (ExtractFromImage) on the board JPG
// to read the Danish clue texts painted inside the clue squares.
// It asks for position-based output (row/col of the clue cell) because our internal
// A1/D1 slot IDs (from BuildKrydsordSlots) do not visually correspond 1:1 to what
// the model sees; we do a deterministic spatial match afterward.
// Explicitly instructs the model to ignore the Klub Lotto logo/branding and prize icon.
// Parse is tolerant and attempts to recover a JSON array even if the model output is
// truncated or wrapped.
// Caller (runKrydsord) must supply non-nil ac for real solves; --dry-run/--grid bypass.
func ExtractKrydsordClues(ctx context.Context, data KrydsordData, imgBytes []byte, ac llm.VisionProvider) ([]KrydsordClue, error) {
	if ac == nil {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY required for Krydsord clue OCR via vision")
	}
	if len(imgBytes) == 0 {
		return nil, fmt.Errorf("no board image bytes for vision")
	}
	mask := FormatKrydsordMask(data)

	// Use line-based output instead of JSON for robustness against truncation and parsing errors.
	// Even a partial response from the model will give us the clues it managed to output before stopping.
	prompt := fmt.Sprintf(`You are reading a Danish "clues-in-squares" krydsord (crossword) board image from danskespil.dk/klublotto.

CRITICAL LAYOUT RULES — follow these exactly:
- This is a "clues in squares" puzzle: all clues are written inside the dark/clue cells (the "." positions in the mask below). Answer cells ("#") are blank or partially filled.
- Top-row single clues (the horizontal texts in the top row of clue cells, e.g. BE-STEMTE, OPHØJE, NORGE...) normally govern the VERTICAL (down) runs starting downward from that column.
- Left-edge single clues (texts in the leftmost column of clue cells, e.g. ALMINDELIGHED, LØFTE, FORBI...) normally govern the HORIZONTAL (across) runs starting to the right.
- In a SPLIT clue square (one cell contains two lines/texts, one above the other, often with a line between), the UPPER text is usually the clue for the ACROSS run to the right; the LOWER text is for the DOWN run downward.
- The dark red "Klub LOTTO" logo in the upper-left corner (with the small logo) and the small tea-bag icon in the top-right green cell are branding/prize indicators — they are NOT clues. Never transcribe "Klub LOTTO", "Lotto", or the tea bag as a clue unless it is clearly part of a puzzle clue cell.

Geometry mask (. = clue cell that contains text or an icon, # = blank answer cell to fill):
%s

Task: Carefully examine the image and transcribe EVERY clue (text or icon) visible in the . clue cells. For each clue cell, output EXACTLY one line using this precise format (and NOTHING ELSE before or after the lines):

CLUE row=2 col=2 dir=down clue="BE-STEMTE"
CLUE row=2 col=2 dir=across clue="ALMINDELIGHED"
CLUE row=4 col=8 dir=across clue="POTE"
CLUE row=5 col=7 dir=down clue="HELE"
CLUE row=2 col=10 dir=down img=true clue="teabag"

Rules for output (follow strictly):
- row and col are **1-based** coordinates of the clue *cell* (the . cell) containing the text or icon.
- dir must be exactly "across" or "down" (decide using the layout rules above).
- Add **img=true** when the clue cell is a picture / icon / emoji (no text); for those the clue= value is your short English description of the depicted object. Omit img (or img=false) for normal text clues.
- For split cells, output **two** lines with the **same** row/col but different dir + the respective clue text for each part.
- clue= must contain the exact visible text (preserve hyphens like "RED-SKAB", spaces, ÆØÅ, capitalization).
- IMAGE CLUES (important): some clue cells contain ONLY a picture / icon / emoji and NO text. For those, do NOT invent or guess Danish words and do NOT transcribe random letters — instead describe the depicted object with a short ENGLISH noun phrase. Examples of such descriptions: "teabag", "envelope", "moon and stars", "onion", "turnip", "t-shirt", "shirt", "desk lamp", "grill", "barbecue", "ice cream cone", "castle", "lightning bolt", "anchor", "cheese wedges", "cheese", "cherries", "sun", "paint splat", "fish", "key", "apple". A picture cell MUST still produce a CLUE line, with the description as the clue text. If a cell has BOTH a picture and text, output the text.
- TYPESET-TEXT vs DRAWING (critical, read carefully): only transcribe a cell as text when it is clearly printed/typeset LETTERS (uniform font, black on light). A colored or shaded DRAWING/illustration of an object (food, fruit, vegetables, cheese wedges, animals, tools, household items) is a PICTURE — set img=true and describe it; never "read" a Danish word out of a drawing. If you find yourself outputting a word whose letters you are not 100%% certain are actually printed in the cell (e.g. a shape that merely resembles letters), it is almost certainly a picture: emit img=true with an English description instead of guessing the word.
- Small triangular arrows (▼ ▶ ◀ ▲) drawn inside a cell only indicate the answer's reading direction — they are NOT clues and NOT letters. Ignore them (do not output a CLUE line for an arrow, and never include an arrow as part of an answer).
- Hyphenated or multi-line text in one tall clue cell (e.g. "ALMIN-DELIG-HED" or "KOSTU-ME") should be combined into the full natural phrase when possible ("ALMINDELIGHED", "KOSTUME").
- Be exhaustive: every clue cell that has visible content (text OR a picture) must produce a CLUE line, including 1-letter answers (e.g. SMALL→S, TON→T, KILO→K). Do not skip any.
- Be extremely precise with row/col — these will be used to match against the mask geometry.

Here are many correct examples for this exact style of board (use them as strong guidance for positions, directions, and how to handle icons/splits):

CLUE row=2 col=2 dir=down clue="BE-STEMTE"
CLUE row=2 col=3 dir=down clue="OPHØJE"
CLUE row=2 col=4 dir=down clue="NORGE"
CLUE row=2 col=5 dir=down clue="HENSIGT"
CLUE row=2 col=6 dir=down clue="RED-SKAB"
CLUE row=2 col=7 dir=down clue="TRENDY"
CLUE row=2 col=8 dir=down clue="BOLIG"
CLUE row=2 col=9 dir=down clue="FOR-NAVN"
CLUE row=2 col=10 dir=down clue="teabag"
CLUE row=2 col=2 dir=across clue="ALMINDELIGHED"
CLUE row=3 col=2 dir=across clue="LØFTE"
CLUE row=3 col=5 dir=across clue="SCHW. BY"
CLUE row=4 col=2 dir=across clue="FORBI"
CLUE row=4 col=8 dir=across clue="POTE"
CLUE row=4 col=9 dir=down clue="HELE"
CLUE row=5 col=2 dir=across clue="SPIL"
CLUE row=5 col=5 dir=across clue="SPORTSUDSTYR"
CLUE row=5 col=6 dir=down clue="UDBRUD"
CLUE row=5 col=9 dir=across clue="REX"
CLUE row=5 col=10 dir=down clue="FJOLS"
CLUE row=6 col=2 dir=across clue="UDEN"
CLUE row=6 col=3 dir=across clue="ENGELSK TITEL"
CLUE row=6 col=3 dir=down clue="IRRITERE"
CLUE row=6 col=8 dir=across clue="VÆSEN"
CLUE row=6 col=9 dir=down clue="DYR"
CLUE row=7 col=2 dir=across clue="SPEKULERE"
CLUE row=7 col=3 dir=across clue="FRISK"
CLUE row=7 col=4 dir=down clue="HAR"
CLUE row=8 col=2 dir=across clue="TYRKIET"
CLUE row=8 col=4 dir=down clue="moon and stars"
CLUE row=8 col=5 dir=across clue="KAMMERAT"
CLUE row=8 col=6 dir=down clue="FILM"
CLUE row=8 col=9 dir=across clue="LUFTART"
CLUE row=8 col=10 dir=down clue="onion"
CLUE row=9 col=2 dir=across clue="OMRÅDER"
CLUE row=9 col=5 dir=across clue="MÅNED"
CLUE row=9 col=6 dir=down clue="TØNDE"
CLUE row=10 col=2 dir=across clue="FALDE"
CLUE row=10 col=8 dir=across clue="DRIK"
CLUE row=10 col=9 dir=down clue="ØSTRIG"
CLUE row=11 col=2 dir=across clue="TONEN"
CLUE row=11 col=5 dir=across clue="KOSTUME"

Output ONLY the CLUE lines (one per clue cell or split part). If you identify ~35-40 clues, output exactly that many lines and stop. No other text.`, mask)

	text, callErr := ac.ExtractFromImage(ctx, imgBytes, "image/jpeg", prompt)
	if text != "" {
		// Always save the full raw response for debugging (operator can inspect why mapping was bad).
		_ = os.WriteFile(filepath.Join(os.TempDir(), "krydsord-vision-raw.txt"), []byte(text), 0o644)
	}
	if callErr != nil {
		return nil, fmt.Errorf("vision clue extract: %w", callErr)
	}

	// Parse line-based format (robust to truncation — we take whatever lines we got).
	// img=true is optional and marks a picture/icon clue.
	var vclues []visionClue
	re := regexp.MustCompile(`(?i)CLUE\s+row=(\d+)\s+col=(\d+)\s+dir=(across|down)(?:\s+img=(true|false))?\s+clue="([^"]+)"`)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := re.FindStringSubmatch(line); m != nil {
			r, _ := strconv.Atoi(m[1])
			c, _ := strconv.Atoi(m[2])
			vclues = append(vclues, visionClue{
				Row:       r,
				Col:       c,
				Direction: m[3],
				IsImage:   strings.EqualFold(m[4], "true"),
				Clue:      m[5],
			})
		}
	}

	// Map positions reported by vision to our structural slots using geometry + rules.
	// This step corrects many model mistakes on direction/position.
	return mapVisionCluesToSlots(data, vclues), nil
}

// visionClue is the raw output format we request from the vision model (position of the
// clue text in the image grid). Length is optional (the model is told not to worry about it);
// we always use the actual run length from the mask when assigning to a slot.
type visionClue struct {
	Row       int    `json:"row"`
	Col       int    `json:"col"`
	Direction string `json:"direction"`
	Clue      string `json:"clue"`
	IsImage   bool   `json:"is_image,omitempty"`
	Length    int    `json:"length,omitempty"`
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// mapVisionCluesToSlots assigns OCRed clue texts (with visual positions) to the
// slots we computed from the mask. This is deliberately deterministic Go code
// rather than asking the vision model to guess our internal "A1/D3" IDs, which
// do not match the visual reading order the model sees.
func mapVisionCluesToSlots(data KrydsordData, vclues []visionClue) []KrydsordClue {
	slots := BuildKrydsordSlots(data)
	out := make([]KrydsordClue, 0, len(slots))
	for _, s := range slots {
		bestIdx := -1
		bestScore := 1 << 30
		for i, vc := range vclues {
			dist := absInt(vc.Row-s.Row) + absInt(vc.Col-s.Col)
			score := dist * 10
			if strings.EqualFold(vc.Direction, s.Direction) {
				score -= 20 // strong preference for direction match
			}
			// Small bonus if the reported position is exactly the slot start or adjacent in the "clue" direction.
			if (s.Direction == "across" && vc.Col <= s.Col && vc.Row == s.Row) ||
				(s.Direction == "down" && vc.Row <= s.Row && vc.Col == s.Col) {
				score -= 5
			}
			// If model gave a length, small bonus if it matches the actual run length.
			if vc.Length > 0 && vc.Length == s.Length {
				score -= 3
			}
			if score < bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		c := KrydsordClue{
			SlotID:    s.ID,
			Direction: s.Direction,
			Length:    s.Length,
		}
		if bestIdx >= 0 {
			vc := vclues[bestIdx]
			// For 1-letter slots, require a CLOSE, direction-matching clue (the
			// adjacent clue cell). Many 1-letter slots are emitted speculatively
			// (a length-1 run crossing a longer word); if no real clue sits next
			// to it, leave the clue empty so the crossing word fills the cell
			// rather than a mis-mapped clue forcing a wrong letter.
			if s.Length == 1 {
				if absInt(vc.Row-s.Row)+absInt(vc.Col-s.Col) <= 1 && strings.EqualFold(vc.Direction, s.Direction) {
					c.Clue = vc.Clue
					c.IsImage = vc.IsImage
				}
			} else {
				c.Clue = vc.Clue
				c.IsImage = vc.IsImage
			}
		}
		out = append(out, c)
	}
	return out
}

// BuildKrydsordUserSolution turns a validated answer grid into the flat
// row-major user_solution string (same length as solution_secret) with
// letters in answer positions and space in clue positions. This is what the
// vendor API accepts for check_or_save (and correctly handles ÆØÅ).
func BuildKrydsordUserSolution(data KrydsordData, grid []string) string {
	maskRows := krydsordRows(data.SolutionSecret, data.CellCountX, data.CellCountY)
	var b strings.Builder
	for r := 0; r < data.CellCountY; r++ {
		for c := 0; c < data.CellCountX; c++ {
			if maskRows[r][c] == ' ' {
				b.WriteByte(' ')
				continue
			}
			if r < len(grid) {
				row := []rune(grid[r])
				if c < len(row) {
					ch := row[c]
					if (ch >= 'A' && ch <= 'Z') || ch == 'Æ' || ch == 'Ø' || ch == 'Å' {
						b.WriteRune(ch)
						continue
					}
				}
			}
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// SetKrydsordUserSolutionViaAPI POSTs cmd=check_or_save_user_solution while
// the page is the krydsord iframe (so location.href is the vendor origin and
// relative fetch works, with cookies). Mirrors the structure of krydsordFetchJS.
func SetKrydsordUserSolutionViaAPI(ctx context.Context, br *browser.Client, iframeURL, crosswordID, userSol string) error {
	if crosswordID == "" {
		return fmt.Errorf("missing crossword_id for solution save")
	}
	if userSol == "" {
		return fmt.Errorf("empty user_solution")
	}
	// Ensure we evaluate inside the iframe (cross-origin API).
	if iframeURL != "" {
		cur, _ := br.URL(ctx)
		if cur != iframeURL {
			if err := br.Open(ctx, iframeURL); err != nil {
				return fmt.Errorf("reopen krydsord iframe for save: %w", err)
			}
			_ = br.WaitForLoad(ctx, "networkidle")
			time.Sleep(500 * time.Millisecond)
		}
	}
	setJS := fmt.Sprintf(`(async () => {
  const crosswordID = %q;
  const userSol = %q;
  const body = new URLSearchParams({cmd:'check_or_save_user_solution', crossword_id:String(crosswordID), user_solution: userSol});
  const res = await fetch(location.href, {method:'POST', credentials:'include', body});
  const text = await res.text();
  return JSON.stringify({status:res.status, text: text.slice(0,200)});
})()`, crosswordID, userSol)
	raw, err := br.Eval(ctx, setJS)
	if err != nil {
		return fmt.Errorf("set user solution via API: %w (raw=%s)", err, raw)
	}
	_ = raw
	time.Sleep(600 * time.Millisecond)
	return nil
}
