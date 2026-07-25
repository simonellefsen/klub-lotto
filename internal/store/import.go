package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ImportWikiDaily walks wikiDir/daily/ (recursively — files are bucketed as
// daily/YYYY/MM/YYYY-MM-DD.md since 2026-07-25, though flat daily/*.md is still
// accepted for older trees) and upserts each row into the daily_ledger table.
// The markdown table format is the one used in wiki/daily/2026/05/2026-05-31.md:
//
//	| Game | Prompt / clue | Answer | Submitted through parent page | Registered on overview | Notes |
//	|---|---|---|---:|---:|---|
//	| Quiz | ... | ... | yes | yes | ... |
//
// The function is best-effort: rows that don't parse are logged via the
// returned error slice (in addition to any fatal io errors) so the caller
// can decide how strict to be. Re-running is safe — UpsertLedger keys on
// (date, game_slug).
func (s *Store) ImportWikiDaily(ctx context.Context, wikiDir string) ([]string, error) {
	dailyDir := filepath.Join(wikiDir, "daily")

	// Be forgiving: create the expected wiki layout if it doesn't exist yet.
	// This matches what internal/wiki.New does.
	for _, sub := range []string{"daily", "sources", "games", "concepts"} {
		if err := os.MkdirAll(filepath.Join(wikiDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("ensure wiki subdir %s: %w", sub, err)
		}
	}

	// Collect every *.md under daily/ at any depth (daily/YYYY/MM/*.md now, plus
	// any legacy flat daily/*.md).
	var mdFiles []string
	err := filepath.WalkDir(dailyDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			mdFiles = append(mdFiles, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk daily dir: %w", err)
	}
	sort.Strings(mdFiles)

	var warnings []string
	foundAny := false
	for _, path := range mdFiles {
		name := filepath.Base(path)
		foundAny = true
		body, err := os.ReadFile(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		date, err := parseDailyDate(name, string(body))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		// Record the repo-relative path (wiki/daily/YYYY/MM/file.md) as SourcePath.
		src := "wiki/daily/" + name // fallback for a flat/legacy layout
		if rel, relErr := filepath.Rel(wikiDir, path); relErr == nil {
			src = "wiki/" + filepath.ToSlash(rel)
		}
		rows, parseWarnings := parseDailyTable(string(body))
		warnings = append(warnings, prefix(parseWarnings, name)...)
		for _, r := range rows {
			r.Date = date
			r.SourcePath = src
			if _, err := s.UpsertLedger(ctx, r, nil); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s/%s: upsert: %v", name, r.GameSlug, err))
			}
		}
	}

	if !foundAny {
		warnings = append(warnings,
			"no *.md files found in "+dailyDir,
			"The wiki tree on the PVC is empty (this is normal on first deploy).",
			"From your laptop (with the local checkout), copy the wiki content in:",
			"",
			"  POD=$(kubectl -n klub-lotto get pod -l app=klub-lotto -o name)",
			"  kubectl -n klub-lotto cp wiki/. ${POD#pod/}:/var/lib/klub-lotto/wiki/ -c app",
			"",
			"Then re-run inside the pod:",
			"  kubectl -n klub-lotto exec -it deploy/klub-lotto -c app -- \\",
			"    klub-lotto wiki import-db --dsn \"$DATABASE_URL\" --wiki /var/lib/klub-lotto/wiki",
		)
	}

	return warnings, nil
}

// parseDailyDate extracts the date from a YYYY-MM-DD.md filename or, if
// that fails, from the YAML frontmatter `date:` field.
func parseDailyDate(filename, body string) (time.Time, error) {
	stem := strings.TrimSuffix(filename, ".md")
	if t, err := time.Parse("2006-01-02", stem); err == nil {
		return t, nil
	}
	if m := regexp.MustCompile(`(?m)^date:\s*(\d{4}-\d{2}-\d{2})`).FindStringSubmatch(body); len(m) == 2 {
		return time.Parse("2006-01-02", m[1])
	}
	return time.Time{}, fmt.Errorf("no parseable date")
}

// parseDailyTable scans the body for a markdown table whose header matches
// our expected columns and returns one LedgerEntry per data row.
//
// Recognised header (whitespace-flexible):
//
//	| Game | Prompt / clue | Answer | Submitted ... | Registered ... | Notes |
//
// Rows whose first column doesn't match a known game slug are skipped.
func parseDailyTable(body string) ([]LedgerEntry, []string) {
	var (
		out      []LedgerEntry
		warnings []string
		inTable  bool
	)
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !inTable {
			low := strings.ToLower(line)
			if strings.HasPrefix(low, "| game ") && strings.Contains(low, "answer") {
				inTable = true
				// Skip the separator row that follows.
				if i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") {
					i++
				}
				continue
			}
			continue
		}
		if !strings.HasPrefix(line, "|") {
			inTable = false
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) < 6 {
			warnings = append(warnings, fmt.Sprintf("row %d: expected 6 cells, got %d", i+1, len(cells)))
			continue
		}
		slug := gameSlugFromName(cells[0])
		if slug == "" {
			warnings = append(warnings, fmt.Sprintf("row %d: unknown game %q", i+1, cells[0]))
			continue
		}
		out = append(out, LedgerEntry{
			GameSlug:   slug,
			Prompt:     cells[1],
			Answer:     cells[2],
			Submitted:  parseYesNo(cells[3]),
			Registered: parseYesNo(cells[4]),
			Notes:      cells[5],
		})
	}
	return out, warnings
}

// splitMarkdownRow splits "| a | b | c |" into [a, b, c], stripping
// surrounding whitespace from each cell.
func splitMarkdownRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func parseYesNo(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	switch low {
	case "yes", "y", "true", "✓", "x":
		return true
	}
	return false
}

// gameSlugFromName maps the human-readable game names in the daily table
// to the slugs we use in the games table.
func gameSlugFromName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "quiz":
		return "quiz"
	case "ordknuden":
		return "ordknuden"
	case "ordkløver", "ordkloever":
		return "ordkloever"
	case "sudoku":
		return "sudoku"
	case "krydsord":
		return "krydsord"
	case "blok for blok", "blok-for-blok":
		return "blok-for-blok"
	}
	return ""
}

func prefix(ss []string, pfx string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = pfx + ": " + s
	}
	return out
}
