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

## Patch — finalize-daemon macaroon path

- Cloud-init's systemd unit pointed `BOLTHUB_LND_MACAROON_PATH` at
  `/var/lib/docker/volumes/litd_lit-data/_data/admin.macaroon`. Litd
  v0.16.x actually writes its macaroon to `<lit-dir>/<network>/lit.macaroon`
  (per litd's `DefaultMacaroonFilename = "lit.macaroon"` and
  `DefaultMacaroonPath = filepath.Join(DefaultLitDir, DefaultNetwork, ...)`),
  which on the host via the `lit-data` volume is
  `/var/lib/docker/volumes/litd_lit-data/_data/mainnet/lit.macaroon`. Two
  errors in one path: wrong filename, and missing the `mainnet/` segment.
- Net effect: the daemon's startup `os.ReadFile()` returned ENOENT,
  `log.Fatalf` fired, systemd restarted every 5s under
  `Restart=on-failure / RestartSec=5`, the daemon was never running when
  the user clicked "I have created my wallet", and Caddy returned 502 on
  every browser POST to `/.well-known/bolthub/v1/finalize`. Bug had been
  present since the daemon was added; only surfaced when a real prod
  deploy got far enough through the flow to invoke finalize.
- The daemon binary is unchanged — only the env var injected by cloud-init
  changed, so this patch does not require a daemon-source rebuild and the
  pinned binary digests in `image-digests.json` stay valid.

- Cloud-init body is byte-stable for a given set of placeholder substitutions; BoltHub never
  modifies the template.
- `verify.sh` is invoked after every `docker compose pull` and aborts the deploy on mismatch.
- Template placeholders are documented at the top of
  [`node.cloud-init.yaml.tmpl`](./node.cloud-init.yaml.tmpl) and in the renderer source at
  `packages/node-provisioner/src/cloud-init.ts`.
