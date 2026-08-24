#!/usr/bin/env bash
#
# Runs the concurrency proof end to end:
#   1. provision a fresh event with a fixed seat count
#   2. fire N concurrent booking attempts at it with k6
#   3. ask the database, independently, whether anything was sold twice
#
# The API must already be running with PAYMENT_MODE=always_success, so that a
# simulated decline cannot be mistaken for a lost race.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SEATS="${SEATS:-50}"
ATTEMPTS="${ATTEMPTS:-500}"
BASE_URL="${BASE_URL:-http://localhost:8080}"
SCENARIO="loadtest/scenario.json"

mkdir -p loadtest/results

if ! command -v k6 >/dev/null 2>&1; then
  echo "k6 is not installed. See https://k6.io/docs/get-started/installation/" >&2
  exit 1
fi

echo "Checking the API is reachable at ${BASE_URL} ..."
if ! curl -fsS "${BASE_URL}/health" >/dev/null; then
  echo "The API is not responding. Start it with: make up" >&2
  exit 1
fi

mode=$(curl -fsS "${BASE_URL}/health" >/dev/null && echo ok)
if [ "${mode}" != "ok" ]; then
  echo "The API health check failed." >&2
  exit 1
fi

echo "Provisioning a fresh event with ${SEATS} seats and ${ATTEMPTS} users ..."
(cd backend && go run ./cmd/loadtest -seats "${SEATS}" -users "${ATTEMPTS}") > "${SCENARIO}"

event_id=$(python3 -c "import json;print(json.load(open('${SCENARIO}'))['event_id'])")
echo "  event: ${event_id}"

echo
echo "Firing ${ATTEMPTS} concurrent booking attempts ..."
k6 run \
  -e "SCENARIO=${ROOT}/${SCENARIO}" \
  -e "BASE_URL=${BASE_URL}" \
  -e "ATTEMPTS=${ATTEMPTS}" \
  --quiet \
  loadtest/booking.js

./loadtest/verify.sh
