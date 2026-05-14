# node-launcher-templates v1

Initial release.

## Pinned images

- `lightninglabs/lightning-terminal:v0.16.1-alpha`
- `caddy:2-builder` (xcaddy build stage)
- `caddy:2`

See [`image-digests.json`](./image-digests.json) for SHA-256 digests.

## Patch — verify.sh + Caddy pre-pull

- `verify.sh` now filters by `mediaType` so only docker image-index entries are
  fed into `docker image inspect`. The script previously failed deploys when the
  manifest contained any non-image artefact (e.g. a daemon binary checked via a
  separate sha256 path).
- `node.cloud-init.yaml.tmpl` pre-pulls `caddy:2` and `caddy:2-builder` by digest
  immediately before `verify.sh` runs. They're consumed only as Dockerfile FROM
  bases by `compose up --build`, so `compose pull` skipped them and verify.sh had
  nothing to inspect — every Caddy-enabled deploy aborted with
  `digest_verify_failed`. The pre-pull is gated on `/opt/litd/caddy/Dockerfile`
  existing, so BYO/no-Caddy deploys are unaffected.

## Patch — pin finalize-daemon binary digests

- `image-digests.json` now pins the linux/amd64 and linux/arm64 finalize-daemon
  binaries with `mediaType: application/octet-stream`. The cloud-init's
  daemon-install step already had a sha256 verification block that was a no-op
  whenever the manifest lacked these entries; with them present it now actually
  enforces the pin.
- `scripts/refresh-digests.sh` skips non-image entries so it doesn't try to
  resolve the binary names against Docker Hub.
- `.github/workflows/release.yml` fails the release if the pinned daemon digests
  don't match the freshly-built binaries, so a Go-toolchain drift or stale
  manifest can't ship to production silently.

## Patch — first-prod-deploy plumbing pass

End-to-end finalize never worked on a Caddy-enabled deploy until this
patch — every layer of the network + auth path between the user's
browser and the on-VM daemon had a small bug that only surfaced in a
real prod smoke-test. Five coupled fixes landed together so the
release artifact is internally consistent:

1. **Daemon binds `0.0.0.0:7681` instead of `127.0.0.1:7681`.** Caddy
   runs in a docker container and reaches the host via
   `extra_hosts: host.docker.internal:host-gateway`, which resolves to
   the bridge gateway IP — not loopback. A 127.0.0.1 bind on the host
   is unreachable from the bridge, so `reverse_proxy
   host.docker.internal:7681` got TCP RST.
2. **UFW now allows the docker bridge subnet to reach `:7681`.**
   Docker bypasses UFW only for *published* ports; for a naked host
   service like the daemon, packets from the bridge hit the INPUT
   chain where UFW's default-deny silently drops them. Added
   `ufw allow in from 172.16.0.0/12 to any port 7681 proto tcp` so
   the bridge-resident Caddy can reach the host-resident daemon
   without exposing it to the public NIC.
3. **`BOLTHUB_LND_BASE_URL` flipped from `:8443` to `:8080`.** The
   litd HTTPS listener at 8443 serves the React UI as catch-all and
   does NOT proxy the REST routes the daemon needs — every call to
   `/v1/macaroon` and `/v1/sessions` returned 200 + text/html. The
   real REST gateway is `--lnd.restlisten=0.0.0.0:8080`. Verified by
   curling each port against a fresh litd v0.16.1-alpha.
4. **`BOLTHUB_LND_MACAROON_PATH` points at LND's admin macaroon, not
   `lit.macaroon`.** The daemon talks to LND's REST gateway, which
   sits behind LND's macaroon middleware; LND validates against its
   own root key, not litd's. Litd's super-macaroon failed with
   `signature mismatch after caveat verification`. Correct path is
   `/var/lib/docker/volumes/litd_lnd-data/_data/data/chain/bitcoin/<network>/admin.macaroon`.
5. **Daemon now loads two macaroons.** LND admin authenticates against
   LND-native endpoints (`BakeMacaroon` at `/v1/macaroon`); litd
   super-macaroon authenticates against litd-specific endpoints
   (`CreateLNCSession` at `/v1/sessions`). Litd's session subserver
   has its own macaroon validator with a separate root key, so a
   single macaroon cannot pass both. New env var
   `BOLTHUB_LITD_MACAROON_PATH` carries the litd super-macaroon path,
   default
   `/var/lib/docker/volumes/litd_lit-data/_data/<network>/lit.macaroon`.
   Daemon source change requires a binary rebuild — pinned daemon
   digests in `image-digests.json` updated to the new reproducible
   build values.

This patch supersedes the earlier-open PRs #5, #6, #7, #8 — none of
them were merged individually because they all needed to land
together (and #5 had the wrong macaroon path, since corrected).

## Patch — BakeMacaroon decodes hex correctly (was silently base64-mangling LND's response)

LND's `BakeMacaroonResponse` proto declares the macaroon field as
"hex encoded". The previous `internal/lnd.BakeMacaroon` attempted
base64 decode first and only fell back on parse failure, but **hex
strings are a subset of the base64 alphabet** (`[a-fA-F0-9]`), so
base64 decode silently "succeeds" on a hex input and produces unrelated
bytes. The downstream `hex.EncodeToString` then re-encoded those
garbage bytes, and the macaroon BoltHub stored could never authenticate
against LND — every subsequent `/v1/getinfo` call from BoltHub returned
500 with `cannot determine data format of binary-encoded macaroon`.

Net effect:
- **monitoring macaroon** stored after finalize never worked → BoltHub's
  health-check polling never succeeded → nodes stuck in `syncing`
  state in the dashboard even after LND was fully synced.
- **invoices macaroon** stored after finalize never worked → the
  gateway would have failed to mint L402 invoices once the smoke test
  reached billing flows.

Fix: detect hex first by length parity + alphabet, fall back to base64
for any non-LND gateway that genuinely returns base64, otherwise pass
through unchanged.

Daemon source change → binary rebuild required. Reproducibly built
new digests pinned in `image-digests.json`:

  amd64: 8bac70a1… → ea5a2c53…
  arm64: ac2d69a0… → 858fd641…

Note: existing nodes that were finalized before this patch have
already-mangled macaroons stored in BoltHub's DB — they cannot be
recovered from the bad bytes. Those nodes need to be re-finalized
(or, for full recovery, re-deployed). New finalize calls after this
patch will store correctly-encoded macaroons.

## Patch — verify.sh inspects images by digest, not by tag

- `verify.sh` now does `docker image inspect ${ref}@${expected}` instead of
  `docker image inspect ${ref}` against the bare `name:tag`. Docker does not
  preserve the tag when pulling `name:tag@sha256:<digest>` — only the digest
  survives in the local store, with `RepoTags` empty. So the previous
  tag-based lookup returned "No such image" for every digest-pinned image
  (the lit image pulled by `compose pull`, plus the Caddy bases pre-pulled
  by an earlier patch), reporting them as "no local digest" and aborting
  every Caddy-enabled deploy. Asking docker directly for the digest
  reference is also simpler and answers the question we actually care
  about — "is this exact pinned image present?"

## Patch — bolthub-finalize LNC session scoped to TYPE_MACAROON_CUSTOM

The on-VM daemon previously asked litd for `TYPE_MACAROON_ADMIN` when
minting the `bolthub-finalize` LNC pairing. Admin includes
`onchain:write` (arbitrary `SendCoins`) and `offchain:write` (pay
invoices, force-close channels) — a compromise of the user's browser
`localStorage` (XSS, malicious extension) could drain the node's
on-chain balance or force-close every channel to an attacker-controlled
peer.

This patch narrows the session to `TYPE_MACAROON_CUSTOM` with exactly
the permissions the bolthub dashboard actually uses:

- `info:read`     — getInfo (verify pairing, alias / pubkey)
- `address:write` — newAddress for on-chain deposit
- `peers:read`    — listPeers (confirm LSP peer connect)
- `peers:write`   — connectPeer to the LSP before channel open
- `offchain:read` — exportAllChannelBackups for SCB autobackup
- `onchain:read`  — view-only on-chain state (future surfaces)

A compromised browser can still cause minor mischief (open peer
connections, generate deposit addresses) but cannot move funds. Users
who want full admin access (e.g. Zeus mobile as their primary wallet
UI) mint their own admin session via the LIT Web UI on port 8443 —
that channel is unchanged.

Daemon source change → binary rebuild required. Reproducibly built
new digests pinned in `image-digests.json`:

  amd64: 1d4fdcd8… → b9d5a917…
  arm64: 4160420a… → 002582c5…

## Notes

- Cloud-init body is byte-stable for a given set of placeholder substitutions; BoltHub never
  modifies the template.
- `verify.sh` is invoked after every `docker compose pull` and aborts the deploy on mismatch.
- Template placeholders are documented at the top of
  [`node.cloud-init.yaml.tmpl`](./node.cloud-init.yaml.tmpl) and in the renderer source at
  `packages/node-provisioner/src/cloud-init.ts`.
