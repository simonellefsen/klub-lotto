//go:build ignore

// NOTE: This file is temporarily excluded from builds because it was written
// against older versions of the llm.Provider and browser.Client APIs
// (JSONGenerator, Snapshot, KeyboardType, findRefByName, OrdknudeURL, etc.).
// The web binary and the new `ledger attach-image` command do not depend on it.
//
// Remove this build tag (or rename the file back to .go) once the Ordknuden
// solver is brought up to date with the current abstractions.

package klublotto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/llm"
)

const ordknudeLen = 5

type OrdknudeTile struct {
	Letter     string `json:"letter"`
	ClassName  string `json:"className"`
	Background string `json:"background"`
	Mark       string `json:"mark"`
}

type OrdknudeGuess struct {
	Word      string   `json:"word"`
	Marks     []string `json:"marks"`
	Rationale string   `json:"rationale,omitempty"`
}

type OrdknudeResult struct {
	History []OrdknudeGuess `json:"history"`
	Solved  bool            `json:"solved"`
	Outcome string          `json:"outcome"`
	Answer  string          `json:"answer,omitempty"`
}

// OpenOrdknude opens the signed Ordknuden iframe. The iframe URL changes
// daily and includes a checksum, so we first load the parent page.
func OpenOrdknude(ctx context.Context, br *browser.Client) error {
	if err := br.Open(ctx, OrdknudeURL); err != nil {
		return err
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(1500 * time.Millisecond)

	src, err := br.Eval(ctx, `(() => {
  const iframe = document.querySelector('iframe[src*="ordknuden"], .kl-game__iframe');
  return iframe?.src || '';
})()`)
	if err != nil {
		return fmt.Errorf("extract Ordknuden iframe: %w", err)
	}
	if strings.TrimSpace(src) == "" {
		return errors.New("could not find Ordknuden iframe on parent page")
	}
	if err := br.Open(ctx, src); err != nil {
		return fmt.Errorf("open Ordknuden iframe: %w", err)
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	time.Sleep(1500 * time.Millisecond)
	return nil
}

func PlayOrdknude(ctx context.Context, br *browser.Client, suggester llm.JSONGenerator, maxGuesses int) (OrdknudeResult, error) {
	if maxGuesses <= 0 || maxGuesses > 6 {
		maxGuesses = 6
	}
	if solved, answer, err := readOrdknudeSolved(ctx, br); err == nil && solved {
		return OrdknudeResult{Solved: true, Outcome: "solved", Answer: answer}, nil
	}
	if err := startOrdknude(ctx, br); err != nil {
		return OrdknudeResult{}, err
	}

	var rejected []string
	for turn := 0; turn < maxGuesses; turn++ {
		if solved, answer, err := readOrdknudeSolved(ctx, br); err == nil && solved {
			history, _ := ReadOrdknudeHistory(ctx, br)
			return OrdknudeResult{History: history, Solved: true, Outcome: "solved", Answer: answer}, nil
		}
		history, err := ReadOrdknudeHistory(ctx, br)
		if err != nil {
			return OrdknudeResult{}, err
		}
		if len(history) > 0 && allCorrect(history[len(history)-1].Marks) {
			return OrdknudeResult{History: history, Solved: true, Outcome: "solved"}, nil
		}
		if len(history) >= maxGuesses {
			return OrdknudeResult{History: history, Solved: false, Outcome: "out of guesses"}, nil
		}

		guess, rationale, err := nextOrdknudeGuess(ctx, suggester, history, rejected)
		if err != nil {
			if fallback := fallbackOrdknudeGuess(history, rejected); fallback != "" {
				guess = fallback
				rationale = "fallback after " + suggester.Name() + " failure: " + err.Error()
			} else {
				return OrdknudeResult{History: history, Outcome: "error: " + err.Error()}, err
			}
		}
		before := len(history)
		if err := submitOrdknudeGuess(ctx, br, guess); err != nil {
			return OrdknudeResult{History: history, Outcome: "error: " + err.Error()}, err
		}
		time.Sleep(1800 * time.Millisecond)
		if solved, answer, err := readOrdknudeSolved(ctx, br); err == nil && solved {
			after, _ := ReadOrdknudeHistory(ctx, br)
			return OrdknudeResult{History: after, Solved: true, Outcome: "solved", Answer: answer}, nil
		}

		after, err := ReadOrdknudeHistory(ctx, br)
		if err != nil {
			return OrdknudeResult{}, err
		}
		if len(after) <= before {
			rejected = append(rejected, guess)
			_ = clearOrdknudeInput(ctx, br)
			if len(rejected) >= 5 {
				return OrdknudeResult{History: history, Outcome: "game rejected too many guesses"}, errors.New("game rejected too many guesses")
			}
			continue
		}
		after[len(after)-1].Rationale = rationale
		if allCorrect(after[len(after)-1].Marks) {
			return OrdknudeResult{History: after, Solved: true, Outcome: "solved"}, nil
		}
	}

	history, err := ReadOrdknudeHistory(ctx, br)
	if err != nil {
		return OrdknudeResult{}, err
	}
	return OrdknudeResult{History: history, Solved: false, Outcome: "stopped after max guesses"}, nil
}

func startOrdknude(ctx context.Context, br *browser.Client) error {
	if rows, err := readOrdknudeRows(ctx, br); err == nil && len(rows) >= 6 {
		return nil
	}
	snap, err := br.SnapshotInteractive(ctx)
	if err != nil {
		return fmt.Errorf("snapshot Ordknuden start: %w", err)
	}
	if ref := findRefByName(snap, []string{"SPIL Ordknuden", "Spil Ordknuden", "SPIL ORDKNUDEN"}); ref != "" {
		if err := br.Click(ctx, ref); err != nil {
			return fmt.Errorf("click Ordknuden start: %w", err)
		}
		time.Sleep(1500 * time.Millisecond)
		_ = br.WaitForLoad(ctx, "networkidle")
	}
	_, _ = br.Eval(ctx, `(() => {
  const close = Array.from(document.querySelectorAll('button')).find((b) => /luk|close/i.test(b.getAttribute('aria-label') || b.textContent || ''));
  if (close) close.click();
  return '';
})()`)
	if rows, err := readOrdknudeRows(ctx, br); err == nil && len(rows) >= 6 {
		return nil
	}
	return errors.New("could not start Ordknuden board")
}

func ReadOrdknudeHistory(ctx context.Context, br *browser.Client) ([]OrdknudeGuess, error) {
	rows, err := readOrdknudeRows(ctx, br)
	if err != nil {
		return nil, err
	}
	var history []OrdknudeGuess
	for _, row := range rows {
		if len(row) != ordknudeLen {
			continue
		}
		var word strings.Builder
		marks := make([]string, 0, ordknudeLen)
		complete := true
		for _, tile := range row {
			letter := normalizeOrdknudeWord(tile.Letter)
			if letter == "" || tile.Mark == "" || tile.Mark == "pending" {
				complete = false
				break
			}
			word.WriteString(letter)
			marks = append(marks, tile.Mark)
		}
		if complete {
			history = append(history, OrdknudeGuess{Word: word.String(), Marks: marks})
		}
	}
	return history, nil
}

func readOrdknudeSolved(ctx context.Context, br *browser.Client) (bool, string, error) {
	snap, err := br.Snapshot(ctx)
	if err != nil {
		return false, "", err
	}
	low := strings.ToLower(snap)
	if !strings.Contains(low, "vundet") && !strings.Contains(low, "du fandt det rigtige svar") {
		return false, "", nil
	}
	lines := strings.Split(snap, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "- text:") {
			answer := normalizeOrdknudeWord(strings.TrimSpace(strings.TrimPrefix(line, "- text:")))
			if isDanishFiveLetterWord(answer) {
				return true, answer, nil
			}
		}
	}
	return true, "", nil
}

func readOrdknudeRows(ctx context.Context, br *browser.Client) ([][]OrdknudeTile, error) {
	const js = `(() => JSON.stringify(Array.from(document.querySelectorAll('._row_mtlsa_42')).map((row) =>
  Array.from(row.querySelectorAll('._tile_mtlsa_52')).map((tile) => ({
    letter: (tile.innerText || tile.textContent || '').trim().toUpperCase(),
    className: String(tile.className || ''),
    background: getComputedStyle(tile).backgroundColor
  }))
)))()`
	out, err := br.Eval(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("read Ordknuden board: %w", err)
	}
	var rows [][]OrdknudeTile
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse Ordknuden board: %w", err)
	}
	for i := range rows {
		for j := range rows[i] {
			rows[i][j].Letter = normalizeOrdknudeWord(rows[i][j].Letter)
			rows[i][j].Mark = classifyOrdknudeTile(rows[i][j])
		}
	}
	return rows, nil
}

func classifyOrdknudeTile(tile OrdknudeTile) string {
	bg := strings.ToLower(strings.TrimSpace(tile.Background))
	if hasOrdknudeStateClass(tile.ClassName, "_m_") || isRGB(bg, func(r, g, b int) bool { return g > 120 && r < 60 && b < 80 }) {
		return "correct"
	}
	if hasOrdknudeStateClass(tile.ClassName, "_o_") || isRGB(bg, func(r, g, b int) bool { return r > 90 && g < 50 && b < 50 }) {
		return "absent"
	}
	if isRGB(bg, func(r, g, b int) bool { return r > 150 && g > 90 && b < 80 }) {
		return "present"
	}
	if tile.Letter == "" || strings.Contains(bg, "255, 255, 255") || strings.Contains(bg, "white") {
		return "pending"
	}
	return "pending"
}

func hasOrdknudeStateClass(className, prefix string) bool {
	for _, cls := range strings.Fields(className) {
		if strings.HasPrefix(cls, prefix) {
			return true
		}
	}
	return false
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

type geminiOrdknudeResponse struct {
	Guess     string `json:"guess"`
	Rationale string `json:"rationale"`
}

func nextOrdknudeGuess(ctx context.Context, suggester llm.JSONGenerator, history []OrdknudeGuess, rejected []string) (string, string, error) {
	prompt := ordknudePrompt(history, rejected)
	raw, err := suggester.GenerateJSON(ctx, prompt, 0)
	if err != nil {
		if guess := fallbackOrdknudeGuess(history, rejected); guess != "" {
			return guess, "fallback after " + suggester.Name() + " error: " + err.Error(), nil
		}
		return "", "", err
	}
	var res geminiOrdknudeResponse
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &res); err != nil {
		if guess := fallbackOrdknudeGuess(history, rejected); guess != "" {
			return guess, "fallback after " + suggester.Name() + " JSON parse error: " + err.Error(), nil
		}
		return "", "", fmt.Errorf("parse %s Ordknuden answer: %w (raw=%s)", suggester.Name(), err, raw)
	}
	guess := normalizeOrdknudeWord(res.Guess)
	if validOrdknudeGuess(guess, history, rejected) {
		return guess, strings.TrimSpace(res.Rationale), nil
	}
	if fallback := fallbackOrdknudeGuess(history, append(rejected, guess)); fallback != "" {
		return fallback, "fallback; " + suggester.Name() + " suggested invalid/inconsistent word " + guess, nil
	}
	return "", "", fmt.Errorf("%s suggested invalid/inconsistent word %q and no fallback candidate matched", suggester.Name(), guess)
}

func ordknudePrompt(history []OrdknudeGuess, rejected []string) string {
	var b strings.Builder
	b.WriteString("Du løser Dansk Spils Ordknuden, et dansk Wordle-spil.\n")
	b.WriteString("Regler: Ordet er på præcis 5 bogstaver. Grøn/correct betyder rigtigt bogstav på rigtig plads. Gul/present betyder bogstavet findes i ordet, men på forkert plads. Rød/absent betyder bogstavet findes ikke i ordet, med normal Wordle-håndtering af dubletter. Dansk alfabet kan bruge Æ, Ø og Å.\n")
	b.WriteString("Vigtigt: Brug kun rigtige danske opslagsord, som med rimelighed findes i en dansk ordbog som ODS/ordnet.dk eller Den Danske Ordbog. Foreslå ikke svenske, norske eller engelske former. Foreslå ikke navne, forkortelser, bøjningsformer med uklart grundord eller sjældne fremmedsprogede former.\n")
	b.WriteString("Spillet gemmer hvert accepteret gæt permanent, så vælg et ord der både er dansk ordbogsord og opfylder al feedback.\n")
	b.WriteString("Svar kun med JSON: {\"guess\":\"SALAT\",\"rationale\":\"kort begrundelse\"}.\n")
	b.WriteString("Krav: guess skal være ét almindeligt dansk ord på 5 bogstaver, store bogstaver, ingen mellemrum, ingen bindestreg, og må ikke være brugt før.\n\n")
	if len(history) == 0 {
		b.WriteString("Ingen gæt er afgivet endnu.\n")
	} else {
		b.WriteString("Tidligere gæt og feedback:\n")
		for _, h := range history {
			fmt.Fprintf(&b, "- %s: %s\n", h.Word, strings.Join(h.Marks, ","))
		}
	}
	if len(rejected) > 0 {
		b.WriteString("Ord som spillet eller validatoren afviste: ")
		b.WriteString(strings.Join(rejected, ", "))
		b.WriteString("\n")
	}
	return b.String()
}

func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end >= start {
		return s[start : end+1]
	}
	return s
}

func validOrdknudeGuess(word string, history []OrdknudeGuess, rejected []string) bool {
	if !isDanishFiveLetterWord(word) {
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

func isDanishFiveLetterWord(word string) bool {
	if utf8.RuneCountInString(word) != ordknudeLen {
		return false
	}
	for _, r := range word {
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r == 'Æ' || r == 'Ø' || r == 'Å' {
			continue
		}
		return false
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
		if normalizeOrdknudeWord(r) == word {
			return true
		}
	}
	return false
}

func fallbackOrdknudeGuess(history []OrdknudeGuess, rejected []string) string {
	candidates := append([]string(nil), ordknudeCandidates...)
	sort.SliceStable(candidates, func(i, j int) bool {
		return ordknudeScore(candidates[i]) > ordknudeScore(candidates[j])
	})
	for _, c := range candidates {
		word := normalizeOrdknudeWord(c)
		if validOrdknudeGuess(word, history, rejected) {
			return word
		}
	}
	return ""
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

func allCorrect(marks []string) bool {
	if len(marks) != ordknudeLen {
		return false
	}
	for _, m := range marks {
		if m != "correct" {
			return false
		}
	}
	return true
}

func submitOrdknudeGuess(ctx context.Context, br *browser.Client, guess string) error {
	if !isDanishFiveLetterWord(guess) {
		return fmt.Errorf("invalid Ordknuden guess %q", guess)
	}
	snap, err := br.SnapshotInteractive(ctx)
	if err == nil {
		for _, r := range guess {
			ref := findRefByName(snap, []string{strings.ToLower(string(r)), strings.ToUpper(string(r))})
			if ref == "" {
				return fmt.Errorf("could not find on-screen key for %q", string(r))
			}
			if err := br.Click(ctx, ref); err != nil {
				return fmt.Errorf("click key %q for %s: %w", string(r), guess, err)
			}
			time.Sleep(120 * time.Millisecond)
		}
		if ref := findRefByName(snap, []string{"Retur", "RETUR", "Enter"}); ref != "" {
			return br.Click(ctx, ref)
		}
	}
	if err := br.KeyboardType(ctx, strings.ToLower(guess)); err != nil {
		return fmt.Errorf("type %s: %w", guess, err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := br.Press(ctx, "Enter"); err != nil {
		return fmt.Errorf("submit %s: %w", guess, err)
	}
	return nil
}

func clearOrdknudeInput(ctx context.Context, br *browser.Client) error {
	for i := 0; i < ordknudeLen; i++ {
		if err := br.Press(ctx, "Backspace"); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func normalizeOrdknudeWord(s string) string {
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
