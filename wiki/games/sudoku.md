---
kind: game
game: sudoku
tags: [klublotto, game, sudoku]
updated: 2026-05-31
---

# Sudoku

Sudoku is embedded on `https://danskespil.dk/klublotto/dagens-sudoku` from
`sudoku.klublotto.danskespil.mgame.nu`.

## Automation Notes

- The parent page exposes only a cross-origin iframe, not the individual cells.
- Opening the iframe URL directly is useful for DOM extraction, but completion
  should happen through the embedded parent page so the `gameCompleted`
  `postMessage` reaches Danske Spil.
- The iframe DOM renders cells as `.cell-r-c`; readonly givens have
  `cell-readonly`.
- The game uses localStorage key `default`, containing a nested
  `sudokuYYYY-MM-DD` entry with `placement`, `seed`, `difficulty`, `time`, and
  `paused`.
- The puzzle is deterministic from date seed and difficulty, but it is simpler
  and safer to extract the visible givens and solve with a local Sudoku solver.
- Coordinate automation must measure the iframe viewport, not a direct
  top-level debug viewport. On 2026-05-31, the embedded iframe was 1248x800;
  direct 1280x720 measurements caused a one-cell horizontal shift.

## 2026-05-31 Observation

Extracted givens, with `0` for blanks:

```text
400006085
683705000
509004031
008360900
700829410
390457802
900108006
000000109
160000340
```

Solved grid:

```text
412936785
683715294
579284631
248361957
756829413
391457862
934178526
825643179
167592348
```

The game showed `Virkelig godt gået!` after completion through the embedded
parent-page iframe. Returning to Spil & Quiz showed a checkmark on Sudoku.

