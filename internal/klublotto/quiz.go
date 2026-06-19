package klublotto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
	"github.com/simonellefsen/klub-lotto/internal/llm"
)

var ErrLoginRequired = errors.New("login required")

// QuizRound is one extracted question, ready to send to an LLM.
type QuizRound struct {
	Prompt  string   // the actual quiz question
	Options []string // the answer buttons in display order
	// OptionRefs are the @eN refs we use to click the chosen option.
	OptionRefs []string
	// Raw is the full snapshot text we extracted the question from —
	// useful for the wiki log and for debugging when extraction misfires.
	Raw string
}

// QuizResult records what happened end-to-end.
type QuizResult struct {
	Round       QuizRound
	Votes       []llm.Vote
	ChosenIndex int    // -1 if we couldn't pick
	Submitted   bool   // true if we actually clicked the option
	Outcome     string // "submitted" | "correct" | "wrong" | "skipped" | "error: <msg>"
}

// OpenQuiz navigates to the quiz page. Falls back to discovering the URL
// from the "Vælg spil" menu if the direct path no longer exists.
func OpenQuiz(ctx context.Context, br *browser.Client) error {
	if err := br.Open(ctx, QuizURL); err != nil {
		return err
	}
	_ = br.WaitForLoad(ctx, "networkidle")
	// If we landed on a 404 or the marketing page, fall back to menu nav.
	cur, _ := br.URL(ctx)
	if !strings.Contains(cur, "quiz") {
		// Click the "Vælg spil" mega menu and then a "Quiz" link.
		_ = tryClickFirst(ctx, br,
			"text=Vælg spil",
			"button:has-text('Vælg spil')",
		)
		if err := tryClickFirst(ctx, br,
			"text=Quiz",
			"a:has-text('Quiz')",
		); err != nil {
			return fmt.Errorf("could not find Quiz entry: %w", err)
		}
		_ = br.WaitForLoad(ctx, "networkidle")
	}
	return nil
}

// snapshotLine matches one row of agent-browser's `snapshot -i` output:
//
//   - button "Submit" [ref=e2]
//   - textbox "Email" [ref=e3]
var snapshotLine = regexp.MustCompile(`(?m)^\s*-\s+(\w+)\s+"([^"]+)"\s+\[[^\]]*\bref=(e\d+)[^\]]*\]`)

// ExtractRound parses an interactive snapshot and pulls out what looks like
// the current quiz question plus its answer buttons.
//
// The heuristic: the question is the longest heading/text element on the
// page; the answers are the consecutive button/link rows under it whose
// names are short and not "Indsend"/"Næste". This is fragile — once we see
// a real production snapshot we'll harden it.
func ExtractRound(snap string) (QuizRound, error) {
	if IsLoginRequired("", snap) {
		return QuizRound{Raw: snap}, ErrLoginRequired
	}

	matches := snapshotLine.FindAllStringSubmatch(snap, -1)
	if len(matches) == 0 {
		return QuizRound{}, errors.New("snapshot had no parseable elements")
	}

	var (
		round       QuizRound
		seenButtons []match
		inQuizRadio bool
		quizRadio   bool
	)

	for _, m := range matches {
		role, name, ref := strings.ToLower(m[1]), strings.TrimSpace(m[2]), m[3]
		switch role {
		case "heading", "paragraph", "text", "generic":
			// Pick the longest text-ish element with a question mark
			// as our prompt; fall back to longest non-button text.
			if strings.Contains(name, "?") && len(name) > len(round.Prompt) {
				round.Prompt = name
			} else if round.Prompt == "" && len(name) > 20 {
				round.Prompt = name
			}
		case "radio":
			if q := strings.Index(name, "?"); q >= 0 {
				round.Prompt = name[:q+1]
				seenButtons = nil
				inQuizRadio = true
				quizRadio = true
			}
		case "labeltext":
			if !inQuizRadio || isControlLabel(name) {
				continue
			}
			seenButtons = append(seenButtons, match{name: name, ref: ref})
		case "button", "link", "clickable":
			if quizRadio {
				if inQuizRadio {
					inQuizRadio = false
				}
				continue
			}
			if isControlLabel(name) {
				continue
			}
			seenButtons = append(seenButtons, match{name: name, ref: ref})
		}
	}

	// If we found at least 2 candidate buttons treat them as answers.
	// Strip any "A) " / "0. " etc. so stored Options are bare names (e.g. "Japan"),
	// matching what cursor lines expose and what we want in logs/provider prompts.
	if len(seenButtons) >= 2 && len(seenButtons) <= 8 {
		for _, b := range seenButtons {
			round.Options = append(round.Options, stripOptionPrefix(b.name))
			round.OptionRefs = append(round.OptionRefs, "@"+b.ref)
		}
	}

	round.Raw = snap
	if round.Prompt == "" {
		return round, errors.New("could not identify quiz prompt in snapshot")
	}
	if len(round.Options) < 2 {
		return round, errors.New("could not identify answer options in snapshot")
	}
	return round, nil
}

func IsLoginRequired(pageURL, snap string) bool {
	u := strings.ToLower(pageURL)
	if strings.Contains(u, "/log-ind") || strings.Contains(u, "source=klublottorestriction") {
		return true
	}
	if looksLoggedIn(pageURL, snap, "") {
		return false
	}
	low := strings.ToLower(snap)
	return strings.Contains(low, `link "log ind"`) ||
		strings.Contains(low, `button "log ind"`) ||
		strings.Contains(low, `link "opret konto"`)
}

// ExtractRoundFromScreenshot takes a screenshot of the current quiz page and
// uses Claude vision to read the question and answer options. This is more
// robust than DOM/iframe parsing because it works regardless of rendering
// technique (iframes, shadow DOM, canvas, etc.).
//
// The screenshot is saved to dataDir/quiz-ocr.png for debugging.
// OptionRefs are returned as "iframe >> text=<option>" selectors since the
// quiz widget is known to be embedded inside an iframe on the parent page.
func ExtractRoundFromScreenshot(ctx context.Context, br *browser.Client, dataDir string, ac *llm.Anthropic) (QuizRound, error) {
	shotPath := filepath.Join(dataDir, "quiz-ocr-"+time.Now().UTC().Format("20060102-150405")+".png")
	if err := br.Screenshot(ctx, shotPath); err != nil {
		return QuizRound{}, fmt.Errorf("screenshot for vision: %w", err)
	}
	imgBytes, err := os.ReadFile(shotPath)
	if err != nil {
		return QuizRound{}, fmt.Errorf("read screenshot %s: %w", shotPath, err)
	}

	const prompt = `This is a screenshot of a Danish quiz page called "Dagens Quiz" (Klub Lotto).
Find the quiz question — it is prominently displayed in large text and ends with a question mark.
Find the answer options labeled A, B, and C (or 1/2/3 etc).
Return ONLY valid JSON in this exact format, with no other text:
{"question": "the full question text including the ?", "options": ["bare answer one text", "bare answer two text", "bare answer three text"]}
The options array must contain ONLY the bare answer text for each choice. NEVER include the letter prefix (A, B, C), number, "A) ", "0. " or similar in the option strings — just the answer content itself.`

	raw, err := ac.ExtractFromImage(ctx, imgBytes, "image/png", prompt)
	if err != nil {
		return QuizRound{}, fmt.Errorf("vision extract: %w", err)
	}

	// Strip markdown code fences the model may add despite instructions.
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var data struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return QuizRound{}, fmt.Errorf("parse vision JSON: %w (raw=%s)", err, text)
	}
	if data.Question == "" {
		return QuizRound{}, errors.New("vision: could not identify quiz prompt in screenshot")
	}
	if len(data.Options) < 2 {
		return QuizRound{}, fmt.Errorf("vision: found question but only %d option(s)", len(data.Options))
	}

	// Strip prefixes from vision-returned options so we always store bare names
	// (defensive against the prompt example and model variations). This makes
	// printed options, chosenText for ledger, and provider prompts clean.
	for i := range data.Options {
		data.Options[i] = stripOptionPrefix(data.Options[i])
	}

	// The quiz widget is inside an iframe; use Playwright iframe selectors for
	// clicking. Falls back gracefully if the quiz moves to the main page.
	optionRefs := make([]string, len(data.Options))
	for i, opt := range data.Options {
		optionRefs[i] = "iframe >> text=" + opt
	}
	return QuizRound{
		Prompt:     data.Question,
		Options:    data.Options,
		OptionRefs: optionRefs,
		Raw:        text,
	}, nil
}

type match struct{ name, ref string }

// isControlLabel filters out buttons we know aren't answers (Submit,
// Next, navigation, language switcher, etc).
func isControlLabel(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	for _, bad := range []string{
		"afgiv svar", "indsend", "send", "næste", "fortsæt", "spil", "log ud", "log ind",
		"min konto", "menu", "luk", "tilbage", "submit", "next", "play",
	} {
		if low == bad {
			return true
		}
	}
	// Buttons longer than 80 chars are almost certainly not answer chips.
	if len(s) > 80 {
		return true
	}
	return false
}

// Submit clicks the option ref. It does NOT also click "Indsend" — many
// quiz UIs auto-advance on answer; callers can chase a Submit afterwards
// if the snapshot still shows one.
func Submit(ctx context.Context, br *browser.Client, optionRef string) error {
	return br.Click(ctx, optionRef)
}

// SubmitQuizOption selects the chosen answer using the cursor-interactive
// elements exposed by `agent-browser snapshot -i -C` (the "clickable "Japan"
// [ref=e90] [cursor:pointer]" targets), then clicks "Afgiv svar".
//
// We stay on the current page (parent with embedded quiz). The -i -C snapshot
// already surfaces the direct clickable refs for the options and the submit
// button, exactly as demonstrated by manual `snapshot -i -C` + `click @e90` +
// click button. This matches the reliable click pattern used for other games.
//
// A small JS fallback is kept for robustness.

func stripOptionPrefix(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return s
	}
	// Work with runes to safely handle unicode dashes (– —) and multi-byte chars (ÆØÅ etc).
	// Skips common quiz enumerators from labels or model output: "A) ", "A. ", "A ", "0. ",
	// "3) ", "B: ", "10. " etc. Stops before name text even if name starts with A-D (e.g. "Atlanterhavet").
	runes := []rune(s)
	if len(runes) < 2 {
		return s
	}
	first := runes[0]
	labelIsDigit := first >= '0' && first <= '9'
	if !(labelIsDigit || (first >= 'A' && first <= 'D') || (first >= 'a' && first <= 'd')) {
		return s
	}
	// Consume the label: a run of digits ("10") or a single A–D letter.
	i := 1
	if labelIsDigit {
		for i < len(runes) && runes[i] >= '0' && runes[i] <= '9' {
			i++
		}
	}
	// A real enumerator is FOLLOWED by a separator. Consume one or more. If none
	// follow, this wasn't a label — leave the text intact. This keeps decade
	// options like "1900'erne" (digits then an apostrophe, no separator) and
	// names like "Atlanterhavet" whole, while still stripping "3) 1920'erne".
	sepStart := i
	for i < len(runes) {
		c := runes[i]
		if c == ' ' || c == ')' || c == '.' || c == ':' || c == '-' || c == '–' || c == '—' {
			i++
			continue
		}
		break
	}
	if i > sepStart && i < len(runes) {
		return strings.TrimSpace(string(runes[i:]))
	}
	return s
}
func SubmitQuizOption(ctx context.Context, br *browser.Client, optionText string) error {
	// Do not navigate to the raw iframe src. The parent page snapshot (with -i -C)
	// already exposes the direct cursor-interactive click targets for the options
	// (the "clickable "Japan" [ref=e90] [cursor:pointer]" elements).
	// Clicking those refs from the parent context works (as the user demonstrated
	// manually), selects the answer, and makes "AFGIV SVAR" clickable.
	//
	// We stay on the current page (the one with the embedded quiz), do a cursor
	// snapshot, find the exact ref for the option text, click it, then click the
	// submit button via the same mechanism.

	// Select the option using cursor snapshot + ref (the reliable way).
	// Normalizes both the snap name and the optionText so "Japan" matches
	// "A Japan", "A) Japan", etc. (the cursor:clickable lines from -i -C).
	snap, _ := br.SnapshotInteractiveCursor(ctx)
	if ref := findQuizOptionRef(snap, optionText); ref != "" {
		if err := br.Click(ctx, ref); err != nil {
			// fall through to fallback
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		// Fallback: try normal snapshot with flexible ref find.
		snap2, _ := br.SnapshotInteractive(ctx)
		if ref2 := findQuizOptionRef(snap2, optionText); ref2 != "" {
			if err := br.Click(ctx, ref2); err == nil {
				time.Sleep(500 * time.Millisecond)
			} else {
				// ignore, fall to JS
			}
		} else {
			// Last resort JS targeting label/card (from the DOM insight).
			optJSON, _ := json.Marshal(optionText)
			clickJS := `((opt) => {
  function norm(s) {
    s = (s || '').trim();
    s = s.replace(/^[0-9A-Da-d][\s).:–—-]*\s*/, '');
    return s.toLowerCase();
  }
  var want = norm(opt);

  var labels = document.querySelectorAll('label');
  for (var i = 0; i < labels.length; i++) {
    var lab = labels[i];
    var t = (lab.textContent || lab.innerText || '').trim();
    if (norm(t) === want) {
      lab.click();
      return JSON.stringify({ok: true, via: 'label'});
    }
  }

  var cards = document.querySelectorAll('.kl-quiz__option, [class*="quiz__option"], [class*="quiz-option"], [data-letter]');
  for (var i = 0; i < cards.length; i++) {
    var card = cards[i];
    var t = (card.textContent || card.innerText || '').trim();
    if (norm(t).indexOf(want) >= 0 || want.indexOf(norm(t)) >= 0) {
      card.click();
      return JSON.stringify({ok: true, via: 'card'});
    }
  }

  var all = Array.from(document.querySelectorAll('*'));
  var best = null;
  var bestArea = 0;
  for (var i = 0; i < all.length; i++) {
    var el = all[i];
    var t = (el.innerText || el.textContent || '').trim();
    var r = el.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) {
      if (norm(t) === want || norm(t).indexOf(want) >= 0 || want.indexOf(norm(t)) >= 0) {
        var area = r.width * r.height;
        if (area > bestArea) {
          bestArea = area;
          best = el;
        }
      }
    }
  }
  if (best) {
    best.click();
    return JSON.stringify({ok: true, via: 'largest'});
  }
  return JSON.stringify({ok: false});
})(` + string(optJSON) + `)`

			raw, err := br.Eval(ctx, clickJS)
			if err != nil {
				return fmt.Errorf("JS click option %q: %w", optionText, err)
			}
			var result struct{ OK bool `json:"ok"` }
			json.Unmarshal([]byte(raw), &result)
			if !result.OK {
				return fmt.Errorf("option %q not found via any method", optionText)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// Now click "Afgiv svar". Use cursor snapshot + ref for reliability.
	if err := clickAfgivSvar(ctx, br); err != nil {
		return fmt.Errorf("click Afgiv svar: %w", err)
	}

	return nil
}

// findQuizOptionRef normalizes names from both the optionText and the snapshot
// line so that bare "Japan" matches "A Japan", "A) Japan", "A. Japan" etc.
// from the cursor-interactive "clickable" lines.
func findQuizOptionRef(snap, optionText string) string {
	if optionText == "" {
		return ""
	}
	want := stripOptionPrefix(optionText)
	for _, m := range snapshotLine.FindAllStringSubmatch(snap, -1) {
		name := stripOptionPrefix(strings.TrimSpace(m[2]))
		ref := m[3]
		if strings.EqualFold(name, want) || strings.EqualFold(name, optionText) {
			return "@" + ref
		}
	}
	return ""
}

// clickAfgivSvar tries to click the submit button using the reliable snapshot/ref
// method (same as other games). Retries a few times because the button may appear
// or change state after option selection.
func clickAfgivSvar(ctx context.Context, br *browser.Client) error {
	for attempt := 0; attempt < 4; attempt++ {
		snap, err := br.SnapshotInteractiveCursor(ctx)
		if err == nil {
			if ref := FindRefByName(snap, []string{"Afgiv svar", "AFGIV SVAR", "Afgiv svar"}); ref != "" {
				if cerr := br.Click(ctx, ref); cerr == nil {
					return nil
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	// Last resort: the old CSS/text selectors.
	return tryClickFirst(ctx, br,
		`button:has-text("Afgiv svar")`,
		`button:has-text("AFGIV SVAR")`,
		`text=Afgiv svar`,
	)
}
