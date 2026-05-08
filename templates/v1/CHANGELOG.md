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

## Patch — point daemon at LND REST gateway (8080), not litd HTTPS UI (8443)

- Cloud-init's systemd unit set `BOLTHUB_LND_BASE_URL=https://127.0.0.1:8443`,
  the litd HTTPS listener. That listener serves the React UI as the
  catch-all and does NOT proxy the REST routes the daemon needs:

  - POST /v1/macaroon  → 200 text/html (UI fallback)
  - POST /v1/sessions  → 200 text/html (UI fallback)

- The actual REST gateway is `--lnd.restlisten=0.0.0.0:8080` inside the
  litd container. Both LND-native endpoints (BakeMacaroon) AND
  litd-specific endpoints (CreateLNCSession) are served there in
  integrated mode. Verified by curling each port against a fresh litd
  v0.16.1-alpha; 8080 returned application/json, 8443 returned
  text/html for the same paths.
- Net effect: the daemon's `BakeMacaroon` was parsing an HTML body as
  JSON and returning `bad response: invalid character '<' looking for
  beginning of value`, which the handler turned into an opaque 502
  "lnd: bake monitoring failed" toast in the dashboard.
- The host-side port publishing for 8080 is a consumer-renderer
  concern (template just has `{{LITD_HOST_PORTS}}` placeholder).
  Consumers should publish `127.0.0.1:8080:8080` on host loopback so
  the daemon (host systemd service) can reach the REST gateway without
  exposing it externally.

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

## Notes

- Cloud-init body is byte-stable for a given set of placeholder substitutions; BoltHub never
  modifies the template.
- `verify.sh` is invoked after every `docker compose pull` and aborts the deploy on mismatch.
- Template placeholders are documented at the top of
  [`node.cloud-init.yaml.tmpl`](./node.cloud-init.yaml.tmpl) and in the renderer source at
  `packages/node-provisioner/src/cloud-init.ts`.
