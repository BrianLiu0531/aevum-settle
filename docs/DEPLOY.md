# Deploying a public demo

Three ways, from least to most setup. All of them need the router deployed on QDay first
(`cd contracts && npm run deploy`, see the README).

## 1. Any machine with Docker

```bash
cp .env.example .env        # fill in keys, ROUTER_ADDRESS, START_BLOCK, WEBHOOK_SECRET
docker compose --profile demo up -d --build
```

Settler on `:8080` (dashboard `/`, owner console `/console`), merchant on `:8787`. Put both behind
any HTTPS reverse proxy (Caddy: `reverse_proxy localhost:8080`) to share them. Merchant invoices
persist in the `merchant-data` volume and settler state in `pgdata`.

## 2. Render (Blueprint, free tier)

[`render.yaml`](../render.yaml) defines Postgres, the settler and the merchant.

1. Push the repo to GitHub, then in Render: **New → Blueprint** and pick the repo.
2. Render asks for the values marked `sync: false`:

   | Service | Variable | Value |
   |---|---|---|
   | aevum-settler | `ROUTER_ADDRESS`, `START_BLOCK` | from `npm run deploy` |
   | aevum-settler | `WEBHOOK_URL` | `https://aevum-merchant-XXXX.onrender.com/webhooks/settle` |
   | aevum-merchant | `ROUTER_ADDRESS` | same as above |
   | aevum-merchant | `MERCHANT_ADDRESS` | the address that gets paid |
   | aevum-merchant | `MERCHANT_PRIVATE_KEY` | optional: enables x402 `exact` and refunds; needs a little tQDAY |
   | aevum-merchant | `SETTLER_URL` | `https://aevum-settler-XXXX.onrender.com` |
   | aevum-merchant | `MERCHANT_PUBLIC_URL` | `https://aevum-merchant-XXXX.onrender.com` |

   `WEBHOOK_SECRET` is generated once and shared by both services; `MERCHANT_ADMIN_TOKEN` (for
   the refund endpoint) is generated too.
3. Point the agent at it: `MERCHANT_URL=https://aevum-merchant-XXXX.onrender.com node demo/agent.js scenarios`.

Free-tier notes: web services sleep after ~15 minutes without traffic. The settler is reorg-safe
and catches up from its cursor when it wakes, but webhooks wait until then, so use a paid instance
for the settler if that matters. The free disk is ephemeral, so merchant invoices reset on each
deploy (the settler's payments are in Postgres and survive). Free Postgres expires after 30 days.

## 3. Your own Kubernetes / VM

The settler is a single static binary (`settler/Dockerfile`, distroless, non-root) configured
only by environment variables, with `/healthz` for liveness and `/metrics` for Prometheus. Run
one replica per chain: a second instance is safe (writes are serialised by an advisory lock and
cursor check) but does no extra work. The merchant is a stateless Node process apart from its
SQLite file; mount `/app/data` on a volume.
