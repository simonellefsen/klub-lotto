---
kind: concept
tags: [klublotto, auth, mitid, security]
updated: 2026-05-31T00:00:00Z
---

# MitID handoff

Danske Spil's Klub Lotto is gated behind **MitID**, Denmark's national
electronic ID system. There is no username/password fallback for ordinary
Danish accounts — only Færøerne residents see a password form. This
means a fully automated login is impossible by design: MitID requires
either the MitID app (push to phone), a code display (hardware token), or
chipkort/biometrics.

## Our approach: one-time handoff, session persistence

1. `klub-lotto login` opens the browser **headed** and navigates to the
   Danske Spil login page.
2. The cookie banner is dismissed with **"Fravælg alle"** (decline
   optional cookies — the privacy-preserving default).
3. We click `Log på med MitID` to take you to the MitID screen.
4. The program prints a clear handoff message and **waits**. You
   complete MitID in the open Chrome window — app confirm, biometrics,
   whatever your setup uses.
5. Two signals end the wait:
   - **Auto-detection** (preferred): every 2s we check if the URL has
     left `id-dlo.danskespil.dk` AND the accessibility tree shows
     logged-in markers (`Log ud`, `Min konto`, `Dine præmier`,
     `Optjente lodder`).
   - **Manual override**: press Enter in the terminal once you see the
     Klub Lotto page. We verify once and proceed.
6. Timeout: 5 minutes.

Once authenticated, agent-browser saves the session state
(cookies + localStorage) under `~/.agent-browser/sessions/klublotto/`.
Subsequent `klub-lotto quiz` runs reuse it and skip the handoff entirely.

## How long does the session last?

Unknown — we'll learn it the hard way the first time it expires. Danish
gambling sites typically issue session cookies in the days-to-weeks
range. The log entry in `wiki/log.md` for each successful login should
let us correlate "last MitID" with "first failed quiz" once we have
data.

If the cookie expires:

- `klub-lotto quiz` will get redirected to the login page.
- The same handoff flow kicks in automatically.
- You re-do MitID once, then daily runs continue.

## Headless mode does not work for this

The MitID flow needs you visible. If you ever pass `--headless`, the
first run after the cookie expires will hang and time out because
nothing can complete MitID. Two safe patterns:

- **Local Mac, headed (current PoC):** `make quiz` runs headed by
  default; you re-auth manually when needed.
- **k8s CronJob (future):** keep state in cnpg-postgres. When the
  pod sees an expired cookie it fires a notification (Slack/email) and
  exits non-zero; you VPN in, run a headed re-auth, and the next cron
  tick picks up the new cookie.

## Why "Fravælg alle" for cookies?

The banner offers four categories — `NØDVENDIGE` (required, can't
disable), `FUNKTIONELLE` (login persistence), `STATISTIK`,
`MARKEDSFØRING`. The system prompt's privacy rule says decline
non-essential. `NØDVENDIGE` is enough for session login on most
modern sites; "Fravælg alle" leaves only that on.

If we ever see auth break because of this, switch the dismiss step to
`Tillad valgte` with `FUNKTIONELLE` explicitly enabled.

## Security notes

- The MitID flow runs in your real Chrome process (via agent-browser's
  CDP-controlled Chrome for Testing). It never sees your MitID
  credentials — they're typed/tapped on your phone or hardware token.
- The persisted session state is a JSON file containing the Danske
  Spil session cookie. **Encrypt it at rest**:
  ```bash
  export AGENT_BROWSER_ENCRYPTION_KEY=$(openssl rand -hex 32)
  ```
  Add that line to `~/.zshrc` so every run finds the key.
- `.env.local` still contains `DANSKESPIL_USERNAME` and
  `DANSKESPIL_PASSWORD` from your initial setup. These are unused
  today; safe to remove, but harmless if kept.

## See also

- [Quiz game](../games/quiz.md)
- [agent-browser](agent-browser.md)
- [RUN.md](../../RUN.md)
