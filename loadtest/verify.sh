#!/usr/bin/env bash
#
# The independent check. k6 reports what the API told it; this asks the
# database directly whether any seat was ever sold twice.
#
# It is deliberately separate from the load test: a bug that made the API
# report success wrongly would still be caught here.

set -euo pipefail

PSQL_CONTAINER="${PSQL_CONTAINER:-seatsync-postgres}"
PGUSER="${POSTGRES_USER:-seatsync}"
PGDATABASE="${POSTGRES_DB:-seatsync}"

run_sql() {
  if command -v psql >/dev/null 2>&1 && [ -n "${DATABASE_URL:-}" ]; then
    psql "$DATABASE_URL" -qtA -c "$1"
  else
    docker exec -i "$PSQL_CONTAINER" psql -U "$PGUSER" -d "$PGDATABASE" -qtA -c "$1"
  fi
}

echo
echo "──────────────────────────────────────────────────────────"
echo "  DATABASE VERIFICATION"
echo "──────────────────────────────────────────────────────────"

# The check from the brief: no (event, seat) pair may be confirmed twice.
duplicates=$(run_sql "
  SELECT count(*) FROM (
    SELECT event_id, seat_id
    FROM booking_seats
    WHERE confirmed
    GROUP BY event_id, seat_id
    HAVING count(*) > 1
  ) AS d;")

echo "  Duplicate confirmed (event, seat) pairs : ${duplicates}"

# A booking must never be confirmed without a successful payment, nor the
# reverse. Either would mean the confirm transaction was not atomic.
orphan_bookings=$(run_sql "
  SELECT count(*) FROM bookings b
  WHERE b.status = 'confirmed'
    AND NOT EXISTS (
      SELECT 1 FROM payments p WHERE p.booking_id = b.id AND p.status = 'succeeded');")

orphan_payments=$(run_sql "
  SELECT count(*) FROM payments p
  WHERE p.status = 'succeeded'
    AND NOT EXISTS (
      SELECT 1 FROM bookings b WHERE b.id = p.booking_id AND b.status = 'confirmed');")

echo "  Confirmed bookings without a payment    : ${orphan_bookings}"
echo "  Successful payments without a booking   : ${orphan_payments}"

# A seat flagged confirmed whose booking is not confirmed would mean the
# denormalised flag had drifted from the booking it belongs to.
flag_drift=$(run_sql "
  SELECT count(*) FROM booking_seats bs
  JOIN bookings b ON b.id = bs.booking_id
  WHERE bs.confirmed AND b.status <> 'confirmed';")

echo "  Seats flagged sold on an unsold booking : ${flag_drift}"

# Double charging.
double_charged=$(run_sql "
  SELECT count(*) FROM (
    SELECT booking_id FROM payments
    WHERE status = 'succeeded'
    GROUP BY booking_id HAVING count(*) > 1
  ) AS d;")

echo "  Bookings charged more than once         : ${double_charged}"
echo "──────────────────────────────────────────────────────────"

total=$((duplicates + orphan_bookings + orphan_payments + flag_drift + double_charged))

if [ "$total" -eq 0 ]; then
  echo "  PASS  every invariant holds"
  echo "──────────────────────────────────────────────────────────"
  echo
  exit 0
fi

echo "  FAIL  ${total} invariant violation(s) found"
echo "──────────────────────────────────────────────────────────"
echo
exit 1
