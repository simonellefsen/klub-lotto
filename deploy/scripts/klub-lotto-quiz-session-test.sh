#!/usr/bin/env bash
set -Eeuo pipefail

SESSION="${AGENT_BROWSER_SESSION:-${AGENT_BROWSER_SESSION_NAME:-klublotto}}"
MODE="${1:-quiz-dry}"

export DISPLAY="${DISPLAY:-:99}"
export KLUBLOTTO_HEADED="${KLUBLOTTO_HEADED:-true}"
export AGENT_BROWSER_SESSION="$SESSION"
export AGENT_BROWSER_SESSION_NAME="$SESSION"
export AGENT_BROWSER_PROFILE="${AGENT_BROWSER_PROFILE:-/var/lib/agent-browser/chrome-profile}"

ab() {
  agent-browser --json --session "$SESSION" --session-name "$SESSION" "$@"
}

echo "session: $SESSION"
echo "display: ${DISPLAY}"
echo "profile: ${AGENT_BROWSER_PROFILE}"
echo
echo "agent-browser session check:"
ab session
echo
echo "current URL before test:"
ab get url || true
echo

case "$MODE" in
  snapshot)
    ab snapshot -i
    ;;
  open-quiz)
    ab open "https://danskespil.dk/klublotto/dagens-quiz"
    ab wait 2500 || true
    echo "current URL after open:"
    ab get url || true
    echo
    ab snapshot -i
    ;;
  quiz-dry)
    klub-lotto quiz --dry-run
    ;;
  quiz-submit|submit)
    klub-lotto quiz --submit
    ;;
  *)
    cat >&2 <<EOF
usage: klub-lotto-quiz-session-test [snapshot|open-quiz|quiz-dry|quiz-submit]

Runs against the active agent-browser session using both:
  --session $SESSION
  --session-name $SESSION

Default: quiz-dry
EOF
    exit 2
    ;;
esac
