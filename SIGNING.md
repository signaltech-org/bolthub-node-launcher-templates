# Signing template + daemon releases with cosign

Every published `templates/vN` release ships a `SHA256SUMS` file that
covers ALL artifacts in the release — the cloud-init template, the
image-digests manifest, the verify script, the changelog, AND the
finalize-daemon binaries for `linux-amd64` and `linux-arm64`. That
single `SHA256SUMS` is signed once with cosign, so verifying any one
artifact in a release reduces to:

```bash
sha256sum -c SHA256SUMS
```

after the signature on `SHA256SUMS` has been verified.

## How releases are signed

Releases are signed in GitHub Actions by the workflow at
[`.github/workflows/release.yml`](./.github/workflows/release.yml) using
**cosign keyless mode** (Sigstore). There is no long-lived signing key.
Instead, each signature carries a Sigstore certificate proving the
signing identity is *this workflow run, in this repository*. The
certificate is bundled into `SHA256SUMS.bundle`.

This means:

- **There is no private key in BoltHub's possession to steal.** A
  rogue insider would need to push a malicious tag to this public repo
  and have CI build it. That action is logged in the GitHub audit log
  and visible in the public Actions UI.
- **Verification does not require trusting BoltHub's domain or DNS.**
  It only requires trusting (a) Sigstore's root CA and (b) that the
  signing identity is `https://github.com/signaltech-org/bolthub-node-launcher-templates/...`.

## Verifying a release locally

```bash
cd templates/v1   # or download SHA256SUMS + SHA256SUMS.bundle from the release

cosign verify-blob \
  --bundle SHA256SUMS.bundle \
  --certificate-identity-regexp 'https://github.com/signaltech-org/bolthub-node-launcher-templates/.*' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  SHA256SUMS

# Then verify the SHA256SUMS file against the actual files
sha256sum -c SHA256SUMS
```

If `cosign verify-blob` exits 0 and `sha256sum -c` reports `OK` for
every line, the release is authentic.

## Optional: pinning a long-lived public key

For environments without internet access to Sigstore's roots, BoltHub
can additionally publish a long-lived cosign keypair and double-sign
each release with it. The public key is at the repo root as
[`cosign.pub`](./cosign.pub).

> **Status:** the committed `cosign.pub` is currently a placeholder.
> Until a real keypair is generated and the workflow is updated to
> double-sign, the keyless flow above is the only authoritative path.
> This is intentional for v1 — keyless avoids a private key handling
> burden and is verifiable by anyone with public internet access.

## On-VM verification

`verify.sh` (shipped under `templates/vN/`) is also installed on every
node at `/opt/bolthub/verify.sh` by cloud-init, and is executed:

1. After every `docker compose pull`, to re-confirm pinned image digests; and
2. Optionally as a periodic systemd timer (operator-managed) to detect drift.

If the user wants to additionally verify the template they got is the
one BoltHub claims to have rolled out:

```bash
# On the VM
cat /opt/bolthub/template-version    # e.g. v1
cat /opt/bolthub/template-sha256     # sha256 of the template body the renderer emitted

# Locally, against the matching release tag in this repo
shasum -a 256 templates/v1/node.cloud-init.yaml.tmpl
```

The two SHA-256 values must match exactly. If they do not, the node was
provisioned with a template that does not appear in the public repo —
treat that as a compromise indication and migrate funds off the node.
