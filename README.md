# node-launcher-templates

Versioned, signed cloud-init templates and the on-VM finalize-daemon used by
[BoltHub](https://bolthub.ai) to provision self-custodial Lightning nodes.

This repo is intentionally a separate, public, audit-only artifact. BoltHub's
private platform code does not live here. Anyone — a tenant, a security
researcher, a competing wallet — can clone it, rebuild it, and verify byte-for-
byte that the cloud-init script and finalize-daemon binary running on a given
VM came from a signed release tag of this repo.

## Why a separate repo

A Lightning node holds the user's funds. If BoltHub silently changed
cloud-init, swapped image digests, or added a backdoored finalize-daemon,
the next time the user re-deployed they could be handing the keys to an
attacker without ever seeing the diff.

This repo closes that hole:

1. **Append-only versions.** `templates/v1/`, `templates/v2/`, … We never
   edit a published version. To fix a bug we ship `vN+1` and bump nodes
   on next re-deploy.
2. **Signed releases.** Every release tag (`templates/v1`, `templates/v2`, …)
   ships a `SHA256SUMS` file covering every artifact, signed by cosign in
   keyless mode against a GitHub Actions OIDC identity rooted in this repo.
3. **Pinned image digests.** `image-digests.json` lists the exact
   `sha256:…` for every container image cloud-init pulls. `verify.sh`
   re-checks them after every `docker compose pull`.
4. **Signed finalize-daemon binary.** The daemon is built reproducibly
   in CI from this repo and listed in the same `SHA256SUMS`. Cloud-init
   refuses to start the daemon if its on-disk SHA-256 does not match.
5. **Provenance recorded per-node.** Every node row in BoltHub's database
   stores the resolved `template_version`, `template_sha256`, and
   `image_digests`. The dashboard surfaces them and `/.well-known/bolthub/v1/info`
   on the VM exposes the live values for cross-checking.

If the SHA-256 the dashboard reports for a node does not appear on a
release of this repo, that node was provisioned with an unaudited template.
Treat it as a compromise indication and migrate funds off.

## Layout

```
templates/
  v1/
    node.cloud-init.yaml.tmpl  static template body, {{PLACEHOLDER}} substitution only
    image-digests.json         pinned sha256 digest for every container image
    verify.sh                  on-VM digest verifier (run after every pull)
    SHA256SUMS                 signed list of file SHA-256s (templates AND daemon binary)
    CHANGELOG.md               version history

finalize-daemon/             Go service that runs on every tenant VM
  cmd/finalize-daemon/         entrypoint
  internal/tokens/             HMAC one-time-token verification + replay defense
  internal/lnd/                local litd REST client
  internal/handler/            HTTP handler

scripts/
  refresh-digests.sh           helper: re-resolve image-digests.json upstream

cosign.pub                   cosign public key (or empty placeholder if release uses keyless)
SIGNING.md                   how releases are signed and how to verify them
```

## Verifying a release

```bash
git clone https://github.com/signaltech-org/bolthub-node-launcher-templates
cd node-launcher-templates

# 1. Pull a specific release's signed manifest
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/SHA256SUMS
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/SHA256SUMS.bundle

# 2. Verify the signature was made by THIS repo's CI
cosign verify-blob \
  --bundle SHA256SUMS.bundle \
  --certificate-identity-regexp 'https://github.com/signaltech-org/bolthub-node-launcher-templates/.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  SHA256SUMS

# 3. Pull every artifact and confirm it matches the signed manifest
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/node.cloud-init.yaml.tmpl
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/image-digests.json
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/verify.sh
curl -fsSLO https://github.com/signaltech-org/bolthub-node-launcher-templates/releases/download/templates/v1/bolthub-finalize-daemon-linux-amd64
sha256sum -c SHA256SUMS
```

If any line of `sha256sum -c` says `FAILED`, the artifact does not match
what was signed — do not deploy it.

## Placeholders consumed by BoltHub's renderer

The cloud-init body never contains conditional logic. Every `{{NAME}}`
slot is substituted as a pre-rendered string by BoltHub's caller, so the
template body is byte-stable regardless of the operator's choices. The
slots in `templates/v1/node.cloud-init.yaml.tmpl` are:

| Slot                         | Purpose |
| ---------------------------- | ------- |
| `{{SSH_KEY_BLOCK}}`          | optional `ssh_authorized_keys` block (operator BYO key) |
| `{{TOR_PACKAGES}}`           | extra apt packages when Tor mode is on |
| `{{UFW_CADDY_PORTS}}`        | extra ufw rules opening Caddy 80/443 in clearnet mode |
| `{{TOR_COMMANDS}}`           | extra runcmd lines that install/configure Tor |
| `{{NODE_FQDN}}`              | the node's public clearnet FQDN, or `.onion` |
| `{{LND_NETWORK}}`            | `mainnet` / `testnet` / `signet` |
| `{{LITD_PASSWORD_HASH}}`     | argon2id hash of the litd UI password |
| `{{LITD_PASSWORD_SALT}}`     | argon2id salt the daemon validates against |
| `{{BOLTHUB_NODE_ID}}`        | the node's BoltHub UUID |
| `{{BOLTHUB_API_BASE_URL}}`   | the BoltHub API origin the daemon phones home to |
| `{{BOLTHUB_WEBHOOK_SECRET}}` | per-node HMAC secret shared with BoltHub |
| `{{TEMPLATE_VERSION}}`       | the version directory name (`v1`, `v2`, …) |
| `{{FINALIZE_DAEMON_URL_BASE}}` | release-asset base URL for the daemon binary |

## Adding a new template version

1. Copy `templates/v<latest>/` to `templates/v<latest+1>/`.
2. Edit `node.cloud-init.yaml.tmpl` and bump `image-digests.json` via
   `scripts/refresh-digests.sh`.
3. Update `templates/v<latest+1>/CHANGELOG.md`.
4. Open a PR; CI runs shellcheck + `jq` + `go vet` + `go test`.
5. After merge, push tag `templates/v<latest+1>`. The release workflow
   builds the daemon for linux/amd64 + linux/arm64, generates and
   cosign-signs `SHA256SUMS`, and uploads everything to a GitHub release.

## Reporting issues

Security issues should go to security@bolthub.ai (PGP fingerprint
published in the main BoltHub repo's `SECURITY.md`). Public bug reports
in this repo's GitHub Issues are fine for everything else.

## License

MIT — see [`LICENSE`](./LICENSE).
