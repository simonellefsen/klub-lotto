package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExportWikiDaily regenerates wiki/daily/YYYY-MM-DD.md from the DB for
// every day in the (from, to) range. Files are overwritten in place; the
// header preserves the human-friendly format we already use in
// wiki/daily/2026-05-31.md so the markdown diff stays meaningful.
//
// Called after each game run by the web server (and by `klub-lotto wiki
// export` from the CLI) so the wiki stays current.
func (s *Store) ExportWikiDaily(ctx context.Context, wikiDir string, from, to time.Time) ([]string, error) {
	entries, err := s.ListLedger(ctx, from, to)
	if err != nil {
		return nil, err
	}
	// Group by date.
	byDate := map[string][]LedgerEntry{}
	for _, e := range entries {
		k := e.Date.Format("2006-01-02")
		byDate[k] = append(byDate[k], e)
	}
	if err := os.MkdirAll(filepath.Join(wikiDir, "daily"), 0o755); err != nil {
		return nil, err
	}
	var written []string
	for date, es := range byDate {
		path := filepath.Join(wikiDir, "daily", date+".md")
		sort.Slice(es, func(i, j int) bool { return es[i].GameSlug < es[j].GameSlug })
		if err := os.WriteFile(path, []byte(renderDaily(date, es)), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	sort.Strings(written)
	return written, nil
}

// renderDaily produces the markdown body for one day's ledger.
func renderDaily(date string, es []LedgerEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nkind: daily-ledger\ndate: %s\ntags: [klublotto, daily-ledger, answers]\nupdated: %s\n---\n\n",
		date, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "# Klub Lotto Daily Ledger — %s\n\n", date)
	fmt.Fprintln(&b, "_This file is exported from Postgres on every game run. Do not edit by hand — your changes will be overwritten on the next run. Edit the DB or the entity pages instead._")

	fmt.Fprintln(&b, "## Answers")
	fmt.Fprintln(&b, "| Game | Prompt / clue | Answer | Submitted through parent page | Registered on overview | Notes |")
	fmt.Fprintln(&b, "|---|---|---|---:|---:|---|")
	for _, e := range es {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			mdEscape(e.GameName),
			mdEscape(e.Prompt),
			mdEscape(e.Answer),
			yesNo(e.Submitted),
			yesNo(e.Registered),
			mdEscape(e.Notes))
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Related Pages")
	for _, e := range es {
		fmt.Fprintf(&b, "- [%s](../games/%s.md)\n", e.GameName, e.GameSlug)
	}
	return b.String()
}

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
