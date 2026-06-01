#!/usr/bin/env bash
# scripts/diag.sh — dump cluster state into .klublotto/diag-YYYYmmdd-HHMMSS.txt
# so the assistant can read it from the mounted workspace.
#
# Safe to commit (no secrets). The output file is gitignored.
set -uo pipefail
cd "$(dirname "$0")/.."

OUT_DIR=".klublotto"
mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/diag-$(date -u +%Y%m%d-%H%M%S).txt"

echo "writing $OUT"
{
  echo "=== date ==="
  date -u
  echo
  echo "=== kubectl version ==="
  kubectl version --short 2>&1 || kubectl version 2>&1 | head -5
  echo
  echo "=== current-context ==="
  kubectl config current-context 2>&1
  echo
  echo "=== nodes ==="
  kubectl get nodes -o wide 2>&1
  echo
  echo "=== namespace ==="
  kubectl get ns klub-lotto 2>&1
  echo
  echo "=== get pods/svc/pvc/secrets ==="
  kubectl -n klub-lotto get pods,svc,pvc,secrets -o wide 2>&1
  echo
  echo "=== get cluster (cnpg) ==="
  kubectl -n klub-lotto get cluster 2>&1
  kubectl -n klub-lotto get cluster -o yaml 2>&1 | head -120
  echo
  echo "=== describe deployment ==="
  kubectl -n klub-lotto describe deploy klub-lotto 2>&1
  echo
  echo "=== describe pod(s) ==="
  for p in $(kubectl -n klub-lotto get pods -l app=klub-lotto -o name 2>/dev/null); do
    echo "--- describe $p ---"
    kubectl -n klub-lotto describe "$p" 2>&1
  done
  echo
  echo "=== events (last 50) ==="
  kubectl -n klub-lotto get events --sort-by=.lastTimestamp 2>&1 | tail -50
  echo
  echo "=== logs (current) ==="
  kubectl -n klub-lotto logs deploy/klub-lotto --tail=200 --all-containers=true 2>&1
  echo
  echo "=== logs (previous, if container restarted) ==="
  kubectl -n klub-lotto logs deploy/klub-lotto --previous --tail=100 --all-containers=true 2>&1
  echo
  echo "=== cnpg pods + logs ==="
  kubectl -n klub-lotto get pods -l cnpg.io/cluster=klublotto-db -o wide 2>&1
  for p in $(kubectl -n klub-lotto get pods -l cnpg.io/cluster=klublotto-db -o name 2>/dev/null); do
    echo "--- logs $p (tail=80) ---"
    kubectl -n klub-lotto logs "$p" --tail=80 2>&1
  done
  echo
  echo "=== local image (docker-desktop) ==="
  docker images klub-lotto 2>&1
} >> "$OUT" 2>&1

echo "wrote $OUT"
ls -la "$OUT"
