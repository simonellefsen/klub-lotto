package klublotto

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

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
var snapshotLine = regexp.MustCompile(`(?m)^\s*-\s+(\w+)\s+"([^"]+)"\s+\[ref=(e\d+)\]`)

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
	)

	for _, m := range matches {
		role, name, ref := m[1], m[2], m[3]
		switch role {
		case "heading", "paragraph", "text", "generic":
			// Pick the longest text-ish element with a question mark
			// as our prompt; fall back to longest non-button text.
			if strings.Contains(name, "?") && len(name) > len(round.Prompt) {
				round.Prompt = name
			} else if round.Prompt == "" && len(name) > 20 {
				round.Prompt = name
			}
		case "button", "link", "radio":
			if isControlLabel(name) {
				continue
			}
			seenButtons = append(seenButtons, match{name: name, ref: ref})
		}
	}

	// If we found at least 2 candidate buttons treat them as answers.
	if len(seenButtons) >= 2 && len(seenButtons) <= 8 {
		for _, b := range seenButtons {
			round.Options = append(round.Options, b.name)
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
	low := strings.ToLower(snap)
	return strings.Contains(low, `link "log ind"`) ||
		strings.Contains(low, `button "log ind"`) ||
		strings.Contains(low, `link "opret konto"`)
}

type match struct{ name, ref string }

// isControlLabel filters out buttons we know aren't answers (Submit,
// Next, navigation, language switcher, etc).
func isControlLabel(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	for _, bad := range []string{
		"indsend", "send", "næste", "fortsæt", "spil", "log ud", "log ind",
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
