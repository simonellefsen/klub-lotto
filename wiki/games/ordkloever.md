---
kind: game
game: ordkloever
tags: [klublotto, game, ordkloever, clover, word-game]
updated: 2026-05-31
---

# Ordkløver

Ordkløver is a daily Danish phrase puzzle embedded from Immer Spiele on
`https://danskespil.dk/klublotto/dagens-ordkloever`.

## Rules

- Guess the daily riddle in as few attempts as possible.
- The game starts with 12 guesses.
- `Gæt bogstav` guesses one letter. If the letter appears in the answer, all
  matching positions are revealed.
- `Gæt gåde` guesses the full answer.
- Each letter guess costs one attempt.
- Each full riddle guess costs one attempt.
- `Brug ledetråd` costs two attempts.

## Automation Notes

- The board can contain multiple words and fixed spaces.
- The on-screen keyboard includes `Æ`, `Ø`, and `Å`.
- The category and board shape are visible before any guesses are spent.
- The Immer Spiele API can reveal the board, but completing the game directly
  in the iframe/API may not register with Danske Spil. For ticket credit,
  submit through the embedded iframe on the Danske Spil parent page.

## 2026-06-07

- Category: `Person`
- Answer: `SMAGSDOMMER` (11 letters)
- Board at submission: `S _ A _ S D O _ _ E R` — letters E, R, S, O confirmed green; N, I, T, A, D dark
- Guessed letters: `ADEORSINT` (10 attempts used)
- Game showed: `Flot præstation` — correctly accepted
- Win detection missed the success screen (final-fallback path submitted the answer but did not recognise the result banner)

## 2026-05-31 Observation

- Category: `Tilstand`
- Board shape: `9 / 3 1 / 9`
- Hint from API: `Når man er lettere udfordret på første sal`
- Answer: `ROTERENDE FIS I KASKETTEN`
- Submitted through the embedded Danske Spil page and the game showed:
  `Du har knækket koden! ... Som belønning får du dagens lod.`
- The Spil & Quiz page then showed `12` earned tickets total and two Sunday
  stars, confirming the parent-page path registered.

