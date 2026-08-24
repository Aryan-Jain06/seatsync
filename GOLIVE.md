# Going live on a real URL

A click-by-click walkthrough to get SeatSync running on the public internet,
end to end, for free.

At the end you will have a URL you can send to anyone. They open it, create an
account, pick seats, watch a countdown, pay, and see the seats turn confirmed —
and if two people open it at once, they see each other's holds appear live.

**Time:** 45–60 minutes the first time.
**Cost:** nothing. Every service here has a real free tier.
**Prerequisites:** a GitHub account (you have one), an email address, and a
browser. A card is *not* needed for any of these four.

> **On free tiers.** Providers change their offers. Everything here was
> accurate when written; if a screen looks different, the shape of what you
> need is still the same — a connection string, a host, a password.

---

## The shape of it

Four services. Each does one thing.

```
┌──────────────┐        ┌──────────────┐
│   Vercel     │───────▶│    Koyeb     │
│  (frontend)  │  HTTPS │  (Go API)    │
│  Next.js     │◀──────▶│  + WebSocket │
└──────────────┘  WSS   └──────┬───────┘
                                │
                   ┌────────────┴────────────┐
                   ▼                         ▼
            ┌─────────────┐          ┌──────────────┐
            │    Neon     │          │   Upstash    │
            │ PostgreSQL  │          │    Redis     │
            └─────────────┘          └──────────────┘
```

Do them in this order. Each step needs a value from the one before it.

---

## Worksheet

Keep this open and fill it in as you go. You will paste these repeatedly, and
hunting for them later is where the time goes.

```
1. Neon connection string  : postgres://______________________________
2. Upstash host:port        : ______________________________:6379
3. Upstash password         : ______________________________
4. JWT secret (you generate): ______________________________
5. Koyeb API URL            : https://______________________.koyeb.app
6. Vercel frontend URL      : https://______________________.vercel.app
```

Generate #4 right now, before you start:

```bash
openssl rand -base64 48
```

No `openssl`? Use https://generate-secret.vercel.app/48 or any password
generator set to 48+ characters. **Do not reuse the example value from
`.env.example`** — the server refuses to start with it when `APP_ENV=production`,
which is deliberate.

---

## Step 1 — PostgreSQL on Neon

1. Go to **https://neon.tech** → **Sign up** → continue with GitHub.
2. Create a project. Name it `seatsync`. Any region — pick one near you.
3. On the dashboard, find **Connection string** and copy it.

It looks like:

```
postgres://neondb_owner:npg_XXXX@ep-cool-name-12345.ap-southeast-1.aws.neon.tech/neondb?sslmode=require
```

**Write it into worksheet line 1.**

Keep `?sslmode=require` on the end. Neon rejects unencrypted connections, and
without it the backend will fail to connect with a confusing TLS error.

> **What you get free:** ~0.5 GB storage, one project. Far more than this needs.
> The database suspends after ~5 minutes idle and wakes on the next connection,
> so the first request after a quiet period takes a second or two. That is the
> free tier working as designed, not a bug in the app.

---

## Step 2 — Redis on Upstash

1. Go to **https://upstash.com** → **Sign up** → continue with GitHub.
2. **Create Database**. Name it `seatsync`. Pick a region near your Neon one.
3. Choose the **free** plan.
4. On the database page, find the connection details. You want three things:
   - **Endpoint** (looks like `apn1-cool-name-12345.upstash.io`)
   - **Port** (usually `6379`)
   - **Password** (a long string; there is a copy button)

**Write endpoint:port into line 2, password into line 3.**

> **Important:** Upstash accepts **TLS connections only**. SeatSync supports
> this — you will set `REDIS_TLS=true` in the next step. Miss it and the
> backend fails to start with `connect to redis: context deadline exceeded`,
> which looks like a network problem but is not.

> **What you get free:** a daily command allowance. A demo will not come close.
> A real launch might — see [Watching the limits](#watching-the-limits).

---

## Step 3 — The Go API on Koyeb

This is the longest step. It is also the one that gives you a URL.

1. Go to **https://koyeb.com** → **Sign up** → continue with GitHub.
2. **Create Service** → **GitHub** → authorise Koyeb → pick
   **`Aryan-Jain06/seatsync`**.

3. Configure the build:

   | Field | Value |
   |---|---|
   | Branch | `main` |
   | Builder | **Dockerfile** |
   | Work directory | `backend` |
   | Dockerfile location | `Dockerfile` |

   The work directory matters. Set it to `backend`, and the Dockerfile location
   is then just `Dockerfile` relative to it — not `backend/Dockerfile`.

4. Configure the service:

   | Field | Value |
   |---|---|
   | Port | `8080` |
   | Health check path | `/health/ready` |
   | Instance type | the free one (`nano`) |
   | Regions | one, near your database |

   Use `/health/ready`, not `/health`. The former checks that Postgres and
   Redis are actually reachable; the latter only checks the process is alive.
   You want the platform to know the difference.

5. **Environment variables.** Add each of these. Substitute your worksheet
   values where marked.

   ```
   DATABASE_URL          = «worksheet line 1»
   REDIS_ADDR            = «worksheet line 2»
   REDIS_PASSWORD        = «worksheet line 3»
   REDIS_TLS             = true
   JWT_SECRET            = «worksheet line 4»
   RUN_MIGRATIONS        = true
   APP_ENV               = production
   TRUST_PROXY_HEADERS   = true
   ENABLE_HSTS           = true
   RATE_LIMIT_ENABLED    = true
   PAYMENT_MODE          = random
   CORS_ALLOWED_ORIGINS  = http://localhost:3000
   ```

   Mark `JWT_SECRET`, `DATABASE_URL` and `REDIS_PASSWORD` as **secret** if
   Koyeb offers the option.

   Three of these deserve a note:

   - **`RUN_MIGRATIONS=true`** applies the database schema on boot. There is no
     separate migration step to run.
   - **`TRUST_PROXY_HEADERS=true`** is correct *here* because Koyeb terminates
     TLS and sets `X-Forwarded-For` itself. Do **not** set it on a server that
     is directly reachable — any client could then forge its address and rate
     limiting would stop working. The server logs a warning when this is on, so
     you can tell at a glance.
   - **`CORS_ALLOWED_ORIGINS`** is a placeholder for now. You will come back and
     fix it in Step 6, once Vercel has given you a URL.

6. **Deploy.** First build takes 3–5 minutes.

7. When it goes green, copy the URL Koyeb shows you
   (`https://something-yourname.koyeb.app`). **Write it into line 5.**

8. **Verify before moving on:**

   ```bash
   curl https://YOUR-API.koyeb.app/health/ready
   ```

   You want `{"status":"ready"}`. If you get anything else, jump to
   [Troubleshooting](#troubleshooting) — do not continue, because every later
   step assumes this works.

   ```bash
   curl https://YOUR-API.koyeb.app/events
   ```

   Expect `{"events":[]}`. Empty is correct — the schema exists but there is no
   data yet. That is the next step.

---

## Step 4 — Load the demo data

The database is empty. Without this, the site works but shows nothing.

**There is a wrinkle worth understanding.** The backend image is built `FROM
scratch` — it contains the binaries and nothing else. No shell, no package
manager. This is good for security but it means **you cannot open a terminal on
the running service and run a seed command.** There is no terminal to open.

So you seed *from your own machine*, pointing at the Neon database directly.
Neon is reachable from anywhere, which is what makes this work.

Pick whichever you have installed.

### If you have Go

```bash
git clone https://github.com/Aryan-Jain06/seatsync.git
cd seatsync/backend

DATABASE_URL="«worksheet line 1»" go run ./cmd/seed
```

### If you have Docker but not Go

```bash
git clone https://github.com/Aryan-Jain06/seatsync.git
cd seatsync

docker build -t seatsync-backend ./backend
docker run --rm \
  -e DATABASE_URL="«worksheet line 1»" \
  --entrypoint /seed \
  seatsync-backend
```

The `--entrypoint /seed` matters. The image's entrypoint is `/server`, so
without overriding it you would start the API instead of seeding.

### Either way, verify

```bash
curl https://YOUR-API.koyeb.app/events
```

You should now see four events. If you do, the hard part is done.

---

## Step 5 — The frontend on Vercel

1. Go to **https://vercel.com** → **Sign up** → continue with GitHub.
2. **Add New** → **Project** → import **`Aryan-Jain06/seatsync`**.

3. Configure:

   | Field | Value |
   |---|---|
   | Framework Preset | Next.js (detected automatically) |
   | Root Directory | **`frontend`** ← click Edit and set this |

   The root directory is the one people miss. Leave it at the repository root
   and the build fails, because there is no `package.json` there.

4. **Environment Variables** — add both, using your line 5 value:

   ```
   NEXT_PUBLIC_API_BASE_URL = https://YOUR-API.koyeb.app
   NEXT_PUBLIC_WS_BASE_URL  = wss://YOUR-API.koyeb.app
   ```

   **Look carefully at the second one.** It is `wss://`, not `ws://`, and not
   `https://`. A page served over HTTPS is not allowed to open an insecure
   WebSocket — the browser blocks it silently, with nothing in the UI to
   explain why. Live seat updates would simply never arrive while everything
   else worked perfectly. This single character causes more confusion than any
   other part of this document.

5. **Deploy.** Takes about a minute.

6. Copy the URL Vercel gives you. **Write it into line 6.**

---

## Step 6 — Close the loop

Your frontend now knows about the API. The API does not yet trust the frontend.

Right now, opening the site gives you a page that loads and then fails on
every request, with a CORS error in the browser console. That is expected —
you set `CORS_ALLOWED_ORIGINS` to a placeholder in Step 3.

1. Go back to **Koyeb** → your service → **Settings** → **Environment
   variables**.
2. Change `CORS_ALLOWED_ORIGINS` to your Vercel URL from line 6:

   ```
   CORS_ALLOWED_ORIGINS = https://your-app.vercel.app
   ```

   No trailing slash. `https://your-app.vercel.app/` will not match.

3. **Redeploy.** Wait for green.

---

## Step 7 — Verify it actually works

Open your Vercel URL and walk the whole path. Do not skip to the end; each
check tells you a different thing works.

- [ ] **The events page lists four events.** → the API, the database and CORS
      are all working together.
- [ ] **Register a new account.** → auth and token issuing work.
- [ ] **Open an event.** The seat map renders in colour. → the seat map read
      path works.
- [ ] **Click two or three seats, hold them.** A 5:00 countdown starts. → Redis
      locking works.
- [ ] **Open the same event in a second browser window.** Your held seats show
      as taken there, without a refresh. → **the WebSocket works.** This is the
      check that catches a `ws://` mistake.
- [ ] **Pay.** Seats turn confirmed in both windows. → the confirm transaction
      and the broadcast work.
- [ ] **Visit `/me/bookings`.** The booking is listed. → the whole thing is
      persisted.

The payment fails 10% of the time on purpose — `PAYMENT_MODE=random`. If it
declines, press Retry. It reuses the same idempotency key, which is the entire
point of that design. Seeing a decline is a feature, not a fault.

If all seven pass, you are live. Send someone the Vercel URL.

---

## Troubleshooting

Ordered by how often each actually happens.

| Symptom | Cause | Fix |
|---|---|---|
| Backend won't start, logs say `connect to redis: context deadline exceeded` | `REDIS_TLS` not set | Set `REDIS_TLS=true`. Upstash is TLS-only |
| Everything works but seats never update live in a second window | `ws://` instead of `wss://` | Fix `NEXT_PUBLIC_WS_BASE_URL` on Vercel, redeploy |
| Page loads, every request fails, console shows CORS | Step 6 not done, or a trailing slash | `CORS_ALLOWED_ORIGINS` must match the Vercel URL exactly, no trailing `/` |
| Vercel build fails, `package.json not found` | Root directory not set | Set Root Directory to `frontend` |
| `/events` returns `{"events":[]}` | Not seeded | Do Step 4 |
| Backend won't start, `JWT_SECRET is still the example value` | Placeholder secret with `APP_ENV=production` | Generate a real one. This guard is intentional |
| Backend won't start, TLS error mentioning Postgres | `sslmode=require` stripped from the URL | Put it back |
| First request after a quiet spell takes seconds | Neon suspended when idle | Normal on the free tier |
| Random `429` responses | Rate limiting doing its job | Expected. Only raise the limits if legitimate use is being blocked |
| Login fails repeatedly then returns 429 | Ten failed attempts per minute per address | Wait a minute. This is the brute-force guard |

**Reading the logs.** Koyeb's log view is the fastest way to diagnose a
backend that will not start. The app logs structured JSON, and the failure
reason is always on the last line before it exits.

---

## Watching the limits

Four things to know about running on free tiers.

**Neon suspends when idle** and wakes on connection. First request after a
quiet period is slow. Harmless for a demo.

**Upstash meters commands.** Roughly, each booking costs ~5 Redis commands and
each HTTP request costs 1 more for the rate limiter. A demo is nowhere near the
limit. If you ever exceed it, run Valkey on a small VM instead — see
`INTEGRATIONS.md` §4.

**Koyeb's free instance is small.** Fine for demonstration traffic. It does not
sleep, which is why it is recommended here over Render, whose free services
sleep after 15 minutes and take ~30 seconds to wake.

**Neon's connection ceiling matters if you scale up.** The backend opens up to
25 connections. One instance is fine. If you ever run more, add PgBouncer —
`DEPLOYMENT.md` covers it.

---

## A custom domain

Optional, and both platforms make it easy.

**Vercel:** Settings → Domains → add yours → set the DNS records it shows you.
TLS is issued automatically.

**Koyeb:** Settings → Domains → same shape.

If you point the API at a custom domain, **update two things or the site
breaks**: `NEXT_PUBLIC_API_BASE_URL` and `NEXT_PUBLIC_WS_BASE_URL` on Vercel,
and `CORS_ALLOWED_ORIGINS` on Koyeb.

Putting **Cloudflare** in front is free and worth doing — DDoS protection and a
WAF for twenty minutes of setup. `DEPLOYMENT.md` §12 covers it.

---

## Deploying changes from now on

Both platforms watch your repository. Push to `main` and both rebuild
automatically. No further action.

CI runs first and independently, so if you break something the tests go red on
GitHub even though the deploy proceeds. Watch the Actions tab.

---

## One thing to be straight about

**Payments are simulated.** No gateway is contacted, no money moves, nothing is
charged. That was a deliberate constraint on this project, not an oversight.

Say so plainly when you show it to anyone. The interesting claim this project
makes is not "it takes payments" — it is "500 people cannot book the same 50
seats," which is proved on every commit in the Actions tab.

If you ever want real payments, `INTEGRATIONS.md` §1 explains what changes and
why it is more than swapping one function.

---

## Once you're live

Send the Vercel URL. Two things worth mentioning to whoever you send it to:

1. **Open it in two windows** to see the live seat map. That is the part people
   find surprising, and it is invisible in one window.
2. **The GitHub repo** — https://github.com/Aryan-Jain06/seatsync — where the
   Actions tab re-proves the zero-double-booking guarantee on every push. For a
   technical audience, that is the stronger artifact.
