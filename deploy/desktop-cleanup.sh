#!/bin/sh
# desktop-cleanup.sh
# Keeps the virtual X desktop clean in the klub-lotto pod.
# Runs under supervisord with DISPLAY=:99.

set +e   # We want the loop to keep running even if individual commands fail

echo "[desktop-cleanup] starting desktop hygiene loop (DISPLAY=${DISPLAY:-:99})"

# Give Xvfb/fluxbox a moment on first start
sleep 3

force_clean() {
    xsetroot -solid "#1e1e2e" 2>/dev/null || true
    # Kill the exact annoying fbsetbg xmessage dialog and any similar popups
    pkill -f 'xmessage.*fbsetbg' 2>/dev/null || true
    pkill -f 'xmessage.*wallpaper' 2>/dev/null || true
    # Also nuke any other stray xmessage that might appear
    pkill xmessage 2>/dev/null || true
}

# Initial forced cleanup
force_clean
echo "[desktop-cleanup] initial cleanup done"

# Keep it clean forever
while true; do
    force_clean
    sleep 25
done
