#!/usr/bin/env bash
# Builds the stack from sibling repositories, starts it and runs end-to-end tests.
#   scripts/e2e.sh            build + up + test (stack left running)
#   KEEP=0 scripts/e2e.sh     tear down afterwards
set -euo pipefail
cd "$(dirname "$0")/../compose"
[ -f .env ] || cp .env.example .env
docker compose up -d --build
docker compose ps --format 'table {{.Service}}\t{{.State}}\t{{.Status}}'
cd ../e2e
if command -v go >/dev/null; then
  go test -tags e2e -count=1 -v ./...
else
  docker run --rm --network host -v "$PWD":/src -w /src golang:1.26-alpine go test -tags e2e -count=1 -v ./...
fi
status=$?
if [ "${KEEP:-1}" = "0" ]; then (cd ../compose && docker compose down -v); fi
exit $status
