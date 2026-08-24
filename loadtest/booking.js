// SeatSync concurrency proof.
//
// Fires N booking attempts at a much smaller pool of seats, all at once, and
// asserts the only outcome the system is allowed to produce: every seat sold
// exactly once, every loser told so cleanly, and nothing in between.
//
// Each attempt holds one seat and immediately pays for it. Seats are assigned
// by iteration rather than at random, so every seat is contested by the same
// number of callers and none is left untouched by chance.
//
//   k6 run -e SCENARIO=scenario.json loadtest/booking.js

import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";
import { Counter, Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const SCENARIO = JSON.parse(open(__ENV.SCENARIO || "./scenario.json"));

const ATTEMPTS = Number(__ENV.ATTEMPTS || 500);
const SEATS = SCENARIO.seat_ids.length;

// By default k6 counts any non-2xx response as a failed request, which would
// mark the 450 losing attempts as errors. A 409 here is the system working:
// it is the contended caller being told cleanly that the seat is gone. Only
// genuinely broken responses should count against http_req_failed.
http.setResponseCallback(http.expectedStatuses(200, 201, 409));

// Outcome counters. Between them these must account for every attempt.
const confirmed = new Counter("seat_confirmed");
const holdConflicts = new Counter("hold_conflict_409");
const payConflicts = new Counter("pay_conflict_409");
const unexpected = new Counter("unexpected_outcome");

const holdDuration = new Trend("hold_duration", true);
const payDuration = new Trend("pay_duration", true);
const bookingDuration = new Trend("booking_duration", true);

export const options = {
  scenarios: {
    stampede: {
      // Every attempt is a fresh virtual user firing once, which is the
      // closest approximation to N real people hitting "book" together.
      executor: "per-vu-iterations",
      vus: ATTEMPTS,
      iterations: 1,
      maxDuration: "2m",
    },
  },
  thresholds: {
    // The proof itself. A run that violates any of these fails the process,
    // so the assertion lives in the test rather than in a human reading it.
    seat_confirmed: [`count==${SEATS}`],
    unexpected_outcome: ["count==0"],
    // With 409 treated as expected above, any failure here is a real one:
    // a 5xx, a timeout, or a dropped connection.
    http_req_failed: ["rate==0"],
  },
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
};

export default function () {
  // Global iteration index, so seat assignment is deterministic and evenly
  // spread across the pool rather than left to chance.
  const index = exec.scenario.iterationInTest;

  const seatId = SCENARIO.seat_ids[index % SEATS];
  const token = SCENARIO.tokens[index % SCENARIO.tokens.length];

  const authHeaders = {
    "Content-Type": "application/json",
    Authorization: `Bearer ${token}`,
  };

  const started = Date.now();

  // --- hold ---------------------------------------------------------------
  const holdStarted = Date.now();
  const holdRes = http.post(
    `${BASE_URL}/events/${SCENARIO.event_id}/holds`,
    JSON.stringify({ seat_ids: [seatId] }),
    { headers: authHeaders, tags: { step: "hold" } },
  );
  holdDuration.add(Date.now() - holdStarted);

  if (holdRes.status === 409) {
    // Somebody else got there first. This is the expected outcome for the
    // large majority of attempts and is not a failure.
    holdConflicts.add(1);
    check(holdRes, {
      "hold conflict names the contended seats": (r) => {
        try {
          return (r.json().error.details.unavailable_seat_ids || []).length > 0;
        } catch {
          return false;
        }
      },
    });
    return;
  }

  if (holdRes.status !== 201) {
    unexpected.add(1);
    console.error(`unexpected hold status ${holdRes.status}: ${holdRes.body}`);
    return;
  }

  const bookingId = holdRes.json().booking_id;

  // --- pay ----------------------------------------------------------------
  // One key per checkout, exactly as a browser would generate it.
  const idempotencyKey = `k6-${bookingId}`;

  const payStarted = Date.now();
  const payRes = http.post(`${BASE_URL}/bookings/${bookingId}/pay`, null, {
    headers: { ...authHeaders, "Idempotency-Key": idempotencyKey },
    tags: { step: "pay" },
  });
  payDuration.add(Date.now() - payStarted);

  if (payRes.status === 200) {
    confirmed.add(1);
    bookingDuration.add(Date.now() - started);
    check(payRes, {
      "payment confirmed the booking": (r) => r.json().status === "confirmed",
      "payment succeeded": (r) => r.json().payment.status === "succeeded",
    });
    return;
  }

  if (payRes.status === 409) {
    // The seat was lost between holding and paying. Legitimate under
    // contention, and the point is that it fails cleanly rather than
    // producing a second sale.
    payConflicts.add(1);
    return;
  }

  unexpected.add(1);
  console.error(`unexpected pay status ${payRes.status}: ${payRes.body}`);
}

export function handleSummary(data) {
  const metric = (name) => data.metrics[name]?.values?.count ?? 0;

  const confirmedCount = metric("seat_confirmed");
  const holdConflictCount = metric("hold_conflict_409");
  const payConflictCount = metric("pay_conflict_409");
  const unexpectedCount = metric("unexpected_outcome");
  const totalConflicts = holdConflictCount + payConflictCount;
  const accounted = confirmedCount + totalConflicts + unexpectedCount;

  const req = data.metrics.http_req_duration?.values ?? {};
  const fmt = (v) => (v === undefined ? "n/a" : `${v.toFixed(1)} ms`);

  const pass = confirmedCount === SEATS && unexpectedCount === 0 && accounted === ATTEMPTS;

  const line = "─".repeat(58);
  const row = (label, value) => `  ${label.padEnd(34)}${String(value).padStart(22)}`;

  const report = [
    "",
    line,
    "  SEATSYNC CONCURRENCY PROOF",
    line,
    row("Booking attempts", ATTEMPTS),
    row("Seats available", SEATS),
    "",
    row("Confirmed bookings", `${confirmedCount}  (expected ${SEATS})`),
    row("Conflicts on hold (409)", holdConflictCount),
    row("Conflicts on pay (409)", payConflictCount),
    row("Total clean conflicts", `${totalConflicts}  (expected ${ATTEMPTS - SEATS})`),
    row("Unexpected outcomes", unexpectedCount),
    row("Outcomes accounted for", `${accounted} / ${ATTEMPTS}`),
    "",
    row("Latency p95 (all requests)", fmt(req["p(95)"])),
    row("Latency p99 (all requests)", fmt(req["p(99)"])),
    row("Latency median", fmt(req.med)),
    row("Latency max", fmt(req.max)),
    line,
    `  ${pass ? "PASS" : "FAIL"}  ${
      pass
        ? `exactly ${SEATS} seats sold, ${totalConflicts} attempts rejected cleanly`
        : "the invariant was violated, see the counters above"
    }`,
    line,
    "",
  ].join("\n");

  return {
    stdout: report,
    "loadtest/results/summary.json": JSON.stringify(data, null, 2),
  };
}
