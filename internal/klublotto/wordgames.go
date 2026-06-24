package klublotto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/llm"
)

const DanishAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZÆØÅ"

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// spaceOutBoardPositions converts a board where each word group's positions are
// concatenated ("BONBON_LAN_") into the space-separated-per-position form that
// the prompt builders expect ("B O N B O N _ L A N _"), preserving the " / "
// word-group separators. Groups that already contain spaces are left as-is.
func spaceOutBoardPositions(board string) string {
	groups := strings.Split(board, "/")
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if strings.Contains(g, " ") {
			out = append(out, g) // already one token per position
			continue
		}
		toks := make([]string, 0, len([]rune(g)))
		for _, r := range g {
			toks = append(toks, string(r))
		}
		out = append(out, strings.Join(toks, " "))
	}
	return strings.Join(out, " / ")
}

// visionBoardResponse is the JSON schema returned by the vision model.
type visionBoardResponse struct {
	Category string `json:"category"`
	Attempts struct {
		Used      int    `json:"used"`
		Total     int    `json:"total"`
		Remaining int    `json:"remaining"`
		RawText   string `json:"raw_text"`
	} `json:"attempts"`
	Board struct {
		Rows []struct {
			RowIndex int `json:"row_index"`
			Tiles    []struct {
				Column   int    `json:"column"`
				Revealed bool   `json:"revealed"`
				Letter   string `json:"letter"`
			} `json:"tiles"`
		} `json:"rows"`
		TileCount   int      `json:"tile_count"`
		PatternRows []string `json:"pattern_rows"`
		FullPattern string   `json:"full_pattern"`
	} `json:"board"`
	Keyboard struct {
		Keys []struct {
			Letter string `json:"letter"`
			State  string `json:"state"`
		} `json:"keys"`
		CorrectLetters   []string `json:"correct_letters"`
		IncorrectLetters []string `json:"incorrect_letters"`
		AvailableLetters []string `json:"available_letters"`
	} `json:"keyboard"`
}

// parseOrdKloeverVisionJSON converts a raw JSON vision response into an
// OrdKloeverState. Returns (state, tileCount, ok).
func parseOrdKloeverVisionJSON(text string) (OrdKloeverState, int, bool) {
	clean := ExtractJSONObject(text)
	if clean == "" {
		return OrdKloeverState{}, 0, false
	}
	var resp visionBoardResponse
	if err := json.Unmarshal([]byte(clean), &resp); err != nil {
		return OrdKloeverState{}, 0, false
	}

	var st OrdKloeverState
	st.Category = strings.TrimSpace(resp.Category)
	st.Attempts = resp.Attempts.Used

	// Build Board (space-separated tokens, words joined with " / ") and Shape
	// (word lengths joined with "+") from pattern_rows. The vision prompt emits
	// one pattern_rows entry per WORD of the phrase (multi-word lines split on
	// gaps, dash line-wraps joined), so each entry maps to one word/length here.
	// Each pattern_row character is one tile: letter or "_" for blank.
	patternRows := resp.Board.PatternRows
	if len(patternRows) == 0 && resp.Board.FullPattern != "" {
		// Fallback: split full_pattern by space to get rows.
		patternRows = strings.Fields(resp.Board.FullPattern)
	}
	if len(patternRows) > 0 {
		var shapeParts, boardParts []string
		for _, row := range patternRows {
			if row == "" {
				continue
			}
			shapeParts = append(shapeParts, strconv.Itoa(len([]rune(row))))
			var toks []string
			for _, ch := range row {
				toks = append(toks, string(ch))
			}
			boardParts = append(boardParts, strings.Join(toks, " "))
		}
		st.Shape = strings.Join(shapeParts, "+")
		st.VisualShape = st.Shape
		st.Board = strings.Join(boardParts, " / ")
		st.VisualBoard = st.Board
	}

	// GuessedLetters = correct (green) + incorrect (dark/dimmed).
	var guessed []string
	guessed = append(guessed, resp.Keyboard.CorrectLetters...)
	guessed = append(guessed, resp.Keyboard.IncorrectLetters...)
	if len(guessed) > 0 {
		st.GuessedLetters = CleanGuessedLetters(strings.Join(guessed, " "))
	}

	// Tile count: prefer explicit field, fall back to sum of pattern row lengths.
	tileCount := resp.Board.TileCount
	if tileCount == 0 {
		for _, row := range patternRows {
			tileCount += len([]rune(row))
		}
	}

	return st, tileCount, true
}

// countVisionBoardTokens returns the tile count from a raw vision response.
// Handles both the new JSON format (reads board.tile_count / pattern_rows)
// and the legacy plain-text BOARD: line format.
func countVisionBoardTokens(text string) int {
	// Try JSON format first.
	if clean := ExtractJSONObject(text); clean != "" && strings.Contains(clean, `"board"`) {
		var resp visionBoardResponse
		if json.Unmarshal([]byte(clean), &resp) == nil {
			if resp.Board.TileCount > 0 {
				return resp.Board.TileCount
			}
			count := 0
			for _, row := range resp.Board.PatternRows {
				count += len([]rune(row))
			}
			if count > 0 {
				return count
			}
		}
	}
	// Fall back to legacy plain-text BOARD: line.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(line), "BOARD:") {
			boardStr := strings.TrimSpace(line[len("BOARD:"):])
			normalized := strings.ReplaceAll(boardStr, "/", " ")
			count := 0
			for _, tok := range strings.Fields(normalized) {
				if tok != "" {
					count++
				}
			}
			return count
		}
	}
	return 0
}

type WordCandidate struct {
	Answer     string `json:"answer"`
	Confidence string `json:"confidence"`
	Rationale  string `json:"rationale"`
}

type OrdKloeverState struct {
	Category       string `json:"category"`
	Hint           string `json:"hint"`
	Shape          string `json:"shape"`
	Board          string `json:"board"`
	VisualShape    string `json:"visualShape"`
	VisualBoard    string `json:"visualBoard"`
	GuessedLetters string `json:"guessedLetters"`
	Attempts       int    `json:"attempts"`
	Solved         bool   `json:"solved"`
	Raw            string `json:"raw"`
	IframeURL      string `json:"iframeURL"`
}

type OrdknudeTile struct {
	Letter     string `json:"letter"`
	ClassName  string `json:"className"`
	Background string `json:"background"`
	Mark       string `json:"mark"`
}

type OrdknudeGuess struct {
	Word  string   `json:"word"`
	Marks []string `json:"marks"`
}

type OrdknudeState struct {
	History   []OrdknudeGuess `json:"history"`
	Solved    bool            `json:"solved"`
	Answer    string          `json:"answer,omitempty"`
	Remaining int             `json:"remaining"`
	Raw       string          `json:"raw"`
	IframeURL string          `json:"iframeURL"`
}

func NormalizeDanishLetters(s string) string {
	s = strings.TrimSpace(strings.ToUpper(s))
	var b strings.Builder
	for _, r := range s {
		r = unicode.ToUpper(r)
		if (r >= 'A' && r <= 'Z') || r == 'Æ' || r == 'Ø' || r == 'Å' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanGuessedLetters normalizes the GUESSED string from vision/JS but drops
// placeholder words like "NONE", "INGEN", "NO" etc that vision may emit when
// no letters are used yet. This prevents polluting the blocked set with N/O/E
// on fresh boards and wasting probes on common letters.
func CleanGuessedLetters(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "NONE" || s == "INGEN" || s == "NO" || s == "N/A" || s == "NOT VISIBLE" || s == "UKENDT" || strings.HasPrefix(s, "NONE") || strings.HasPrefix(s, "INGEN") || len(s) > 40 {
		return ""
	}
	return NormalizeDanishLetters(s)
}

func NormalizeDanishPhrase(s string) string {
	s = strings.TrimSpace(strings.ToUpper(s))
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		r = unicode.ToUpper(r)
		if (r >= 'A' && r <= 'Z') || r == 'Æ' || r == 'Ø' || r == 'Å' {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if unicode.IsSpace(r) || r == '-' || r == '/' {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func DanishLettersIn(s string) string {
	seen := map[rune]bool{}
	var out strings.Builder
	for _, r := range NormalizeDanishPhrase(s) {
		if !strings.ContainsRune(DanishAlphabet, r) || seen[r] {
			continue
		}
		seen[r] = true
		out.WriteRune(r)
	}
	return out.String()
}

func LengthPattern(pattern string) []int {
	var out []int
	for _, m := range regexp.MustCompile(`\d+`).FindAllString(pattern, -1) {
		var n int
		if _, err := fmt.Sscanf(m, "%d", &n); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func DanishPhraseWordLengths(phrase string) []int {
	phrase = NormalizeDanishPhrase(phrase)
	if phrase == "" {
		return nil
	}
	words := strings.Fields(phrase)
	out := make([]int, 0, len(words))
	for _, word := range words {
		out = append(out, utf8.RuneCountInString(word))
	}
	return out
}

func PhraseMatchesLengthPattern(phrase, pattern string) bool {
	want := LengthPattern(pattern)
	if len(want) == 0 {
		return true
	}
	got := DanishPhraseWordLengths(phrase)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func PhraseMatchesMask(phrase, mask string) bool {
	mask = strings.TrimSpace(strings.ToUpper(mask))
	if mask == "" {
		return true
	}
	phrase = NormalizeDanishPhrase(phrase)
	if phrase == "" {
		return false
	}
	// Normalize spaced-blank format produced by Ordkløver vision extraction:
	// "_ _ _ _ _ _ _ _ _ / _ _ _" → word groups ["_________", "___"]
	// "A _ R / _ _ _"             → ["A_R", "___"]
	//
	// Strategy: split on "/" to get per-word groups; within each group, if
	// every whitespace-separated token is exactly one character (letter or
	// wildcard), collapse them into a single run.  Otherwise keep as-is
	// (handles already-compact forms like "HELLO" or "__AB_").
	rawParts := strings.Split(mask, "/")
	var maskWords []string
	for _, part := range rawParts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tokens := strings.Fields(part)
		if len(tokens) == 0 {
			continue
		}
		allSingle := true
		for _, t := range tokens {
			if len([]rune(t)) > 1 {
				allSingle = false
				break
			}
		}
		if allSingle {
			// "_ _ _ _ _" → "_____", "A _ C" → "A_C"
			maskWords = append(maskWords, strings.Join(tokens, ""))
		} else {
			// Already compact: "HELLO", "__AB_", etc.
			maskWords = append(maskWords, part)
		}
	}
	if len(maskWords) == 0 {
		// Fallback: old behaviour (split by whitespace, "/" treated as space).
		maskWords = strings.Fields(strings.ReplaceAll(mask, "/", " "))
	}

	// Structural dashes ("-") are pre-revealed separators, not typed letters.
	// NormalizeDanishPhrase turns dashes into word breaks, so the phrase carries
	// no dash; strip dashes from each mask word so position counts line up
	// (e.g. mask "TR___E__-" → "TR___E__" matches "TRYGHEDS").
	for i := range maskWords {
		maskWords[i] = strings.ReplaceAll(maskWords[i], "-", "")
	}

	phraseWords := strings.Fields(phrase)
	if len(maskWords) != len(phraseWords) {
		return false
	}
	for i, maskWord := range maskWords {
		mr := []rune(maskWord)
		pr := []rune(phraseWords[i])
		if len(mr) != len(pr) {
			return false
		}
		for j, r := range mr {
			if r == '_' || r == '?' || r == '.' {
				continue
			}
			if r != pr[j] {
				return false
			}
		}
	}
	return true
}

// BoardWrongLetters returns letters from triedLetters that do NOT appear as
// revealed characters in the board string — these are definitively wrong (not
// in the answer at all). Letters that appear on the board are correct and
// should NOT be included here.
//
// Used to build the "forkerte bogstaver" list for LLM prompts so the model
// is not confused by mixing correct/revealed letters with wrong ones.
func BoardWrongLetters(board, triedLetters string) string {
	// Build set of letters actually on the board (revealed = correct).
	boardSet := map[rune]bool{}
	for _, r := range []rune(NormalizeDanishLetters(board)) {
		if r != '_' {
			boardSet[r] = true
		}
	}
	// Walk tried letters, keeping only those absent from the board.
	seen := map[rune]bool{}
	var out strings.Builder
	for _, r := range []rune(NormalizeDanishLetters(triedLetters)) {
		if !boardSet[r] && !seen[r] {
			if out.Len() > 0 {
				out.WriteRune(' ')
			}
			out.WriteRune(r)
			seen[r] = true
		}
	}
	return out.String()
}

// BoardShapeFromString derives the TYPED-LETTER word-length pattern from the
// board string, e.g. "_ R _ _ B _ R _ / S _ _ _" → "8+4".
// The board uses spaced format (one token per position, "/" between words).
// A "-" token is a structural dash (pre-revealed, like a space) and is NOT a
// typed letter, so it is excluded from the count — the count reflects how many
// keyboard letters the answer needs, matching how we validate/submit guesses.
// This cross-checks the vision-extracted Shape field which can be off by ±1.
func BoardShapeFromString(board string) string {
	if board == "" {
		return ""
	}
	parts := strings.Split(board, "/")
	var counts []string
	for _, part := range parts {
		tokens := strings.Fields(strings.TrimSpace(part))
		if len(tokens) == 0 {
			continue
		}
		// In spaced format every token is a single character (letter, "_", or "-").
		// Count letter/blank positions but skip structural dashes.
		total := 0
		for _, t := range tokens {
			if t == "-" {
				continue
			}
			total += len([]rune(t))
		}
		if total > 0 {
			counts = append(counts, strconv.Itoa(total))
		}
	}
	return strings.Join(counts, "+")
}

// EffectiveShapeForMatching returns the word-length pattern to validate guessed
// phrases against. When the board contains structural dashes ("-"), those count
// toward the game's displayed shape (e.g. "9+8") but are not typed letters, so a
// correct answer like "TRYGHEDS-NARKOMAN" normalizes to 8+8 letters. In that
// case we derive the shape from the board with dashes excluded so the correct
// answer is not wrongly filtered out. Falls back to the extracted shape.
func EffectiveShapeForMatching(board, shape string) string {
	if strings.Contains(board, "-") {
		if bs := BoardShapeFromString(board); bs != "" {
			return bs
		}
	}
	return shape
}

// Win/loss/error-screen detectors (IsOrdKloeverWinText, IsOrdknudeWinText,
// IsDanskeSpilErrorScreen, OrdknudeSolvedViaIframe) live in screens.go.

// BoardHasHit reports whether any letter from the given probe set appears as a
// revealed (non-underscore) character in the board string. Used to detect
// whether an initial letter-probe round produced at least one correct hit.
func BoardHasHit(board string, letters []string) bool {
	board = strings.ToUpper(board)
	for _, l := range letters {
		l = NormalizeDanishLetters(l)
		for _, ch := range l {
			if strings.ContainsRune(board, ch) {
				return true
			}
		}
	}
	return false
}

func FilterCandidatesByLengthPattern(cands []WordCandidate, pattern string) ([]WordCandidate, int) {
	if len(LengthPattern(pattern)) == 0 {
		return cands, 0
	}
	var out []WordCandidate
	dropped := 0
	for _, cand := range cands {
		if PhraseMatchesLengthPattern(cand.Answer, pattern) {
			out = append(out, cand)
		} else {
			dropped++
		}
	}
	return out, dropped
}

func FilterCandidatesByMask(cands []WordCandidate, mask string) ([]WordCandidate, int) {
	if strings.TrimSpace(mask) == "" {
		return cands, 0
	}
	var out []WordCandidate
	dropped := 0
	for _, cand := range cands {
		if PhraseMatchesMask(cand.Answer, mask) {
			out = append(out, cand)
		} else {
			dropped++
		}
	}
	return out, dropped
}

func SuggestOrdKloeverLetters(cands []WordCandidate, already string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	blocked := map[rune]bool{}
	for _, r := range DanishLettersIn(already) {
		blocked[r] = true
	}
	type score struct {
		letter rune
		score  int
	}
	scores := map[rune]int{}
	for i, cand := range cands {
		weight := 20 - i
		if weight < 1 {
			weight = 1
		}
		switch strings.ToLower(cand.Confidence) {
		case "high":
			weight += 10
		case "medium":
			weight += 4
		}
		for _, r := range DanishLettersIn(cand.Answer) {
			if !blocked[r] {
				scores[r] += weight
			}
		}
	}
	frequency := []rune("ERNTAISLODGKMFHVBPYUCJÆØÅXQWZ")
	if len(scores) == 0 {
		var out []string
		for _, r := range frequency {
			if !blocked[r] {
				out = append(out, string(r))
				if len(out) == limit {
					return out
				}
			}
		}
		return out
	}
	var ranked []score
	for r, s := range scores {
		ranked = append(ranked, score{letter: r, score: s})
	}
	index := map[rune]int{}
	for i, r := range frequency {
		index[r] = i
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return index[ranked[i].letter] < index[ranked[j].letter]
	})
	var out []string
	for _, item := range ranked {
		out = append(out, string(item.letter))
		if len(out) == limit {
			break
		}
	}
	return out
}

func OpenOrdKloever(ctx context.Context, br *browser.Client) error {
	return openParentGame(ctx, br, OrdKloeverURL)
}

func OpenOrdknude(ctx context.Context, br *browser.Client) error {
	return openParentGame(ctx, br, OrdknudeURL)
}

func openParentGame(ctx context.Context, br *browser.Client, url string) error {
	if err := br.Open(ctx, url); err != nil {
		return err
	}
	// WaitSettled caps the networkidle wait (danskespil keeps tracker connections
	// open, which would otherwise stall ~30s on the welcome screen); downstream
	// snapshot / keyboard-readiness retries handle anything not yet painted.
	br.WaitSettled(ctx)
	time.Sleep(1200 * time.Millisecond)
	return nil
}

// ExtractOrdKloeverState extracts the current Ordkløver game state. Pass an
// optional second VisionProvider as vp2 to use as an on-error fallback: when the
// primary model fails (commonly a context-deadline timeout from a flaky model),
// the fallback re-reads the board instead of the round being abandoned.
func ExtractOrdKloeverState(ctx context.Context, br *browser.Client, ac llm.VisionProvider, vp2 ...llm.VisionProvider) (OrdKloeverState, error) {
	var fallbackVP llm.VisionProvider
	if len(vp2) > 0 {
		fallbackVP = vp2[0]
	}
	var st OrdKloeverState

	// Remember parent for immerspiele credit (same reason as Ordknude).
	parentURL := OrdKloeverURL
	if u, _ := br.URL(ctx); u != "" && strings.Contains(u, "danskespil.dk") {
		parentURL = u
	}

	cur, _ := br.URL(ctx)

	// Prefer vision crop from parent (no top-level iframe open) if ac available.
	if ac != nil {
		if v, ok := extractOrdKloeverViaVision(ctx, br, ac, fallbackVP); ok {
			st = v
			raw, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
			st.Raw = raw
			// A win banner in the parent body ("Flot præstation! Du løste ordkløver
			// med stil!") is the authoritative solved signal. On a win the board /
			// category vision read comes back empty — identical to the launcher — so
			// we MUST check this BEFORE the empty-fields branch below, which would
			// otherwise re-click SPIL ORDKLØVER and re-vision, overwriting st (and
			// wiping st.Raw) so the win is lost.
			if IsOrdKloeverWinText(raw) {
				st.Solved = true
				return st, nil
			}
			// If vision saw welcome/not-started (empty fields), try to start then re-vision.
			// Parent start may not reach inside iframe, so also do explicit frame+click for the launcher button.
			if st.Category == "" && st.Hint == "" && st.Shape == "" && st.Board == "" {
				_ = startWordGameIfPresent(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER", "Spil Ordkløver", "Spil Ordkløver")
				// Frame-based start (reliable for immerspiele content).
				if ferr := EnterGameFrame(ctx, br); ferr == nil {
					defer LeaveFrame(br)
					isnap, _ := br.SnapshotInteractive(ctx)
					if ref := FindRefByName(isnap, []string{"SPIL ORDKLØVER", "Spil Ordkløver", "spil ordkloever"}); ref != "" {
						_ = br.Click(ctx, ref)
						time.Sleep(1500 * time.Millisecond)
					}
				} else {
					LeaveFrame(br)
				}
				if v2, ok2 := extractOrdKloeverViaVision(ctx, br, ac, fallbackVP); ok2 {
					st = v2
				}
			}
			// Cross-check vision board-length with a quick DOM tile count.
			// NOTE: this JS runs in the *parent* page context and cannot access the
			// cross-origin immerspiele iframe DOM — it will almost always return empty.
			// The log below confirms whether the DOM traversal contributed anything.
			domRaw, _ := br.Eval(ctx, ordKloeverBoardJS)
			var domBoard struct {
				Shape string `json:"shape"`
				Board string `json:"board"`
			}
			domOK := json.Unmarshal([]byte(domRaw), &domBoard) == nil && domBoard.Board != ""
			if !domOK {
				fmt.Printf("   [dom cross-check] no board from DOM (cross-origin iframe — expected)\n")
			} else {
				const danishLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZÆØÅ"
				domCount := len(strings.Fields(domBoard.Board))
				visionCount := len(strings.Fields(st.Board))
				domHasLetters := strings.ContainsAny(strings.ToUpper(domBoard.Board), danishLetters)
				visionHasLetters := strings.ContainsAny(strings.ToUpper(st.Board), danishLetters)
				fmt.Printf("   [dom cross-check] DOM board=%q (%d tokens)  vision board=%q (%d tokens)\n",
					domBoard.Board, domCount, st.Board, visionCount)
				// The DOM board is read INSIDE the iframe (works now that
				// agent-browser can eval in OOPIFs) and is the ground truth for
				// which tiles are revealed. Adopt it whenever vision produced no
				// usable board, or when the DOM shows revealed letters and vision
				// does not — previously a vision-empty board (the common case
				// today) was silently discarded, leaving the final guess with no
				// answer pattern at all.
				// The DOM board concatenates letters within a word group
				// ("BONBON_LAN_"), but downstream prompts expect one space-
				// separated token per position ("B O N _"). Convert on adoption.
				domSpaced := spaceOutBoardPositions(domBoard.Board)
				switch {
				case strings.TrimSpace(st.Board) == "" && domBoard.Board != "":
					fmt.Printf("   [dom cross-check] ✓ adopting DOM board (vision produced none)\n")
					st.Board = domSpaced
					if domBoard.Shape != "" {
						st.Shape = domBoard.Shape
					}
				case domHasLetters && !visionHasLetters:
					fmt.Printf("   [dom cross-check] ✓ adopting DOM board (DOM has revealed letters, vision does not)\n")
					st.Board = domSpaced
					if domBoard.Shape != "" {
						st.Shape = domBoard.Shape
					}
				case domCount > 0 && visionCount > 0 && domCount != visionCount && (domHasLetters || abs(domCount-visionCount) == 1):
					fmt.Printf("   [dom cross-check] ✓ correcting board from DOM (%d→%d tokens)\n", visionCount, domCount)
					st.Board = domSpaced
					if domBoard.Shape != "" {
						st.Shape = domBoard.Shape
					}
				default:
					fmt.Printf("   [dom cross-check] keeping vision board\n")
				}
			}
			// Read keyboard state from inside the iframe — runs in iframe context so
			// the cross-origin restriction does not apply.
			domCorrect, domIncorrect := extractKloeverKeyboardViaDOM(ctx, br)
			domAllTried := unionGuessedLetters(domCorrect, domIncorrect)
			if domAllTried == "" {
				fmt.Printf("   [dom keyboard] could not read keyboard from iframe DOM\n")
			} else {
				fmt.Printf("   [dom keyboard] correct (green)=%q  incorrect (dark)=%q\n", domCorrect, domIncorrect)
				before := st.GuessedLetters
				st.GuessedLetters = unionGuessedLetters(st.GuessedLetters, domAllTried)
				if st.GuessedLetters != before {
					fmt.Printf("   [dom keyboard] ✓ enriched: vision=%q + dom=%q → %q\n", before, domAllTried, st.GuessedLetters)
				} else {
					fmt.Printf("   [dom keyboard] dom matches vision guessed — no change (%q)\n", st.GuessedLetters)
				}
			}
			// Restore parent ONLY if we actually navigated away. The vision path
			// just screenshots the parent page (and dips into the iframe via
			// br.Frame, which does not navigate the top-level), so we're still on
			// the parent. Re-opening it here reloads the embed back to its
			// "Velkommen" launcher, forcing a SPIL ORDKLØVER re-click on the next
			// step and causing the board↔welcome flicker between rounds.
			if curNow, _ := br.URL(ctx); !strings.Contains(curNow, "danskespil.dk") || !strings.Contains(curNow, "ordkloever") {
				_ = br.Open(ctx, parentURL)
				br.WaitSettled(ctx)
				time.Sleep(500 * time.Millisecond)
			}
			return st, nil
		}
	}

	_ = startWordGameIfPresent(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER", "Spil Ordkløver", "Spil Ordkløver")
	iframe, _ := br.Eval(ctx, `(() => Array.from(document.querySelectorAll('iframe')).map(f => f.src).find(s => /ordkloever|ordklover|clover/i.test(s)) || '')()`)
	st.IframeURL = unwrapAgentBrowserString(iframe)
	if st.IframeURL != "" {
		if err := br.Open(ctx, st.IframeURL); err != nil {
			return st, fmt.Errorf("open Ordkløver iframe: %w", err)
		}
		br.WaitSettled(ctx)
		time.Sleep(800 * time.Millisecond)
		_ = startWordGameIfPresent(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER", "Spil Ordkløver", "Spil Ordkløver")
	}
	raw, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
	st.Raw = raw
	if IsOrdKloeverWinText(raw) {
		st.Solved = true
	}
	st.Category = firstCapture(raw, `(?im)\bKategori:\s*([^\n]+)`)
	st.Hint = firstCapture(raw, `(?im)\bLedetråd:\s*([^\n]+)`)
	if st.Hint == "" {
		st.Hint = firstCapture(raw, `(?im)\bHint:\s*([^\n]+)`)
	}
	if attempts := firstCapture(raw, `(?im)(\d+)\s*/\s*12`); attempts != "" {
		_, _ = fmt.Sscanf(attempts, "%d", &st.Attempts)
	}
	boardRaw, _ := br.Eval(ctx, ordKloeverBoardJS)
	var board struct {
		Shape       string `json:"shape"`
		Board       string `json:"board"`
		VisualShape string `json:"visualShape"`
		VisualBoard string `json:"visualBoard"`
		Guessed     string `json:"guessed"`
	}
	if err := json.Unmarshal([]byte(boardRaw), &board); err == nil {
		if strings.TrimSpace(board.Shape) != "" {
			st.Shape = strings.TrimSpace(board.Shape)
		}
		st.Board = strings.TrimSpace(board.Board)
		st.VisualShape = strings.TrimSpace(board.VisualShape)
		st.VisualBoard = strings.TrimSpace(board.VisualBoard)
		st.GuessedLetters = CleanGuessedLetters(board.Guessed)
	}
	if st.Category == "" {
		st.Category = firstCapture(raw, `(?im)^Kategori[: ]+(.+)$`)
	}
	if st.Category == "" && st.Hint == "" && st.Shape == "" {
		st.Raw = "current URL before iframe extraction: " + cur + "\n\n" + st.Raw
	}

	// Restore parent for credit (ordkloever is also an immerspiele embed).
	if err := br.Open(ctx, parentURL); err != nil {
		// best effort
	} else {
		br.WaitSettled(ctx)
		time.Sleep(800 * time.Millisecond)
	}

	// Salvage: if body/raw contained the vision json (from previous or injected), populate fields.
	if (st.Category == "" || st.Shape == "" || st.Board == "") && (strings.Contains(st.Raw, `"CATEGORY"`) || strings.Contains(st.Raw, `"SHAPE"`) || strings.Contains(st.Raw, `"BOARD"`)) {
		var j map[string]interface{}
		if json.Unmarshal([]byte(st.Raw), &j) == nil {
			if v, ok := j["CATEGORY"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") { st.Category = v }
			if v, ok := j["HINT"].(string); ok && v != "" && !strings.EqualFold(v, "Not visible") { st.Hint = v }
			if v, ok := j["SHAPE"].(string); ok && v != "" && !strings.EqualFold(v, "Unknown") { st.Shape = v; if st.VisualShape == "" { st.VisualShape = v } }
			if v, ok := j["BOARD"].(string); ok && v != "" { st.Board = v; if st.VisualBoard == "" { st.VisualBoard = v } }
			if v, ok := j["GUESSED"].(string); ok && v != "" { st.GuessedLetters = CleanGuessedLetters(v) }
			if v, ok := j["ATTEMPTS"].(string); ok && st.Attempts == 0 {
				if idx := strings.Index(v, "/"); idx > 0 {
					if n, _ := strconv.Atoi(strings.TrimSpace(v[:idx])); n > 0 { st.Attempts = n }
				}
			}
		}
	}

	return st, nil
}

func extractOrdKloeverViaVision(ctx context.Context, br *browser.Client, ac llm.VisionProvider, fallback llm.VisionProvider) (OrdKloeverState, bool) {
	if ac == nil {
		return OrdKloeverState{}, false
	}
	shotPath := filepath.Join(os.TempDir(), "ordkloever-vision-"+time.Now().UTC().Format("20060102-150405")+".png")
	if err := br.Screenshot(ctx, shotPath); err != nil {
		return OrdKloeverState{}, false
	}

	// Use robust rect finding (same as ordknuden, tries iframe then title container then center)
	rectJS := `(() => {
	  const dpr = window.devicePixelRatio || 1;
	  let ifr = document.querySelector('iframe[src*="ordkloever"], iframe[src*="ordklover"], iframe[src*="clover"], .kl-game__iframe');
	  if (ifr) {
	    const r = ifr.getBoundingClientRect();
	    if (r.width >= 50 && r.height >= 50) {
	      // Use full iframe rect; prompt tells model to ignore kb at bottom and focus on upper puzzle/rebus/slots.
	      return JSON.stringify({ok:true, left: Math.round(r.left * dpr), top: Math.round(r.top * dpr), width: Math.round(r.width * dpr), height: Math.round(r.height * dpr), dpr: dpr});
	    }
	  }
	  // Try to locate kb area ("Gæt bogstav" or letter buttons) and crop the area above it for the puzzle/hint.
	  const kbEls = Array.from(document.querySelectorAll('button, [role="button"]')).filter(el => {
	    const t = (el.textContent || el.innerText || el.getAttribute('aria-label') || '').trim().toUpperCase();
	    return /GÆT BOGSTAV|GÆT GÅDE|BRUG LEDETRÅD|^[A-ZÆØÅ]$/.test(t);
	  });
	  if (kbEls.length > 3) {
	    const minTop = Math.min(...kbEls.map(el => el.getBoundingClientRect().top));
	    const w = window.innerWidth || 1200;
	    const top = Math.round((minTop - 220) * dpr); // generous above kb for slots + hint + cat
	    const hh = Math.round(240 * dpr);
	    return JSON.stringify({ok:true, left: Math.round(w*0.05*dpr), top: Math.max(0, top), width: Math.round(w*0.9*dpr), height: hh, dpr: dpr});
	  }
	  const title = Array.from(document.querySelectorAll('h1,h2,div')).find(el => /Ordkløver/i.test((el.textContent || '').trim()));
	  if (title) {
	    let container = title.parentElement || title;
	    for (let i = 0; i < 3 && container; i++) {
	      const r = container.getBoundingClientRect();
	      if (r.width >= 200 && r.height >= 200) {
	        return JSON.stringify({ok:true, left: Math.round(r.left * dpr), top: Math.round(r.top * dpr), width: Math.round(r.width * dpr), height: Math.round(r.height * dpr), dpr: dpr});
	      }
	      container = container.parentElement;
	    }
	  }
	  const w = window.innerWidth || 1200;
	  const h = window.innerHeight || 800;
	  return JSON.stringify({ok:true, left: Math.round(w*0.05), top: Math.round(h*0.08), width: Math.round(w*0.9), height: Math.round(h*0.55), dpr: dpr});
	})()`
	rawRect, _ := br.Eval(ctx, rectJS)
	var rinfo struct {
		Ok     bool    `json:"ok"`
		Left   int     `json:"left"`
		Top    int     `json:"top"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Dpr    float64 `json:"dpr"`
	}
	json.Unmarshal([]byte(rawRect), &rinfo)

	imgBytes, err := os.ReadFile(shotPath)
	if err != nil {
		return OrdKloeverState{}, false
	}
	if rinfo.Ok && rinfo.Width > 0 && rinfo.Height > 0 {
		f, err := os.Open(shotPath)
		if err == nil {
			full, err := png.Decode(f)
			f.Close()
			if err == nil {
				bounds := full.Bounds()
				crop := image.Rect(rinfo.Left, rinfo.Top, rinfo.Left+rinfo.Width, rinfo.Top+rinfo.Height)
				crop = crop.Intersect(bounds)
				if !crop.Empty() {
					if sub, ok := full.(interface{ SubImage(image.Rectangle) image.Image }); ok {
						croppedImg := sub.SubImage(crop)
						var buf bytes.Buffer
						if png.Encode(&buf, croppedImg) == nil {
							imgBytes = buf.Bytes()
						}
					}
				}
			}
		}
	}

	// save for debug
	{
		ts := time.Now().UTC().Format("20060102-150405")
		_ = os.WriteFile(filepath.Join(".klublotto", "ordkloever-vision-input-"+ts+".png"), imgBytes, 0o644)
	}

	prompt := `You are a computer vision extraction system.

Analyze the attached screenshot of a Danish "Wheel of Fortune" style game.

IMPORTANT:
The solution may be:
- a single word
- multiple words
- a person's name
- a phrase
- a sentence

Do NOT attempt to solve the puzzle.

Return ONLY valid JSON.

Schema:

{
  "category": null,

  "attempts": {
    "used": null,
    "total": null,
    "remaining": null,
    "raw_text": null
  },

  "board": {
    "rows": [
      {
        "row_index": 0,
        "tiles": [
          {
            "column": 0,
            "revealed": false,
            "letter": null
          }
        ]
      }
    ],

    "tile_count": 0,

    "pattern_rows": [],

    "full_pattern": ""
  },

  "keyboard": {
    "keys": [
      {
        "letter": "A",
        "state": "correct"
      }
    ],

    "correct_letters": [],
    "incorrect_letters": [],
    "available_letters": []
  }
}

BOARD EXTRACTION — board.rows (physical tile layout)

board.rows captures the physical layout, one entry per visual line of tiles.
Each visible tile position must be represented.

For each tile:

- revealed=true if a letter is visible
- revealed=false if blank

For hidden tiles:
    letter=null

For revealed tiles:
    letter=<visible letter>

Character encoding (used by both board.rows tiles and pattern_rows below):
represent a hidden/blank tile with "_" and a revealed tile with its letter.

Example — one visual line of tiles encodes to a string:

    B R _ _ _ T E   ->   "BR___TE"

WORD SEGMENTATION — pattern_rows MUST be WORDS, not visual rows

The board.rows / tiles above capture the PHYSICAL tile layout (one entry per
visual line, with column positions). The fields pattern_rows and full_pattern
are DIFFERENT: they capture the LOGICAL phrase split into its individual WORDS.
A visual line is NOT necessarily one word — it may hold several words, and a
single word may be split across two lines. Build pattern_rows with exactly ONE
entry per word, using "_" for each blank tile and the visible letter for each
revealed tile, by applying these rules:

1. MULTIPLE WORDS ON ONE LINE: a clear gap between two groups of tiles on the
   same visual line separates two different words. Emit one entry per group.
   Look actively for these gaps — do not merge separate groups into one word.
   Example line "B I N G    O G"  ->  "BING", "OG".

2. WORD SPLIT ACROSS LINES WITH A DASH: when a line ends with a hyphen "-" and
   the word continues on the next line (line wrapping), it is ONE word.
   Concatenate the parts and DROP the hyphen.
   Example:
       LOKOMOTIV-
       FØRER
   -> one word "LOKOMOTIVFØRER".

3. A hyphen "-" that sits BETWEEN two letters on the SAME line is a real
   character of the word — keep it (e.g. "TV-AVIS").

4. Combine the rules as needed. Example board:
       VÆRKSTEDS-
       MESTER HOS
       BING OG GRØN-
       DAHL
   -> words: "VÆRKSTEDSMESTER", "HOS", "BING", "OG", "GRØNDAHL".

The field full_pattern must be all words joined with a single space.

tile_count is the total number of letter/blank tiles across all words (the sum
of the word lengths) — it excludes word-separating gaps and any dropped
line-wrap hyphen.

Example: the board shows "_ I N _    O _" on the first line (two words) and
"_ R _ N D A _ L" on the second line (one word) — three words total:

{
  "pattern_rows": [
    "_IN_",
    "O_",
    "_R_NDA_L"
  ],

  "full_pattern": "_IN_ O_ _R_NDA_L"
}

KEYBOARD EXTRACTION – CRITICAL SECTION (Be Extremely Precise)

The keyboard is in the dark red panel at the bottom. Analyze it row by row.

There are THREE distinct visual states for keys:
- GREEN background   → state="correct"   (bright green — clearly stands out)
- DARK background    → state="incorrect" (noticeably darker than normal keys, almost black or very dark — clearly different from the standard dark red/burgundy)
- NORMAL background  → state="available" (standard dark red/burgundy — the majority of keys)

Instructions for keyboard analysis:
1. Process the keyboard row by row from top to bottom.
2. For EVERY single key, determine its state by comparing its background color to neighboring keys.
3. Pay special attention to keys that look darker than the majority — these are the incorrect guesses.
4. Do NOT assume a key is normal just because you don't immediately notice it — actively check each one.
5. A key that is even slightly darker than its neighbors should be classified as "incorrect", not "available".

Ignore non-letter keys:
- GÆT
- backspace
- Gæt bogstav
- Gæt gåde
- Brug ledetråd

Output one object per letter key.

EDGE CASES

If no interactive game board is visible (launcher/welcome screen): return tile_count=0 and empty rows and pattern_rows.

If the game is finished or already answered: set attempts.used equal to attempts.total (both 12).
`

	// Helper: get a printable name from any vision provider (optional Name() method).
	providerName := func(vp llm.VisionProvider) string {
		type namer interface{ Name() string }
		if n, ok := vp.(namer); ok {
			return n.Name()
		}
		return fmt.Sprintf("%T", vp)
	}

	fmt.Printf("   [vision] model: %s\n", providerName(ac))
	fmt.Printf("   [vision] prompt (%d chars):\n", len(prompt))
	for _, l := range strings.Split(strings.TrimSpace(prompt), "\n") {
		fmt.Printf("      | %s\n", l)
	}

	text, err := ac.ExtractFromImage(ctx, imgBytes, "image/png", prompt)
	usedProvider := providerName(ac)
	if err != nil {
		fmt.Printf("   [vision] primary error: %v\n", err)
		// On any primary failure (commonly a context-deadline timeout from a
		// flaky model/route), fall back to a second vision model rather than
		// abandoning the round. The fallback runs on its own detached time
		// budget so an already-exhausted parent deadline doesn't doom it too.
		if fallback == nil {
			return OrdKloeverState{}, false
		}
		fmt.Printf("   [vision] falling back to %s...\n", providerName(fallback))
		fbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		text, err = fallback.ExtractFromImage(fbCtx, imgBytes, "image/png", prompt)
		cancel()
		if err != nil {
			fmt.Printf("   [vision] fallback error: %v\n", err)
			return OrdKloeverState{}, false
		}
		usedProvider = providerName(fallback)
		fmt.Printf("   [vision] fallback succeeded via %s\n", usedProvider)
	}
	text = strings.TrimSpace(text)
	ts := time.Now().UTC().Format("20060102-150405")
	_ = os.WriteFile(filepath.Join(".klublotto", "ordkloever-vision-raw-"+ts+".txt"), []byte(text), 0o644)

	fmt.Printf("   [vision] raw response (%s):\n", usedProvider)
	for _, l := range strings.Split(text, "\n") {
		fmt.Printf("      | %s\n", l)
	}
	fmt.Printf("   [vision] board tokens: %d\n", countVisionBoardTokens(text))

	st, tileCount, parseOK := parseOrdKloeverVisionJSON(text)
	st.Raw = text
	if !parseOK {
		fmt.Printf("   [vision] could not parse JSON response — treating as not-started\n")
		return OrdKloeverState{Raw: text}, false
	}
	fmt.Printf("   [vision] parsed: category=%q attempts=%d tile_count=%d board=%q guessed=%q\n",
		st.Category, st.Attempts, tileCount, st.Board, st.GuessedLetters)

	// NOT_STARTED: no board found yet (launcher screen).
	if tileCount == 0 && st.Attempts == 0 {
		return OrdKloeverState{Raw: text}, true
	}
	// FINISHED: game over (used all attempts).
	if st.Attempts >= 12 {
		return OrdKloeverState{Solved: false, Attempts: 12, Raw: text}, true
	}
	return st, true
}

// ordKloeverKeyboardJS reads the virtual keyboard state from INSIDE the iframe.
// Must be run via br.Frame("iframe.kl-game__iframe") to avoid cross-origin block.
//
// Classification priority:
//   1. CSS class names (correct / wrong / absent / incorrect / used / disabled)
//   2. data-state / data-status attribute
//   3. Computed background colour:
//        green  (g > 120 && g > r+50 && g > b+30) → correct (found in word)
//        dark   (r+g+b < 150)                     → incorrect (not in word)
//   4. disabled / aria-disabled attribute
//
// Returns JSON: {"correct":["E","R",…],"incorrect":["N",…]}
const ordKloeverKeyboardJS = `(() => {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZÆØÅ';
  const correct = [], incorrect = [];
  for (const el of document.querySelectorAll('button,[role="button"]')) {
    const text = (el.textContent || el.innerText || el.getAttribute('aria-label') || '').trim().toUpperCase();
    const chars = Array.from(text).filter(c => alphabet.includes(c));
    if (chars.length !== 1) continue;
    const letter = chars[0];

    // 1. CSS class
    const cls = (el.className || '').toLowerCase();
    if (/\b(correct|found|right|hit|green)\b/.test(cls))                 { correct.push(letter);   continue; }
    if (/\b(wrong|incorrect|absent|miss|dark|used|disabled|played)\b/.test(cls)) { incorrect.push(letter); continue; }

    // 2. data-state / data-status
    const ds = (el.getAttribute('data-state') || el.getAttribute('data-status') || '').toLowerCase();
    if (ds === 'correct' || ds === 'present')                            { correct.push(letter);   continue; }
    if (ds === 'absent'  || ds === 'wrong' || ds === 'incorrect')        { incorrect.push(letter); continue; }

    // 3. Computed background colour
    const bg = getComputedStyle(el).backgroundColor;
    const m = (bg.match(/\d+/g) || []).map(Number);
    if (m.length >= 3) {
      const [r, g, b] = m;
      if (g > 120 && g > r + 50 && g > b + 30) { correct.push(letter);   continue; } // green
      if (r + g + b < 150)                      { incorrect.push(letter); continue; } // very dark
    }

    // 4. disabled
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') { incorrect.push(letter); }
  }
  return JSON.stringify({ correct, incorrect });
})()`

// extractKloeverKeyboardViaDOM switches into the game iframe, reads every
// keyboard button's state via colour/class/attribute heuristics, and returns
// all already-tried letters (correct=green AND incorrect=dark) as a
// space-separated uppercase string.  Returns "" on any error.
func extractKloeverKeyboardViaDOM(ctx context.Context, br *browser.Client) (correct, incorrect string) {
	if err := EnterGameFrame(ctx, br); err != nil {
		return "", ""
	}
	defer LeaveFrame(br)

	raw, err := br.Eval(ctx, ordKloeverKeyboardJS)
	if err != nil {
		return "", ""
	}
	var result struct {
		Correct   []string `json:"correct"`
		Incorrect []string `json:"incorrect"`
	}
	if json.Unmarshal([]byte(raw), &result) != nil {
		return "", ""
	}
	c := CleanGuessedLetters(strings.Join(result.Correct, " "))
	i := CleanGuessedLetters(strings.Join(result.Incorrect, " "))
	return c, i
}

// unionGuessedLetters merges two space-separated guessed-letter strings,
// deduplicating while preserving order (a first, then new letters from b).
func unionGuessedLetters(a, b string) string {
	seen := map[rune]bool{}
	var out strings.Builder
	for _, r := range []rune(NormalizeDanishLetters(a + " " + b)) {
		if r == ' ' || seen[r] {
			continue
		}
		if out.Len() > 0 {
			out.WriteRune(' ')
		}
		out.WriteRune(r)
		seen[r] = true
	}
	return out.String()
}

const ordKloeverBoardJS = `(() => {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZÆØÅ';

  // ── Shared: keyboard guessed-letters (used by both strategies) ──────────────
  const guessedLetters = () => Array.from(document.querySelectorAll('button,[role="button"]')).map(el => {
    const text = (el.innerText || el.textContent || el.getAttribute('aria-label') || '').trim().toUpperCase();
    const ch = Array.from(text).find(c => alphabet.includes(c));
    if (!ch || Array.from(text).filter(c => alphabet.includes(c)).length !== 1) return '';
    const cls = String(el.className || '').toLowerCase();
    const style = getComputedStyle(el);
    const used = el.disabled || el.getAttribute('aria-disabled') === 'true' ||
      /used|disabled|correct|wrong|active|selected/.test(cls) ||
      Number(style.opacity || '1') < 0.95;
    return used ? ch : '';
  }).filter(Boolean).join('');

  // ── Strategy 1: CSS grid-column-start (authoritative, animation-proof) ──────
  // The game sets grid-column-start as an INLINE style on each tile element.
  // This gives us the exact column position regardless of background color,
  // size changes from CSS transitions, or "current input cursor" highlights.
  const gridTiles = [];
  for (const el of document.querySelectorAll('[class*="tile"],[class*="cell"]')) {
    const col = parseInt(el.style.gridColumnStart || '0') || 0;
    if (col <= 0) continue; // no inline grid position — skip
    const row = parseInt(el.style.gridRowStart || '1') || 1;
    const isControl = el.tagName.toLowerCase() === 'button' || !!el.closest('button,[role="button"]');
    if (isControl) continue;
    const text = (el.innerText || el.textContent || '').trim().toUpperCase();
    const chars = Array.from(text).filter(ch => alphabet.includes(ch) || ch === '-');
    gridTiles.push({col, row, letter: chars[0] || '_'});
  }

  if (gridTiles.length >= 4) {
    // Sort by (row, col) — the game always uses row=1 for single-row answers,
    // row=1,2 for two-row answers (e.g. a name on two lines).
    gridTiles.sort((a, b) => a.row - b.row || a.col - b.col);

    // Group tiles by row, then split each row into word-groups by gaps in column numbers.
    // A gap > 1 in column indices means there's a deliberate visual space (word separator).
    const byRow = {};
    for (const t of gridTiles) {
      (byRow[t.row] = byRow[t.row] || []).push(t);
    }
    const rowGroups = [];
    for (const rowKey of Object.keys(byRow).map(Number).sort((a, b) => a - b)) {
      const tiles = byRow[rowKey].sort((a, b) => a.col - b.col);
      const groups = [];
      let cur = [];
      let prevCol = null;
      for (const t of tiles) {
        if (prevCol !== null && t.col > prevCol + 1) {
          if (cur.length) groups.push(cur);
          cur = [];
        }
        cur.push(t);
        prevCol = t.col;
      }
      if (cur.length) groups.push(cur);
      if (groups.length) rowGroups.push(groups);
    }
    const allGroups = rowGroups.flat();
    const groupText = g => g.map(t => t.letter).join('');
    return JSON.stringify({
      shape: allGroups.map(g => String(g.length)).join(' / '),
      board: allGroups.map(groupText).join(' / '),
      visualShape: rowGroups.map(row => row.map(g => String(g.length)).join(' + ')).join(' / '),
      visualBoard: rowGroups.map(row => row.map(groupText).join(' ')).join(' / '),
      guessed: guessedLetters(),
      source: 'grid-column'
    });
  }

  // ── Strategy 2: bounding-rect fallback (used when grid positions absent) ────
  // Some game versions don't set inline grid-column-start; fall back to
  // geometric tile detection using getBoundingClientRect.
  const buttonTops = Array.from(document.querySelectorAll('button,[role="button"]')).map(el => {
    const text = String(el.innerText || el.textContent || el.getAttribute('aria-label') || '').trim().toUpperCase();
    if (!/(GÆT BOGSTAV|GÆT GÅDE|BRUG LEDETRÅD|GAET BOGSTAV|GAET GADE)/.test(text)) return 0;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 ? r.top : 0;
  }).filter(Boolean);
  const boardBottom = buttonTops.length ? Math.min(...buttonTops) - 12 : window.innerHeight * 0.72;
  const rows = [];
  let boxes = Array.from(document.querySelectorAll('[class*="tile"],[class*="cell"],[class*="letter"],[class*="square"],[role="gridcell"]')).map(el => {
    const r = el.getBoundingClientRect();
    const text = (el.innerText || el.textContent || '').trim().toUpperCase();
    const chars = Array.from(text).filter(ch => alphabet.includes(ch) || ch === '-');
    const cls = String(el.className || '').toLowerCase();
    const tag = el.tagName.toLowerCase();
    const isControl = tag === 'button' || tag === 'input' || tag === 'textarea' || !!el.closest('button,[role="button"]');
    return {x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height),
            letter: chars[0] || '_', cls, isControl, wordLetters: chars.length};
  }).filter(b =>
    !b.isControl &&
    b.w >= 36 && b.h >= 36 && b.w <= 120 && b.h <= 120 &&
    b.y > 80 && b.y + b.h < boardBottom &&
    b.w / b.h > 0.55 && b.w / b.h < 1.55 &&
    b.wordLetters <= 1 &&
    !/(key|keyboard|button|attempt|forsøg|forsoeg|hint|logo|icon)/.test(b.cls)
  );
  boxes.sort((a, b) => (b.w * b.h) - (a.w * a.h));
  const deduped = [];
  for (const b of boxes) {
    if (!deduped.some(x => Math.abs(x.x - b.x) < 4 && Math.abs(x.y - b.y) < 4)) deduped.push(b);
  }
  boxes = deduped.sort((a, b) => a.y - b.y || a.x - b.x);
  for (const b of boxes) {
    let row = rows.find(r => Math.abs(r.y - b.y) < Math.max(12, b.h * 0.35));
    if (!row) { row = {y: b.y, boxes: []}; rows.push(row); }
    row.boxes.push(b);
  }
  const rowGroups2 = [];
  for (const row of rows.sort((a, b) => a.y - b.y)) {
    const bs = row.boxes.sort((a, b) => a.x - b.x);
    const widths = bs.map(b => b.w).sort((a, b) => a - b);
    const medianWidth = widths.length ? widths[Math.floor(widths.length / 2)] : 64;
    const splitAt = Math.max(24, medianWidth * 0.45);
    const groups = [];
    let cur = [], prev = null;
    for (const b of bs) {
      const gap = prev ? b.x - (prev.x + prev.w) : 0;
      if (prev && gap > splitAt) { if (cur.length) groups.push(cur); cur = []; }
      cur.push(b); prev = b;
    }
    if (cur.length) groups.push(cur);
    if (groups.length) rowGroups2.push(groups);
  }
  const groups2 = rowGroups2.flat();
  const groupText2 = g => g.map(b => b.letter).join('');
  return JSON.stringify({
    shape: groups2.map(g => String(g.length)).join(' / '),
    board: groups2.map(groupText2).join(' / '),
    visualShape: rowGroups2.map(row => row.map(g => String(g.length)).join(' + ')).join(' / '),
    visualBoard: rowGroups2.map(row => row.map(groupText2).join(' ')).join(' / '),
    guessed: guessedLetters(),
    source: 'bounding-rect'
  });
})()`

// extractBoardViaVision uses a screenshot of the *parent* Danske Spil page
// (which renders the embedded immerspiele game visually) + Anthropic vision
// to read the current guesses and their color marks. This lets us get accurate
// history for the LLM prompt *without ever navigating the top-level page to the
// cross-origin immerspiele iframe URL*. This is required to keep game events
// (and thus the daily lod/checkmark on the Spil & Quiz overview) registered
// with Danske Spil.
func extractBoardViaVision(ctx context.Context, br *browser.Client, ac llm.VisionProvider) (history []OrdknudeGuess, remaining int, solved bool, err error) {
	if ac == nil {
		return nil, 6, false, fmt.Errorf("no vision client")
	}
	shotPath := filepath.Join(os.TempDir(), "ordknude-vision-"+time.Now().UTC().Format("20060102-150405")+".png")
	if err := br.Screenshot(ctx, shotPath); err != nil {
		return nil, 6, false, err
	}

	// Get the iframe rect from the parent page (CSS pixels). Screenshot may be at device pixel ratio.
	// More robust: try known iframe selectors first; if not found or tiny, fall back to the element
	// containing the "Ordknuden" title or a large central area that usually contains the red game board.
	rectJS := `(() => {
	  const dpr = window.devicePixelRatio || 1;
	  let ifr = document.querySelector('iframe[src*="ordknuden"], iframe[src*="ordknude"], .kl-game__iframe');
	  if (ifr) {
	    const r = ifr.getBoundingClientRect();
	    if (r.width >= 50 && r.height >= 50) {
	      return JSON.stringify({
	        ok:true,
	        left: Math.round(r.left * dpr),
	        top: Math.round(r.top * dpr),
	        width: Math.round(r.width * dpr),
	        height: Math.round(r.height * dpr),
	        dpr: dpr
	      });
	    }
	  }
	  // Fallback 1: find the container near the "Ordknuden" title
	  const title = Array.from(document.querySelectorAll('h1,h2,div')).find(el => (el.textContent || '').trim() === 'Ordknuden');
	  if (title) {
	    let container = title.parentElement || title;
	    // go up a few levels to find a wider red-ish game area
	    for (let i = 0; i < 3 && container; i++) {
	      const style = window.getComputedStyle(container);
	      if (style.backgroundColor && style.backgroundColor.includes('rgb')) {
	        const r = container.getBoundingClientRect();
	        if (r.width >= 200 && r.height >= 200) {
	          return JSON.stringify({
	            ok:true,
	            left: Math.round(r.left * dpr),
	            top: Math.round(r.top * dpr),
	            width: Math.round(r.width * dpr),
	            height: Math.round(r.height * dpr),
	            dpr: dpr
	          });
	        }
	      }
	      container = container.parentElement;
	    }
	  }
	  // Fallback 2: large central viewport area (covers the typical game location on these pages)
	  const w = window.innerWidth || 1200;
	  const h = window.innerHeight || 800;
	  const left = Math.round(w * 0.1);
	  const top = Math.round(h * 0.15);
	  const width = Math.round(w * 0.8);
	  const height = Math.round(h * 0.7);
	  return JSON.stringify({ok:true, left, top, width, height, dpr});
	})()`
	rawRect, _ := br.Eval(ctx, rectJS)
	var rinfo struct {
		Ok     bool    `json:"ok"`
		Left   int     `json:"left"`
		Top    int     `json:"top"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Dpr    float64 `json:"dpr"`
	}
	json.Unmarshal([]byte(rawRect), &rinfo)

	imgBytes, err := os.ReadFile(shotPath)
	if err != nil {
		return nil, 6, false, err
	}

	// Crop to the iframe area for much better vision accuracy on the game tiles only.
	cropped := false
	if rinfo.Ok && rinfo.Width > 0 && rinfo.Height > 0 {
		f, err := os.Open(shotPath)
		if err == nil {
			full, err := png.Decode(f)
			f.Close()
			if err == nil {
				bounds := full.Bounds()
				crop := image.Rect(rinfo.Left, rinfo.Top, rinfo.Left+rinfo.Width, rinfo.Top+rinfo.Height)
				crop = crop.Intersect(bounds)
				if !crop.Empty() {
					if sub, ok := full.(interface{ SubImage(image.Rectangle) image.Image }); ok {
						croppedImg := sub.SubImage(crop)
						var buf bytes.Buffer
						if png.Encode(&buf, croppedImg) == nil {
							imgBytes = buf.Bytes()
							cropped = true
						}
					}
				}
			}
		}
	}

	// Safety fallback generous center crop: if the iframe selector gave no/small rect (or we didn't crop),
	// take a large central portion of the full screenshot. This reliably includes the red game area + title + tiles + kb
	// even if the exact iframe rect is off (different embed, scroll, partial input row like "Y", layout changes).
	// The prompt tells the model to focus only on the central red game tiles and ignore partial bottom input rows.
	if !cropped {
		f2, err := os.Open(shotPath)
		if err == nil {
			full2, err := png.Decode(f2)
			f2.Close()
			if err == nil {
				b := full2.Bounds()
				w, h := b.Dx(), b.Dy()
				// generous center: ~12% from edges, covers the typical game board location
				cx0 := b.Min.X + int(float64(w)*0.10)
				cy0 := b.Min.Y + int(float64(h)*0.10)
				cx1 := b.Min.X + int(float64(w)*0.90)
				cy1 := b.Min.Y + int(float64(h)*0.88)
				c2 := image.Rect(cx0, cy0, cx1, cy1).Intersect(b)
				if !c2.Empty() {
					if sub2, ok := full2.(interface{ SubImage(image.Rectangle) image.Image }); ok {
						cimg := sub2.SubImage(c2)
						var buf2 bytes.Buffer
						if png.Encode(&buf2, cimg) == nil {
							imgBytes = buf2.Bytes()
							cropped = true // treat as cropped for save
						}
					}
				}
			}
		}
	}

	// Always persist the exact bytes (full page or tight iframe crop) that we send to vision.
	// This is the "what the model actually saw" for debugging parse failures, crop rects, etc.
	{
		ts := time.Now().UTC().Format("20060102-150405")
		inputPath := filepath.Join(os.TempDir(), "ordknude-vision-input-"+ts+".png")
		_ = os.WriteFile(inputPath, imgBytes, 0o644)
		_ = os.MkdirAll(".klublotto", 0o755)
		_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-vision-input-"+ts+".png"), imgBytes, 0o644)
		if cropped {
			_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-vision-cropped-"+ts+".png"), imgBytes, 0o644)
		}
	}

	prompt := `You are reading a screenshot of the Ordknuden (Danish 5-letter Wordle-like) game from Danske Spil / Klub Lotto.

SPECIAL CASES (output these exact tokens if they apply):
- If you see a start/welcome screen with a prominent "SPIL ORDKNUDEN" button and no letter tiles filled → output: NOT_STARTED
- If the 5x6 grid is entirely blank white tiles (no letters at all) → output: NO_GUESSES
- If you see "Ordknuden besvaret!", "spillet slut", "du vandt", "ordet var", "ingen flere forsøg", "TILBAGE TIL SPIL & QUIZ" → output: FINISHED

Otherwise, read the board:

STEP 1 — Read the WORDS: Identify each completed row (rows with all 5 letters AND color feedback). Ignore blank rows and ignore any partial row with only 1–4 letters (no color on all 5 tiles). Read the 5 uppercase letters from each completed row top to bottom.

STEP 2 — Read the KEYBOARD (at the bottom of the screen). The large keyboard keys show each letter's status clearly:
- BRIGHT GREEN key → that letter is "correct" (right letter, right position)
- BRIGHT YELLOW or AMBER key → that letter is "present" (right letter, wrong position)
- VERY DARK MAROON key (much darker than unused keys) → that letter is "absent"
- MEDIUM DARK (same as unused keys) → letter not yet used

List which keys are GREEN and which are YELLOW. Then for each word from step 1, assign marks based on the keyboard:
- Letter is GREEN on keyboard → "correct"
- Letter is YELLOW on keyboard → "present"
- Letter not highlighted → "absent"

Output format — ONLY these lines (no markdown, no explanation, no numbering):
WORD absent,absent,present,absent,absent

One line per completed guess row (top to bottom). Uppercase word, then a space, then 5 comma-separated marks.

Example: board has SPROG (R is present in 3rd position), keyboard shows R=yellow:
SPROG absent,absent,present,absent,absent`
	text, err := ac.ExtractFromImage(ctx, imgBytes, "image/png", prompt)
	if text != "" {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "ordknude-vision-raw.txt"), []byte(text), 0o644)
		_ = os.MkdirAll(".klublotto", 0o755)
		_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-vision-raw.txt"), []byte(text), 0o644)
	}
	if err != nil {
		return nil, 6, false, err
	}
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)
	if upper == "NOT_STARTED" || text == "" {
		return nil, 6, false, nil
	}
	if upper == "NO_GUESSES" {
		return nil, 6, false, nil
	}
	if strings.Contains(upper, "FINISHED") || strings.Contains(upper, "BESVARET") {
		// daily round already answered / game over for the day; no more attempts
		return nil, 0, false, nil
	}
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		word := NormalizeDanishLetters(parts[0])
		if len([]rune(word)) != 5 {
			continue
		}
		markPart := strings.ToLower(strings.TrimSpace(parts[1]))
		marks := strings.Split(markPart, ",")
		if len(marks) != 5 {
			marks = strings.Fields(strings.ReplaceAll(markPart, ",", " "))
		}
		for i := range marks {
			m := strings.TrimSpace(marks[i])
			switch {
			case strings.Contains(m, "correct") || strings.Contains(m, "green"):
				marks[i] = "correct"
			case strings.Contains(m, "present") || strings.Contains(m, "yellow") || strings.Contains(m, "orange"):
				marks[i] = "present"
			default:
				marks[i] = "absent"
			}
		}
		if len(marks) == 5 {
			history = append(history, OrdknudeGuess{Word: word, Marks: marks})
		}
	}
	// Tolerant salvage: if the primary line parse found nothing (model added prose/JSON/fences despite
	// instructions, common when the ExtractFromImage system prompt says "JSON only"), regex-extract any
	// "WORD mark,mark,..." sequences and parse them. This makes us robust like the krydsord line parser.
	if len(history) == 0 && text != "" {
		salvageRe := regexp.MustCompile(`(?i)([A-ZÆØÅ]{5})[\s:,\-–—]*((?:absent|present|correct|green|yellow|orange|gray|grey)[,\s]*){4,5}(?:absent|present|correct|green|yellow|orange|gray|grey)`)
		for _, sub := range salvageRe.FindAllString(text, -1) {
			sub = strings.TrimSpace(sub)
			parts := strings.SplitN(sub, " ", 2)
			if len(parts) != 2 {
				continue
			}
			word := NormalizeDanishLetters(parts[0])
			if len([]rune(word)) != 5 {
				continue
			}
			markPart := strings.ToLower(strings.TrimSpace(parts[1]))
			marks := strings.Split(markPart, ",")
			if len(marks) != 5 {
				marks = strings.Fields(strings.ReplaceAll(markPart, ",", " "))
			}
			for i := range marks {
				m := strings.TrimSpace(marks[i])
				switch {
				case strings.Contains(m, "correct") || strings.Contains(m, "green"):
					marks[i] = "correct"
				case strings.Contains(m, "present") || strings.Contains(m, "yellow") || strings.Contains(m, "orange"):
					marks[i] = "present"
				default:
					marks[i] = "absent"
				}
			}
			if len(marks) == 5 {
				dup := false
				for _, h := range history {
					if h.Word == word {
						dup = true
						break
					}
				}
				if !dup {
					history = append(history, OrdknudeGuess{Word: word, Marks: marks})
				}
			}
		}
	}
	remaining = 6 - len(history)
	if len(history) > 0 {
		last := history[len(history)-1]
		allC := true
		for _, m := range last.Marks {
			if m != "correct" {
				allC = false
				break
			}
		}
		solved = allC
	}
	return history, remaining, solved, nil
}

// extractOrdknudeBoardFromFrame switches into the game iframe with br.Frame(),
// reads the completed tile rows directly from the DOM, then resets to the main
// frame. This is the most reliable extraction method: no screenshot crop
// ambiguity, no vision misinterpretation of dark-on-dark tiles.
//
// Returns the completed (submitted) guesses. Rows that are blank or currently
// being typed (no status class on tiles) are excluded.
// buildOrdknudeStateFromWords is a shared helper used by multiple extraction
// paths. Given a list of guessed words (letters only), it calls
// getColorMarksViaVision to classify the tile colors and populates st.History.
func buildOrdknudeStateFromWords(ctx context.Context, br *browser.Client, ac llm.VisionProvider, words []string, lowerRaw string, st *OrdknudeState) (OrdknudeState, error) {
	// PRIMARY: read the exact tile colours from the game DOM (getComputedStyle inside
	// the iframe). The green tiles are an unambiguous rgb(1,158,1) and absent ones
	// rgb(136,0,3), so this is far more reliable than asking a vision model to read
	// the small, dark tiles — which mis-classified e.g. the green D in GRØDE as absent.
	allMarks := getColorMarksViaDOM(ctx, br, words)
	if allMarks != nil {
		fmt.Println("   [dom] read tile colours from the board DOM (exact)")
	} else {
		// FALLBACK: vision (screenshot + LLM) only when the DOM read didn't line up.
		var visErr error
		allMarks, visErr = getColorMarksViaVision(ctx, br, ac, words)
		if visErr != nil {
			fmt.Printf("   [vision] color classification failed (%v), using all-absent fallback\n", visErr)
			allMarks = make([][]string, len(words))
			for i := range allMarks {
				allMarks[i] = []string{"absent", "absent", "absent", "absent", "absent"}
			}
		} else {
			fmt.Println("   [vision] read tile colours via screenshot (DOM unavailable)")
		}
	}
	for i, w := range words {
		st.History = append(st.History, OrdknudeGuess{Word: w, Marks: allMarks[i]})
	}
	st.Remaining = 6 - len(st.History)
	if st.Remaining < 0 {
		st.Remaining = 0
	}
	if len(st.History) > 0 {
		last := st.History[len(st.History)-1]
		allCorrect := true
		for _, m := range last.Marks {
			if m != "correct" {
				allCorrect = false
				break
			}
		}
		if allCorrect {
			st.Solved = true
			st.Answer = last.Word
			st.Remaining = 0
		}
	}
	if strings.Contains(lowerRaw, "besvaret") {
		st.Remaining = 0
	}
	return *st, nil
}

// extractOrdknudeWordsFromFrame switches into the game iframe, reads the
// accessibility snapshot, and parses the flat text node that the game exposes.
//
// The Ordknude game renders all board letters as a single text node in the
// accessibility tree:
//
//	- text: s p a l t d r ø n e d r o n e …
//
// We split by space, group in chunks of 5, and return the submitted words.
// Color marks are NOT available here — they require a separate vision call.
func extractOrdknudeWordsFromFrame(ctx context.Context, br *browser.Client) ([]string, error) {
	// Get the actual iframe src URL from the parent page so we can use it as
	// a precise frame selector. agent-browser's `frame` command may accept a URL
	// substring or exact URL in addition to CSS selectors/names.
	iframeURL, _ := br.Eval(ctx, `(() => {
		const f = document.querySelector('.kl-game__iframe, iframe[src*="ordknude"], iframe');
		return f ? f.src : '';
	})()`)
	iframeURL = strings.TrimSpace(iframeURL)

	var sels []string
	if iframeURL != "" && strings.HasPrefix(iframeURL, "http") {
		sels = append(sels, iframeURL) // try exact URL first
		// Also try just the origin+path without query/fragment
		if idx := strings.IndexAny(iframeURL, "?#"); idx > 0 {
			sels = append(sels, iframeURL[:idx])
		}
	}
	sels = append(sels, GameIframe, "iframe[src*='ordknuden']", "iframe[src*='ordknude']", "iframe")

	var frameErr error
	for _, sel := range sels {
		if frameErr = br.Frame(ctx, sel); frameErr == nil {
			fmt.Printf("   [frame] switched to game iframe using: %q\n", sel[:min(60, len(sel))])
			break
		}
	}
	if frameErr != nil {
		return nil, fmt.Errorf("could not switch to game iframe: %w", frameErr)
	}
	defer LeaveFrame(br)

	// Click through the welcome screen if present.
	welcomeSnap, _ := br.SnapshotInteractive(ctx)
	if ref := FindRefByName(welcomeSnap, []string{"SPIL ORDKNUDEN", "Spil Ordknuden"}); ref != "" {
		_ = br.Click(ctx, ref)
		time.Sleep(1500 * time.Millisecond)
	}

	time.Sleep(400 * time.Millisecond)
	fullSnap, snapErr := br.Snapshot(ctx)
	if snapErr != nil {
		return nil, fmt.Errorf("frame snapshot: %w", snapErr)
	}
	_ = os.MkdirAll(".klublotto", 0o755)
	_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-frame-snap.txt"), []byte(fullSnap), 0o644)

	// Parse the "- text: <letters>" node from the snapshot.
	// The game exposes all guessed letters as one space-separated text node.
	words := parseWordsFromSnapTextNode(fullSnap)
	if len(words) == 0 {
		// Log the snapshot for debugging.
		sample := fullSnap
		if len(sample) > 600 {
			sample = sample[:600] + "…"
		}
		fmt.Printf("   [snap-diag] no words found in frame snapshot (first 600 chars):\n%s\n", sample)
	}
	return words, nil
}

// parseWordsFromSnapTextNode extracts guessed words from the accessibility
// snapshot in one of two formats:
//
//  1. A single text node: "- text: s p a l t d r ø n e …" (older agent-browser
//     output when switching into the frame with br.Frame()).
//
//  2. Individual StaticText nodes directly under the Iframe element — the
//     format produced by the parent-page snapshot (snapshot / snapshot -F):
//
//	- Iframe [ref=eXX]
//	  - StaticText "Ø"   ← first letter of first guess
//	  - StaticText "R"
//	  …
//	  - StaticText "E"   ← last letter of last guess
//	  - generic          ← keyboard; stop here
//	    - button "Q" …
//
// This function handles both formats so existing call sites keep working.
func parseWordsFromSnapTextNode(snap string) []string {
	// Format 1: single "- text: a b c …" line.
	for _, line := range strings.Split(snap, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- text:") {
			continue
		}
		content := strings.TrimSpace(strings.TrimPrefix(line, "- text:"))
		if content == "" {
			continue
		}
		chars := strings.Fields(content)
		var words []string
		for i := 0; i+4 < len(chars); i += 5 {
			word := NormalizeDanishLetters(strings.ToUpper(strings.Join(chars[i:i+5], "")))
			if len([]rune(word)) == 5 {
				words = append(words, word)
			}
		}
		if len(words) > 0 {
			return words
		}
	}

	// Format 2: individual StaticText nodes under the Iframe element.
	return extractOrdknudeLettersFromSnap(snap)
}

// extractOrdknudeLettersFromSnap reads board letters from the parent-page
// accessibility snapshot. The Ordknude iframe exposes each tile letter as an
// individual StaticText node at the direct-child depth of the Iframe element,
// before the keyboard generic container.
//
// The caller should pass the output of br.Snapshot(ctx) (the full, non-interactive
// snapshot from the parent page) — no frame-switching required.
func extractOrdknudeLettersFromSnap(snap string) []string {
	lines := strings.Split(snap, "\n")

	// Find the Iframe element (parent-page snapshot): its direct-child StaticText
	// nodes hold the letters. When we snapshot from INSIDE the frame (br.Frame
	// now succeeds for OOPIFs), there is NO Iframe element and the letters sit at
	// the TOP level instead — handle both.
	iframeIdx := -1
	iframeIndent := 0
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "- Iframe") {
			iframeIdx = i
			iframeIndent = len(line) - len(trimmed)
			break
		}
	}

	// startIdx/childIndent select the depth at which the per-tile StaticText
	// letters appear: under the Iframe (parent snapshot) or at the top level
	// (in-frame snapshot).
	startIdx := 0
	childIndent := 0
	if iframeIdx >= 0 {
		startIdx = iframeIdx + 1
		childIndent = iframeIndent + 2 // agent-browser uses 2 spaces per level
	}

	var letters []string
	for _, line := range lines[startIdx:] {
		if line == "" {
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		lineIndent := len(line) - len(trimmed)

		if iframeIdx >= 0 && lineIndent <= iframeIndent {
			break // left the Iframe section
		}
		if lineIndent != childIndent {
			continue // skip deeper-nested nodes (e.g. button contents)
		}

		// At the direct-child level: stop at the keyboard container.
		if strings.HasPrefix(trimmed, "- generic") {
			break
		}
		// Collect single-letter StaticText nodes: - StaticText "Ø"
		if strings.HasPrefix(trimmed, "- StaticText \"") && strings.HasSuffix(trimmed, "\"") {
			inner := trimmed[len("- StaticText \"") : len(trimmed)-1]
			runes := []rune(inner)
			if len(runes) == 1 {
				ch := NormalizeDanishLetters(string(unicode.ToUpper(runes[0])))
				if len([]rune(ch)) == 1 {
					letters = append(letters, ch)
				}
			}
		}
	}

	if len(letters) < 5 {
		return nil
	}

	// Group into 5-letter words.
	var words []string
	for i := 0; i+5 <= len(letters); i += 5 {
		word := strings.Join(letters[i:i+5], "")
		words = append(words, word)
	}
	return words
}

// getColorMarksViaVision takes a screenshot of the current page and asks the
// getColorMarksViaDOM reads the exact tile colours straight from the game DOM
// inside the iframe (getComputedStyle background-color) and classifies each tile
// as correct/present/absent by RGB. The board colours are unambiguous —
// rgb(1,158,1) green / rgb(136,0,3) dark — so this is the reliable primary path,
// unlike vision which mis-read e.g. the green D in GRØDE as absent.
//
// It returns one mark row per input word (matched by the tile letters), or nil if
// the board can't be read or doesn't line up with the known words — in which case
// the caller falls back to vision. Leaves the frame on main.
func getColorMarksViaDOM(ctx context.Context, br *browser.Client, words []string) [][]string {
	if err := EnterGameFrame(ctx, br); err != nil {
		return nil
	}
	defer func() { _ = br.Frame(ctx, "main") }()
	// Collect single-letter, tile-sized elements, group them into rows by their top
	// coordinate, and keep only rows of exactly 5 (the board; the keyboard rows have
	// 7-11 keys). Each tile carries its computed background colour.
	out, err := br.Eval(ctx, `(() => {
  const tiles = [...document.querySelectorAll('*')].filter(k => {
    const t = (k.innerText || k.textContent || '').trim().toUpperCase();
    const r = k.getBoundingClientRect();
    return t.length === 1 && /[A-ZÆØÅ]/.test(t) && r.width > 15 && r.height > 15 && r.width < 90 && r.height < 90;
  });
  const rows = {};
  for (const k of tiles) {
    const r = k.getBoundingClientRect();
    const key = Math.round(r.top / 10) * 10;
    (rows[key] = rows[key] || []).push({ letter: (k.innerText || '').trim().toUpperCase(), left: Math.round(r.left), background: getComputedStyle(k).backgroundColor, className: String(k.className || '') });
  }
  const result = Object.keys(rows).map(Number).sort((a, b) => a - b)
    .map(y => rows[y].sort((a, b) => a.left - b.left))
    .filter(row => row.length === 5);
  return JSON.stringify(result);
})()`)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	var rows [][]OrdknudeTile
	if json.Unmarshal([]byte(out), &rows) != nil {
		return nil
	}
	byWord := map[string][]string{}
	for _, row := range rows {
		if len(row) != 5 {
			continue
		}
		var w strings.Builder
		marks := make([]string, 5)
		ok := true
		for j, t := range row {
			letter := NormalizeDanishLetters(t.Letter)
			mark := classifyOrdknudeTile(t)
			if letter == "" || mark == "pending" {
				ok = false
				break
			}
			w.WriteString(letter)
			marks[j] = mark
		}
		if ok {
			byWord[w.String()] = marks
		}
	}
	allMarks := make([][]string, len(words))
	for i, word := range words {
		m, found := byWord[NormalizeDanishLetters(word)]
		if !found {
			return nil // board didn't line up with the known words — let vision try
		}
		allMarks[i] = m
	}
	return allMarks
}

// vision model to classify the tile colors for the known guessed words.
// Since the words are already known, Claude only needs to read the colors.
func getColorMarksViaVision(ctx context.Context, br *browser.Client, ac llm.VisionProvider, words []string) ([][]string, error) {
	shotPath := filepath.Join(os.TempDir(), "ordknude-colors-"+time.Now().UTC().Format("20060102-150405")+".png")
	if err := br.Screenshot(ctx, shotPath); err != nil {
		return nil, err
	}
	imgBytes, err := os.ReadFile(shotPath)
	if err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-colors-input.png"), imgBytes, 0o644)

	// Keyboard-first approach: instead of reading the small (and hard to distinguish)
	// tile colors, we read the KEYBOARD at the bottom of the screen. The keyboard
	// letter keys are large and use unmistakable colors:
	//   - BRIGHT GREEN key  → letter is "correct" (right letter, right position)
	//   - BRIGHT YELLOW/AMBER key → letter is "present" (right letter, wrong position)
	//   - VERY DARK MAROON key → letter is "absent" (not in the word)
	//   - MEDIUM DARK (unchanged) key → letter not yet used
	//
	// Reading keyboard colors is far more reliable than reading the small dark-maroon
	// absent tiles in the grid (which Claude confuses with the dark red background).
	//
	// Limitation: if the same letter was "present" in guess N but later "correct"
	// in guess M>N, the keyboard shows green but guess N's tile shows yellow.
	// In that case this approach would over-report "correct" for the earlier guess.
	// This is rare and acceptable — the LLM still gets accurate info about letter
	// membership even if the positional mark is sometimes slightly off.
	var sb strings.Builder
	sb.WriteString("This is a screenshot of the Ordknude (Danish 5-letter Wordle) game.\n\n")
	sb.WriteString("The submitted guesses from top to bottom are:\n")
	for i, w := range words {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, w))
	}
	sb.WriteString(`
TASK: Read the color of each letter tile in the game board (centre of screen).

The three tile colors are:
  A = VERY DARK BROWN / DARK MAROON tile  →  "absent"
  B = BRIGHT ORANGE or AMBER (golden-yellow, like amber stone)  →  "present"
  C = BRIGHT GREEN (pure grass-green, like a green traffic light)  →  "correct"

Do NOT confuse B (orange/amber) with C (green). They are completely different hues.

Step 1 — count: scan ALL visible tiles and count how many are bright GREEN (color C).
Step 2 — locate: for each green tile, note which row (1=top) and column (1=left).
Step 3 — assign: read each submitted row left to right, label each tile A/B/C, then map to absent/present/correct.

Also cross-check with the keyboard at the bottom of the screen:
  • Bright GREEN key  →  that letter has a "correct" tile somewhere
  • Bright ORANGE/AMBER key  →  that letter has a "present" tile somewhere
  • Dark key  →  that letter is "absent" in all guesses

Output ONLY these lines (no markdown, no explanation, no json):
`)
	for _, w := range words {
		sb.WriteString(fmt.Sprintf("%s: absent,absent,absent,absent,absent\n", w))
	}
	sb.WriteString("\nReplace each 'absent' with correct/present/absent.")

	text, err := ac.ExtractFromImage(ctx, imgBytes, "image/png", sb.String())
	if err != nil {
		return nil, err
	}
	_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-colors-raw.txt"), []byte(text), 0o644)

	// Parse the response: one line per word "WORD: m1,m2,m3,m4,m5"
	allMarks := make([][]string, len(words))
	for i := range allMarks {
		allMarks[i] = []string{"absent", "absent", "absent", "absent", "absent"}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		// Strip JSON-style quotes — vision sometimes returns {"WORD": [...]} format.
		word := NormalizeDanishLetters(strings.Trim(strings.TrimSpace(parts[0]), `"'`))
		markStr := strings.ToLower(strings.TrimSpace(parts[1]))
		// Vision sometimes wraps marks in a JSON array: ["correct", "absent", ...],
		// The trailing comma (on non-last JSON entries) causes a spurious 6th split
		// element → len check fails. Strip brackets and trailing comma first.
		markStr = strings.TrimSuffix(strings.TrimSpace(markStr), ",")
		markStr = strings.Trim(strings.TrimSpace(markStr), "[]")
		marks := strings.Split(markStr, ",")
		if len(marks) != 5 {
			continue
		}
		for j := range marks {
			m := strings.TrimSpace(marks[j])
			switch {
			case strings.Contains(m, "correct"):
				marks[j] = "correct"
			case strings.Contains(m, "present"):
				marks[j] = "present"
			default:
				marks[j] = "absent"
			}
		}
		// Match against known words by position or by word text.
		for i, w := range words {
			if w == word {
				allMarks[i] = marks
				break
			}
		}
	}
	return allMarks, nil
}

func ExtractOrdknudeState(ctx context.Context, br *browser.Client, ac llm.VisionProvider) (OrdknudeState, error) {
	var st OrdknudeState

	// Check the parent page text first for end-of-game markers.
	// When all 6 guesses are used (win or loss), the game replaces the iframe
	// with a full-page result screen. We detect this before trying the frame.
	raw, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
	st.Raw = raw
	lowerRaw := strings.ToLower(raw)
	endPhrases := []string{
		"det rigtige svar var",   // loss screen: "The correct answer was: gummi"
		"lige ved og næsten",     // loss: "Close but no cigar"
		"tillykke",               // win: "Congratulations"
		"du vandt",               // win: "You won"
		"super imponerende",      // win banner: "Super imponerende!"
		"fandt frem til dagens ord", // win banner: "Du fandt frem til dagens ord"
		"ord-haj",                // win banner: "Du er en sand ord-haj!"
		"besvaret",               // already answered today
		"allerede besvaret",
		"du har allerede",
		"dagens første lod",      // lod awarded = completed
	}
	for _, phrase := range endPhrases {
		if strings.Contains(lowerRaw, phrase) {
			st.Remaining = 0
			// The board is wiped to a 0-guess overlay on a win, so rely on the
			// banner text (not "tillykke"/"du vandt" alone — Ordknuden's real win
			// copy is "Super imponerende! … ord-haj!").
			if IsOrdknudeWinText(raw) {
				st.Solved = true
				st.Answer = findFiveLetterAnswer(raw)
			}
			// Try to get the history from the frame even on end screen (may fail gracefully).
			if frameWords, err := extractOrdknudeWordsFromFrame(ctx, br); err == nil && len(frameWords) > 0 {
				allMarks, _ := getColorMarksViaVision(ctx, br, ac, frameWords)
				if allMarks == nil {
					allMarks = make([][]string, len(frameWords))
					for i := range allMarks {
						allMarks[i] = []string{"absent", "absent", "absent", "absent", "absent"}
					}
				}
				for i, w := range frameWords {
					st.History = append(st.History, OrdknudeGuess{Word: w, Marks: allMarks[i]})
				}
			}
			return st, nil
		}
	}

	// We stay on the Danske Spil parent page at all times. Never Open the
	// cross-origin immerspiele iframe URL as top-level, because that prevents
	// the game from posting completion events to the parent (no daily lod or
	// checkmark on the Spil & Quiz overview).

	// PRIMARY (fast): read board letters from the parent-page full snapshot.
	// The Ordknude iframe exposes each guessed tile letter as a direct-child
	// StaticText node under the Iframe accessibility element — no frame-switch
	// needed. The welcome screen ("SPIL ORDKNUDEN") hides these nodes, so we
	// click through it if the first attempt finds nothing, then retry.
	//
	// Confirmed: snapshot / snapshot -F / snapshot -C give identical output for
	// this page — the iframe content is included in the base accessibility tree.
	_ = os.MkdirAll(".klublotto", 0o755)
	for attempt := 0; attempt < 2; attempt++ {
		if parentSnap, err := br.Snapshot(ctx); err == nil {
			if snapWords := parseWordsFromSnapTextNode(parentSnap); len(snapWords) > 0 {
				fmt.Printf("   [snap] found %d guessed words: %s\n", len(snapWords), strings.Join(snapWords, ", "))
				_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-snap.txt"), []byte(parentSnap), 0o644)
				return buildOrdknudeStateFromWords(ctx, br, ac, snapWords, lowerRaw, &st)
			}
			if attempt == 0 {
				// No letters yet — welcome screen may be showing. Click through it.
				fmt.Println("   [snap] no board letters found; clicking through welcome screen if present...")
				_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-snap-pre-start.txt"), []byte(parentSnap), 0o644)
				_ = startWordGameIfPresent(ctx, br, "SPIL ORDKNUDEN", "Spil Ordknuden")
				time.Sleep(1200 * time.Millisecond)
			}
		}
	}

	// SECONDARY: frame-switch path (legacy; needed only if the base snapshot
	// ever stops exposing iframe content, e.g. on a truly cross-origin page).
	if frameWords, frameErr := extractOrdknudeWordsFromFrame(ctx, br); frameErr == nil && len(frameWords) > 0 {
		fmt.Printf("   [frame] found %d guessed words: %s\n", len(frameWords), strings.Join(frameWords, ", "))
		return buildOrdknudeStateFromWords(ctx, br, ac, frameWords, lowerRaw, &st)
	} else if frameErr != nil {
		fmt.Printf("   [frame] word extraction failed (%v), trying parent -F snapshot...\n", frameErr)

		// INTERMEDIATE: try SnapshotWithFrames (= same content as bare Snapshot
		// on this page; kept as a fallback in case future agent-browser versions
		// differ). parseWordsFromSnapTextNode now handles the individual StaticText
		// format exposed by this page.
		if parentSnap, pErr := br.SnapshotWithFrames(ctx); pErr == nil {
			_ = os.MkdirAll(".klublotto", 0o755)
			_ = os.WriteFile(filepath.Join(".klublotto", "ordknude-parent-F-snap.txt"), []byte(parentSnap), 0o644)
			if parentWords := parseWordsFromSnapTextNode(parentSnap); len(parentWords) > 0 {
				fmt.Printf("   [parent-F] found %d guessed words: %s\n", len(parentWords), strings.Join(parentWords, ", "))
				return buildOrdknudeStateFromWords(ctx, br, ac, parentWords, lowerRaw, &st)
			}
			fmt.Printf("   [parent-F] no letters found in snapshot, falling back to vision\n")
		}
	}

	_ = startWordGameIfPresent(ctx, br, "SPIL ORDKNUDEN", "Spil Ordknuden")

	// FALLBACK: vision (screenshot of parent + LLM) to read the board.
	if ac != nil {
		if h, rem, sol, verr := extractBoardViaVision(ctx, br, ac); verr == nil {
			st.History = h
			st.Remaining = rem
			st.Solved = sol
			raw, _ := br.Eval(ctx, `document.body ? document.body.innerText : ""`)
			st.Raw = raw
			if st.Solved {
				st.Answer = findFiveLetterAnswer(raw)
			}
			lowerRaw := strings.ToLower(raw)
			if strings.Contains(lowerRaw, "besvaret") || strings.Contains(lowerRaw, "allerede besvaret") || strings.Contains(lowerRaw, "du har allerede") {
				st.Remaining = 0
			}
			needEnsure := (len(h) == 0 && st.Remaining > 0)
			if needEnsure {
				_ = ensureOrdknudeGameStarted(ctx, br)
				if h2, rem2, sol2, verr2 := extractBoardViaVision(ctx, br, ac); verr2 == nil && (len(h2) > len(h) || sol2) {
					st.History = h2
					st.Remaining = rem2
					st.Solved = sol2
				}
			}
			return st, nil
		}
	}

	// LAST RESORT: parent-page DOM (cross-origin, likely empty for iframe games).
	_ = startWordGameIfPresent(ctx, br, "SPIL ORDKNUDEN", "Spil Ordknuden")
	_ = ensureOrdknudeGameStarted(ctx, br)

	// raw was already read at the top; re-read to refresh after game-start clicks.
	raw, _ = br.Eval(ctx, `document.body ? document.body.innerText : ""`)
	st.Raw = raw
	if strings.Contains(strings.ToLower(raw), "vundet") {
		st.Solved = true
	}
	// Note: readOrdknudeRows may return no rows from parent due to cross-origin iframe.
	// Vision path (preferred when ac provided) handles accurate history without switching.
	rows, err := readOrdknudeRows(ctx, br)
	if err != nil {
		// ignore, may be empty
		rows = nil
	}
	for _, row := range rows {
		if len(row) != 5 {
			continue
		}
		var word strings.Builder
		var marks []string
		complete := true
		for _, tile := range row {
			if tile.Letter == "" || tile.Mark == "" || tile.Mark == "pending" {
				complete = false
				break
			}
			word.WriteString(tile.Letter)
			marks = append(marks, tile.Mark)
		}
		if complete {
			st.History = append(st.History, OrdknudeGuess{Word: word.String(), Marks: marks})
		}
	}
	st.Remaining = 6 - len(st.History)
	if st.Solved {
		st.Answer = findFiveLetterAnswer(raw)
	}

	// No iframe navigation in this path; ensure we are on parent.
	if u, _ := br.URL(ctx); isImmerspieleURL(u) || !strings.Contains(u, "danskespil.dk") {
		_ = br.Open(ctx, OrdknudeURL)
		br.WaitSettled(ctx)
		time.Sleep(500 * time.Millisecond)
	}

	return st, nil
}

func isImmerspieleURL(u string) bool {
	lu := strings.ToLower(u)
	return strings.Contains(lu, "immerspiele") || strings.Contains(lu, "klub-lotto.immerspiele.com")
}

// ensureOrdknudeGameStarted clicks the "SPIL ORDKNUDEN" welcome-screen button
// via the frames-inclusive parent snapshot. If the button is not visible (the
// game is already active — e.g. a guess was just made), no click is performed.
//
// The old approach (coordinate click at iframe 0.5 × 0.68) accidentally hit
// keyboard keys like Y when the game was already running, causing spurious
// input. Using FindRefByName ensures we only click when there is actually a
// welcome screen to dismiss.
func ensureOrdknudeGameStarted(ctx context.Context, br *browser.Client) error {
	snap, err := br.SnapshotInteractiveWithFrames(ctx)
	if err != nil {
		return nil // best effort
	}
	ref := FindRefByName(snap, []string{"SPIL ORDKNUDEN", "Spil Ordknuden"})
	if ref == "" {
		return nil // game already active — do not click anything
	}
	_ = br.Click(ctx, ref)
	time.Sleep(1500 * time.Millisecond)
	return nil
}

func readOrdknudeRows(ctx context.Context, br *browser.Client) ([][]OrdknudeTile, error) {
	// Robust board reader for the immerspiele ordknuden game.
	// Tries multiple structural patterns because class names are often CSS-module hashed
	// (_row_xxx, _tile_xxx etc). We look for clusters of ~5 square tile-like children
	// that have letters or colored backgrounds, preferring upper-page ones (the board)
	// over the bottom keyboard.
	out, err := br.Eval(ctx, `(() => {
  const candidates = [];
  const all = Array.from(document.querySelectorAll('*'));
  for (const el of all) {
    const kids = Array.from(el.children || []);
    const tiles = kids.filter(k => {
      const r = k.getBoundingClientRect();
      const txt = (k.innerText || k.textContent || '').trim().toUpperCase();
      const isLetterish = txt.length === 1 && /[A-ZÆØÅ]/.test(txt);
      const isSizedTile = r.width > 15 && r.height > 15 && r.width < 90 && r.height < 90;
      return (isLetterish || isSizedTile) && (r.width * r.height > 200);
    }).map(k => {
      const r = k.getBoundingClientRect();
      return {
        letter: (k.innerText || k.textContent || '').trim().toUpperCase(),
        className: String(k.className || ''),
        background: getComputedStyle(k).backgroundColor,
        top: r.top,
        left: r.left
      };
    }).filter(t => t.letter || t.className || t.background);
    if (tiles.length >= 5) {
      // sort left-to-right
      tiles.sort((a,b) => a.left - b.left);
      candidates.push({ y: tiles[0].top || 0, tiles: tiles.slice(0,5) });
    }
  }
  // take the topmost groups that have exactly 5 tiles (the board rows), up to 6
  candidates.sort((a,b) => a.y - b.y);
  const board = candidates.filter(c => c.tiles.length === 5).slice(0,6).map(c => c.tiles);
  // final filter: only rows that are clearly in the game area (not too low like keyboard)
  const filtered = board.filter((row, idx) => {
    const avgTop = row.reduce((s,t) => s + (t.top||0), 0) / row.length;
    return avgTop < (window.innerHeight * 0.85); // board is upper
  });
  return JSON.stringify(filtered.length ? filtered : board);
})()`)
	if err != nil {
		return nil, fmt.Errorf("read Ordknuden board: %w", err)
	}
	var rows [][]OrdknudeTile
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse Ordknuden board: %w", err)
	}
	for i := range rows {
		for j := range rows[i] {
			rows[i][j].Letter = NormalizeDanishLetters(rows[i][j].Letter)
			rows[i][j].Mark = classifyOrdknudeTile(rows[i][j])
		}
	}
	return rows, nil
}

func classifyOrdknudeTile(tile OrdknudeTile) string {
	bg := strings.ToLower(strings.TrimSpace(tile.Background))
	cls := strings.ToLower(tile.ClassName)
	if strings.Contains(cls, "correct") || strings.Contains(cls, "green") || isRGB(bg, func(r, g, b int) bool { return g > 120 && r < 90 && b < 100 }) {
		return "correct"
	}
	if strings.Contains(cls, "present") || strings.Contains(cls, "yellow") || strings.Contains(cls, "orange") || isRGB(bg, func(r, g, b int) bool { return r > 150 && g > 90 && b < 100 }) {
		return "present"
	}
	if strings.Contains(cls, "absent") || strings.Contains(cls, "wrong") || isRGB(bg, func(r, g, b int) bool { return r > 90 && g < 80 && b < 80 }) {
		return "absent"
	}
	return "pending"
}

var rgbRe = regexp.MustCompile(`rgba?\((\d+),\s*(\d+),\s*(\d+)`)

func isRGB(s string, pred func(r, g, b int) bool) bool {
	m := rgbRe.FindStringSubmatch(s)
	if len(m) != 4 {
		return false
	}
	var r, g, b int
	_, _ = fmt.Sscanf(m[1], "%d", &r)
	_, _ = fmt.Sscanf(m[2], "%d", &g)
	_, _ = fmt.Sscanf(m[3], "%d", &b)
	return pred(r, g, b)
}

func IsDanishFiveLetterWord(word string) bool {
	word = NormalizeDanishLetters(word)
	if utf8.RuneCountInString(word) != 5 {
		return false
	}
	for _, r := range word {
		if !strings.ContainsRune(DanishAlphabet, r) {
			return false
		}
	}
	return true
}

func BuildOrdknudePrompt(st OrdknudeState, rejected []string) string {
	var b strings.Builder
	b.WriteString("Du løser Dansk Spils Ordknuden — et dansk 5-bogstavs Wordle-spil.\n\n")
	b.WriteString("KRAV TIL KANDIDATER:\n")
	b.WriteString("- Hvert forslag SKAL være et rigtigt dansk ord på præcis 5 bogstaver (inkl. Æ, Ø, Å).\n")
	b.WriteString("- Brug KUN ord der findes i Den Danske Ordbog (ordnet.dk/ddo). Opfundne eller norske/svenske former er ugyldige.\n")
	b.WriteString("- Svar KUN med ét (1) bedste gæt som JSON: {\"answer\":\"ORDXY\",\"confidence\":\"high|medium|low\",\"rationale\":\"...\"}\n")
	b.WriteString("- Returner præcis ÉT ord — ikke en liste, ikke flere kandidater. Vælg selv det bedste.\n\n")

	b.WriteString("FARVE-REGLER (Wordle):\n")
	b.WriteString("- 🟩 correct (grøn):  bogstavet er KORREKT og på den RIGTIGE PLADS. Det SKAL stå der i alle fremtidige gæt.\n")
	b.WriteString("- 🟨 present (gul):   bogstavet ER i ordet, men på en FORKERT PLADS. Placer det et andet sted — aldrig på de pladser det har fået gult eller grå.\n")
	b.WriteString("- ⬛ absent (grå):    bogstavet ER IKKE i ordet. Brug det aldrig igen.\n\n")
	b.WriteString("EKSEMPEL:\n")
	b.WriteString("  TALER → ⬛⬛🟨⬛🟩   (T,A fravær; E gul på plads 3 — skal flyttes; R fravær; L grøn på plads 5)\n")
	b.WriteString("  DÅRLIGT næste gæt: GILET (E stadig på plads 3 — overtræder gul-reglen)\n")
	b.WriteString("  GODT næste gæt:    KNÆLE (E på plads 4, L på plads 5 ✓)\n\n")

	if len(st.History) == 0 {
		b.WriteString("Ingen accepterede gæt endnu — start med et godt 5-bogstavs dansk ord med varierede bogstaver.\n")
	} else {
		b.WriteString("Tidligere gæt (position 1-5, 1=venstre):\n")
		for i, h := range st.History {
			emojis := make([]string, len(h.Marks))
			for j, m := range h.Marks {
				switch m {
				case "correct":
					emojis[j] = "🟩"
				case "present":
					emojis[j] = "🟨"
				default:
					emojis[j] = "⬛"
				}
			}
			fmt.Fprintf(&b, "%d. %s  %s  (%s)\n", i+1, h.Word, strings.Join(emojis, ""), strings.Join(h.Marks, ","))
		}

		// Derive explicit constraints from history so the LLM doesn't have to.
		// correctAt[pos] = letter (1-indexed), presentForbidden[letter] = set of forbidden positions,
		// mustInclude = set of letters that must appear, absentLetters = letters not in word.
		correctAt := map[int]rune{}               // pos → letter
		presentForbidden := map[rune][]int{}       // letter → positions where it's forbidden (present/absent)
		mustInclude := map[rune]bool{}             // letters known to be in the word
		absentLetters := map[rune]bool{}           // letters not in word at all

		for _, h := range st.History {
			runes := []rune(h.Word)
			for i, mark := range h.Marks {
				if i >= len(runes) {
					break
				}
				ch := unicode.ToUpper(runes[i])
				pos := i + 1 // 1-indexed
				switch mark {
				case "correct":
					correctAt[pos] = ch
				case "present":
					mustInclude[ch] = true
					presentForbidden[ch] = append(presentForbidden[ch], pos)
				case "absent":
					// Only mark absent if not also correct/present elsewhere
					presentForbidden[ch] = append(presentForbidden[ch], pos)
					absentLetters[ch] = true
				}
			}
		}
		// Letters that are mustInclude are NOT absent (grey for a letter can coexist with
		// yellow of the same letter meaning "only N occurrences"). Remove from absent if present.
		for ch := range mustInclude {
			delete(absentLetters, ch)
		}
		// Also remove from absent if correct anywhere.
		for _, ch := range correctAt {
			delete(absentLetters, ch)
		}

		b.WriteString("\nAFLEDTE BEGRÆNSNINGER (beregnet automatisk fra feedback — SKAL overholdes præcist):\n")

		// Correct positions
		if len(correctAt) > 0 {
			positions := make([]int, 0, len(correctAt))
			for p := range correctAt {
				positions = append(positions, p)
			}
			sort.Ints(positions)
			b.WriteString("GRØNNE (fastlåste pladser, bogstavet SKAL stå her):\n")
			for _, p := range positions {
				fmt.Fprintf(&b, "  - Position %d = '%c'\n", p, correctAt[p])
			}
		}

		// Must-include with forbidden positions
		if len(mustInclude) > 0 {
			mustLetters := make([]rune, 0, len(mustInclude))
			for ch := range mustInclude {
				mustLetters = append(mustLetters, ch)
			}
			sort.Slice(mustLetters, func(i, j int) bool { return mustLetters[i] < mustLetters[j] })
			b.WriteString("GULE (bogstav SKAL være i ordet, men IKKE på disse forbudte pladser):\n")
			for _, ch := range mustLetters {
				forbidden := presentForbidden[ch]
				// deduplicate
				seen := map[int]bool{}
				dedup := []int{}
				for _, p := range forbidden {
					if !seen[p] {
						seen[p] = true
						dedup = append(dedup, p)
					}
				}
				sort.Ints(dedup)
				forbidStrs := make([]string, len(dedup))
				for i, p := range dedup {
					forbidStrs[i] = fmt.Sprintf("%d", p)
				}
				fmt.Fprintf(&b, "  - '%c' SKAL være i ordet — FORBUDT på plads: %s\n", ch, strings.Join(forbidStrs, ", "))
			}
		}

		// Absent letters
		if len(absentLetters) > 0 {
			absentList := make([]rune, 0, len(absentLetters))
			for ch := range absentLetters {
				absentList = append(absentList, ch)
			}
			sort.Slice(absentList, func(i, j int) bool { return absentList[i] < absentList[j] })
			absent := make([]string, len(absentList))
			for i, ch := range absentList {
				absent[i] = string(ch)
			}
			fmt.Fprintf(&b, "GRÅ (bogstaverne MÅ IKKE være i ordet): %s\n", strings.Join(absent, ", "))
		}
	}
	if len(rejected) > 0 {
		fmt.Fprintf(&b, "Afviste ord (må ikke foreslås): %s\n", strings.Join(rejected, ", "))
	}
	b.WriteString("\nForeslå 3-6 forskellige kandidater der overholder ALLE regler ovenfor. Prioritér ord der tester nye bogstaver og respekterer gule positioner.\n")
	b.WriteString("VIGTIGT: Hvis GULE begrænsninger er i konflikt med GRØNNE (dvs. et gult bogstav ikke har nogen ledig plads fordi alle andre pladser er låst af grønne), ignorér da de umulige GULE begrænsninger og foreslå ord der matcher alle GRØNNE pladser og udelukker GRÅ bogstaver. Notér konflikten i rationale. Returnér ALTID mindst 3 kandidater — tom liste er ikke tilladt.\n")
	return b.String()
}

func BuildOrdKloeverPrompt(st OrdKloeverState, maxProbe int) string {
	var b strings.Builder

	// Header — keep it minimal; thinking models loop when given too much to reason about.
	b.WriteString("Du løser Dansk Spils Ordkløver — et Wheel-of-Fortune-lignende gætteleg med danske udtryk.\n")
	b.WriteString("Skriv INGEN analyse eller forklaring. Returner straks kun JSON:\n")
	b.WriteString(`{"candidates":[{"answer":"FRASE","confidence":"high|medium|low","rationale":"kort"}]}` + "\n")
	switch {
	case maxProbe <= 0:
		b.WriteString("Giv kun 1 svar — dette er SIDSTE FORSØG. Brug KUN korrekte danske udtryk.\n\n")
	case maxProbe == 1:
		b.WriteString("Giv eksakt 1 svar på løsningen, eller alternativt foreslå ét nyt bogstav vi kan gætte. Brug KUN korrekte danske udtryk.\n\n")
	default:
		b.WriteString("Giv eksakt 1 svar på løsningen, eller alternativt foreslå op til to nye bogstaver vi kan gætte. Brug KUN korrekte danske udtryk.\n\n")
	}

	fmt.Fprintf(&b, "Kategorien er: %s\n", st.Category)
	if st.Hint != "" && st.Hint != "none" {
		fmt.Fprintf(&b, "Ledetråd: %s\n", st.Hint)
	}

	// Human-readable shape: "1 ord på 9 bogstaver" / "3 ord på 4, 2 og 7 bogstaver"
	wordLengths := LengthPattern(st.Shape)
	if len(wordLengths) > 0 {
		lenStrs := make([]string, len(wordLengths))
		for i, l := range wordLengths {
			lenStrs[i] = fmt.Sprintf("%d", l)
		}
		n := len(wordLengths)
		var lenDesc string
		switch n {
		case 1:
			lenDesc = lenStrs[0]
		case 2:
			lenDesc = lenStrs[0] + " og " + lenStrs[1]
		default:
			lenDesc = strings.Join(lenStrs[:n-1], ", ") + " og " + lenStrs[n-1]
		}
		fmt.Fprintf(&b, "Svarmønster er: %d ord på %s bogstaver\n", n, lenDesc)
	} else if st.Shape != "" {
		fmt.Fprintf(&b, "Svarmønster: %s\n", st.Shape)
	}

	if st.Board != "" {
		// Present each word group on its own line so the model counts positions
		// correctly. The board is "/"-separated word groups; within a group each
		// space-separated token is one position: a letter, "_" (unknown), or "-"
		// (a structural dash that is PART of the answer and occupies a position
		// but is NOT typed — like a space between words).
		groups := strings.Split(st.Board, "/")
		hasDash := false
		gi := 0
		for _, grp := range groups {
			tokens := strings.Fields(grp)
			if len(tokens) == 0 {
				continue
			}
			gi++
			compact := strings.Join(tokens, "")
			var knownLines []string
			for i, t := range tokens {
				switch t {
				case "_":
					// unknown — skip
				case "-":
					hasDash = true
					knownLines = append(knownLines, fmt.Sprintf("position %d = bindestreg '-'", i+1))
				default:
					knownLines = append(knownLines, fmt.Sprintf("position %d = %s", i+1, t))
				}
			}
			fmt.Fprintf(&b, "Ord %d (%d tegn, ingen mellemrum): %s\n", gi, len(tokens), compact)
			if len(knownLines) > 0 {
				fmt.Fprintf(&b, "   Kendte positioner: %s\n", strings.Join(knownLines, ", "))
			}
		}
		if hasDash {
			b.WriteString("BEMÆRK: En bindestreg '-' er en fast del af svaret (fx et bindestregsord) — den optælles i tegn-antallet, men skrives ikke som et bogstav. Medtag bindestregen i dit svar, fx \"TRYGHEDS-NARKOMAN\".\n")
		}
		b.WriteString("Kandidater SKAL have præcis de rigtige bogstaver/tegn på de angivne positioner.\n")
	}
	if st.GuessedLetters != "" {
		fmt.Fprintf(&b, "Allerede prøvede bogstaver: %s\n", st.GuessedLetters)
	}

	b.WriteString("\nSTRATEGI:\n")
	b.WriteString("1. Match alle kendte bogstaver/tegn præcist (position for position).\n")
	b.WriteString("2. Brug IKKE bogstaver fra 'Allerede prøvede bogstaver' medmindre de vises i svarmønsteret.\n")
	b.WriteString("3. Gentag IKKE tidligere forkerte fulde gæt.\n")
	return b.String()
}

func ParseCandidateJSON(raw string) ([]WordCandidate, error) {
	clean := strings.TrimSpace(raw)
	// Strip markdown fences if the model added them.
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)

	// Thinking models (e.g. gemini-3.5-flash) emit reasoning text before JSON.
	// Scan for the first '[' or '{' so we extract only the JSON portion.
	if firstBrace := strings.IndexAny(clean, "[{"); firstBrace > 0 {
		clean = clean[firstBrace:]
	}

	// Format 1: bare JSON array. Elements may be objects ([{"answer":…}]) OR
	// plain strings (["BINDE","FINDE"]) — some models (e.g. gpt-5.5) return the
	// latter despite the prompt asking for objects.
	if strings.HasPrefix(clean, "[") {
		var raws []json.RawMessage
		if err := json.Unmarshal([]byte(ExtractJSONArray(clean)), &raws); err == nil && len(raws) > 0 {
			if cands := decodeCandidateList(raws, "", ""); len(cands) > 0 {
				return cands, nil
			}
		}
	}

	// Format 2: {"candidates":[…]}  or  {"answer":"…"}  (existing models). The
	// candidates entries may again be objects or plain strings, with the
	// confidence/rationale carried at the top level — apply those as defaults.
	var wrapped struct {
		Candidates []json.RawMessage `json:"candidates"`
		Answer     string            `json:"answer"`
		Confidence string            `json:"confidence"`
		Rationale  string            `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(ExtractJSONObject(clean)), &wrapped); err != nil {
		return nil, err
	}
	cands := decodeCandidateList(wrapped.Candidates, wrapped.Confidence, wrapped.Rationale)
	if len(cands) == 0 && wrapped.Answer != "" {
		cands = append(cands, WordCandidate{Answer: NormalizeDanishPhrase(wrapped.Answer), Confidence: wrapped.Confidence, Rationale: wrapped.Rationale})
	}
	return cands, nil
}

// KrydsordBatchClue is one clue in a batched candidate request.
type KrydsordBatchClue struct {
	SlotID  string
	Clue    string
	Length  int
	IsImage bool
}

// BuildKrydsordBatchPrompt builds a single prompt asking the word provider for
// candidates for EVERY clue at once (replacing one LLM call per clue). The
// model returns JSON keyed by slot id. Image clues arrive as English object
// descriptions (e.g. "turnip", "desk lamp", "t-shirt", "grill"); the prompt
// tells the model to answer with the Danish word for the depicted object.
func BuildKrydsordBatchPrompt(clues []KrydsordBatchClue) string {
	var b strings.Builder
	b.WriteString("Du løser et dansk krydsord (clues-in-squares). For HVER ledetråd nedenfor, giv 1-8 sandsynlige danske svar med PRÆCIS det angivne antal bogstaver.\n")
	b.WriteString("Regler:\n")
	b.WriteString("- Svar er danske ord eller forkortelser, KUN bogstaver (ÆØÅ tilladt), INGEN mellemrum eller tegn.\n")
	b.WriteString("- Antallet af bogstaver skal være præcis 'len'.\n")
	b.WriteString("- Hvis ledetråden er en engelsk beskrivelse af et billede (fx \"turnip\", \"desk lamp\", \"t-shirt\", \"grill\"), så svar med det danske ord for tingen (fx ROE, LAMPE, TSHIRT, GRILL).\n")
	b.WriteString("Returnér KUN JSON på formen: {\"slots\":[{\"id\":\"A1\",\"candidates\":[\"ORD1\",\"ORD2\"]},{\"id\":\"D3\",\"candidates\":[\"ORD\"]}]}\n\n")
	b.WriteString("Ledetråde:\n")
	for _, c := range clues {
		clueText := c.Clue
		kind := ""
		if c.IsImage {
			// Make the picture-ness explicit in the clue text itself so the model
			// never mistakes the English description for a literal word to translate.
			clueText = "an image of a " + c.Clue
			kind = " (BILLEDE — svar med det danske ord for tingen på billedet)"
		}
		fmt.Fprintf(&b, "- id=%s len=%d clue=%q%s\n", c.SlotID, c.Length, clueText, kind)
	}
	return b.String()
}

// ParseKrydsordBatchCandidates parses the batched {"slots":[...]} JSON into a
// per-slot map, keeping only candidates whose letter count matches the slot's
// requested length (want[slotID]). Tolerant of markdown fences and leading
// reasoning text, like ParseCandidateJSON.
func ParseKrydsordBatchCandidates(raw string, want map[string]int) (map[string][]WordCandidate, error) {
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)
	if i := strings.IndexAny(clean, "[{"); i > 0 {
		clean = clean[i:]
	}
	var wrapped struct {
		Slots []struct {
			ID         string            `json:"id"`
			Candidates []json.RawMessage `json:"candidates"`
		} `json:"slots"`
	}
	if err := json.Unmarshal([]byte(ExtractJSONObject(clean)), &wrapped); err != nil {
		return nil, err
	}
	out := map[string][]WordCandidate{}
	for _, s := range wrapped.Slots {
		if s.ID == "" {
			continue
		}
		want, hasLen := want[s.ID]
		var good []WordCandidate
		for _, c := range decodeCandidateList(s.Candidates, "", "") {
			if !hasLen || len([]rune(NormalizeDanishLetters(c.Answer))) == want {
				good = append(good, c)
			}
		}
		if len(good) > 0 {
			out[s.ID] = good
		}
	}
	return out, nil
}

// ParseKrydsordAnswerMap parses an assembler response that gives one answer per
// slot id, into a slotID->answer map. Accepts {"answers":{"A1":"ORD"}},
// {"answers":[{"id":"A1","answer":"ORD"}]}, or a bare {"A1":"ORD"} object.
// Tolerant of markdown fences and leading reasoning text.
func ParseKrydsordAnswerMap(raw string) (map[string]string, error) {
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "```json")
	clean = strings.TrimPrefix(clean, "```")
	clean = strings.TrimSuffix(clean, "```")
	clean = strings.TrimSpace(clean)
	if i := strings.IndexAny(clean, "[{"); i > 0 {
		clean = clean[i:]
	}
	obj := ExtractJSONObject(clean)
	out := map[string]string{}

	var wrapped struct {
		Answers json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal([]byte(obj), &wrapped); err == nil && len(wrapped.Answers) > 0 {
		if addKrydsordAnswerEntries(wrapped.Answers, out) {
			return out, nil
		}
	}
	if addKrydsordAnswerEntries(json.RawMessage(obj), out) {
		return out, nil
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no per-slot answers parsed from %q", truncateForErr(raw, 120))
	}
	return out, nil
}

func addKrydsordAnswerEntries(raw json.RawMessage, out map[string]string) bool {
	// Object form: {"A1":"ORD"}.
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
		for k, v := range m {
			if id := strings.TrimSpace(k); id != "" {
				out[id] = strings.TrimSpace(v)
			}
		}
		return len(out) > 0
	}
	// Array form: [{"id":"A1","answer":"ORD"}].
	var arr []struct {
		ID     string `json:"id"`
		Answer string `json:"answer"`
		Word   string `json:"word"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		for _, e := range arr {
			a := e.Answer
			if a == "" {
				a = e.Word
			}
			if id := strings.TrimSpace(e.ID); id != "" {
				out[id] = strings.TrimSpace(a)
			}
		}
		return len(out) > 0
	}
	return false
}

func truncateForErr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// decodeCandidateList converts raw JSON candidate entries into WordCandidates,
// tolerating each element being either an object ({"answer":…}) or a bare
// string ("BINDE"). For bare strings the supplied confidence/rationale (usually
// from the enclosing object) are applied as defaults.
func decodeCandidateList(raws []json.RawMessage, defConfidence, defRationale string) []WordCandidate {
	var out []WordCandidate
	for _, r := range raws {
		var obj WordCandidate
		if err := json.Unmarshal(r, &obj); err == nil && obj.Answer != "" {
			obj.Answer = NormalizeDanishPhrase(obj.Answer)
			out = append(out, obj)
			continue
		}
		var s string
		if err := json.Unmarshal(r, &s); err == nil && strings.TrimSpace(s) != "" {
			out = append(out, WordCandidate{Answer: NormalizeDanishPhrase(s), Confidence: defConfidence, Rationale: defRationale})
		}
	}
	return out
}

// ExtractJSONArray returns the substring from the first '[' to the matching ']'.
func ExtractJSONArray(s string) string {
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start >= 0 && end >= start {
		return s[start : end+1]
	}
	return s
}

func ExtractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end >= start {
		return s[start : end+1]
	}
	return s
}

// --- Ordknude fallback logic (ported from ignored ordknude.go for resilience in auto-play) ---

func FallbackOrdknudeGuess(history []OrdknudeGuess, rejected []string) string {
	candidates := append([]string(nil), ordknudeCandidates...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return ordknudeScore(candidates[i]) > ordknudeScore(candidates[j])
	})
	// Strict pass: honour all Wordle constraints (green position + present must-include + absent exclusion).
	for _, c := range candidates {
		word := NormalizeDanishLetters(c)
		if validOrdknudeGuess(word, history, rejected) {
			return word
		}
	}
	// Relaxed pass: only enforce green (correct) positions and absent (gray) exclusions.
	// Used when present (yellow) constraints are unsatisfiable (e.g. all other positions locked by greens).
	for _, c := range candidates {
		word := NormalizeDanishLetters(c)
		if validOrdknudeGuessRelaxed(word, history, rejected) {
			return word
		}
	}
	return ""
}

// validOrdknudeGuessRelaxed checks only:
//   - word has not been tried/rejected
//   - correct (green) letters are at their required positions
//   - absent (gray) letters do not appear in the word
//
// It ignores present (yellow) must-include and position-forbidden constraints,
// which can become unsatisfiable when all remaining positions are locked by greens.
func validOrdknudeGuessRelaxed(word string, history []OrdknudeGuess, rejected []string) bool {
	if !IsDanishFiveLetterWord(word) {
		return false
	}
	if usedOrdknudeWord(word, history, rejected) {
		return false
	}
	runes := []rune(word)
	for _, h := range history {
		hRunes := []rune(h.Word)
		for i, mark := range h.Marks {
			if i >= len(hRunes) || i >= len(runes) {
				break
			}
			ch := unicode.ToUpper(hRunes[i])
			switch mark {
			case "correct":
				if unicode.ToUpper(runes[i]) != ch {
					return false
				}
			case "absent":
				// absent means the letter doesn't appear in the answer
				// (with the caveat about duplicates, but this is the safe approximation)
				for _, r := range runes {
					if unicode.ToUpper(r) == ch {
						return false
					}
				}
			}
		}
	}
	return true
}

func ordknudeScore(word string) int {
	seen := map[rune]bool{}
	score := 0
	for _, r := range word {
		if seen[r] {
			score -= 2
			continue
		}
		seen[r] = true
		switch r {
		case 'E', 'R', 'A', 'N', 'T', 'S', 'L':
			score += 5
		case 'I', 'D', 'O', 'G', 'K':
			score += 3
		default:
			score++
		}
	}
	if word == "SALAT" {
		score += 20
	}
	return score
}

func validOrdknudeGuess(word string, history []OrdknudeGuess, rejected []string) bool {
	if !IsDanishFiveLetterWord(word) {
		return false
	}
	if usedOrdknudeWord(word, history, rejected) {
		return false
	}
	for _, h := range history {
		if !sameMarks(scoreOrdknudeGuess(word, h.Word), h.Marks) {
			return false
		}
	}
	return true
}

// ConsistentWithOrdknudeHistory reports whether word could still be the secret
// given every prior guess and its observed marks (the standard Wordle filter:
// the candidate, scored as if it were the answer, must reproduce each guess's
// colours). Used to prune a reusable candidate pool after a wrong guess without
// re-querying the LLM. It does NOT check tried/rejected — pair it with
// filterOrdknudeCandidates for that.
func ConsistentWithOrdknudeHistory(word string, history []OrdknudeGuess) bool {
	word = NormalizeDanishLetters(word)
	if !IsDanishFiveLetterWord(word) {
		return false
	}
	for _, h := range history {
		if !sameMarks(scoreOrdknudeGuess(word, NormalizeDanishLetters(h.Word)), h.Marks) {
			return false
		}
	}
	return true
}

// ConsistentWithOrdknudeGreens is the HARD floor: it only requires that every
// confirmed-green (correct) letter sits at its known position. A confirmed green
// is the most reliable signal on the board (a solid-green tile), and any valid
// answer MUST contain it — so a word that contradicts one (e.g. GRUBE when the
// pattern is G R _ D E: the green at position 4 is D, but GRUBE has B there) can
// never be the answer and must never be submitted, even if the fuller
// ConsistentWithOrdknudeHistory check over-prunes due to a mis-read yellow.
func ConsistentWithOrdknudeGreens(word string, history []OrdknudeGuess) bool {
	word = NormalizeDanishLetters(word)
	if !IsDanishFiveLetterWord(word) {
		return false
	}
	runes := []rune(word)
	for _, h := range history {
		hr := []rune(NormalizeDanishLetters(h.Word))
		for i, m := range h.Marks {
			if m == "correct" && i < len(hr) {
				if i >= len(runes) || runes[i] != hr[i] {
					return false
				}
			}
		}
	}
	return true
}

func usedOrdknudeWord(word string, history []OrdknudeGuess, rejected []string) bool {
	for _, h := range history {
		if h.Word == word {
			return true
		}
	}
	for _, r := range rejected {
		if NormalizeDanishLetters(r) == word {
			return true
		}
	}
	return false
}

func scoreOrdknudeGuess(secret, guess string) []string {
	secretRunes := []rune(secret)
	guessRunes := []rune(guess)
	marks := make([]string, len(guessRunes))
	counts := map[rune]int{}
	for i, g := range guessRunes {
		if i < len(secretRunes) && g == secretRunes[i] {
			marks[i] = "correct"
		} else if i < len(secretRunes) {
			counts[secretRunes[i]]++
		}
	}
	for i, g := range guessRunes {
		if marks[i] == "correct" {
			continue
		}
		if counts[g] > 0 {
			marks[i] = "present"
			counts[g]--
		} else {
			marks[i] = "absent"
		}
	}
	return marks
}

func sameMarks(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var ordknudeCandidates = []string{
	"SALEN", "SALAT", "SALÆR", "SALTO", "SALSA", "SALUT", "SALIG", "SALGS",
	"TALER", "LARES", "SILER", "RENAL", "ALERT", "RANKE", "SKRAL",
	"SKARE", "SKOLE", "SKIVE", "SKYDE", "STARE", "STILE", "STORE",
	"STRÅL", "TRANE", "TREVL", "TROLD", "ROTER", "ROTTE", "RÅBER",
	"RÅDNE", "RÅDIG", "RAKET", "RAMPE", "RAMTE", "RANET", "RANDE",
	"RETTE", "RETUR", "REBUS", "REGEL", "REJSE", "RENTE", "RENSE",
	"REMSE", "RIMET", "RISER", "RYSTE", "RYGTE", "RULLE", "RUNDE",
	"ANDEN", "ANDRE", "ANSER", "ANTAL", "ANRET", "ARENA", "ARMEN",
	"ASIER", "ASTER", "AVLER", "AVLSE", "ALENE", "ALMIN", "ALBUM",
	"ELSKR", "ELSKE", "ELVER", "ENDDA", "ENDNU", "ENGEN", "ENKEL",
	"ENORM", "ENTER", "ETAGE", "ETISK", "TANTE", "TASKE", "TAVLE",
	"TEMPO", "TIDEN", "TIGER", "TILDE", "TIMER", "TINGE", "TJEKS",
	"TOAST", "TOMAT", "TOMME", "TOWER", "TRÆER", "TURNE", "TYDEL",
	"DRAGE", "DREJE", "DRENG", "DRIVE", "DRØJE", "DÅSEN", "DELTA",
	"DENNE", "DIGTE", "DINER", "DOLKE", "DOMME", "DONOR", "DUGER",
	"GADEN", "GALDE", "GAMLE", "GANEN", "GARDE", "GAVER", "GEBYR",
	"GIDER", "GIVET", "GLADE", "GLANS", "GLEMT", "GODER", "GRAVE",
	"KABEL", "KALDE", "KAMEL", "KANAL", "KANON", "KAREN", "KASTE",
	"KILER", "KILDE", "KLODE", "KNÆGT", "KODER", "KOGER", "KUGLE",
	"LINJE", "LITER", "LOKAL", "LOMME", "LOTTO", "LUGTE", "LYKKE",
	"MAGTE", "MALER", "MANER", "MANGE", "MARKE", "MELDE", "MENER",
	"METER", "MIDTE", "MORAL", "MÅLER", "MØDER", "MØNTE", "NABER",
	"NATUR", "NAVNE", "NEDRE", "NEMME", "NETTO", "NYDER", "NÆSTE",
	"OPERA", "ORDEN", "ORDET", "OVALE", "PANDE", "PENGE", "PIGEN",
	"PILOT", "PLADE", "PLADS", "PLEJE", "PRIME", "PRØVE", "PUNKT",
	"ÆBLER", "ÆREDE", "ØJNER", "ØSTER", "ÅBENT", "ÅNDER", "ÅRETS",
}

func LoadRejectedWords(dataDir string) []string {
	raw, err := os.ReadFile(filepath.Join(dataDir, "ordknude-rejected.txt"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if w := NormalizeDanishLetters(line); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func RecordRejectedWord(dataDir, word string) error {
	word = NormalizeDanishLetters(word)
	if word == "" {
		return nil
	}
	rejected := LoadRejectedWords(dataDir)
	for _, w := range rejected {
		if w == word {
			return nil
		}
	}
	return os.WriteFile(filepath.Join(dataDir, "ordknude-rejected.txt"), []byte(strings.Join(append(rejected, word), "\n")+"\n"), 0o600)
}

func FindRefByName(snap string, names []string) string {
	for _, m := range snapshotLine.FindAllStringSubmatch(snap, -1) {
		name, ref := strings.TrimSpace(m[2]), m[3]
		for _, want := range names {
			if strings.EqualFold(name, want) {
				return "@" + ref
			}
		}
	}
	return ""
}

// FindRefByChildText finds a ref for an element whose immediate child is a
// StaticText node with the given text. This handles the pattern:
//
//	- generic [ref=eXX] clickable [cursor:pointer]
//	  - StaticText "5"
//
// which the newer agent-browser emits for iframe buttons that don't have
// their own accessible name but contain visible text.
func FindRefByChildText(snap, text string) string {
	refRe := regexp.MustCompile(`\[ref=(e\d+)\]`)
	lines := strings.Split(snap, "\n")
	for i, line := range lines {
		m := refRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Check the immediately next non-empty line for a matching StaticText.
		for j := i + 1; j < len(lines) && j <= i+2; j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" {
				continue
			}
			want := `- StaticText "` + text + `"`
			if strings.HasPrefix(next, want) {
				return "@" + m[1]
			}
			break // only look at first non-empty child line
		}
	}
	return ""
}

// clickByVisibleTextInFrame clicks — inside the game iframe — the most specific
// visible element whose lowercased textContent contains any of names. Returns
// true if it clicked. This is the robust launcher-click path: unlike
// FindRefByName (which matches an element's accessible *name* exactly), it finds
// labels that live in a child text node and tolerates special characters (JS
// toLowerCase folds Ø→ø natively), which is why it succeeds on "SPIL ORDKLØVER".
func clickByVisibleTextInFrame(ctx context.Context, br *browser.Client, names ...string) bool {
	low := make([]string, 0, len(names))
	for _, n := range names {
		if s := strings.TrimSpace(strings.ToLower(n)); s != "" {
			low = append(low, s)
		}
	}
	if len(low) == 0 {
		return false
	}
	namesJSON, err := json.Marshal(low)
	if err != nil {
		return false
	}
	if EnterGameFrame(ctx, br) != nil {
		return false
	}
	defer LeaveFrame(br)
	js := `(function(names){var els=[...document.querySelectorAll("button,div,span,a")].filter(function(e){` +
		`if(!e.offsetParent)return false;var t=(e.textContent||"").toLowerCase();` +
		`return names.some(function(n){return t.indexOf(n)>=0;});});` +
		`if(els.length){els[els.length-1].click();return "clicked";}return "";})(` + string(namesJSON) + `)`
	out, _ := br.Eval(ctx, js)
	return strings.Contains(out, "clicked")
}

// StartOrdKloeverIfLauncher dismisses the Ordkløver "SPIL ORDKLØVER" welcome
// screen if it is showing, so the caller can enter the game before the first
// (expensive) vision read — a vision pass on the launcher is ~15s wasted and the
// re-vision after it doubles that. No-op when the game is already in play.
func StartOrdKloeverIfLauncher(ctx context.Context, br *browser.Client) error {
	return startWordGameIfPresent(ctx, br, "SPIL ORDKLØVER", "SPIL ORDKLOEVER", "Spil Ordkløver")
}

func startWordGameIfPresent(ctx context.Context, br *browser.Client, names ...string) error {
	// Use frames-inclusive snapshot so "SPIL ORDKLØVER" etc. inside the game iframe
	// are visible in the parent tree (as the user snapshot with -F demonstrated).
	snap, err := br.SnapshotInteractiveWithFrames(ctx)
	if err != nil {
		snap, err = br.SnapshotInteractive(ctx)
		if err != nil {
			return err
		}
	}
	if ref := FindRefByName(snap, names); ref != "" {
		if err := br.Click(ctx, ref); err != nil {
			return err
		}
		br.WaitSettled(ctx)
		time.Sleep(1200 * time.Millisecond)
	} else if clickByVisibleTextInFrame(ctx, br, names...) {
		// Ref-by-name missed it — the launcher label is a child text node (and/or
		// carries a special char like Ø that the accessible name doesn't expose).
		// A textContent click inside the game iframe is the robust fallback (this
		// is exactly why ordknude/blok, whose labels match by name/text, worked
		// while ordkløver's "SPIL ORDKLØVER" did not).
		br.WaitSettled(ctx)
		time.Sleep(1200 * time.Millisecond)
	}
	// Best-effort close of the "Sådan spiller du" modal if it auto-opens.
	_, _ = br.Eval(ctx, `(() => {
	  const close = Array.from(document.querySelectorAll('button')).find((b) => {
	    const aria = (b.getAttribute('aria-label') || '').trim();
	    const text = (b.textContent || '').trim();
	    if (/luk|close/i.test(aria)) return true;
	    if (/^(luk|close)$/i.test(text)) return true;
	    return false;
	  });
	  if (close) close.click();
	  return '';
	})()`)
	return nil
}

func firstCapture(s, expr string) string {
	re := regexp.MustCompile(expr)
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func findFiveLetterAnswer(raw string) string {
	// First try: look explicitly for "Det rigtige svar var" followed by the answer.
	lower := strings.ToLower(raw)
	if idx := strings.Index(lower, "det rigtige svar var"); idx >= 0 {
		rest := strings.TrimLeft(raw[idx+len("det rigtige svar var"):], " :\n\r\t\"")
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			word := NormalizeDanishLetters(fields[0])
			if len([]rune(word)) >= 3 {
				return word
			}
		}
	}
	// Fallback: scan for any 5-letter Danish word (upper or lower case).
	re := regexp.MustCompile(`(?i)[A-ZÆØÅ]{5}`)
	matches := re.FindAllString(raw, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		word := NormalizeDanishLetters(matches[i])
		if IsDanishFiveLetterWord(word) {
			return word
		}
	}
	return ""
}
