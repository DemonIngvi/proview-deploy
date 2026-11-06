#!/usr/bin/env bash
# Failure-injection test on the running compose stack (start it with scripts/e2e.sh).
# 1. ClickHouse outage: events are accepted, buffered in Kafka, delivered exactly once after recovery.
# 2. Ingester crash (SIGKILL) during load: no loss after restart.
set -euo pipefail
cd "$(dirname "$0")/../compose"
E2E_DIR="$(cd ../e2e && pwd)"

run_phase() {
  if command -v go >/dev/null; then
    (cd "$E2E_DIR" && E2E_CHAOS_PHASE=$1 go test -tags e2e -count=1 -run TestChaos -v ./...)
  else
    docker run --rm --network host -e E2E_CHAOS_PHASE=$1 -v "$E2E_DIR":/src -w /src golang:1.26-alpine \
      go test -tags e2e -count=1 -run TestChaos -v ./...
  fi
}

echo "== scenario 1: ClickHouse outage"
run_phase baseline
docker compose stop clickhouse
run_phase send
echo "-- ingester while ClickHouse is down (expect retries, no commits):"
sleep 5; docker compose logs --since 60s event-ingester | grep -c "insert failed, retrying" || true

docker compose start clickhouse
run_phase verify

echo "== scenario 2: ingester killed mid-stream"
run_phase baseline
docker compose kill -s SIGKILL event-ingester
run_phase send
docker compose start event-ingester
run_phase verify
rm -f "$E2E_DIR/chaos-state.json"
echo "chaos: OK"
