---
kind: game
game: kaedeord
tags: [klublotto, game, kaedeord, word-grid, immerspiele]
updated: 2026-11-18
---

# Kædeord

Kædeord ("chain words") is a Boggle-style Klub Lotto game added in November
2026. Trace a word through touching letter tiles; the word is removed, the
tiles above fall, and a column that empties closes up. Played by
`make kaedeord` (`klub-lotto kaedeord`); `make kaedeord-dry` prints the plan
without playing.

- Parent page: `https://danskespil.dk/klublotto/dagens%20k%C3%A6deord`. The
  path really does contain a space; it's the Spil & Quiz overview's own link.
- Game iframe: `iframe.kl-game__iframe` →
  `klub-lotto.immerspiele.com/games/present/fallout?...`. immerspiele's
  internal name for the game is **fallout**.
- Launcher: `/dlo/webapi/gateway/klublotto/v1/games/kaedeord/gamelauncher/web`
  (`completed: true` once played).

## Rules, from the game's own bundle

Read from `themes/builds/34/h1YI9XGdw/assets/index-C4FjS5ci.js` on 2026-11-18.
`internal/klublotto/kaedeord.go` ports each rule:

- **Board**: the puzzle's `board` string, one letter per tile, row by row. A
  digit after a letter is its multiplier (`W4` is a W worth ×4). The first
  board was 7×7 with one ×4 tile.
- **Paths**: a word is 3+ tiles, each one of the 8 neighbours of the last.
  The edges wrap ("som en sand gespenst, kan du gå gennem vægge"): column 0
  touches the last column, and the topmost occupied row touches the bottom
  row. Both wraps follow the board as it shrinks.
- **Removal**: tiles above a removed tile fall; a column that empties is
  dropped and the columns to its right shift left.
- **Points**: length² × the product of the word's multipliers (WEBINAR through
  the ×4 W = 7² × 4 = 196). Clearing the board adds `boardClearBonus` (100).
- **Lifelines**: `numberOfDeleteOperations` (3) deletes one tile;
  `numberOfSwapOperations` (1) swaps two. Neither scores.
- **Words**: the game validates against its own word list
  (`/public/assets/fallout/words-<hash>.txt`, from the puzzle's `wordList`):
  680,496 Retskrivningsordbogen inflections of 3–32 letters, front-coded
  (a base-36 shared-prefix count, then the new letters). The game's decoder
  never adds the file's last entry, and ours matches.
- **Input**: click (mousedown) tiles in order, then the last tile again to
  submit. A click on a tile that doesn't touch the last one resets the
  selection. Clicks are ignored while a removal animates.
- **Ending**: a cleared board ends the game. Otherwise "Afslut" opens a
  confirm dialog whose "Bekræft" submits the score. 50+ points earns the lod.

## How the solver plays

- Reads the board from the DOM: every `div[data-tile-id]` sits in a CSS grid
  cell, and the cell index gives its position. Multipliers show as "×N", and
  the lifeline buttons show their remaining count.
- Downloads the word list once and caches it as
  `.klublotto/kaedeord-words-<hash>.txt`, then builds a trie over the
  29-letter alphabet.
- Beam search (200 positions per depth) over every word on every position,
  ranked by points plus 2 per tile removed. A stuck position may spend a
  delete or swap that opens up a word, and a finished position that has as
  many deletes as tiles left is cleared with them. On the first board, beam
  200 solves in ~13s.
- Clicks with real mouse events (iframe offset + tile centre). It checks
  after each click that the tile was selected (its classes changed), and
  after each step that the live board equals the simulated one, re-planning
  from the live board if not.

The game's puzzle API (`/games/api/fallout/puzzles/latest`) needs the
embedding page's API key, so the solver reads the rendered board instead of
calling it.

## 2026-11-18: first play

Score **568**, board cleared. The plan worked first time with no re-plans
and no lifelines: WEBINAR (+196) · PEJLEVOGNS (+100) · FULDFEDT (+64) ·
HYPHEST (+49) · VÅG (+9) · MÆRK (+16) · BØJ (+9) · KICK (+16) · CAM (+9) +
100 clear bonus.

- Parent after the game: "Du har ordet i din magt! Godt klaret! Du samlede
  minimum 50 point. … (Du har allerede optjent dagens lod)". The iframe is
  removed.
- Parent on a later visit: "Intet nyt her! Du har allerede spillet Kædeord –
  men der kommer en ny igen i morgen." `AlreadyPlayed` matches it, so a
  rerun exits 0 and leaves the ledger as it is.
