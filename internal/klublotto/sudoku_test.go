package klublotto

import "testing"

func TestSolveSudokuMay312026(t *testing.T) {
	givens, err := ParseSudokuGrid(`
400006085
683705000
509004031
008360900
700829410
390457802
900108006
000000109
160000340
`)
	if err != nil {
		t.Fatal(err)
	}
	solved, ok := SolveSudoku(givens)
	if !ok {
		t.Fatal("SolveSudoku() returned no solution")
	}
	want := `412936785
683715294
579284631
248361957
756829413
391457862
934178526
825643179
167592348`
	if got := FormatSudokuGrid(solved); got != want {
		t.Fatalf("solution:\n%s\nwant:\n%s", got, want)
	}
}

func TestParseSudokuGridRequires81Cells(t *testing.T) {
	if _, err := ParseSudokuGrid("123"); err == nil {
		t.Fatal("ParseSudokuGrid() error = nil, want failure")
	}
}
