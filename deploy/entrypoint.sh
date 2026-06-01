#!/usr/bin/env bash
# Entrypoint: tiny shim that exec's supervisord. Kept as a separate file
# so a future readiness probe or environment massage has a place to go.
set -euo pipefail

# noVNC ships with a `vnc.html` we want the web UI to point at via
# /vnc/vnc.html. websockify serves /usr/share/novnc at /, but Debian's
# packaging sometimes installs the symlink as `vnc_lite.html`. Make sure
# vnc.html exists for the iframe URL we hardcoded in index.html.
if [[ -f /usr/share/novnc/vnc_lite.html && ! -f /usr/share/novnc/vnc.html ]]; then
  ln -sf /usr/share/novnc/vnc_lite.html /usr/share/novnc/vnc.html
fi

# Clean up fluxbox aggressively so the ugly "fbsetbg: I can't find an app
# to set the wallpaper" xmessage dialog never appears in this minimal Xvfb
# environment (especially when /root is on a PVC and old state lingers).
# We force a dark solid background and kill any stray xmessage dialogs.
mkdir -p /root/.fluxbox
mkdir -p "${AGENT_BROWSER_PROFILE:-/var/lib/agent-browser/chrome-profile}"
rm -f "${AGENT_BROWSER_PROFILE:-/var/lib/agent-browser/chrome-profile}"/SingletonCookie \
      "${AGENT_BROWSER_PROFILE:-/var/lib/agent-browser/chrome-profile}"/SingletonLock \
      "${AGENT_BROWSER_PROFILE:-/var/lib/agent-browser/chrome-profile}"/SingletonSocket \
      2>/dev/null || true

# Nuclear cleanup of any previous fluxbox state that could trigger fbsetbg
rm -rf /root/.fluxbox/* 2>/dev/null || true
rm -f /root/.fluxbox/.* 2>/dev/null || true

cat > /root/.fluxbox/init << 'EOF'
session.screen0.rootCommand: sh -c 'xsetroot -solid "#1e1e2e" 2>/dev/null; xset s off -dpms 2>/dev/null || true'
session.screen0.toolbar.visible: false
session.screen0.toolbar.widthPercent: 100
EOF

# Early attempt to set background (Xvfb may not be fully up yet, that's ok)
xsetroot -solid "#1e1e2e" 2>/dev/null || true

# Kill any xmessage dialogs that might have popped from previous fluxbox runs
pkill -f xmessage 2>/dev/null || true

echo "fluxbox init written + aggressive cleanup (dark #1e1e2e bg, toolbar hidden, xmessage killed)"
echo "Current fluxbox init:"
cat /root/.fluxbox/init || true

# -n forces nodaemon so the container's PID 1 stays alive. -c points at
# Debian's main config which auto-includes /etc/supervisor/conf.d/*.conf
# (our klub-lotto.conf).
exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
