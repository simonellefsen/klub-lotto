# Gateway Requirements for klub-lotto

This file captures the **exact** Traffic Policy requirements on the shared ngrok gateway
so that the `/klub-lotto` app works reliably (including all the debug and action endpoints
added for MitID login).

## The Problem We Keep Hitting

When the gateway rule for `/klub-lotto` is incomplete or ordered after a default rule,
requests leak to whatever the "default" upstream on that ngrok hostname serves (usually
the main Danske Spil site). Symptoms:
- `/klub-lotto/debug/start-login` → shows Danske Spil instead of starting the job
- HTMX calls to `/actions/*`, `/auth`, `/ledger` return 404 from the gateway
- Only the very first page load sometimes works

## Required Rule (sibling repo: shared-ngrok-gateway)

Put something like the following in the gateway's TrafficPolicy (or equivalent NgrokTrafficPolicy
resource) **before** any broad/default rules.

```yaml
# Example TrafficPolicy fragment (adapt names/namespaces as needed)
apiVersion: ngrok.k8s.ngrok.com/v1alpha1
kind: NgrokTrafficPolicy
metadata:
  name: klub-lotto-route
  namespace: <gateway-namespace>
spec:
  policy:
    - match:
        path: /klub-lotto*
      actions:
        # Strip the /klub-lotto prefix so the app sees clean paths at root
        - type: url-rewrite
          config:
            match: ^/klub-lotto(/.*)?$
            replace: '${1:/}'
        - type: forward-internal
          config:
            url: http://klub-lotto.internal:80
```

### Important ordering notes
- This rule must win for everything under `/klub-lotto`.
- If you have an OAuth / identity provider step, either bypass it for `/klub-lotto*` or put the rewrite+forward before the identity action.
- Test with the full set of paths listed below after every change.

## Paths That Must Reach the Pod

These are the ones the UI and MitID flow actually use (as of 2026-05-31):

- `GET  /klub-lotto`
- `GET  /klub-lotto/ledger`
- `GET  /klub-lotto/ledger/{id}`
- `GET  /klub-lotto/auth` (and `?force=1`)
- `POST /klub-lotto/actions/login`
- `POST /klub-lotto/actions/run/{game}`
- `GET  /klub-lotto/actions/status`
- `GET  /klub-lotto/debug/start-login`
- `GET  /klub-lotto/debug/start-test-window`
- `POST /klub-lotto/debug/test-window`
- `GET  /klub-lotto/debug/x11`
- `GET  /klub-lotto/vnc/*`
- `GET  /klub-lotto/websockify` (and the WebSocket upgrade)
- All noVNC static assets: `/core/`, `/app/`, `/vendor/`, `/include/`, `/sounds/`
- `GET  /klub-lotto/healthz` (for gateway health checks)

## Local Reference

The AgentEndpoint side lives here:
- `deploy/k8s/70-ngrok-internal.yaml`

The top of that file contains the same recommended policy snippet and the full list of paths.

## Verification After Changing the Gateway

From your machine (with a valid session cookie if OAuth is in front):

```bash
curl -I https://unground-uncraftily-vivienne.ngrok-free.dev/klub-lotto/debug/start-login
curl -I https://unground-uncraftily-vivienne.ngrok-free.dev/klub-lotto/actions/status
curl -I https://unground-uncraftily-vivienne.ngrok-free.dev/klub-lotto/debug/x11
```

All three should return something from the klub-lotto pod (usually 200 or 303), **not** a 404 from the gateway or content from the Danske Spil site.

When the routing is correct, clicking the orange "Start MitID (direct link)" button on the live page will reliably start a job visible in `kubectl logs`.

## History

This file was created because repeated partial gateway rules caused the MitID "Trigger" flow (both HTMX and direct links) to stop working even though the initial `/klub-lotto` page loaded.