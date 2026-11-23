package klublotto

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/browser"
)

// The quiz renders in the parent page as plain DOM (read 2026-11-23):
//
//	div.kl-quiz
//	  div.kl-quiz__question            the full question text
//	  div.kl-quiz__options
//	    div.kl-quiz__option
//	      input[type=radio][name=quiz-option][value="Fønikerne"]#option-1
//	      label[for=option-1]          "Fønikerne"
//	  button "AFGIV SVAR"              class kl-button--primary-inverted-disabled
//	                                   until an option is checked
//
// The snapshot sees the same quiz through agent-browser, which cuts accessible
// names at 100 characters, so a long question loses its "?" there. The DOM has
// the whole question and the options exactly as the page spells them.
const quizDOMJS = `(() => {
  const q = document.querySelector('.kl-quiz__question');
  const opts = Array.from(document.querySelectorAll('input[name="quiz-option"]')).map(i => {
    const l = i.id ? document.querySelector('label[for="' + CSS.escape(i.id) + '"]') : null;
    return {id: i.id || '', value: i.value || '', label: l ? (l.textContent || '').trim() : '', checked: !!i.checked};
  });
  const b = Array.from(document.querySelectorAll('button')).find(x => /afgiv svar/i.test(x.textContent || ''));
  return JSON.stringify({
    question: q ? (q.textContent || '').replace(/\s+/g, ' ').trim() : '',
    options: opts,
    button: b ? {found: true, disabled: !!b.disabled || /disabled/i.test(String(b.className)) || b.getAttribute('aria-disabled') === 'true'} : {found: false, disabled: false},
  });
})()`

type quizDOMOption struct {
	ID      string `json:"id"`
	Value   string `json:"value"`
	Label   string `json:"label"`
	Checked bool   `json:"checked"`
}

// Text is the option as the page shows it.
func (o quizDOMOption) Text() string {
	if t := strings.TrimSpace(o.Label); t != "" {
		return t
	}
	return strings.TrimSpace(o.Value)
}

type quizDOM struct {
	Question string          `json:"question"`
	Options  []quizDOMOption `json:"options"`
	Button   struct {
		Found    bool `json:"found"`
		Disabled bool `json:"disabled"`
	} `json:"button"`
}

func (d quizDOM) texts() []string {
	out := make([]string, len(d.Options))
	for i, o := range d.Options {
		out[i] = o.Text()
	}
	return out
}

func (d quizDOM) checked() string {
	for _, o := range d.Options {
		if o.Checked {
			return o.Text()
		}
	}
	return ""
}

func readQuizDOM(ctx context.Context, br *browser.Client) (quizDOM, bool) {
	var d quizDOM
	raw, err := br.Eval(ctx, quizDOMJS)
	if err != nil || json.Unmarshal([]byte(raw), &d) != nil {
		return quizDOM{}, false
	}
	return d, len(d.Options) > 0
}

// QuizRoundFromPage reads the question and options straight from the quiz DOM.
// ok is false when the page doesn't have the layout described above.
func QuizRoundFromPage(ctx context.Context, br *browser.Client) (QuizRound, bool) {
	d, ok := readQuizDOM(ctx, br)
	if !ok || d.Question == "" || len(d.Options) < 2 {
		return QuizRound{}, false
	}
	r := QuizRound{Prompt: d.Question, Options: d.texts()}
	for _, o := range d.Options {
		r.OptionRefs = append(r.OptionRefs, "#"+o.ID)
	}
	return r, true
}

// CompleteQuizPrompt returns the page's full question when the snapshot only
// gave a cut-off start of it, and prompt unchanged otherwise.
func CompleteQuizPrompt(ctx context.Context, br *browser.Client, prompt string) string {
	d, ok := readQuizDOM(ctx, br)
	if !ok || d.Question == "" {
		return prompt
	}
	return completeQuizPrompt(prompt, d.Question)
}

func completeQuizPrompt(prompt, full string) string {
	p := strings.Join(strings.Fields(prompt), " ")
	f := strings.Join(strings.Fields(full), " ")
	if p != "" && len(f) > len(p) && strings.HasPrefix(strings.ToLower(f), strings.ToLower(p)) {
		return f
	}
	return prompt
}

// quizOptionIndex finds the page option meant by want: an exact match (case
// and "A) "-style enumerators ignored), else the one option clearly closest. The second covers a screenshot read that misspells an option — on
// 2026-11-23 OCR read Fønikerne as "Fenikerne", the exact lookup found nothing,
// and the run went on to submit without an answer selected. -1 when nothing
// matches or no option is clearly closest.
func quizOptionIndex(options []string, want string) int {
	w := strings.ToLower(strings.TrimSpace(stripOptionPrefix(want)))
	if w == "" {
		return -1
	}
	for i, o := range options {
		if strings.ToLower(strings.TrimSpace(stripOptionPrefix(o))) == w {
			return i
		}
	}
	best, bestD, nextD := -1, 1<<30, 1<<30
	for i, o := range options {
		d := editDistance([]rune(strings.ToLower(strings.TrimSpace(stripOptionPrefix(o)))), []rune(w))
		switch {
		case d < bestD:
			best, bestD, nextD = i, d, bestD
		case d < nextD:
			nextD = d
		}
	}
	// Up to a third of the word may be misread (rn read as m is two edits),
	// but the best option must beat the next one by two edits, so near-twins
	// like 1920'erne/1930'erne are never guessed between.
	limit := len([]rune(w)) / 3
	if limit < 1 {
		limit = 1
	}
	if best < 0 || bestD > limit || nextD-bestD < 2 {
		return -1
	}
	return best
}

func editDistance(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// waitQuizSelected polls until the page shows option text checked and AFGIV
// SVAR enabled.
func waitQuizSelected(ctx context.Context, br *browser.Client, text string, budget time.Duration) (quizDOM, bool) {
	deadline := time.Now().Add(budget)
	for {
		d, ok := readQuizDOM(ctx, br)
		if ok && d.checked() == text && (!d.Button.Found || !d.Button.Disabled) {
			return d, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return d, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// selectQuizOptionDOM selects the page option matching optionText and confirms
// the page took it. It returns the option as the page spells it. handled is
// false when the page doesn't have the quiz DOM, so the caller can fall back to
// the older snapshot/JS route.
func selectQuizOptionDOM(ctx context.Context, br *browser.Client, optionText string) (label string, handled bool, err error) {
	d, ok := readQuizDOM(ctx, br)
	if !ok || len(d.Options) < 2 {
		return "", false, nil
	}
	texts := d.texts()
	idx := quizOptionIndex(texts, optionText)
	if idx < 0 {
		return "", true, fmt.Errorf("answer %q matches none of the page's options %q", optionText, texts)
	}
	label = texts[idx]
	if label != optionText {
		fmt.Printf("       answer %q is the page's option %q\n", optionText, label)
	}

	// 1. A real click on the label's snapshot ref — the route that has worked
	//    since the quiz moved into the parent page.
	snapCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	snap, _ := br.SnapshotInteractiveCursor(snapCtx)
	cancel()
	if ref := findQuizOptionRef(snap, label); ref != "" {
		if br.Click(ctx, ref) == nil {
			if _, ok := waitQuizSelected(ctx, br, label, 2*time.Second); ok {
				return label, true, nil
			}
		}
	}

	// 2. Click the label (or the radio) from the page.
	id, _ := json.Marshal(d.Options[idx].ID)
	_, _ = br.Eval(ctx, `((id) => { const i = document.getElementById(id); if (!i) return 'none'; const l = document.querySelector('label[for="' + CSS.escape(id) + '"]'); (l || i).click(); return 'ok'; })(`+string(id)+`)`)
	if got, ok := waitQuizSelected(ctx, br, label, 2*time.Second); ok {
		return label, true, nil
	} else {
		return label, true, fmt.Errorf("clicked %q but the page did not select it (checked=%q, AFGIV SVAR disabled=%v)", label, got.checked(), got.Button.Disabled)
	}
}
