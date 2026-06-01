# Ngrok Setup for klub-lotto (2026-06)

## Two Endpoints

| File                    | Domain / Path                              | Purpose                              | OAuth? | Recommended for daily use? |
|-------------------------|--------------------------------------------|--------------------------------------|--------|----------------------------|
| 70-ngrok-internal.yaml  | unground-.../klub-lotto                    | Old shared path                      | Yes (global) | Only if you need the old URL |
| 75-ngrok-public.yaml    | https://klub-lotto.ngrok.app               | **New dedicated domain** (root)      | No     | **Yes** – clean & stable |

## How to Use the New Dedicated Domain

1. Make sure you have already reserved the domain `klub-lotto.ngrok.app` in your ngrok account.

2. Apply the new manifest (from the klub-lotto repo root):

   ```bash
   kubectl apply -f deploy/k8s/75-ngrok-public.yaml
   ```

3. Check that the AgentEndpoint becomes Ready:

   ```bash
   kubectl -n klub-lotto get agentendpoints
   ```

   You should see `klub-lotto-public` with `READY=True` and the correct URL.

4. Access the app at:

   ```
   https://klub-lotto.ngrok.app
   ```

   Everything (UI, MitID trigger, VNC, debug endpoints, etc.) works at the root of this domain.

## Why This Is Better

- No shared OAuth policy → no mysterious redirects to idp.ngrok.com.
- No path prefix leaking → `/debug/start-login` actually works.
- The JavaScript in the app (the subpath fixer) automatically detects that it is running at root and adjusts all HTMX calls and links correctly.
- The VNC/noVNC configuration also becomes simpler (`path=websockify` instead of `path=klub-lotto/websockify`).

## Keeping the Old Path (Optional)

You can leave `70-ngrok-internal.yaml` applied if you still want the old URL
`https://unground-uncraftily-vivienne.ngrok-free.dev/klub-lotto` to continue working
(for bookmarks, other people, etc.). The two endpoints can coexist.

When you are ready to fully migrate, you can remove the old internal binding or just stop advertising the old URL.

## Verification Commands

```bash
# Should return 200 (or 302 if you still have some auth in front)
curl -I https://klub-lotto.ngrok.app

# These should no longer 404 or leak to the Danske Spil site
curl -I https://klub-lotto.ngrok.app/debug/start-login
curl -I https://klub-lotto.ngrok.app/debug/x11
curl -I https://klub-lotto.ngrok.app/actions/status
```

## Next Steps After Applying

- Update any bookmarks / documentation to the new domain.
- In the klub-lotto UI, the "Open full VNC in new tab" links will automatically use the correct `path=websockify` because of the dynamic prefix detection.
- You can now focus on the MitID flow without gateway routing surprises.

If you later want to move the management of this public endpoint into the shared gateway repo's template system (instead of applying it directly from the klub-lotto repo), let us know and we can add it to the `gateway.template.yaml` + render process.