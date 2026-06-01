---
kind: concept
tags: [klublotto, automation, agent-browser]
updated: 2026-05-31T00:00:00Z
---

# agent-browser

We drive the browser via [vercel-labs/agent-browser](https://github.com/vercel-labs/agent-browser):
native Rust CLI, daemon-backed, accessibility-tree snapshots with stable
`@eN` element refs. The Go wrapper lives in `internal/browser/`.

## Why this and not Playwright/Selenium

- Snapshot+ref workflow is purpose-built for LLM-driven flows.
- Sessions persist cleanly across runs (`--session klublotto --session-name klublotto`).
- One CLI binary, no Node toolchain.

## Session model

We set both `AGENT_BROWSER_SESSION=klublotto` and
`AGENT_BROWSER_SESSION_NAME=klublotto` in k8s. `AGENT_BROWSER_SESSION`
selects the isolated live browser session; `AGENT_BROWSER_SESSION_NAME`
selects the auto-save/load persistence name. Keeping them equal means the
VNC browser and automation commands operate on the same session and persisted
state. Encryption at rest if you set `AGENT_BROWSER_ENCRYPTION_KEY` to a
64-char hex string — recommended in production:

```bash
export AGENT_BROWSER_ENCRYPTION_KEY=$(openssl rand -hex 32)
```

## Headed vs headless

PoC default is **headed** so the operator can watch and intervene. Pass
`--headless` once the flow is reliable enough to schedule.

## Core workflow we use

```
agent-browser --session klublotto --session-name klublotto --headed open https://danskespil.dk/klublotto
agent-browser --session klublotto --session-name klublotto snapshot -i        # get @e refs
agent-browser --session klublotto --session-name klublotto fill <sel> <val>
agent-browser --session klublotto --session-name klublotto click @e2
agent-browser --session klublotto --session-name klublotto wait --load networkidle
```

The Go wrapper just shells these out, parses `--json`, and exposes
typed methods.

## Selectors

In rough order of preference:

1. Refs from a fresh `snapshot -i` (`@e2`). Stable for the lifetime of
   the snapshot.
2. `find role <role> --name "..."` and `find label`. Survives UI churn
   better than CSS.
3. CSS as a last resort. The `klublotto.fillByAnyOf` helper passes
   several CSS candidates so we don't depend on any single class name.

## Cross-origin iframes

Klub Lotto games hosted by Immer Spiele live in cross-origin iframes. Parent
page snapshots can identify the iframe but cannot expose the game controls
inside it. Use `agent-browser eval` to get the iframe bounding box, then use
`agent-browser mouse move/down/up` for coordinate clicks inside the embedded
frame, with screenshots after each irreversible step.

Do not complete ticket-earning games by opening the Immer Spiele iframe as a
top-level page unless registration does not matter. The iframe posts completion
events to `window.parent`, so the Danske Spil page must be the parent if we
want the earned ticket to register.

## Anti-bot considerations

Danske Spil is a regulated gambling site and may run bot-detection. The
agent-browser daemon already uses a real Chrome (not headless-flagged
unless we ask for it), so for headed runs we're indistinguishable from
a user. If detection lands, the next moves are:

1. `--profile "Default"` to ride on the user's actual Chrome profile.
2. Run via `--proxy` from a residential IP.
3. Use Kernel/Browserbase providers with stealth mode.

## See also

- [Quiz game](../games/quiz.md)
- [Immer Spiele embeds](immerspiele.md)
- [LLM providers](llm-providers.md)
