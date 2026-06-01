# klub-lotto

Automated daily-game player for [Danske Spil Klub Lotto](https://danskespil.dk/klublotto).

Logs in, navigates to today's games, and solves them. The Quiz game scrapes the question + options and asks configured LLMs which answer to submit. Ordknuden uses a provider-agnostic JSON word suggester, defaulting to Gemini for now.

## Stack

- **Go** CLI (single static binary).
- [agent-browser](https://github.com/vercel-labs/agent-browser) (Vercel Labs) for the actual browser automation — wrapped as a subprocess. K8s sets both `AGENT_BROWSER_SESSION=klublotto` and `AGENT_BROWSER_SESSION_NAME=klublotto` so the live browser session and persisted state stay aligned.
- Three pluggable LLM providers behind a single `Provider` interface. The PoC ships with a `compare` subcommand that asks all three and shows their answers side-by-side before submitting.

## Status

Proof of concept. Runs locally on macOS against your own Klub Lotto account. K8s/cnpg-postgres deployment is planned, not built.

See [`RUN.md`](RUN.md) for the exact commands to try it.

## Layout

```
cmd/klub-lotto/      CLI entrypoint (cobra-style subcommands, but stdlib flag for now)
internal/browser/    agent-browser subprocess wrapper + snapshot parsing
internal/llm/        Provider interfaces + OpenAI / xAI / Gemini / Anthropic / OpenRouter implementations
internal/klublotto/  Site-specific flows (login, quiz)
internal/config/     .env.local loader, secret resolution
internal/store/      State persistence (file for PoC; cnpg-postgres later)
scripts/             One-off helpers (recon, headed runs)
```
