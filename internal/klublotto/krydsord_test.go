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
		if slot.Length < 1 {
			t.Fatalf("slot %s should not be empty: %+v", slot.ID, slot)
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

func TestBuildKrydsordSlotsIncludesSingleCellAnswers(t *testing.T) {
	// Each '#' here is an isolated answer cell (clue cells on all four sides),
	// i.e. a 1-letter answer like SMALL->S. These must still produce a slot so
	// they get a clue and are filled — previously they were dropped (>=2 only).
	mask := strings.Join([]string{
		".....",
		".#.#.",
		".....",
		".#.#.",
		".....",
	}, "\n")
	data := KrydsordData{
		SolutionSecret: secretFromKrydsordMask(mask),
		CellCountX:     5,
		CellCountY:     5,
	}
	if err := ValidateKrydsordData(data); err != nil {
		t.Fatalf("ValidateKrydsordData: %v", err)
	}
	slots := BuildKrydsordSlots(data)
	if len(slots) != 4 {
		t.Fatalf("expected 4 single-cell slots, got %d: %+v", len(slots), slots)
	}
	for _, s := range slots {
		if s.Length != 1 || len(s.Cells) != 1 {
			t.Fatalf("expected a length-1 slot, got %+v", s)
		}
	}
	found := false
	for _, s := range slots {
		if s.Row == 2 && s.Col == 2 && s.Length == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a length-1 slot at row 2 col 2: %+v", slots)
	}
}

func TestParseKrydsordBatchCandidates(t *testing.T) {
	raw := "```json\n" +
		`{"slots":[` +
		`{"id":"A1","candidates":["ROE","ROER"]},` + // ROER (4) filtered out for len 3
		`{"id":"D14","candidates":["TSHIRT"]},` +
		`{"id":"A2","candidates":["XXXX"]}` + // wrong length for len 2 -> A2 absent
		`]}` + "\n```"
	want := map[string]int{"A1": 3, "D14": 6, "A2": 2}
	got, err := ParseKrydsordBatchCandidates(raw, want)
	if err != nil {
		t.Fatalf("ParseKrydsordBatchCandidates: %v", err)
	}
	if len(got["A1"]) != 1 || got["A1"][0].Answer != "ROE" {
		t.Fatalf("A1 expected [ROE] after length filter, got %+v", got["A1"])
	}
	if len(got["D14"]) != 1 || got["D14"][0].Answer != "TSHIRT" {
		t.Fatalf("D14 expected [TSHIRT], got %+v", got["D14"])
	}
	if _, ok := got["A2"]; ok {
		t.Fatalf("A2 had no correctly-sized candidate and should be absent, got %+v", got["A2"])
	}
}

func TestParseKrydsordAnswerMap(t *testing.T) {
	cases := []string{
		"```json\n{\"answers\":{\"A1\":\"LOMMEKNIV\",\"D1\":\"LUFTBALLON\"}}\n```",
		`{"answers":[{"id":"A1","answer":"LOMMEKNIV"},{"id":"D1","word":"LUFTBALLON"}]}`,
		`Here you go: {"A1":"LOMMEKNIV","D1":"LUFTBALLON"}`,
	}
	for i, raw := range cases {
		got, err := ParseKrydsordAnswerMap(raw)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got["A1"] != "LOMMEKNIV" || got["D1"] != "LUFTBALLON" {
			t.Fatalf("case %d: got %+v", i, got)
		}
	}
	if _, err := ParseKrydsordAnswerMap("not json"); err == nil {
		t.Fatalf("expected error for non-JSON input")
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
