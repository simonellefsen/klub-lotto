---
kind: concept
tags: [klublotto, llm, providers]
updated: 2026-05-31T00:00:00Z
---

# LLM providers

The Quiz solver calls every configured provider in parallel and takes the
majority vote. This page tracks what we know about each one in this
specific context (Danish multiple-choice trivia).

Quiz providers implement `llm.Provider` (`ChooseOne`). Free-form games such
as Ordknuden use the narrower `llm.JSONGenerator` interface so the game
logic can swap Gemini, OpenAI, xAI, Anthropic, or OpenRouter without knowing
provider-specific wire formats.

## Currently configured

| Provider | Default model | Why |
|---|---|---|
| Anthropic | `claude-sonnet-4-6` | Strong on Danish, careful reasoning; first in the vote order so it breaks ties |
| OpenAI | `gpt-5.4` | Current OpenAI frontier model; supports Chat Completions and structured output |
| Gemini | `gemini-2.5-flash` | Cheap, fast, good general knowledge, `response_mime_type` JSON |
| xAI | `grok-4-fast` | Cheapest of the four; pure prompt-driven JSON (no structured output) |
| OpenRouter | `google/gemini-2.5-flash` | Optional Ordknuden JSON suggester via OpenAI-compatible routing |

Models are overridable in code (`llm.NewAnthropic(key, "claude-haiku-4-5-20251001")`,
`llm.NewOpenAI(key, "gpt-5.4-mini")`, etc.) but the PoC ships with defaults
chosen for accuracy on Danish trivia. OpenAI can also be overridden without
code changes via `OPENAI_MODEL` in `.env.local`.

Ordknuden's model is selected with `ORDKNUDE_PROVIDER` or
`klub-lotto ordknude --provider <name>`, where `<name>` is one of
`gemini`, `openai`, `xai`, `anthropic`, or `openrouter`. OpenRouter uses
`OPENROUTER_API_KEY` and optional `OPENROUTER_MODEL`.

## API details

| Provider | Endpoint | Auth header | Structured output |
|---|---|---|---|
| Anthropic | `https://api.anthropic.com/v1/messages` | `x-api-key` + `anthropic-version: 2023-06-01` | none (prompt-driven JSON) |
| OpenAI | `https://api.openai.com/v1/chat/completions` | `Authorization: Bearer …` | `response_format: {type: json_object}` |
| Gemini | `https://generativelanguage.googleapis.com/v1beta/models/<model>:generateContent?key=…` | key in query string | `response_mime_type: application/json` |
| xAI | `https://api.x.ai/v1/chat/completions` | `Authorization: Bearer …` | none (prompt-driven JSON) |
| OpenRouter | `https://openrouter.ai/api/v1/chat/completions` | `Authorization: Bearer …` | model-dependent; prompt-driven JSON |

## Observations

_Filled in as we accumulate `wiki/sources/quiz-*.md` files. Look for
patterns like "Gemini is consistently right on history, OpenAI on
sport"._

- _(no data yet)_

## Tie-breaking

`llm.Majority` picks the most-voted index and breaks ties by provider
order. Provider order in `cmd/klub-lotto/main.go` is currently
`[Anthropic, OpenAI, Gemini, xAI]`. Move the most trustworthy one to
position 0 once we have evidence.

## Benchmark plan

When we have ~30 quiz rounds in `wiki/sources/`:

1. Extract `(question, options, correct_answer)` from each.
2. Replay each provider against the historical set.
3. Score by per-provider accuracy and by which provider's confidence
   correlates best with correctness.
4. File the results under `wiki/concepts/benchmark-<date>.md`.

## See also

- [Quiz game](../games/quiz.md)
- [agent-browser](agent-browser.md)
