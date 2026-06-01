---
kind: concept
tags: [klublotto, immerspiele, iframe, automation]
updated: 2026-05-31T10:17:00Z
---

# Immer Spiele Embeds

Some Klub Lotto games are hosted by Immer Spiele in a cross-origin iframe on
the Danske Spil page. The parent page is still important: the iframe owns the
game UI and game API calls, while the parent page receives game lifecycle
events and appears to be what Danske Spil uses to register the earned ticket.

## Shape

- Parent URL example: `https://danskespil.dk/klublotto/dagens-ordkloever`.
- Embedded frame example:
  `https://klub-lotto.immerspiele.com/games/present/clover?...`.
- The iframe URL includes signed parameters such as `launcher`, `checksum`,
  `platform`, and `date`.
- The iframe HTML carries a public game API key in `data-api-key` and a proxy
  path like `/games/api`.

## API

The clover game loads the current puzzle from:

```text
GET /games/api/clover/puzzles/latest
```

The request uses:

- `Authorization: Bearer <iframe data-api-key>`
- `X-Immer-Player-ID: <checksum from iframe URL>`

The puzzle response includes the board, category, attempt count, hint text,
player status, current state, and keyboard letters.

Game actions are posted to:

```text
POST /games/api/clover/puzzles/{id}
```

Observed action payloads:

- Letter guess: `[1, "A"]`
- Whole riddle guess: `[2, "ROTERENDE FIS I KASKETTEN"]`
- Hint: `[3]`

## Registration Risk

Opening the Immer Spiele iframe URL directly is useful for inspection, but it
can bypass Danske Spil registration. The iframe posts lifecycle events to
`window.parent`, including `gameStarted`, `gameEvent`, `gameCompleted`,
`gameFailed`, and `gameClose`. When the iframe is opened as the top-level page,
`window.parent` is just the iframe page itself, so Danske Spil never receives
those messages.

For anything that must earn a Klub Lotto ticket, keep the Danske Spil parent
page open and interact with the iframe while it is embedded. Coordinate clicks
through `agent-browser mouse` worked for Ordkløver when the accessibility
snapshot could not see into the cross-origin frame.

There are two separate pieces of state:

- Immer Spiele game state: whether the game itself is won, plus the revealed
  board/guess history.
- Danske Spil parent state: whether the completed game counted as the daily
  completed-game ticket and gets shown with a checkmark on the Spil & Quiz
  overview.

These can diverge. Ordknuden was won in Immer Spiele state after direct iframe
play, but the Spil & Quiz overview did not show its tile as completed. Opening
Ordknuden later showed `Vundet!!!` and `SALÆR`, proving the vendor state was
won even though the parent page did not mark the tile.

The page also says only one of the six games can earn the second daily ticket:
`Du kan optjene ét lod ved at løse et af de seks spil herunder. Resten er bare
for sjov.` The conditions section says the first daily ticket is earned by
opening Spil & Quiz, and the second by completing one optional game/quiz.

## agent-browser Notes

- Parent snapshots only show the iframe element; they do not expose the
  cross-origin game controls.
- `agent-browser eval` on the parent can still locate the iframe URL and
  bounding box.
- `agent-browser mouse move/down/up` can click coordinates inside the embedded
  iframe.
- Use screenshots after each click to verify state before making irreversible
  guesses. These games persist accepted guesses; there are no do-overs.

Krydsord is a different vendor (`iframes.krydsord.dk`) but follows the same
parent-registration pattern: the embedded frame posts `gameStarted`,
`gameEvent`, and `gameCompleted` messages to `window.parent`. For completion
credit, make the final `Tjek løsning` click inside the iframe while it is
embedded in the Danske Spil parent page.

Krydsord-specific API notes:

- `cmd=get_data_and_image` returns a board image plus `solution_secret`, a
  row-major mask of clue/non-answer cells versus answer cells.
- `cmd=check_or_save_user_solution` can save a partial `solution_user`.
- `cmd=hint` returns per-cell correctness for a proposed `user_solution`; it
  is useful for debugging but increments the game's hint counter.
- Raw browser keystrokes dropped `Æ`/`Ø` in the crossword iframe. Loading a
  saved `solution_user` rendered Danish letters correctly.
