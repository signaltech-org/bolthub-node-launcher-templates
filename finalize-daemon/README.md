# bolthub finalize-daemon

A small Go service that runs on the user's Lightning node VM. Cloud-init
starts it as a systemd unit (`bolthub-finalize.service`) and Caddy reverse
proxies `/.well-known/bolthub/*` to it on loopback.

Its only job is to take BoltHub's server out of the post-boot finalize
loop:

1. Cloud-init writes a per-node webhook secret onto the VM (the same one
   BoltHub uses for inbound webhooks). The daemon reads it from
   `BOLTHUB_WEBHOOK_SECRET`.
2. The browser fetches a one-time-token from BoltHub
   (`GET /nodes/:id/finalize-handover`) — token is signed with the
   webhook secret and is valid for 10 minutes.
3. The browser POSTs the token to
   `https://<node-fqdn>/.well-known/bolthub/v1/finalize` (this daemon).
4. The daemon validates the token, asks the local litd to:
   - bake a read-only **monitoring** macaroon, and
   - bake a mint-only **invoices** macaroon, and
   - mint an admin **LNC pairing phrase**.
5. The daemon phones the two scoped macaroons home to the BoltHub API
   (signed with HMAC over the JSON body using the same webhook secret),
   so BoltHub can persist them for dashboard / health-check use.
6. The daemon returns the **LNC pairing phrase** to the browser. **It is
   never sent home.** The browser stores it locally (see
   `apps/web/src/stores/node-lnc.ts`).

## Layout

- `cmd/finalize-daemon` — main entrypoint
- `internal/tokens` — HMAC verification + replay-prevention store
- `internal/lnd` — tiny REST client to local litd
- `internal/handler` — HTTP routes; takes interfaces so tests don't need litd

## Endpoints

| Method | Path             | Auth        | Action                                   |
| ------ | ---------------- | ----------- | ---------------------------------------- |
| GET    | `/healthz`       | none        | liveness                                 |
| POST   | `/v1/finalize`   | one-time    | bake macaroons + LNC; phone home; return |

## Build

```sh
make build  # produces dist/bolthub-finalize-daemon-linux-{amd64,arm64}
```

Releases of this repo (`signaltech-org/bolthub-node-launcher-templates`) ship
the matching binary alongside `image-digests.json` under the same
`templates/vN` tag. The daemon binary's SHA-256 is included in the
release `SHA256SUMS` (cosign-signed), and cloud-init refuses to start
the daemon if the on-disk SHA-256 does not match.

## Required environment

| Variable                       | Description                                       |
| ------------------------------ | ------------------------------------------------- |
| `BOLTHUB_NODE_ID`              | the node's UUID                                   |
| `BOLTHUB_WEBHOOK_SECRET`       | shared HMAC secret with BoltHub                   |
| `BOLTHUB_CALLBACK_URL`         | where to POST the baked macaroons                 |
| `BOLTHUB_LISTEN`               | listen addr (default `127.0.0.1:7681`)            |
| `BOLTHUB_LND_BASE_URL`         | local litd REST URL (default `https://127.0.0.1:8443`) |
| `BOLTHUB_LND_MACAROON_PATH`    | admin macaroon path on disk                       |
| `BOLTHUB_CONSUMED_TOKENS_PATH` | append-only consumed-token log (replay defense)   |
