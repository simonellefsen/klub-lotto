---
kind: game
game: ordknuden
tags: [klublotto, game, ordknuden, wordle]
updated: 2026-05-31
---

# Ordknuden

Ordknuden is a Danish Wordle-style Klub Lotto game. The parent page is
`https://danskespil.dk/klublotto/dagens-ordknuden`, but the playable board
is embedded in a signed `klub-lotto.immerspiele.com` iframe.

## Rules

- Guess one Danish five-letter word in six attempts or fewer.
- Green/correct means the letter is correct and in the correct position.
- Yellow/present means the letter is in the word but in a different position.
- Red/absent means the letter is not in the word, with normal duplicate-letter
  Wordle handling.
- Danish letters `Æ`, `Ø`, and `Å` are valid and appear on the on-screen
  keyboard.

## Automation Notes

- Default to Gemini for now, but keep the solver provider-agnostic through
  `llm.JSONGenerator`. OpenAI, xAI, Anthropic, and OpenRouter can be selected
  without changing board logic.
- Prompt the selected model to use real Danish dictionary words, preferably words that
  would appear in ODS/ordnet.dk or Den Danske Ordbog. Explicitly reject
  Swedish, Norwegian, English, names, unclear inflections, and non-dictionary
  forms.
- Every accepted guess is persisted by Klub Lotto; there are no do-overs.
  Always read the existing board before choosing the next word.
- Submit by clicking the on-screen keyboard buttons, not by raw text typing.
  Raw `keyboard type` dropped `Æ` in `SALÆR`, leaving only `SALR`.
- If the UI reports `Ordet findes ikke i vores database`, treat the word as
  rejected, clear the row, and feed that rejection back into the next model
  prompt.

## 2026-05-31 Observation

The winning answer was `SALÆR`. Accepted guesses before the win were:

- `SALEN`: S/A/L correct; E/N absent.
- `SALAT`: S/A/L correct; second A/T absent.
- `SALDO`: S/A/L correct; D/O absent.
- `SALIG`: S/A/L correct; I/G absent.
- `SALÆR`: solved.

ODS/ordnet.dk has an entry for `salær`, which makes it a good reference
example for words with Danish special letters.

This game was solved through the Immer Spiele/direct iframe path during early
debugging. Later the Danske Spil overview did not show a checkmark on the
Ordknuden tile, while opening Ordknuden still showed `Vundet!!!` and `SALÆR`.
That confirms direct iframe play can persist vendor game state without marking
the Danske Spil parent tile as completed.
