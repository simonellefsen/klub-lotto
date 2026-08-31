package main

import (
	"strings"
	"testing"
)

// TestOrdKloeverMaxProbe pins the endgame probe budget: one attempt is always
// reserved for submitting the answer, and a single round never buys more than 2
// letters. This is the rule the salvage path leans on when the decision LLM
// times out, so it must not drift.
func TestOrdKloeverMaxProbe(t *testing.T) {
	cases := []struct {
		attempts int // attempts already used, out of 12
		want     int
		why      string
	}{
		{0, 2, "early game: probe 2 at a time"},
		{6, 2, "6/12 — still 2"},
		{8, 2, "8/12: 4 left → 2 probes + a guess (the 2026-08-31 case)"},
		{9, 2, "9/12: 3 left → 2 probes, then the last attempt is the answer"},
		{10, 1, "10/12: 2 left → exactly 1 letter, then guess"},
		{11, 0, "11/12: the final attempt MUST be the answer"},
		{12, 0, "out of attempts — never negative"},
	}
	for _, c := range cases {
		if got := ordKloeverMaxProbe(12 - c.attempts); got != c.want {
			t.Errorf("ordKloeverMaxProbe(remaining=%d) = %d, want %d (%s)",
				12-c.attempts, got, c.want, c.why)
		}
	}
}

// TestOrdKloeverEndgamePromptsMatchBudget guards the other half of the rule: the
// prompt text the model sees must agree with the budget the loop enforces. A
// prompt inviting a 2-letter probe on the last attempt would waste the answer.
func TestOrdKloeverEndgamePromptsMatchBudget(t *testing.T) {
	// Last attempt: probing must be forbidden outright, in both prompt halves.
	instr := endgameInstruction(1)
	if !strings.Contains(instr, "SIDSTE FORSØG") {
		t.Errorf("endgameInstruction(1) should demand a final guess, got %q", instr)
	}
	block := endgameActionBlock(1)
	if strings.Contains(block, `"action":"probe"`) {
		t.Errorf("endgameActionBlock(1) must not offer a probe action, got %q", block)
	}

	// Two left: exactly one letter may be probed.
	block = endgameActionBlock(2)
	if !strings.Contains(block, `"letters":["X"]`) {
		t.Errorf("endgameActionBlock(2) should offer a 1-letter probe, got %q", block)
	}

	// Plenty left: up to two letters.
	block = endgameActionBlock(4)
	if !strings.Contains(block, `"letters":["X","Y"]`) {
		t.Errorf("endgameActionBlock(4) should offer a 2-letter probe, got %q", block)
	}
}
