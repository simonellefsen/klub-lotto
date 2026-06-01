---
kind: game
game: krydsord
tags: [klublotto, game, krydsord, crossword, clues-in-squares]
updated: 2026-05-31
---

# Krydsord

Krydsord is a daily Danish clues-in-squares crossword embedded from
`https://iframes.krydsord.dk/` on
`https://danskespil.dk/klublotto/dagens-krydsord`.

## Rules And Layout

- The crossword is a clue-square grid, not a Guardian-style numbered
  across/down puzzle.
- Top-row single clues normally answer vertically downward.
- Left-edge single clues normally answer horizontally to the right.
- In a split clue square, the top clue answers horizontally to the right.
- In a split clue square, the bottom clue answers vertically downward.
- Klub Lotto has not shown arrows in this puzzle so far; if arrows appear,
  they must override the default direction rule.

## Automation Notes

- The iframe API action `cmd=get_data_and_image` returns:
  - `solution_secret`: a row-major mask where spaces are clue/non-answer cells
    and non-spaces are answer cells.
  - `solution_user`: any saved row-major answer state.
  - `cell_count_x`, `cell_count_y`, offsets, and the board image.
- Build slots deterministically from contiguous answer-cell runs. Do not let
  an LLM decide slot geometry from the image alone; Gemini incorrectly allowed
  across answers to jump over clue cells.
- Gemini is useful for clue OCR and candidate generation, but full crossword
  solving in one prompt timed out. A staged flow worked better:
  1. Extract/verify clue-square model.
  2. Generate candidate answers per slot.
  3. Resolve grid constraints or use API validation.
- Raw `agent-browser keyboard type` dropped `Æ`/`Ø` in this iframe. A safer
  fill path is to save a near-complete `solution_user` through the vendor API,
  reload the parent page, fix one ASCII cell through the UI, then click
  `Tjek løsning` in the embedded parent page.
- The hint endpoint returns a per-cell correctness mask for a proposed
  `user_solution`; it can be used as an oracle, but it increments the game's
  hint counter and should be treated as a debugging fallback, not the normal
  solver path.

## Capturing result screenshots for the ledger

After `Tjek løsning` succeeds and you see the "Hvor er du vild!" confirmation:

1. Take a clear screenshot of the parent page showing the filled grid
   (e.g. via the browser or agent-browser `Screenshot`).
2. Crop tightly to just the crossword board (the `image_edit` tool or any
   editor works; see the 2026-05-31 example in `.klublotto/`).
3. Attach to the ledger (Postgres is the source of truth and the web UI
   detail view will display it):

   ```
   klub-lotto ledger attach-image \
     --dsn "$DATABASE_URL" \
     --date 2026-06-01 \
     --game krydsord \
     --image .klublotto/krydsord-2026-06-01-cropped.jpg
   ```

The `attach-image` command (and the `result_image` bytea column + detail
template) were added so every future Krydsord solve automatically ends with a
nice visual record in the ledger.

## 2026-05-31 Observation

- Grid size: 10 columns by 11 rows.
- Solved answer grid, with `.` for clue/non-answer cells:

```text
..........
.ØSTERSØEN
.RÆV.AORTA
.KR.TT..OG
.ETUI.RISE
.N.NEVET..
.RIGTIG.TR
.OS..SNØRE
.T.LOK.LE.
.TEAMET.ES
.ELM.ROERE
```

- Solved through the embedded Danske Spil parent page.
- Result screen showed: `Hvor er du vild! Du løste dagens krydsord som en
  sand ordmester! (Du har allerede optjent dagens lod)`.
- The Spil & Quiz overview then showed a Krydsord checkmark.

