---
kind: game
game: blok-for-blok
tags: [klublotto, game, block-puzzle, agent-browser]
updated: 2026-05-31T12:30:00Z
---

# Blok for Blok

Klub Lotto's 8x8 block-placement game. The game gives three pieces at a
time, awards one point per placed block, and clears full rows or columns.
The daily target is 200 points, but the game only reports completion when
it reaches game-over; `gameCompleted` is emitted when the final score is
at least 200.

## 2026-06-19 Result — FIRST AUTOMATED WIN ✓

- Final score: `200` → game auto-completed at the 200-point goal.
- Win screen (embedded parent): `Du klarede det Blok for blok! … Du samlede 200 point. (Du har allerede optjent dagens lod)`.
- Registered: ✓ checkmark on the Spil & Quiz overview tile.
- Solved live by the Python tooling now committed at `tools/blok/`:
  - `blok.py` — perception (pixel-sampling): `read_board` (8×8 occupancy from the
    empty-maroon mask) + `read_pieces` (tray shape matrices + viewport pickup point).
  - `blok_play.py` — solver + executor: full-trio lookahead greedy
    (`120*lines_cleared + quality`, quality penalises dead 1×1 holes), smooth
    stepped coordinate drags, and **predicted-board** placement verification
    (`apply_p` result), receding-horizon re-read after every move.
  - Run: `BLOK_SHOTDIR=<dir> /tmp/blokenv/bin/python tools/blok/blok_play.py <target_cells>`
    after opening the game and clicking "Start spil". `target_cells` is a cumulative
    placed-cell budget; since score = placed cells + combo bonuses, it's a safe lower
    bound on score (e.g. 190 placed cells reached 200 actual via combos).
  - venv: `/tmp/blokenv` (Pillow + numpy); recreate if missing — system PIL is
    blocked by PEP-668.

### Key learnings (2026-06-19)
- **The game auto-completes at 200 points** — you do NOT have to play to a board-full
  game-over. The board is replaced by the win screen the moment score hits 200.
- Consequently, when the executor's `read_settled` throws
  `could not read board (canvas blank/collapsed?)`, that is very likely the **win
  screen** (board gone), not a failure — screenshot and check before treating it as an error.
- **Concurrency:** the player and any monitoring share ONE agent-browser session;
  `frame` switches race. Never issue agent-browser commands (screenshot/frame/eval)
  while the background player runs. Monitor via its `-u` (unbuffered) log only.
- **Detect placement success against the predicted board, not `board==board`** — a
  placement that immediately clears a line can leave the board identical or emptier,
  which the old equality test misread as a failed drag.
- Staleness/edge-placement (the two 2026-06-18 blockers) did not bite this run; the
  drag animation forces a fresh OOPIF composite so post-move reads were reliably fresh.

## 2026-05-31 Result

- Final score: `228`
- Outcome: `gameCompleted`
- Parent result page: `Du klarede det Blok for blok!`
- Screenshot: `.klublotto/blok-parent-finish-real.png`

## Automation Notes

- The iframe origin is `https://block.klublotto.danskespil.mgame.nu`.
- State is stored under the `default` localStorage object, nested key
  `blockYYYY-MM-DD`.
- Board size is `8x8`; stored row indices are bottom-to-top relative to
  the visual board.
- Shape definitions and the deterministic daily piece stream are in the
  game bundle. For 2026-05-31 the initial pieces were:
  `12.2`, `5.0`, `10.2`.
- Replaying through the direct game iframe can solve the vendor state.
  Opening the embedded Danske Spil parent page afterward loads the stored
  completed game and shows the official result page.
- Slow, stepped drags are more reliable than instant drag jumps. Some
  source shapes have empty centers, so source clicks should target an
  actual occupied mini-cell rather than the tile frame center.
