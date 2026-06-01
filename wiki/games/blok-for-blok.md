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
