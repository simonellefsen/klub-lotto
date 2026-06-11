package klublotto

import (
	"strings"
	"testing"
)

func TestKrydsordMaskAndSlots(t *testing.T) {
	mask := strings.Join([]string{
		"..........",
		".#########",
		".###.#####",
		".##.##..##",
		".####.####",
		".#.#####..",
		".######.##",
		".##..#####",
		".#.###.##.",
		".######.##",
		".###.#####",
	}, "\n")
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(mask),
		CellCountX:     10,
		CellCountY:     11,
	}

	if err := ValidateKrydsordData(data); err != nil {
		t.Fatalf("ValidateKrydsordData: %v", err)
	}
	if got := FormatKrydsordMask(data); got != mask {
		t.Fatalf("mask mismatch\ngot:\n%s\nwant:\n%s", got, mask)
	}

	user := FormatKrydsordUserGrid(data)
	if strings.Contains(user, "#") {
		t.Fatalf("user grid should use underscores, not answer-cell markers:\n%s", user)
	}
	if !strings.Contains(user, "_________") {
		t.Fatalf("user grid should expose answer slots as underscores:\n%s", user)
	}

	slots := BuildKrydsordSlots(data)
	for _, slot := range slots {
		if slot.Length < 2 {
			t.Fatalf("slot %s should not be shorter than 2: %+v", slot.ID, slot)
		}
		if len(slot.Cells) != slot.Length {
			t.Fatalf("slot %s cell count mismatch: %+v", slot.ID, slot)
		}
	}
	if !hasKrydsordSlot(slots, "across", 2, 2, 9) {
		t.Fatalf("expected first across run at row 2 col 2 length 9, got %+v", slots)
	}
	if !hasKrydsordSlot(slots, "down", 2, 2, 10) {
		t.Fatalf("expected first down run at row 2 col 2 length 10, got %+v", slots)
	}
}

func TestValidateKrydsordDataRejectsBadDimensions(t *testing.T) {
	err := ValidateKrydsordData(KrydsordData{
		SolutionSecret: "XXX",
		CellCountX:     2,
		CellCountY:     2,
	})
	if err == nil {
		t.Fatal("expected invalid cell count to be rejected")
	}
}

func TestValidateKrydsordAnswerGridRejectsMaskViolations(t *testing.T) {
	mask := strings.Join([]string{
		"..........",
		".#########",
		".##.###.##",
		".####..##.",
		".###.#####",
		".#.####..#",
		".#####.###",
		".###.#####",
		".####.##.#",
		".##.######",
		".#####.###",
	}, "\n")
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(mask),
		CellCountX:     10,
		CellCountY:     11,
	}
	bad := []string{
		"..........",
		".GADEFESTE",
		".A.SAV.ØL.",
		".R.T..TE.L",
		".N.Y.HEJSA",
		".E.K.K.E.L",
		".S.K.G.S.E",
		".T.E.A.T.R",
		".Y.S.B.E.E",
		".R.Y.T.E.T",
		".E.R.T.E.R",
	}
	check := ValidateKrydsordAnswerGrid(data, bad)
	if check.OK {
		t.Fatal("expected mask violations to be rejected")
	}
	if len(check.Errors) == 0 {
		t.Fatalf("expected detailed validation errors: %+v", check)
	}
}

func TestValidateKrydsordPartialGridAllowsUnknownAnswerCells(t *testing.T) {
	mask := strings.Join([]string{
		"..........",
		".#########",
		".##.###.##",
		".####..##.",
		".###.#####",
		".#.####..#",
		".#####.###",
		".###.#####",
		".####.##.#",
		".##.######",
		".#####.###",
	}, "\n")
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(mask),
		CellCountX:     10,
		CellCountY:     11,
	}
	partial := []string{
		"..........",
		".GADE_____",
		".RO.SAV.__",
		".____..__.",
		".___.HEJSA",
		"._.____.._",
		"._____.___",
		".___.LEGAL",
		".____.__._",
		".__.______",
		"._____.___",
	}
	if check := ValidateKrydsordPartialGrid(data, partial); !check.OK {
		t.Fatalf("expected partial grid to validate: %+v", check)
	}
	if check := ValidateKrydsordAnswerGrid(data, partial); check.OK {
		t.Fatalf("expected strict answer grid to reject unknown cells: %+v", check)
	}
}

func TestValidateKrydsordAnswerGridAcceptsValidGrid(t *testing.T) {
	solved := []string{
		"..........",
		".ØSTERSØEN",
		".RÆV.AORTA",
		".KR.TT..OG",
		".ETUI.RISE",
		".N.NEVET..",
		".RIGTIG.TR",
		".OS..SNØRE",
		".T.LOK.LE.",
		".TEAMET.ES",
		".ELM.ROERE",
	}
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(strings.Join([]string{
			"..........",
			".#########",
			".###.#####",
			".##.##..##",
			".####.####",
			".#.#####..",
			".######.##",
			".##..#####",
			".#.###.##.",
			".######.##",
			".###.#####",
		}, "\n")),
		CellCountX: 10,
		CellCountY: 11,
	}
	check := ValidateKrydsordAnswerGrid(data, solved)
	if !check.OK {
		t.Fatalf("expected solved grid to validate: %+v", check)
	}
}

func secretFromKrydsordMask(mask string) string {
	var b strings.Builder
	for _, ch := range mask {
		switch ch {
		case '.':
			b.WriteByte(' ')
		case '#':
			b.WriteByte('X')
		}
	}
	return b.String()
}

func hasKrydsordSlot(slots []KrydsordSlot, direction string, row, col, length int) bool {
	for _, slot := range slots {
		if slot.Direction == direction && slot.Row == row && slot.Col == col && slot.Length == length {
			return true
		}
	}
	return false
}
