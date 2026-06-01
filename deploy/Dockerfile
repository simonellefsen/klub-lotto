# klub-lotto — single image with everything the pod needs.
#
#   - Xvfb        virtual framebuffer Chrome renders into
#   - x11vnc      exposes the framebuffer as VNC on :5900
#   - websockify  bridges :6080 HTTP/WS → :5900 VNC, with noVNC static UI
#   - chromium    the actual browser agent-browser drives
#   - agent-browser  Vercel Labs CLI (npm install at build time)
#   - klub-lotto, klub-lotto-web   our two Go binaries
#   - supervisord  starts everything and restarts on crash
#
# Image goal: usable on docker-desktop without further wiring. Not
# optimised for size — favour reliability + observability for now.

# -------- Stage 1: build Go binaries --------
FROM golang:1.23-bookworm AS build
WORKDIR /src
# Copy the manifest first for layer caching; tidy in the build stage so a
# fresh checkout without go.sum still builds. GOFLAGS=-mod=mod also lets
# go build resolve missing entries on the fly.
COPY go.mod go.sum* ./
ENV GOFLAGS=-mod=mod
RUN go mod download || true
COPY . .
RUN go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -o /out/klub-lotto      ./cmd/klub-lotto && \
    CGO_ENABLED=0 go build -trimpath -o /out/klub-lotto-web  ./cmd/klub-lotto-web

# -------- Stage 2: runtime --------
FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive \
    DISPLAY=:99 \
    AGENT_BROWSER_SESSION=klublotto \
    AGENT_BROWSER_SESSION_NAME=klublotto \
    AGENT_BROWSER_HEADED=true \
    AGENT_BROWSER_PROFILE=/var/lib/agent-browser/chrome-profile \
    AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium \
    AGENT_BROWSER_ARGS="--start-maximized --disable-session-crashed-bubble" \
    KLUBLOTTO_HEADED=true

# novnc lives in `contrib`, so enable it. websockify, supervisor,
# chromium etc. are in `main`. We install python3-websockify (the
# implementation behind the `websockify` command) explicitly so the
# noVNC entrypoint always finds the bridge.
RUN sed -i 's| main$| main contrib|' /etc/apt/sources.list.d/debian.sources 2>/dev/null || true && \
    apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg unzip git \
        xvfb x11vnc fluxbox x11-xserver-utils \
        novnc python3-websockify \
        supervisor \
        chromium chromium-driver \
        fonts-liberation libnss3 libatk-bridge2.0-0 libcups2 libxss1 \
        libxkbcommon0 libgtk-3-0 libgbm1 libasound2 \
    && rm -rf /var/lib/apt/lists/*

# Node 20 from NodeSource — Bookworm's default Node 18 is too old for
# recent agent-browser releases.
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install agent-browser globally. AGENT_BROWSER_EXECUTABLE_PATH above
# points it at the chromium we just apt-installed, so the daemon doesn't
# try to download Chrome on first boot (no Internet in the pod by default).
RUN npm install -g agent-browser@latest \
    && ln -sf /usr/bin/chromium /usr/local/bin/google-chrome \
    && agent-browser --version

# Bring in our binaries and the supervisor + entrypoint configs.
COPY --from=build /out/klub-lotto      /usr/local/bin/klub-lotto
COPY --from=build /out/klub-lotto-web  /usr/local/bin/klub-lotto-web
COPY deploy/supervisord.conf           /etc/supervisor/conf.d/klub-lotto.conf
COPY deploy/entrypoint.sh              /usr/local/bin/entrypoint.sh
COPY deploy/desktop-cleanup.sh         /usr/local/bin/desktop-cleanup.sh
COPY deploy/scripts/klub-lotto-quiz-session-test.sh /usr/local/bin/klub-lotto-quiz-session-test
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/desktop-cleanup.sh /usr/local/bin/klub-lotto-quiz-session-test

# Persisted at runtime via PVC mounts in k8s:
#   /var/lib/agent-browser   agent-browser state + persistent Chrome profile
#   /var/lib/klub-lotto/wiki exported markdown
RUN mkdir -p /var/lib/agent-browser/chrome-profile /var/lib/klub-lotto/wiki

EXPOSE 8080 6080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
