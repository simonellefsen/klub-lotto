---
kind: concept
tags: [klublotto, deploy, k8s, postgres, novnc]
updated: 2026-05-31T00:00:00Z
---

# k8s deployment (docker-desktop)

The PoC graduated from "Mac-only Go binary" to "single-pod deployment on
docker-desktop with Postgres and a noVNC-driven login UI". This page
captures the architecture; manifests live in `deploy/k8s/`.

## Architecture

```
┌─────────────────────── Browser (your Mac) ────────────────────────┐
│  http://klub-lotto.localhost     (or http://localhost:8080)       │
└───────────────────────────────────────────────────────────────────┘
                            │
        nginx-ingress  ────▶ klub-lotto Service :80 ──▶ pod :8080
                            │
┌───────────────────── Namespace klub-lotto ────────────────────────┐
│                                                                    │
│  Deployment klub-lotto (replicas: 1)                              │
│    container `app`                                                 │
│       supervisord                                                  │
│         Xvfb :99                                                   │
│         fluxbox                                                    │
│         x11vnc → :5900                                             │
│         websockify (noVNC) → :6080                                 │
│         klub-lotto-web (Go) → :8080                                │
│           reverse-proxies /vnc/ to localhost:6080                  │
│           forks `klub-lotto login|quiz|...` on demand              │
│                                                                    │
│  CNPG Cluster klublotto-db (replicas: 2)                          │
│    klublotto-db-rw   primary write                                │
│    klublotto-db-ro   standby read                                 │
│                                                                    │
│  PVC klub-lotto-state                                              │
│    /var/lib/agent-browser   session cookies (survive pod restart) │
│    /var/lib/klub-lotto/wiki exported markdown                     │
└────────────────────────────────────────────────────────────────────┘
```

## Login flow (MitID inside k8s)

1. You open `http://klub-lotto.localhost` in your Mac browser.
2. The page embeds an iframe pointing at `/vnc/vnc.html` — this is the
   noVNC client served by websockify inside the pod, proxied through
   our Go server.
3. You click **Trigger MitID login**. The web server starts a job:
   `klub-lotto login` runs in the same pod with `DISPLAY=:99`, so Chrome
   renders on the virtual framebuffer. x11vnc exposes that framebuffer;
   you see it live in the iframe.
4. You complete MitID by clicking through the noVNC view.
5. agent-browser auto-detects the redirect back to Klub Lotto, writes
   the session cookie to `/var/lib/agent-browser/sessions/klublotto/`
   (which is on the PVC), and the job finishes.
6. A `login_events` row is inserted with `status=completed`.

Subsequent game runs (Quiz / Ordknuden / etc.) reuse the cookie until it
expires. When it does, you re-do step 3 from the same UI — no `kubectl
exec` needed.

## DB schema

See `internal/store/schema.sql`. Four tables:

- `games` — slug, name, description (seeded).
- `daily_ledger` — one row per (date, game). The web UI's main table.
- `runs` — one row per LLM provider call. Powers the detail page.
- `login_events` — MitID audit trail.

The wiki/ directory is **exported** from this schema on every run.
`store.ImportWikiDaily` (also exposed as `klub-lotto wiki import-db` on
the CLI) seeds the DB from an existing wiki if you're migrating an
existing repo. It's idempotent.

## Prereqs

```bash
# docker-desktop with Kubernetes enabled
kubectl version --short

# cnpg operator (one-time, cluster-wide)
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.1.yaml

# nginx-ingress already comes with docker-desktop; verify:
kubectl get ns ingress-nginx
```

## Deploy

```bash
# 1. Real secrets (gitignored; never commit)
cp deploy/k8s/20-secret-env.example.yaml deploy/k8s/20-secret-env.yaml
$EDITOR deploy/k8s/20-secret-env.yaml

# 2. First-time install: build image, apply all manifests
make image
make k8s-up

# 3. Wait for the pod
kubectl -n klub-lotto get pods -w

# 4. Open the UI
open http://klub-lotto.localhost
# or: make port-forward → http://localhost:8080

# 5. After code changes, ship a new image and roll the deployment:
make deploy
```

## Why `make deploy` uses a git-sha image tag

docker-desktop's kubelet caches images by tag. If you rebuild
`klub-lotto:dev` and `kubectl rollout restart`, the kubelet sees the
same tag and keeps using the **cached** image — your changes don't ship.
`make image` solves this by tagging every build with the current git
short SHA (and a `-dirty.<timestamp>` suffix when the worktree is
dirty). `make deploy` then runs `kubectl set image` against that unique
tag, forcing the kubelet to resolve the tag fresh against the local
image store. The floating `klub-lotto:dev` tag is still applied for
ease of debugging via `docker run`.

If you ever revert to a previous build manually, use the SHA tag
explicitly:

```bash
docker images klub-lotto
kubectl -n klub-lotto set image deploy/klub-lotto app=klub-lotto:<sha>
```

## First-boot bootstrap + historical import

The web server auto-imports on startup only if `daily_ledger` is completely
empty **and** `daily/` already contains files in the mounted wiki PVC.

To seed (or re-seed) the DB from an existing local `wiki/` tree (now the
recommended way, since Postgres is the source of truth):

1. Copy the wiki content into the PVC:

   ```bash
   POD=$(kubectl -n klub-lotto get pod -l app=klub-lotto -o name)
   kubectl -n klub-lotto cp wiki/. ${POD#pod/}:/var/lib/klub-lotto/wiki/ -c app
   ```

2. Run the importer (the command that now actually exists):

   ```bash
   kubectl -n klub-lotto exec -it deploy/klub-lotto -c app -- \
     klub-lotto wiki import-db --dsn "$DATABASE_URL" --wiki /var/lib/klub-lotto/wiki
   ```

The CLI command is idempotent and will report what it did. After the
initial seed, the web UI and post-game hooks keep the ledger up to date.
The markdown files under `wiki/` become derived output / history.

## Observations / open items

- The deployment is **single-replica** by design. agent-browser owns
  one Chrome process; running two would race on the session-state PVC.
- noVNC connection is unauthenticated inside the cluster. For
  docker-desktop that's fine because the ingress only binds localhost.
  Outside that, put a basic-auth annotation on the Ingress or front
  with oauth2-proxy.
- Image size is ~1.2GB (Chromium dominates). Acceptable for
  docker-desktop; for a remote cluster, switch to a multi-arch build
  and a registry.
- `klublotto-db-app` Secret is created by the cnpg operator from the
  Cluster's `bootstrap.initdb` spec; we read `uri` from it.
- Public exposure is done via the shared ngrok gateway (see
  `../shared-ngrok-gateway`). The app only needs to maintain the internal
  `AgentEndpoint/klub-lotto-internal` (defined in `70-ngrok-internal.yaml`).

## See also

- [MitID handoff](mitid-handoff.md)
- [agent-browser](agent-browser.md)
- [LLM providers](llm-providers.md)

## Manual verification checklist for follow-up features (024cce1b)

After `make deploy` (or rollout restart), exercise these (documented for coverage of new UI/probe/gateway paths; no automated harness added per minimal-change rule):

**Modal UX (noVNC):**
1. Click "Trigger MitID login" → modal opens with VNC (no unnecessary reload on close/reopen).
2. ⤢ maximize toggle works; ↻ reload VNC button forces fresh src.
3. Complete (or simulate) login job → #job shows "pill ok" for login action → modal auto-closes (via afterSwap + querySelector('.pill.ok')) or jobFinished listener. Prominent "MitID completed — close" + steps text visible.
4. Escape key + header Close work; VNC WS stable during long session (no flashing).

**Live auth badge + probe:**
1. Header pill shows green "Valid session (last verified: just now / Nds ago)" (or red).
2. ↻ button forces immediate probe (bypasses 75s cache), updates pill live via HTMX; 5s client debounce prevents spam.
3. On transient --check failure, falls back to recent login_event "completed" without falsely updating "last verified".
4. Bootstrap on pod start (kubectl rollout restart) records to DB within 30s timeout; /healthz responds promptly even on probe hang.

**Desktop (fluxbox/x11vnc):**
1. `kubectl -n klub-lotto logs deploy/klub-lotto -c app | grep -E '(fluxbox|x11vnc)'` shows no fbsetbg/xmessage/DPMS spam.
2. `kubectl exec ... -- cat /root/.fluxbox/init` contains #1e1e2e + toolbar.visible:false.
3. Restart pod → entrypoint re-writes init robustly; no dialogs in noVNC view.

**Gateway hardening (/klub-lotto path):**
1. `cd ../shared-ngrok-gateway && ENV_FILE=... make health` shows Ready conditions on daytrader-frontend AE + policy.
2. `make apply` runs post-apply curls for "" /klub-lotto* etc.; expect 302 (OAuth) + note on downstream validation.
3. Public https://unground-uncraftily-vivienne.ngrok-free.dev/klub-lotto loads UI (authed); runbook in add-route.md followed on "red" symptoms.

**Critical requirement (as of 2026-05-31):**
The TrafficPolicy in the sibling gateway **must** catch the entire `/klub-lotto*` prefix
and rewrite + forward it to `klub-lotto-internal`. Partial rules are the #1 source of
404s on `/actions/*`, `/debug/start-login`, and requests leaking to the default Danske Spil site.

See the authoritative snippet and list of required paths in:
`deploy/k8s/70-ngrok-internal.yaml` (top of the file).

Run `kubectl -n klub-lotto logs -f deploy/klub-lotto -c app --since=30s | grep -E '(novnc:|GET /auth|job|login)'` during tests. Update this checklist in future edits. Cross-ref shared-ngrok-gateway/wiki/runbooks/add-route.md runbook.
