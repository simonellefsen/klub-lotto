// Package wiki implements the LLM-Wiki pattern (Karpathy) for klub-lotto.
//
// The wiki is a directory of markdown files this program — and, in
// interactive sessions, the user's coding LLM — maintains together.
//
//	wiki/
//	  AGENTS.md          schema: how the wiki is structured
//	  index.md           content-oriented catalog of pages
//	  log.md             chronological append-only run log
//	  games/quiz.md      entity page: what we know about the Quiz game
//	  games/lotto.md     (future)
//	  sources/           one .md per ingested quiz round
//	  concepts/          provider notes, agent-browser tips, etc.
//
// Every Quiz run calls Ingest, which:
//  1. Writes a new sources/quiz-YYYYMMDD-HHMMSS.md page with the prompt,
//     options, every provider's vote, what we submitted, the outcome.
//  2. Appends a line to log.md.
//  3. Updates games/quiz.md statistics (handled here for now; harder
//     analysis is left to the human + their coding LLM via qmd search).
//
// We deliberately keep this self-contained: stdlib only, plain text files.
// You should be able to `git diff` what changed after every run.
package wiki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/simonellefsen/klub-lotto/internal/llm"
)

// Wiki owns a directory of markdown files.
type Wiki struct {
	Root string
}

// New returns a Wiki rooted at root. The directory is created if missing.
func New(root string) (*Wiki, error) {
	if root == "" {
		return nil, errors.New("wiki: empty root")
	}
	for _, sub := range []string{"sources", "games", "concepts"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Wiki{Root: root}, nil
}

// QuizIngest is everything we want recorded about a single quiz round.
type QuizIngest struct {
	Timestamp   time.Time
	Question    string
	Options     []string
	Votes       []llm.Vote
	ChosenIndex int
	Submitted   bool
	Outcome     string // "submitted" | "correct" | "wrong" | "skipped" | "error: ..."
	URL         string // page URL at the time of capture
}

// IngestQuiz writes the source page, updates the log, and bumps the
// quiz entity page. The returned path is the new source page (handy for
// printing it after each run).
func (w *Wiki) IngestQuiz(q QuizIngest) (string, error) {
	if q.Timestamp.IsZero() {
		q.Timestamp = time.Now()
	}
	stamp := q.Timestamp.UTC().Format("20060102-150405")
	name := "quiz-" + stamp + ".md"
	src := filepath.Join(w.Root, "sources", name)

	page := buildQuizSourcePage(q)
	if err := os.WriteFile(src, []byte(page), 0o644); err != nil {
		return "", err
	}

	if err := appendLog(w.Root, q.Timestamp, "ingest",
		fmt.Sprintf("quiz | %s | outcome=%s", trimOne(q.Question, 60), q.Outcome)); err != nil {
		return src, err
	}

	if err := w.updateQuizEntity(q); err != nil {
		return src, err
	}

	if err := w.updateIndex(); err != nil {
		return src, err
	}
	return src, nil
}

// AppendIngestLog appends a log entry in the exact format expected by
// scripts/sync.sh (and shown by `grep '^## \[' wiki/log.md`):
//
//	## [2026-06-04 04:58 UTC] ingest | quiz | Hvilket land har et flag... | outcome=submitted
//
// It is called by the direct game runners in cmd/klub-lotto (quiz, krydsord,
// sudoku, ...) after they write a sources/ file + update the daily ledger.
// This ensures `make quiz` (etc.) produce a fresh, relevant commit subject
// instead of picking up a stale entry from a previous day.
func AppendIngestLog(root string, when time.Time, gameKind, subject, outcome string) error {
	if when.IsZero() {
		when = time.Now()
	}
	body := fmt.Sprintf("%s | %s | outcome=%s", gameKind, trimOne(subject, 60), outcome)
	return appendLog(root, when, "ingest", body)
}

// LogQuery records a query against the wiki. The "operations" section of
// the LLM Wiki spec explicitly suggests filing queries back into the log
// so explorations compound.
func (w *Wiki) LogQuery(question, outcome string) error {
	return appendLog(w.Root, time.Now(), "query",
		fmt.Sprintf("%s | %s", trimOne(question, 60), trimOne(outcome, 80)))
}

// LogLint records a lint pass (e.g. periodic health-check).
func (w *Wiki) LogLint(summary string) error {
	return appendLog(w.Root, time.Now(), "lint", trimOne(summary, 100))
}

func buildQuizSourcePage(q QuizIngest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "kind: quiz-round\n")
	fmt.Fprintf(&b, "date: %s\n", q.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "outcome: %s\n", q.Outcome)
	fmt.Fprintf(&b, "submitted: %t\n", q.Submitted)
	fmt.Fprintf(&b, "tags: [klublotto, quiz, source]\n")
	fmt.Fprintf(&b, "---\n\n")

	fmt.Fprintf(&b, "# Quiz round — %s\n\n", q.Timestamp.UTC().Format("2006-01-02 15:04 MST"))

	fmt.Fprintf(&b, "## Question\n\n> %s\n\n", q.Question)

	fmt.Fprintf(&b, "## Options\n\n")
	for i, o := range q.Options {
		marker := "  "
		if i == q.ChosenIndex {
			marker = "->"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", marker, i, o)
	}
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "## Model votes\n\n")
	if len(q.Votes) == 0 {
		fmt.Fprintf(&b, "_no providers configured_\n\n")
	} else {
		fmt.Fprintf(&b, "| provider | index | option | confidence | latency | rationale |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|\n")
		for _, v := range q.Votes {
			if v.Err != nil {
				fmt.Fprintf(&b, "| %s | error | — | — | %s | %s |\n",
					v.Provider, v.Latency.Round(time.Millisecond),
					mdEscape(v.Err.Error()))
				continue
			}
			optTxt := ""
			if v.Answer.Index >= 0 && v.Answer.Index < len(q.Options) {
				optTxt = q.Options[v.Answer.Index]
			}
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s |\n",
				v.Provider, v.Answer.Index,
				mdEscape(optTxt), v.Answer.Confidence,
				v.Latency.Round(time.Millisecond),
				mdEscape(v.Answer.Rationale))
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "## Submission\n\n")
	fmt.Fprintf(&b, "- chosen index: %d\n", q.ChosenIndex)
	if q.ChosenIndex >= 0 && q.ChosenIndex < len(q.Options) {
		fmt.Fprintf(&b, "- chosen text: %s\n", q.Options[q.ChosenIndex])
	}
	fmt.Fprintf(&b, "- submitted: %t\n", q.Submitted)
	fmt.Fprintf(&b, "- outcome: %s\n", q.Outcome)
	if q.URL != "" {
		fmt.Fprintf(&b, "- page: %s\n", q.URL)
	}
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "## See also\n\n- [Quiz game page](../games/quiz.md)\n")
	return b.String()
}

// updateQuizEntity rewrites wiki/games/quiz.md with cumulative stats. We
// recompute from disk rather than tracking running counters, so the entity
// page is always consistent with the source files. This is what the LLM
// Wiki spec means by "the wiki is the persistent compounding artifact":
// you can blow away games/quiz.md and rebuild it from sources/.
func (w *Wiki) updateQuizEntity(latest QuizIngest) error {
	stats, err := scanQuizSources(filepath.Join(w.Root, "sources"))
	if err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("kind: entity\n")
	b.WriteString("tags: [klublotto, game, quiz]\n")
	fmt.Fprintf(&b, "updated: %s\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString("# Quiz\n\n")
	b.WriteString("Klub Lotto's daily \"tænkespil\" — a single multiple-choice question.\n")
	b.WriteString("Solving it adds tickets ('lodder') to the weekly draw.\n\n")

	b.WriteString("## Stats\n\n")
	fmt.Fprintf(&b, "- rounds recorded: %d\n", stats.total)
	fmt.Fprintf(&b, "- submitted: %d\n", stats.submitted)
	fmt.Fprintf(&b, "- skipped: %d\n", stats.skipped)
	fmt.Fprintf(&b, "- correct: %d\n", stats.correct)
	fmt.Fprintf(&b, "- wrong: %d\n", stats.wrong)
	fmt.Fprintf(&b, "- last round: %s\n", latest.Timestamp.UTC().Format(time.RFC3339))
	fmt.Fprintln(&b)

	b.WriteString("## Known patterns\n\n")
	b.WriteString("_LLM/operator notes go here. Edit by hand or via an ingest session._\n\n")
	b.WriteString("- Questions are typically Danish trivia (history, geography, sport, pop culture).\n")
	b.WriteString("- 4 options is the most common layout, but 2–6 has been observed; the solver\n")
	b.WriteString("  treats any count between 2 and 8 as valid.\n")
	b.WriteString("- The page often auto-advances on tap — there is no separate \"Indsend\" click.\n\n")

	b.WriteString("## Solver behaviour\n\n")
	b.WriteString("`klub-lotto quiz` calls all configured LLM providers in parallel, takes the\n")
	b.WriteString("majority vote, and clicks the corresponding option. Each run is filed under\n")
	b.WriteString("`sources/quiz-YYYYMMDD-HHMMSS.md`.\n\n")

	b.WriteString("## See also\n\n- [Index](../index.md)\n- [Log](../log.md)\n- [Sources directory](../sources/)\n")

	return os.WriteFile(filepath.Join(w.Root, "games", "quiz.md"), []byte(b.String()), 0o644)
}

type quizStats struct {
	total, submitted, skipped, correct, wrong int
}

func scanQuizSources(dir string) (quizStats, error) {
	var s quizStats
	entries, err := os.ReadDir(dir)
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "quiz-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		s.total++
		text := string(body)
		switch {
		case strings.Contains(text, "outcome: correct"):
			s.correct++
			s.submitted++
		case strings.Contains(text, "outcome: wrong"):
			s.wrong++
			s.submitted++
		case strings.Contains(text, "outcome: submitted"):
			s.submitted++
		case strings.Contains(text, "outcome: skipped"):
			s.skipped++
		}
	}
	return s, nil
}

// updateIndex regenerates wiki/index.md from the current filesystem.
// The format is intentionally simple: a category list with one bullet per
// page. The LLM Wiki spec leans on this index for retrieval at small scale.
func (w *Wiki) updateIndex() error {
	var b strings.Builder
	b.WriteString("# klub-lotto wiki — index\n\n")
	b.WriteString("_Auto-regenerated. Hand edits in this file will be overwritten._\n\n")
	b.WriteString("## Schema\n\n- [AGENTS.md](AGENTS.md) — wiki conventions and workflows\n\n")
	b.WriteString("## Games (entities)\n\n")
	if err := listSection(&b, filepath.Join(w.Root, "games"), "games", 80); err != nil {
		return err
	}
	b.WriteString("\n## Concepts\n\n")
	if err := listSection(&b, filepath.Join(w.Root, "concepts"), "concepts", 100); err != nil {
		return err
	}
	b.WriteString("\n## Sources (raw ingests)\n\n")
	if err := listSection(&b, filepath.Join(w.Root, "sources"), "sources", 200); err != nil {
		return err
	}
	b.WriteString("\n## Log\n\n- [log.md](log.md)\n")
	return os.WriteFile(filepath.Join(w.Root, "index.md"), []byte(b.String()), 0o644)
}

func listSection(b *strings.Builder, dir, prefix string, maxEntries int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fmt.Fprintf(b, "- [%s/%s](%s/%s)\n", prefix, e.Name(), prefix, e.Name())
		count++
		if count >= maxEntries {
			fmt.Fprintf(b, "- _… %d more, see `%s/` directly_\n", len(entries)-count, prefix)
			break
		}
	}
	return nil
}

// appendLog adds a chronological entry. Format follows the Karpathy spec
// so `grep '^## \[' log.md | tail -5` shows recent activity.
func appendLog(root string, when time.Time, op, body string) error {
	path := filepath.Join(root, "log.md")
	// Seed the file with a header on first write.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		header := "# klub-lotto wiki — log\n\nAppend-only. One section per ingest/query/lint.\n\n"
		if err := os.WriteFile(path, []byte(header), 0o644); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "## [%s] %s | %s\n\n", when.UTC().Format("2006-01-02 15:04 UTC"), op, body)
	return err
}

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func trimOne(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
