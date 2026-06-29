package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrKrydsordNotSolvedWrap(t *testing.T) {
	// The genuine rejection overlay wraps the sentinel → caller records a loss.
	rejected := fmt.Errorf("%w: %s", errKrydsordNotSolved, "ikke løst korrekt (prøv igen)")
	if !errors.Is(rejected, errKrydsordNotSolved) {
		t.Fatal("rejection error should match errKrydsordNotSolved via errors.Is")
	}
	// A timeout / technical "not confirmed" error must NOT be treated as a loss —
	// it should still surface as a hard error so real breakage is visible.
	technical := fmt.Errorf("Krydsord not confirmed solved: %s", "no success banner (board likely not filled or solution wrong)")
	if errors.Is(technical, errKrydsordNotSolved) {
		t.Fatal("a generic not-confirmed error must NOT match errKrydsordNotSolved")
	}
}

func TestKrydsordAnswerBoard(t *testing.T) {
	grid := []string{
		"......", // all-blocked top border row → cropped away
		".AUTOS", // leading "." is the left border column → cropped
		".DR.EN", // interior "." (blocked clue cell) is kept
		".ABCDE",
	}
	got := krydsordAnswerBoard(grid)
	t.Logf("rendered:\n%s", strings.ReplaceAll(got, "<br>", "\n"))

	// An ASCII pipe would split the Markdown table cell — the board must use the
	// box-drawing separator instead.
	if strings.Contains(got, "|") {
		t.Fatalf("board contains ASCII '|' (breaks table cells): %q", got)
	}

	lines := strings.Split(got, "<br>")
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "`") || !strings.HasSuffix(ln, "`") {
			t.Fatalf("line not backtick-wrapped: %q", ln)
		}
	}

	want := []string{
		"`* │ 12345`", // header: left border column cropped → 5 wide
		"`---------`", // separator: 4 prefix + 5 columns
		"`A │ AUTOS`", // top border row cropped → first data row is A
		"`B │ DR.EN`", // interior blocked cell kept as "."
		"`C │ ABCDE`",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}
