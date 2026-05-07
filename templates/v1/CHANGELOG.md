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

## Patch — UFW must allow docker bridge → host:7681

- Cloud-init's UFW config opened only the externally-facing ports (22 /
  8443 / 9735 + 443/80 with Caddy). Docker bypasses UFW only for
  *published* ports (PREROUTING NAT installs DNAT rules); for a naked
  host service like the finalize daemon, packets from the bridge hit
  the INPUT chain where UFW's default-deny silently drops them. So
  even with the daemon correctly bound to 0.0.0.0:7681, Caddy's
  reverse_proxy `host.docker.internal:7681` got a TCP timeout (not a
  TCP RST) and returned 502 to the browser.
- Added `ufw allow in from 172.16.0.0/12 to any port 7681 proto tcp`
  to the UFW block in cloud-init. 172.16.0.0/12 covers the default
  docker bridge (172.17.0.0/16) plus all compose-created bridges; the
  public NIC is on a different range so the daemon stays internal.

- Cloud-init body is byte-stable for a given set of placeholder substitutions; BoltHub never
  modifies the template.
- `verify.sh` is invoked after every `docker compose pull` and aborts the deploy on mismatch.
- Template placeholders are documented at the top of
  [`node.cloud-init.yaml.tmpl`](./node.cloud-init.yaml.tmpl) and in the renderer source at
  `packages/node-provisioner/src/cloud-init.ts`.
