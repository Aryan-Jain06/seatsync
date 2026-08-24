# SeatSync, explained

This document exists so that the owner of this project can explain every part
of it without hedging. It walks through what each component does, why it was
built that way, what the alternatives were, and what breaks if you change it.

It assumes no prior knowledge of the codebase.

---

## Contents

1. [The problem in one page](#1-the-problem-in-one-page)
2. [How a booking actually happens](#2-how-a-booking-actually-happens)
3. [The components, one by one](#3-the-components-one-by-one)
4. [The design decisions, and what was rejected](#4-the-design-decisions-and-what-was-rejected)
5. [The hard questions](#5-the-hard-questions)
6. [What I would do next](#6-what-i-would-do-next)

---

## 1. The problem in one page

Selling tickets looks like a CRUD app until two people want the same seat.

The naive version:

```sql
SELECT status FROM seats WHERE id = 'A-12';   -- 'available'
-- ... application decides it is free ...
UPDATE seats SET status = 'sold' WHERE id = 'A-12';
```

Two requests can run that `SELECT` at the same moment, both see `available`,
and both proceed. The seat is sold twice. This is a **race condition**, and the
window is microseconds wide — which is exactly why it survives testing and
appears on launch day.

It gets worse than a database bug, because a ticket sale has a *middle*. The
user needs time to enter payment details. So the seat must be reserved before
payment and released if the user wanders off. Now you need:

- Reservations that expire on their own, because users close tabs.
- Payment that can be retried without charging twice, because networks fail.
- An answer for what happens when your infrastructure fails *during* all this.

SeatSync's position is that you cannot solve this with one mechanism. It uses
two, deliberately overlapping:

| | Job | Failure mode |
|---|---|---|
| **Redis lock** | Stop 500 people from contending | Fails *loudly* — someone gets a 409 they shouldn't |
| **Postgres index** | Make a double sale impossible | Cannot fail without the database being broken |

Redis is fast but not authoritative. Postgres is authoritative but shouldn't
be asked 500 times a second for something Redis can answer. Together, the fast
path is fast and the correctness is absolute.

---

## 2. How a booking actually happens

Follow one user through the whole flow.

### Step 1 — They open the seat map

`GET /events/{id}/seatmap`

The server asks two questions and merges the answers:

- **Postgres:** which seats are confirmed sold? (`booking_seats WHERE confirmed`)
- **Redis:** which seats are currently held? (a sorted set, scored by expiry)

The merge order matters and is not arbitrary: **confirmed always wins over
held.** A stale hold entry could linger in Redis for a seat that has since
sold. If "held" won, a sold seat would render as merely reserved and the user
would try to book it. There is a test named exactly this
(`TestConfirmedBeatsAStaleHold`).

Held seats are also cleaned lazily on every read:

```
ZREMRANGEBYSCORE event:{id}:reserved -inf {now}
```

Expired entries are dropped whenever anyone looks. No cleanup job is required
for correctness — a reader fixes the set as a side effect of reading it.

### Step 2 — They select seats and click Hold

`POST /events/{id}/holds` with up to six seat IDs.

This is the sharp end. The requirement is **all or nothing**: if a user picks
four seats and one is taken, they get none, and the three they could have had
must not be left locked.

Doing this with individual `SET NX` calls is broken. Lock three, fail on the
fourth, and now you must release three — but if the process dies between the
failure and the release, three seats are stranded for five minutes.

So acquisition is a single Lua script, which Redis runs atomically:

```lua
-- Pass 1: check every seat. Touch nothing.
for i, key in ipairs(KEYS) do
    if redis.call('GET', key) then
        table.insert(unavailable, i)
    end
end

-- Only if all are free does anything get written.
if #unavailable > 0 then
    return { 0, unavailable }
end

-- Pass 2: take them all.
for _, key in ipairs(KEYS) do
    redis.call('SET', key, value, 'NX', 'EX', ttl)
end
```

Two passes inside one script. Nothing partial can be observed, and nothing
partial can be left behind, because Redis executes the script to completion
without interleaving another command.

On success the server writes a `pending` booking to Postgres and adds the
seats to the visibility sorted set. The response carries `expires_at`, and the
browser counts down from it.

The lock's *value* matters as much as its existence:

```
lock:{eventId}:{seatId} = "{userId}:{lockToken}"
```

The `lockToken` is random per booking. Its purpose appears in the next step.

### Step 3 — They pay

`POST /bookings/{id}/pay` with an `Idempotency-Key` header.

The order of operations here is the most carefully considered part of the
codebase:

```
1. Load the booking, SELECT ... FOR UPDATE      <- serialises concurrent attempts
2. Claim the idempotency key, or replay         <- one key per booking, forever
3. Take a Redis mutex on the booking            <- one caller reaches the provider
4. Verify the seat locks are still ours         <- fail before charging, not after
5. Call the payment provider                    <- the slow part, 1-2 seconds
6. ONE Postgres transaction:
       verify locks again
       flag booking_seats confirmed             <- the unique index adjudicates
       set booking confirmed
       insert the payment row
7. After commit: delete locks, remove from the visibility set
8. Broadcast "confirmed" to WebSocket subscribers
```

Three details are worth dwelling on.

**Locks are verified twice** — before charging and again inside the
transaction. The first check avoids taking money for a seat that already
lapsed. The second closes the window between charging and committing. Neither
alone is enough.

**Redis cleanup happens after the commit, never inside the transaction.** A
Postgres transaction can roll back; a Redis `DEL` cannot. Delete the lock
inside the transaction, have the transaction fail, and the seat is now unlocked
but not sold — free for someone else while this user's client still believes
they own it. Doing it after means the worst case is a lock that lingers until
its TTL, which is harmless.

**The unique index is the actual arbiter.** Step 6 does not ask "is this seat
free?" It attempts the write and lets Postgres refuse it. Asking would
reintroduce the check-then-act race the whole design exists to eliminate.

### Step 4 — Or they don't pay

Two mechanisms, and only one of them is load-bearing.

**Redis expires the lock.** The seat is bookable again the instant the TTL
lapses. Nothing runs. Nothing is scheduled. This is the mechanism that matters.

**The worker reconciles Postgres.** Every two seconds it finds `pending`
bookings past their expiry, marks them `expired`, and broadcasts. This is
bookkeeping so users see accurate history and connected clients update
promptly. If the worker were dead, seats would still free correctly — you would
just have stale-looking rows and slower UI updates.

That asymmetry is deliberate. Anything whose failure silently corrupts state
belongs in Redis TTLs or Postgres constraints, not in a goroutine.

---

## 3. The components, one by one

### PostgreSQL — the record of truth

Eight tables. Seven from the brief, plus `refresh_tokens`.

The one that carries the guarantee:

```sql
CREATE TABLE booking_seats (
    booking_id UUID NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
    seat_id    UUID NOT NULL REFERENCES seats (id),
    event_id   UUID NOT NULL REFERENCES events (id),
    confirmed  BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (booking_id, seat_id)
);

CREATE UNIQUE INDEX uq_booking_seats_confirmed_seat
    ON booking_seats (event_id, seat_id)
    WHERE confirmed;
```

`confirmed` is denormalised from `bookings.status`. That is a real cost —
two places now describe one fact, and they must be kept in step, which is why
they are only ever written together in one transaction.

It buys something not otherwise obtainable: a **partial** unique index. Postgres
can only index columns of the table it is defined on, so a constraint of the
form "unique across `(event_id, seat_id)` but only when the *parent booking* is
confirmed" is impossible without the flag living locally.

Why partial rather than plain? A plain unique index on `(event_id, seat_id)`
would forbid two people from *holding* the same seat. That sounds desirable
but destroys the design: pending rows are how a race becomes visible, and a
user whose hold lapsed could never be replaced by the next buyer, because their
dead row would still occupy the index.

**Money is `BIGINT` in minor units.** Never floats. `0.1 + 0.2 != 0.3` in
binary floating point, and a ticketing system that drifts a paisa per booking
will not reconcile. `NUMERIC` would also be exact, but forces every Go
arithmetic path through a decimal library; integers are exact and native.

Rounding is explicit — half up, not truncation:

```go
return (basePrice*s.PriceMultiplierBP + 50) / 100
```

Truncating would quietly under-charge on every seat with an odd multiplier.

### Redis — the transient claim

Three things live here, and they are unrelated to each other:

| Key | Purpose |
|---|---|
| `lock:{event}:{seat}` | The hold. `SET NX EX 300`. Value proves ownership. |
| `event:{id}:reserved` | Sorted set for the seat map, scored by expiry |
| `ratelimit:*` | Token buckets |

The locks are the point. `SET key value NX EX 300` is a single atomic operation
meaning "create this only if absent, and delete it automatically in 300
seconds." Both halves matter: `NX` is the mutual exclusion, `EX` is what makes
an abandoned checkout self-healing.

Three Lua scripts, each existing because the operation cannot be expressed
safely as a sequence of commands:

**`acquire.lua`** — all-or-nothing multi-seat locking, described above.

**`release.lua`** — compare-and-delete:

```lua
if redis.call('GET', KEYS[i]) == ARGV[1] then
    redis.call('DEL', KEYS[i])
end
```

The check and the delete must be atomic. Without the script:

```
Alice: GET lock:A-12  -> "alice:token1"   (mine, I'll release it)
                          ... Alice's hold expires here ...
                          ... Bob acquires the same seat ...
Alice: DEL lock:A-12                       <- Alice just deleted BOB's lock
```

Alice would release a lock she no longer owned, and Bob's seat would silently
become available while Bob was paying for it. This is the classic distributed
lock bug, and the value comparison is the standard fix. There is a test for
precisely this (`TestReleaseIgnoresLocksOwnedBySomeoneElse`).

**`extend.lua`** — same compare-and-set shape, for renewing a hold.

### The Go API

`chi` for routing. Standard `net/http` handler signatures, so nothing is
framework-specific and any middleware from the ecosystem drops in.

Layering is strict, one direction only:

```
handlers/   HTTP in, HTTP out. No business logic.
services/   Business logic. Knows nothing about HTTP.
repos/      SQL. Knows nothing about business rules.
```

The point is testability. `services` tests run without a server; `repos` tests
run against a real database with no HTTP in sight. A handler that computed
prices would be untestable without spinning up a router.

**Errors** are typed and carry their own HTTP status:

```go
return httpx.Conflict(httpx.CodeSeatsTaken, "Some of those seats are already held.").
    WithDetails(map[string]any{"unavailable_seat_ids": ids})
```

One place renders them. Internal errors are logged with their cause and
returned to the client as a generic message, so a SQL error never reaches a
browser.

**No panics in request paths.** There is a recovery middleware, but it is a
backstop, not a strategy — it logs, returns 500, and keeps the server alive.

**Graceful shutdown** on `SIGINT`/`SIGTERM`: stop accepting connections, let
in-flight requests finish within 15 seconds, stop the worker, drain the
WebSocket hub, close the pools. A request that is mid-payment is not severed
by a deploy.

### The WebSocket hub

One hub, one room per event, one goroutine per client.

The quality bar here is about what happens when a client is *slow* — a laptop
that slept, a phone on a dying connection. The naive hub does:

```go
for _, client := range clients {
    client.send <- message      // blocks if the client is not reading
}
```

One stalled client blocks the broadcast loop, and every other subscriber stops
receiving updates. The whole event goes silent because one person's WiFi is bad.

Instead, each client has a buffered channel and a non-blocking send:

```go
select {
case client.send <- message:
default:
    // The buffer is full: this client cannot keep up.
    // Drop it rather than let it stall everyone else.
    h.evict(client)
}
```

Dropping the slow client is the correct trade. Seat updates are only useful
fresh, the client reconnects automatically, and on reconnect it refetches the
seat map — so it recovers to a correct state rather than replaying a backlog.

Goroutine leaks are tested explicitly: twenty connect/disconnect cycles must
end with the same goroutine count they started with.

### The frontend

Next.js App Router, TypeScript, Tailwind.

**Access tokens live in memory only** — a React ref, never `localStorage`.
A token in `localStorage` is readable by any script that gets injected into the
page; one in a closure dies with the tab. The refresh token is a `httpOnly`
cookie the JavaScript cannot read at all.

The cost is that a page refresh loses the access token, so the app calls
`/auth/refresh` on mount to get a new one. That is a real trade — an extra
round trip on every load in exchange for a token that XSS cannot steal.

**One refresh at a time.** If three requests 401 simultaneously, they must not
each refresh:

```ts
if (!this.refreshPromise) {
    this.refreshPromise = this.doRefresh().finally(() => {
        this.refreshPromise = null;
    });
}
return this.refreshPromise;    // everyone awaits the same one
```

Without this, concurrent refreshes race and rotation invalidates the winner's
token, logging the user out at random.

**The idempotency key is generated once per checkout**, stored in a ref, and
reused for every retry. Generating a new one on retry would defeat the entire
mechanism — that is the single most important line in the checkout component,
and it is commented as such.

---

## 4. The design decisions, and what was rejected

### Redis locks over `SELECT ... FOR UPDATE`

`SELECT FOR UPDATE` is the textbook answer and it is genuinely correct. It was
rejected for reasons of duration, not correctness.

A row lock lives inside a transaction. A hold lasts five minutes. So you would
hold an open Postgres transaction for five minutes per user — 500 concurrent
checkouts means 500 open transactions, each pinning a connection. Postgres
defaults to 100 connections. You would exhaust them at around 20% load.

Worse, long transactions block `VACUUM` from reclaiming dead tuples, so table
bloat grows for as long as the longest open transaction. A five-minute
transaction is a self-inflicted operational wound.

Redis holds the reservation *outside* any transaction. Postgres transactions
in this system last milliseconds.

Where `FOR UPDATE` *is* used: locking the booking row during payment, which
takes milliseconds and genuinely needs serialising.

### Optimistic locking was rejected

A `version` column with `WHERE version = ?` works well for low contention. Here,
contention *is* the workload: 500 people, 50 seats. Optimistic locking would
mean ~450 failed transactions, each having done real work before losing.
Pessimistic locking in Redis rejects them in a millisecond, before Postgres is
involved.

Rule of thumb: optimistic when conflicts are rare, pessimistic when they are
the norm.

### A queue was rejected

Kafka or RabbitMQ in front of bookings would serialise everything and remove
races by construction. It also makes booking asynchronous — the user clicks
Pay and waits for a push telling them whether they got the seat. For 50 seats
that is a worse product, and it adds a broker to operate. Queues earn their
place at a scale this does not reach.

### Denormalising `confirmed` was accepted, reluctantly

It duplicates a fact. It is the only way to express the constraint as a
database-level guarantee rather than application discipline. The duplication is
confined to a single transaction in one repository method, and there is a test
asserting the two never diverge.

### Fail-open rate limiting

If Redis is unreachable, the limiter allows the request. Rate limiting protects
against abuse; refusing all traffic because the limiter is down would convert a
cache outage into a total outage. The service logs loudly when degraded.

For a *payment* mutex the same reasoning inverts — there, failing open risks
double charges, so a Redis failure fails the request instead.

### Rate limiting keyed by identity, not connection

Authenticated callers are metered by user id; anonymous ones by address.
Address-only keying punishes offices behind one NAT and is trivially evaded by
rotating IPs. It also has a practical consequence: the load test runs with rate
limiting **enabled**, because its 500 distinct users each stay well inside the
per-user budget. If limiting were per-IP, the proof would have required turning
protection off — which would have proved less.

---

## 5. The hard questions

### 1. What if Redis dies mid-checkout?

**Nothing is double-sold.** That is guaranteed by Postgres, not Redis.

What degrades: all held seats appear available (the sorted set is gone), so
several users can select the same seat and reach payment. The first to confirm
wins; the rest get a clean 409 at the unique index, having been charged
nothing — the mock provider is called before the transaction, but the
transaction failing means no `confirmed` row and no successful payment row.

Against a *real* gateway there is a genuine exposure: money captured for a
booking that then fails to confirm. The correct production answer is
authorise-then-capture — authorise before the transaction, capture only after
it commits, void on failure. That is noted in the code as the boundary where
the mock diverges from reality.

If Redis is down at hold time, holds fail outright and no bookings are created.
Refusing to sell is the right failure: correctness never depends on Redis being
up.

### 2. Why not just `SELECT FOR UPDATE`?

Covered above. Short version: it is correct but requires holding a database
transaction open for the entire five-minute human-speed checkout. That
exhausts the connection pool at a fraction of target load and blocks `VACUUM`.
Redis moves the long-lived reservation out of the database. Postgres
transactions here last milliseconds.

### 3. How do you prevent lock release races?

Every lock's value is `{userId}:{lockToken}`, and release is a Lua script that
deletes **only** if the value matches:

```lua
if redis.call('GET', KEYS[i]) == ARGV[1] then redis.call('DEL', KEYS[i]) end
```

Without atomic compare-and-delete, this happens: Alice reads the lock and
confirms it is hers; her hold expires; Bob acquires the seat; Alice's `DEL`
lands and destroys Bob's lock. Bob is paying for a seat that is now marked
free.

The `lockToken` is per-booking, not per-user, so a user cannot release their
*own* newer hold with an older booking's credentials either.

### 4. Two payments arrive for the same booking simultaneously. What happens?

Four independent defences, in order:

1. `SELECT ... FOR UPDATE` on the booking serialises them at the database.
2. A Redis mutex on the booking means only one reaches the provider. This is
   the one that prevents a double *charge* — the others prevent a double
   *confirmation*, which is too late if money already moved.
3. The second attempt finds the booking `confirmed` and replays the stored
   result.
4. `uq_payments_one_success_per_booking` refuses a second successful payment
   row regardless.

Any one of these alone would prevent double-selling. All four exist because
they fail differently.

### 5. What if the app crashes between charging and committing?

The money moved; the booking is still `pending`. This is the genuine gap, and
it is inherent to any system that talks to an external service inside a
business transaction — you cannot make a network call and a database commit
atomic.

What the design does: the locks survive (Redis TTL), so nobody else takes the
seats in the meantime. The user retries with the same idempotency key. The
attempt is recorded before the provider is called, so there is a durable trace
of an in-flight charge to reconcile against.

The production answer is a reconciliation job comparing provider records
against local payments, plus authorise/capture so an uncaptured authorisation
expires by itself.

### 6. Why not clean up expired holds with Redis keyspace notifications?

Keyspace notifications are fire-and-forget. If no subscriber is connected when
a key expires, the event is gone — there is no redelivery. A dropped
notification would leave a booking `pending` forever.

The 2-second poll is boring and cannot miss anything: it queries state, not
events, so a restarted worker catches up on everything that lapsed while it was
down. It is also indexed, so the sweep is cheap:

```sql
CREATE INDEX ix_bookings_pending_expiry ON bookings (hold_expires_at)
    WHERE status = 'pending';
```

And critically, the worker is not load-bearing. Redis TTLs free seats. The
worker only updates bookkeeping.

### 7. Isn't polling every 2 seconds wasteful?

It is one indexed query returning almost always zero rows — sub-millisecond.
At 43,200 queries a day, the cost is noise next to the cost of a missed
expiry.

If it ever mattered, the fix is a longer interval (the seat is *already* free
via TTL; only the bookkeeping lags) or `LISTEN/NOTIFY`. Neither is needed at
any plausible scale for this workload.

### 8. How do you know it actually works?

Two independent checks that could disagree.

The load test fires 500 concurrent attempts at 50 seats and asserts exactly 50
confirmations, 450 clean 409s, and zero unexpected outcomes. Seats are assigned
by iteration, not randomly, so every seat is contested by exactly ten callers —
random assignment would leave some seats untouched by chance and weaken the
test.

Then SQL asks the database directly, which is the check that matters, because
it would catch a bug where the API *reported* success incorrectly:

```sql
SELECT event_id, seat_id, COUNT(*) FROM booking_seats
WHERE confirmed GROUP BY 1,2 HAVING COUNT(*) > 1;
```

Zero rows. Plus four more invariants: no confirmed booking without a payment,
no successful payment without a booking, no seat flagged sold on an unsold
booking, no booking charged twice.

At 1000 attempts on the same 50 seats, unchanged.

### 9. Where does this break at 10× the load?

Honestly assessed, in this order:

**Postgres write throughput first.** Every confirmation is a transaction with
several writes. A single primary handles a few thousand a second; beyond that
you need partitioning by event, or a write-behind queue.

**Connection pool next.** 25 connections per instance; more instances multiply
that against Postgres's limit. PgBouncer in transaction mode is the standard
fix and would come before anything else.

**The WebSocket hub is memory-bound**, roughly 10k connections per instance.
It is already per-event, so sharding by event across instances is natural —
but it broadcasts in-process, so multiple instances need Redis pub/sub to reach
subscribers on other nodes. That is the first thing to change when scaling out.

**Redis last.** Single-node Redis handles ~100k ops/sec; seat locking is a
handful of ops per booking. It is nowhere near the bottleneck.

What does *not* break: the correctness guarantee. It is a database constraint,
and it holds at any throughput.

### 10. What is the weakest part of this system?

The gap between calling the payment provider and committing the transaction.
Everything else is protected by a constraint or a TTL; that window is protected
only by retry logic and the fact that the mock provider is not real. A genuine
implementation needs authorise/capture plus reconciliation, and I would not
ship this against real money without them.

Second weakest: the WebSocket hub is in-process. Run two API instances and a
user connected to instance A will not see a hold placed through instance B.
Redis pub/sub between hubs is the fix, and it is the first thing I would build
next.

Third: `TRUST_PROXY_HEADERS` is a footgun by construction. Enabled without a
proxy that overwrites `X-Forwarded-For`, any client can forge its address and
rate limiting becomes decorative. It defaults to off and is commented, but it
is a setting that can be got wrong.

---

## 6. What I would do next

In the order I would actually do them:

1. **Redis pub/sub between WebSocket hubs**, so the system survives running
   more than one API instance. This is the only thing currently blocking
   horizontal scaling.
2. **Authorise/capture payments**, closing the charge-then-crash window.
3. **A reconciliation job** comparing provider records to local payments.
4. **Structured metrics** — holds acquired, conflict rate, confirm latency,
   expiry lag — because the interesting failures here are statistical.
5. **Seat map caching**, currently a full read per request. An event with
   50,000 seats under refresh pressure will feel it.
6. **Admin endpoints** for creating events and venues. The schema and the role
   check exist; the handlers do not.

Deliberately not on the list: a message queue, microservices, or Kubernetes.
None solves a problem this system currently has.
