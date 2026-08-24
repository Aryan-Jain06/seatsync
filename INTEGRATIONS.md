# Connecting SeatSync to other things

How to plug real services into this codebase: payment gateways, email, sign-in
with Google, monitoring, object storage, and the rest.

[DEPLOYMENT.md](DEPLOYMENT.md) covers *where the app runs*. This covers *what
it talks to*.

> **On the code in this document.** Snippets show the shape of each
> integration and how it fits this codebase's interfaces. Third-party APIs
> change — check the provider's current documentation for exact method
> signatures before copying. What will not change is where these things plug
> in, which is the part that is specific to this project.

---

## Contents

- [The seam map](#the-seam-map) — where things plug in
- [Payments](#1-payments-the-important-one) — the one that matters
- [Email](#2-email)
- [Sign in with Google](#3-sign-in-with-google)
- [Redis alternatives](#4-redis-alternatives)
- [PostgreSQL alternatives](#5-postgresql-alternatives)
- [Running more than one instance](#6-running-more-than-one-instance)
- [Metrics](#7-metrics)
- [Error tracking](#8-error-tracking)
- [Logs](#9-logs)
- [Images](#10-images)
- [Analytics](#11-analytics)
- [Cloudflare](#12-cloudflare)
- [What to do first](#what-to-do-first)

---

## The seam map

This codebase was written so that external services attach at interfaces
rather than being scattered through the business logic. Everything is
constructed in one place — `cmd/server/main.go`, lines 104–147 — so that file
is the map.

The interfaces that already exist:

| Interface | Where | Currently | Swap it for |
|---|---|---|---|
| `payments.Provider` | `internal/payments/provider.go:32` | `MockProvider` | Razorpay, Stripe |
| `services.SeatLocker` | `internal/services/holds.go:21` | `locks.Manager` (Redis) | Rarely changed |
| `services.SeatBroadcaster` | `internal/services/holds.go:31` | `realtime.Hub` | A pub/sub hub |
| `services.PaymentMutex` | `internal/services/payments.go:31` | `locks.Manager` | Rarely changed |
| `services.HoldReader` | `internal/services/catalog.go:22` | `locks.Manager` | Rarely changed |

Two things do **not** have interfaces yet, because nothing needed them.
Adding email or SMS means creating one — described below.

The rule this codebase follows, and that you should keep: **services depend on
interfaces, `main.go` decides which implementation.** A service that imports
Razorpay directly cannot be tested without the internet.

---

## 1. Payments (the important one)

Read this section before any other. It is the only integration where getting
it wrong costs money.

### What is there now

`internal/payments/provider.go` defines a one-method interface:

```go
type Provider interface {
    Charge(ctx context.Context, amount int64, idempotencyKey string) (*Result, error)
}
```

`MockProvider` sleeps 1–2 seconds and succeeds 90% of the time. It is wired in
at `cmd/server/main.go:126`.

Note the distinction the interface draws, because it matters: a returned
`error` means the provider was **unreachable** (retryable). A `Result` with
`Succeeded: false` means the provider **answered "declined"** (not retryable
in the same way). Preserve that distinction in any real implementation.

### Why you cannot just implement the interface and ship

The current flow is:

```
charge the card  →  commit the transaction
```

If the commit fails, **you have taken money for a booking that does not
exist.** With a mock that is a rounding error in a demo. With a real gateway
it is a customer complaint and a manual refund.

Real gateways solve this with **authorise then capture**:

```
authorise (hold funds, no charge)
  →  commit the transaction
       →  succeeded?  capture (take the money)
       →  failed?     void (release the hold)
```

An authorisation that is never captured expires by itself, typically in 7 days.
That is the safety property you want: the failure mode is "nothing happened"
rather than "money moved and nothing else did".

So a real provider needs a **three-method** interface, not one:

```go
// Provider charges a payment method using authorise-then-capture, so that a
// failure after authorisation releases the hold rather than stranding funds.
type Provider interface {
    // Authorize reserves funds without moving them.
    Authorize(ctx context.Context, amount int64, idempotencyKey string) (*Authorization, error)
    // Capture takes previously authorised funds. Called only after the
    // booking has been committed.
    Capture(ctx context.Context, authID string) (*Result, error)
    // Void releases an authorisation that will not be captured.
    Void(ctx context.Context, authID string) error
}
```

Then in `internal/services/payments.go`, the call at line 175 becomes
`Authorize`, and after the transaction commits (line 222, where the seat locks
are released) you `Capture`. If the transaction fails, you `Void`.

**This is a real change to the confirm path, not a drop-in.** Budget a day,
and write tests for the void path — it is the one that will not fire during
manual testing.

### Razorpay (India)

Best fit if you are billing in rupees. UPI, cards, net banking.

```bash
go get github.com/razorpay/razorpay-go
```

```bash
RAZORPAY_KEY_ID=rzp_test_xxxxx
RAZORPAY_KEY_SECRET=xxxxx
RAZORPAY_WEBHOOK_SECRET=xxxxx
```

Razorpay amounts are in **paise** — which is what `total_amount` already
stores, so no conversion is needed. That is not a coincidence; it is why money
is stored in minor units.

Two things that surprise people:

- Razorpay's flow is **order-first**: create an order server-side, the browser
  completes payment against it, then you verify. That is a different shape
  from "charge this card", and the frontend checkout drawer needs to open
  their widget rather than posting straight to your API.
- **Payment confirmation is asynchronous.** The browser saying "paid" is not
  authoritative; the webhook is. Which brings us to:

### Webhooks are not optional

A user can close the tab between paying and your server hearing about it. The
gateway will still tell you — via webhook — and if you are not listening, that
booking never confirms and the seats lapse despite being paid for.

Add an endpoint:

```go
// Webhooks arrive from the gateway, not from a browser, so they are exempt
// from auth middleware but MUST verify the signature instead. An unverified
// webhook endpoint lets anyone confirm any booking for free.
r.Post("/webhooks/razorpay", deps.Webhooks.Razorpay)
```

Three rules, all learned the hard way by somebody:

1. **Verify the signature.** Every gateway signs its webhooks with your
   secret. Skipping this means anyone who finds the URL can confirm bookings.
2. **Make it idempotent.** Gateways retry. The same event will arrive more
   than once, and your handler must treat the second one as a no-op. The
   booking confirm path is already idempotent, so route through it.
3. **Return 200 fast.** Do the work asynchronously if it is slow. A timeout
   makes the gateway retry, which multiplies your load exactly when it is
   already high.

Testing locally: `ngrok http 8080` gives a public URL you can register.

### Stripe (elsewhere)

If you are not billing in India, Stripe's Go SDK is better documented and its
`PaymentIntent` API models authorise/capture directly — `capture_method:
manual` is exactly the flow described above.

```bash
go get github.com/stripe/stripe-go/v79
```

Stripe's test mode has card numbers for every failure you want to exercise
(declines, insufficient funds, 3D Secure). Use them. The mock's 10% random
failure is a poor substitute for a real decline path.

### The checklist before real money

- [ ] Authorise/capture, not charge-then-commit
- [ ] Webhook endpoint, signature verified
- [ ] Webhook handler is idempotent
- [ ] Reconciliation job comparing gateway records to the `payments` table
- [ ] Refund path (there is currently no way to cancel a confirmed booking)
- [ ] Tested against the gateway's declined-card numbers

Until all six are ticked, keep `PAYMENT_MODE` mocked and label the deployment
a demo.

---

## 2. Email

There is no email at all. No booking confirmation, no password reset.

### Free options

| | Free tier | |
|---|---|---|
| **Resend** | 3,000/month, 100/day | Cleanest API, easiest start |
| **Brevo** | 300/day | Higher monthly ceiling |
| **AWS SES** | 62,000/month from EC2 | Cheapest at volume, fiddly setup |

### Adding it

There is no interface yet, so create one. Follow the pattern the payment
provider already sets:

```go
// Package notify sends transactional messages.
package notify

// Mailer sends transactional email.
//
// Sending must never block or fail a booking: a confirmed seat with no email
// is a minor annoyance, a failed booking because the mail server was down is
// a lost sale.
type Mailer interface {
    Send(ctx context.Context, to, subject, htmlBody string) error
}
```

Wire it in `main.go` beside the other constructors, and call it **after** the
transaction commits — in `internal/services/payments.go` around line 228,
where the seat updates are already broadcast.

Two rules:

- **Never inside the transaction.** An email send is a network call; holding a
  database transaction open across one is the same mistake as holding one
  across a payment.
- **Never fatal.** Log the failure and carry on. The booking is confirmed
  either way.

```go
// After the booking is confirmed. Failure is logged, never propagated.
go func() {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := mailer.Send(ctx, user.Email, "Your tickets", body); err != nil {
        slog.Error("booking confirmation email failed",
            "booking_id", booking.ID, "error", err)
    }
}()
```

Note the fresh context. Using the request's context would cancel the send the
moment the HTTP response is written.

### Deliverability

Mail from a new domain goes to spam. Set up SPF, DKIM and DMARC records — your
provider gives you the exact DNS entries. This is dull and it is the
difference between your email arriving and not.

---

## 3. Sign in with Google

Currently: email and password only, bcrypt hashed.

OAuth is worth adding because it removes password handling entirely for most
users — no reset flow, no breach exposure, no "password123".

```bash
go get golang.org/x/oauth2
```

```bash
GOOGLE_CLIENT_ID=xxxxx.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=xxxxx
OAUTH_REDIRECT_URL=https://your-domain/auth/google/callback
```

The schema change is small:

```sql
ALTER TABLE users ADD COLUMN google_id TEXT;
CREATE UNIQUE INDEX uq_users_google_id ON users (google_id) WHERE google_id IS NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
```

`password_hash` must become nullable — a Google user has no password. The
partial unique index means many users can have `NULL` while no two share a
Google account, which is the same trick the booking seats index uses.

Two flow notes:

- **Use the `state` parameter** and verify it on the callback. It is the CSRF
  protection for the OAuth handshake, and skipping it is a real vulnerability,
  not a formality.
- **Match on email to link accounts** — but only if the provider says the
  email is verified. Otherwise someone can create an unverified account at
  your user's address and take over by "linking".

After the callback resolves a user, issue tokens exactly as
`internal/services/auth.go` already does. Everything downstream is unchanged.

---

## 4. Redis alternatives

The code uses `github.com/redis/go-redis/v9` and needs nothing exotic —
`SET NX EX`, sorted sets, and `EVAL` for the Lua scripts.

| | Notes |
|---|---|
| **Valkey** | The Linux Foundation fork after Redis's 2024 licence change. Drop-in — change the image and nothing else. |
| **Upstash** | Serverless, free tier, but **metered by command count**. |
| **Redis Cloud** | 30 MB free. Ample; this stores tiny keys. |
| **DragonflyDB** | Redis-compatible, faster at scale. Overkill here. |

Switching to Valkey is one line in `docker-compose.yml`:

```yaml
redis:
  image: valkey/valkey:7-alpine    # was redis:7-alpine
```

### If you use Upstash, read this

Upstash requires TLS, and `REDIS_ADDR` alone will not connect. You need to set
`TLSConfig` on the client in `cmd/server/main.go:75`:

```go
rdb := redis.NewClient(&redis.Options{
    Addr:      cfg.RedisAddr,
    Password:  cfg.RedisPassword,
    TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},  // required by Upstash
})
```

Also count your commands. Per booking this system issues roughly: one `EVAL`
to acquire, one `ZADD`, one `EVAL` to verify, one `EVAL` to release, one
`ZREM` — plus one rate-limit `EVAL` per HTTP request. A busy event can move
through a daily free allowance quickly. If that bites, run Valkey on a small
VM; it is one container and it does not meter.

---

## 5. PostgreSQL alternatives

The schema uses `gen_random_uuid()` (pgcrypto), enum types, and partial unique
indexes. All standard PostgreSQL 13+. Any real Postgres works; **MySQL does
not** — it has no partial indexes, which is the entire guarantee.

| | Free tier | Watch out for |
|---|---|---|
| **Neon** | ~0.5 GB, branching | Suspends when idle; slow first query |
| **Supabase** | 500 MB | Pauses projects after a week idle |
| **Railway** | Trial credit | Not free ongoing |
| **Self-hosted** | Free | You own backups |

Usually only the connection string changes:

```bash
DATABASE_URL=postgres://user:pass@host/db?sslmode=require
```

Keep `sslmode=require` for anything hosted.

### Connection limits

`internal/db/db.go` sets `MaxConns = 25`, chosen so the load test measures
contention rather than queueing. Managed free tiers often allow far fewer.
Exceed it and you get connection errors under load that look like application
bugs.

Either lower it, or put **PgBouncer** in transaction mode in front. PgBouncer
is the right answer once you run more than one instance, because 25 connections
each against a 20-connection limit fails immediately.

---

## 6. Running more than one instance

This is the integration that unblocks scaling, and it is the first item on the
roadmap in [EXPLANATION.md](EXPLANATION.md).

**The problem.** `realtime.Hub` keeps subscribers in a map in memory. A user
connected to instance A never hears about a hold placed through instance B.
Seat maps are still correct on load and after any action — only live push
breaks — but it looks broken.

**The fix.** Redis pub/sub between hubs. Redis is already a dependency, so
this adds no new infrastructure.

The hub already satisfies `services.SeatBroadcaster`
(`internal/services/holds.go:31`), so the change is contained:

```go
// BroadcastSeatUpdates publishes to Redis rather than only to local
// subscribers, so every instance's hub delivers the update.
func (h *PubSubHub) BroadcastSeatUpdates(eventID uuid.UUID, updates []models.SeatUpdate) {
    payload, err := json.Marshal(updates)
    if err != nil {
        slog.Error("marshal seat updates", "error", err)
        return
    }
    if err := h.rdb.Publish(ctx, "seatupdates:"+eventID.String(), payload).Err(); err != nil {
        // Fall back to local delivery: subscribers on this instance still
        // get the update, which is better than nobody getting it.
        slog.Error("publish seat updates", "error", err)
        h.local.BroadcastSeatUpdates(eventID, updates)
    }
}
```

Each instance subscribes to `seatupdates:*` and fans out to its own local
subscribers. The existing `realtime.Hub` becomes the local delivery half and
does not otherwise change — its slow-client eviction and shutdown draining
stay exactly as they are.

**Until this exists, keep `replicas: 1`.**

---

## 7. Metrics

There is no `/metrics` endpoint. Logs are structured JSON on stdout.

```bash
go get github.com/prometheus/client_golang
```

Beyond the usual request rate and latency, the signals that matter for *this*
system:

```go
// The conflict rate is the health signal specific to a booking system: a
// sudden rise means an event is selling out, or something is wrong.
holdConflicts  = promauto.NewCounter(...)   // 409s on hold
holdsAcquired  = promauto.NewCounter(...)
confirmLatency = promauto.NewHistogram(...) // the critical path
expiryLag      = promauto.NewGauge(...)     // worker falling behind
```

And one that should never move:

```go
// Should be zero forever. Alert on any non-zero value: it means the
// guarantee this system exists to provide has been violated.
duplicateConfirmedSeats = promauto.NewGauge(...)
```

Scrape it from Prometheus, or use Grafana Cloud's free tier (10k series).

Expose `/metrics` on a **separate port** from the public API, or behind auth.
Metrics leak information about your traffic and internals.

---

## 8. Error tracking

Structured logs tell you an error happened. Error tracking tells you it
happened 400 times to 30 users starting at 14:02, with a stack trace.

| | Free tier |
|---|---|
| **Sentry** | 5,000 errors/month |
| **GlitchTip** | Open source, self-hostable, Sentry-compatible |

There is one obvious place to hook it — `internal/httpx/httpx.go`, in `Error`,
where 5xx responses are already logged:

```go
if apiErr.Status >= http.StatusInternalServerError {
    sentry.CaptureException(apiErr)     // add this
    slog.ErrorContext(r.Context(), "request failed", ...)
}
```

One integration point catches every internal error in the API, because they
all route through that function. That is what the typed error design bought.

**Scrub before sending.** Tokens, emails and idempotency keys should not leave
your infrastructure. Sentry's `BeforeSend` hook is where you strip them.

---

## 9. Logs

The app writes structured JSON to stdout, which every platform collects.

| | |
|---|---|
| **Grafana Loki** | Self-hosted, pairs with Prometheus |
| **Better Stack** | 1 GB/month free |
| **Platform-native** | Koyeb, Render and Vercel all keep recent logs |

For a demo, the platform's own log view is enough. Add aggregation when you
have more than one instance and grepping stops working.

Never log tokens, passwords or full idempotency keys. Log the booking ID
instead — it correlates just as well and is not a credential.

---

## 10. Images

Events have no images. The `events` table has `title`, `description`,
`starts_at`, `base_price` — no media.

To add them:

```sql
ALTER TABLE events ADD COLUMN image_url TEXT;
```

Then somewhere to put the files:

| | Free tier | |
|---|---|---|
| **Cloudflare R2** | 10 GB, **no egress fees** | Best choice |
| **Backblaze B2** | 10 GB | |
| **AWS S3** | 5 GB for 12 months | Egress costs add up |

R2 is S3-compatible, so the AWS SDK works against it unchanged. Egress is
where object storage bills surprise people, and R2 charges none.

Upload from the browser using **presigned URLs** — the server signs a
short-lived URL and the file goes straight to storage. Do not proxy uploads
through the Go API; it ties up a request for the duration of the transfer for
no benefit.

---

## 11. Analytics

If you want to know which events people look at:

| | |
|---|---|
| **Plausible** | Open source, self-hostable, no cookies |
| **Umami** | Open source, self-hostable |
| **PostHog** | Generous free tier, product analytics |

Plausible or Umami self-hosted are the honest choices — no cookie banner,
because they set no cookies, and no third party receiving your users' browsing.

Add the script in `frontend/src/app/layout.tsx`. Note that the API's
`Content-Security-Policy` is deliberately restrictive; the frontend is served
by Next.js and has its own policy, so this does not conflict.

---

## 12. Cloudflare

Free, and the highest value-per-minute on this list.

Point your domain's nameservers at Cloudflare, enable proxying, and you get
DDoS protection, a WAF, TLS, and caching without touching code.

It **complements** the application rate limiting rather than replacing it:

- **Cloudflare** stops volumetric floods at the edge, before your server pays
  for them.
- **The token buckets** stop a legitimate, authenticated user from hammering a
  specific endpoint — something Cloudflare cannot see, because it does not
  know who your users are.

If you put Cloudflare in front, set `TRUST_PROXY_HEADERS=true` — Cloudflare
overwrites `X-Forwarded-For`, so it becomes trustworthy. Re-read the warning
in [DEPLOYMENT.md](DEPLOYMENT.md#6-production-configuration) about what that
setting does when there is *no* proxy.

---

## What to do first

If you are going to do some of this and not all of it, this order:

1. **Cloudflare.** Twenty minutes, free, immediate protection.
2. **Error tracking.** One line in `httpx.Error` catches everything.
3. **Email.** The product feels broken without a confirmation.
4. **Redis pub/sub for the hub.** Unblocks running more than one instance.
5. **Metrics.** When you need to know *why* something is slow.
6. **Payments.** Last, because it is the largest change and the only one where
   a mistake costs money. Do not start it until the rest is stable.

Sign in with Google, images and analytics are polish. Add them when the
product is real enough to deserve them.
