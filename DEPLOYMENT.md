# Deploying SeatSync

Everything you need to add to run this for real, and how to do it.

Right now the whole system runs from one `docker compose up` on your laptop.
That is by design — but a laptop is not a deployment. This document covers what
has to change, what it costs (nothing, if you pick from the free options here),
and the exact steps.

> **A caution on free tiers.** Providers change their offers regularly. Every
> tier described here was accurate when this was written, but verify before
> committing. Where a provider is likely to shift, it is flagged.

---

## Contents

1. [What the system actually needs](#1-what-the-system-actually-needs)
2. [What is mocked and must be replaced](#2-what-is-mocked-and-must-be-replaced)
3. [Choosing a path](#3-choosing-a-path)
4. [Path A — Managed platforms](#path-a--managed-platforms-easiest)
5. [Path B — Your own cluster with GitOps](#path-b--your-own-cluster-with-gitops)
6. [Secrets](#5-secrets)
7. [Production configuration](#6-production-configuration)
8. [CI/CD](#7-cicd)
9. [Monitoring](#8-monitoring)
10. [Pre-launch checklist](#9-pre-launch-checklist)

---

## 1. What the system actually needs

Four things must exist somewhere. Nothing else is required.

| Component | What it is | Can it be shared? | If it dies |
|---|---|---|---|
| **Go API** | Stateless HTTP + WebSocket server | Run as many as you like\* | Users cannot book |
| **PostgreSQL 16+** | The record of truth | One primary | Everything stops |
| **Redis 7+** (or Valkey) | Seat locks, rate limits | One instance | Holds fail; **nothing is double-sold** |
| **Next.js frontend** | The browser app | Static-ish, any CDN | No UI; the API still works |

\* Seat updates are relayed between instances over Redis pub/sub, so a user
connected to one instance sees holds placed through any other. Verified with
two processes: a hold on `:8080` reaches a subscriber on `:8081`.

Local delivery does not depend on Redis. If the relay cannot reach it, each
instance still serves its own subscribers — a multi-instance deployment
degrades to single-instance behaviour rather than going silent.

That said, do not add instances to feel scalable; add them when one is
measurably saturated. Note that connection count multiplies against your
Postgres limit — see [Connection limits](#connection-limits).

### On Redis and licensing

Redis changed its licence in 2024. If that matters to you, **Valkey** is the
Linux Foundation fork, is a drop-in replacement, and speaks the same protocol —
this codebase needs no changes to use it. Substitute `valkey/valkey:7-alpine`
for `redis:7-alpine` in `docker-compose.yml` and everything works.

---

## 2. What is mocked and must be replaced

Be honest about this before showing anyone a live URL.

### Payments are simulated

`internal/payments/mock.go` sleeps 1–2 seconds and returns success 90% of the
time. **No money moves. No gateway is contacted.**

To take real payments you would:

1. Implement the `Provider` interface against a real gateway — in India,
   Razorpay or Cashfree; elsewhere, Stripe. The interface is one method, so
   this is a contained change.
2. **Switch to authorise-then-capture.** This is not optional. The current flow
   charges, then commits a transaction. If the commit fails you have taken
   money for a booking that does not exist. Authorise before the transaction,
   capture after it commits, void if it fails.
3. Add a reconciliation job comparing gateway records against your `payments`
   table.
4. Handle webhooks, since real gateways confirm asynchronously.

Until all four exist, keep `PAYMENT_MODE` as a mock and label the deployment a
demo. This is the weakest part of the system and pretending otherwise is how
people lose money.

**[INTEGRATIONS.md](INTEGRATIONS.md) covers the actual implementation** —
which interface to change, what authorise/capture looks like in this codebase,
and the webhook rules.

### Email does not exist

No booking confirmations, no password reset. Free options: Resend (3,000/month),
Brevo (300/day), or AWS SES. None is wired in.

### Admin endpoints do not exist

The `admin` role, the middleware, and the schema all exist. The handlers to
create events and venues do not — seed data is loaded by a CLI. You would add
`POST /admin/events` behind the existing `RequireAdmin` middleware.

---

## 3. Choosing a path

| | Path A — Managed | Path B — Your own cluster |
|---|---|---|
| **Time to live** | ~30 minutes | A weekend, first time |
| **Cost** | Free tier | Free (Oracle Always Free) |
| **You operate** | Nothing | The cluster, updates, backups |
| **You learn** | Deployment basics | Kubernetes, GitOps, Argo CD |
| **Sleeps when idle** | Usually yes | No |
| **Good for** | A demo link to share | Matching how your org works |

Given you mentioned Argo CD at work, **Path B is the one that teaches you what
you actually want to know.** But do Path A first — get it live in half an hour,
then rebuild it properly. Having a working reference makes the cluster version
far easier to debug.

---

## Path A — Managed platforms (easiest)

Four free services.

> **For a step-by-step version of this path** — every field, every value, with
> verification after each step and a troubleshooting table — see
> **[GOLIVE.md](GOLIVE.md)**. What follows is the summary.

```
Vercel ─────────► frontend (Next.js)
   │
   └── calls ───► Koyeb or Render ──► Go API
                        │
                        ├──► Neon      (PostgreSQL)
                        └──► Upstash   (Redis)
```

### Step 1 — PostgreSQL on Neon

[neon.tech](https://neon.tech) → new project → copy the connection string.

It looks like `postgres://user:pass@ep-xxx.aws.neon.tech/neondb?sslmode=require`.
Keep `sslmode=require`.

Free tier: ~0.5 GB storage, one project. Ample for a demo. It suspends when
idle and wakes on connection, which shows up as a slow first request.

> **Alternative:** Supabase also gives free Postgres and does not suspend as
> aggressively, but pauses projects after a week of inactivity.

### Step 2 — Redis on Upstash

[upstash.com](https://upstash.com) → create database → copy the endpoint,
port and password.

**Check the free tier limits carefully.** Upstash meters by command count, and
this system uses several Redis commands per booking plus one rate-limit check
per request. A demo will not come close; a real launch might. If you exceed it,
run Valkey on a small VM instead — it is one container.

Set `REDIS_ADDR` to `host:port`, `REDIS_PASSWORD` to the password, and
**`REDIS_TLS=true`** — Upstash accepts TLS connections only. Omit it and the
backend fails to start with `connect to redis: context deadline exceeded`,
which reads like a firewall problem and is neither.

### Step 3 — The API on Koyeb

[koyeb.com](https://koyeb.com) → create service → GitHub → `Aryan-Jain06/seatsync`.

| Setting | Value |
|---|---|
| Build | Dockerfile |
| Dockerfile location | `backend/Dockerfile` |
| Work directory | `backend` |
| Port | `8080` |
| Health check path | `/health/ready` |

Environment variables:

```
DATABASE_URL=<from Neon>
REDIS_ADDR=<from Upstash>
REDIS_PASSWORD=<from Upstash>
REDIS_TLS=true
JWT_SECRET=<generate: openssl rand -base64 48>
RUN_MIGRATIONS=true
CORS_ALLOWED_ORIGINS=https://your-app.vercel.app
TRUST_PROXY_HEADERS=true
ENABLE_HSTS=true
PAYMENT_MODE=random
APP_ENV=production
```

`RUN_MIGRATIONS=true` applies the schema on boot, so there is no separate
migration step. `TRUST_PROXY_HEADERS=true` is correct **here** because Koyeb
terminates TLS and sets `X-Forwarded-For` itself — see the warning in section 6.

> **Render instead of Koyeb** works identically, but its free web services
> sleep after 15 minutes of inactivity (≈30 second cold start) and its free
> Postgres expires after 90 days. Koyeb's free tier does not sleep, which
> matters for a demo link you send to people.

Seeding needs care: the image is built `FROM scratch`, so it has no shell and
there is no terminal to open on the running service. Seed from your own machine
against the hosted database instead, which works because the database is
reachable from anywhere:

```bash
DATABASE_URL="<from Neon>" go run ./cmd/seed      # from backend/
```

[GOLIVE.md](GOLIVE.md) gives a Docker form of the same command.

### Step 4 — The frontend on Vercel

[vercel.com](https://vercel.com) → import `Aryan-Jain06/seatsync`.

| Setting | Value |
|---|---|
| Root directory | `frontend` |
| Framework | Next.js (detected) |

Environment variables:

```
NEXT_PUBLIC_API_BASE_URL=https://your-api.koyeb.app
NEXT_PUBLIC_WS_BASE_URL=wss://your-api.koyeb.app
```

`wss://` not `ws://` — a page served over HTTPS cannot open an insecure
WebSocket, and the browser will block it silently.

### Step 5 — Close the loop

Go back to Koyeb and set `CORS_ALLOWED_ORIGINS` to the Vercel URL. Redeploy.

Then verify, in this order:

```bash
curl https://your-api.koyeb.app/health/ready     # {"status":"ready"}
curl https://your-api.koyeb.app/events           # seeded events
```

Then open the Vercel URL, register, and book a seat. If the seat map loads but
does not update live, the WebSocket URL is wrong — check for `wss://`.

---

## Path B — Your own cluster with GitOps

This is the one that matches how your organisation works.

### First, the thing to be clear about

**Argo CD does not host anything.** This trips people up constantly.

Argo CD is a *deployment* tool. It watches a Git repository containing
Kubernetes manifests and continuously makes your cluster match them. If someone
changes something by hand, Argo CD reverts it. Git becomes the single source of
truth for what is running — that is what "GitOps" means.

So the order is:

```
1. Get a machine            (Oracle Cloud Always Free)
2. Put Kubernetes on it     (k3s)
3. Install Argo CD          (into that cluster)
4. Write manifests          (into a Git repo)
5. Point Argo CD at them    (it deploys, and keeps deploying)
```

You cannot start at step 3.

### Step 1 — A machine, genuinely free forever

**Oracle Cloud Always Free** is the outlier among free tiers: 4 ARM cores and
24 GB of RAM, free permanently, not a trial. That is more than enough for k3s,
Argo CD, Postgres, Redis and this application with room to spare.

[cloud.oracle.com](https://cloud.oracle.com) → sign up (a card is required for
identity verification; the Always Free resources are not charged) → create a
**VM.Standard.A1.Flex** instance with 4 OCPUs and 24 GB, running Ubuntu 22.04.

Two things that catch everyone:

- **Capacity errors are normal.** ARM instances are often unavailable in a
  given region. Retry, or pick a different availability domain. It can take
  several attempts over a few days.
- **Open the ports.** In the VCN security list, allow 80 and 443 inbound.
  Ubuntu's own `iptables` also blocks them by default:
  ```bash
  sudo iptables -I INPUT -p tcp --dport 80 -j ACCEPT
  sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT
  sudo netfilter-persistent save
  ```

> **Alternatives:** Hetzner (~€4/month, far less hassle, not free), or any spare
> machine at home with a Cloudflare Tunnel — which needs no public IP and no
> port forwarding at all.

### Step 2 — Kubernetes, the small kind

Full Kubernetes on one node is overkill. **k3s** is a certified distribution in
a single ~60 MB binary.

```bash
curl -sfL https://get.k3s.io | sh -

# Make kubectl work as your user
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config
sudo chown $USER ~/.kube/config
chmod 600 ~/.kube/config

kubectl get nodes        # should show Ready
```

That is a working cluster, with a load balancer and ingress controller
included.

### Step 3 — Argo CD

```bash
kubectl create namespace argocd
kubectl apply -n argocd -f \
  https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

kubectl wait --for=condition=available --timeout=600s \
  deployment/argocd-server -n argocd

# The initial admin password
kubectl -n argocd get secret argocd-initial-admin-secret \
  -o jsonpath="{.data.password}" | base64 -d; echo
```

Reach the UI:

```bash
kubectl port-forward svc/argocd-server -n argocd 8080:443
```

Then `https://localhost:8080`, user `admin`. **Change that password
immediately.**

> **Flux CD** is the main alternative — lighter, no UI, purely
> `kubectl`-driven. Argo CD's UI makes it much easier to see what is happening
> while learning, so start there.

### Step 4 — Manifests

Argo CD needs a Git repository of Kubernetes YAML. Put it in `deploy/` in this
repo, or a separate `seatsync-deploy` repo — a separate repo is the more common
production pattern, because it lets you deploy without touching application
history.

The shape you need:

```
deploy/
├── namespace.yaml
├── postgres/          StatefulSet + PersistentVolumeClaim + Service
├── redis/             Deployment + Service   (or Valkey)
├── backend/           Deployment + Service + Ingress
├── frontend/          Deployment + Service + Ingress
└── secrets/           SealedSecret          (never plain Secrets)
```

Key points rather than a full listing:

- **Postgres must be a StatefulSet with a PVC.** A Deployment loses its data
  on every restart. This is the single most common way people lose a database
  on Kubernetes.
- **The backend needs both probes:**
  ```yaml
  readinessProbe:
    httpGet: { path: /health/ready, port: 8080 }
  livenessProbe:
    httpGet: { path: /health, port: 8080 }
  ```
  `/health/ready` checks Postgres and Redis; `/health` only checks the process
  is alive. Using `/health/ready` as a liveness probe would restart the pod
  whenever the database blipped, which is exactly wrong.
- **Ingress needs WebSocket annotations** or live updates will fail while
  ordinary requests succeed — a confusing failure to debug:
  ```yaml
  nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
  nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  ```
- **`replicas` may exceed 1.** Seat updates relay between instances over
  Redis. Watch the Postgres connection ceiling rather than the hub.
- **`TRUST_PROXY_HEADERS=true`** is correct here, because ingress-nginx sets
  `X-Forwarded-For`.

### Step 5 — Point Argo CD at it

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: seatsync
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/Aryan-Jain06/seatsync
    targetRevision: main
    path: deploy
  destination:
    server: https://kubernetes.default.svc
    namespace: seatsync
  syncPolicy:
    automated:
      prune: true       # delete things removed from Git
      selfHeal: true    # revert manual changes to the cluster
    syncOptions:
      - CreateNamespace=true
```

```bash
kubectl apply -f argocd-application.yaml
```

From here, deploying is `git push`. Argo CD notices within about three minutes
and reconciles. That is the entire GitOps loop.

### Step 6 — TLS

```bash
kubectl apply -f \
  https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

Add a Let's Encrypt `ClusterIssuer`, annotate your Ingress, and certificates
are issued and renewed automatically. Free, and the renewal is the part you
would otherwise forget.

### Alternatives to GitHub itself

You asked about this specifically. GitHub hosts your *code*; none of the above
depends on it.

| | What it gives you | Free? |
|---|---|---|
| **GitLab** | Git + CI/CD + container registry, self-hostable | Free tier and free self-hosted CE |
| **Gitea / Forgejo** | Lightweight self-hosted Git, runs in one container | Fully free, self-hosted |
| **Codeberg** | Hosted Forgejo, non-profit | Free |

If you self-host Gitea on the same Oracle box, Argo CD can watch that instead
and your entire pipeline depends on nothing external. That is a genuinely
satisfying setup and costs nothing.

---

## 5. Secrets

**Never commit secrets.** `.gitignore` already excludes `.env`.

For Path A: use the platform's environment variable UI. That is what it is for.

For Path B, plain Kubernetes `Secret`s are only base64-encoded — that is not
encryption, and committing one to Git publishes it. Use one of:

- **Sealed Secrets** — encrypt locally, commit the encrypted file safely, the
  controller decrypts in-cluster. Simplest thing that is actually correct.
- **External Secrets Operator** — pulls from a real secret manager.
- **SOPS + age** — encrypt files with a key, works well with GitOps.

Generate the JWT secret properly:

```bash
openssl rand -base64 48
```

Rotating it invalidates every access token, so users are signed out. Refresh
tokens are stored as hashes in Postgres and survive.

---

## 6. Production configuration

Beyond the defaults in `.env.example`:

```bash
APP_ENV=production          # refuses to boot with the example JWT secret
JWT_SECRET=<48 random bytes>
ENABLE_HSTS=true
RATE_LIMIT_ENABLED=true     # never disable outside a load test
CORS_ALLOWED_ORIGINS=https://your-real-domain
```

### The one setting that can be got badly wrong

```bash
TRUST_PROXY_HEADERS=true
```

Enable this **only** when something you control terminates connections and
overwrites `X-Forwarded-For` — a load balancer, ingress-nginx, Cloudflare.

If you enable it while the API is directly reachable, any client can send
`X-Forwarded-For: <anything>` and get a fresh rate-limit bucket on every
request. Rate limiting becomes decorative, and the login endpoint is open to
unlimited password guessing.

The default is `false`. Change it only alongside a proxy, and if you are unsure
whether you have one, you do not.

### Database

- **Enable automated backups.** Neon and Supabase do this; a self-hosted
  Postgres does not. `pg_dump` on a cron to object storage is the minimum.
  Test the restore — an untested backup is a guess.
- **Connection pooling.** The API opens up to 25 connections per instance.
  Managed Postgres free tiers often cap well below that. Either lower
  `MaxConns` in `internal/db/db.go` or put PgBouncer in front.

### Cloudflare in front (free, worth doing)

Point your domain at Cloudflare and enable proxying. You get DDoS protection, a
free WAF, TLS, and caching, without changing any code. It complements the
application-level rate limiting rather than replacing it: Cloudflare stops
floods at the edge, the token buckets stop a *legitimate* user from abusing
specific endpoints.

---

## 7. CI/CD

`.github/workflows/ci.yml` already runs on every push: it boots Postgres and
Redis as services, runs the full test suite under the race detector, runs the
500-request concurrency proof, and verifies the SQL invariants.

That means **your repository publicly demonstrates the zero-double-booking
guarantee on every commit.** For a portfolio project that is the single most
valuable thing in it — point people at the Actions tab, not at a paragraph
claiming it works.

To extend it into deployment:

**Path A:** both Vercel and Koyeb deploy on push automatically once connected.
Nothing to write.

**Path B:** add a job that builds the image, pushes it to GitHub Container
Registry (free for public repos), and updates the image tag in your manifests
repo. Argo CD picks up the change and rolls it out. Do not have CI run
`kubectl apply` directly — that bypasses GitOps and means the cluster no longer
matches Git.

---

## 8. Monitoring

The application logs structured JSON to stdout, which is where every platform
expects it.

What actually matters for *this* system, beyond the usual:

| Signal | Why |
|---|---|
| Hold conflict rate (409s ÷ attempts) | A sudden rise means an event is selling out — or something is broken |
| Confirm latency p95 | The transaction is the critical path |
| Expiry worker lag | If it stalls, bookkeeping drifts from reality |
| Redis availability | Holds fail without it — loudly, but they fail |
| `duplicate confirmed pairs` | Should be zero forever. Alert on any non-zero value. |

That last one is worth a scheduled query. It should never fire. If it ever
does, you have found something genuinely interesting.

Free stack: Prometheus + Grafana in-cluster, or Grafana Cloud's free tier
(10k series). The application does not currently expose a `/metrics` endpoint —
adding one with `prometheus/client_golang` is a contained change and is on the
roadmap in EXPLANATION.md.

---

## 9. Pre-launch checklist

Before anyone else uses it:

- [ ] `JWT_SECRET` is 48 random bytes, not the example value
- [ ] `APP_ENV=production` (this is what enforces the item above)
- [ ] `RATE_LIMIT_ENABLED=true`
- [ ] `TRUST_PROXY_HEADERS` matches reality — `true` only behind a real proxy
- [ ] `CORS_ALLOWED_ORIGINS` lists your domain, not `*`
- [ ] HTTPS everywhere; WebSocket URL is `wss://`
- [ ] Database backups enabled **and a restore tested**
- [ ] Postgres connection ceiling accounts for every replica (PgBouncer if needed)
- [ ] Payments still mocked → the UI says so plainly
- [ ] The duplicate-pairs query is scheduled and alerting
- [ ] Secrets are not in Git

---

## Summary

**To see it live quickly:** Path A. Neon, Upstash, Koyeb, Vercel. About thirty
minutes, free, no infrastructure to run.

**To learn what your organisation actually does:** Path B. Oracle Always Free,
k3s, Argo CD. A weekend, free permanently, and you come out understanding
GitOps rather than having read about it.

**Before real money is involved:** authorise/capture, reconciliation, webhooks,
and a real gateway. Section 2. Do not skip it.
