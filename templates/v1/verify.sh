#!/usr/bin/env bash
# bolthub node-launcher-templates v1 — on-VM digest + signature verifier
#
# Verifies two things, in order:
#
# 1. The pinned image manifest at /opt/bolthub/image-digests.json was not
#    tampered with (optional cosign signature check, only if cosign + the
#    public key + a SHA256SUMS file are present).
# 2. Every image in that manifest is actually present locally with a matching
#    multi-arch index digest. Run after `docker compose pull` so a poisoned
#    mirror or a silently retagged image is detected before LND stores funds.
#
# Exit codes:
#   0 — all checks passed
#   1 — a mismatch, missing dependency, or signature failure. cloud-init
#       treats this as a deploy abort.
set -euo pipefail

DIGESTS_FILE="${BOLTHUB_DIGESTS_FILE:-/opt/bolthub/image-digests.json}"
SUMS_FILE="${BOLTHUB_SUMS_FILE:-/opt/bolthub/SHA256SUMS}"
SUMS_SIG="${BOLTHUB_SUMS_SIG:-/opt/bolthub/SHA256SUMS.sig}"
SUMS_BUNDLE="${BOLTHUB_SUMS_BUNDLE:-/opt/bolthub/SHA256SUMS.bundle}"
COSIGN_PUB="${BOLTHUB_COSIGN_PUB:-/opt/bolthub/cosign.pub}"

if ! command -v jq >/dev/null 2>&1; then
  echo "[bolthub-verify] FATAL: jq not installed" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "[bolthub-verify] FATAL: docker not installed" >&2
  exit 1
fi

if [ ! -r "$DIGESTS_FILE" ]; then
  echo "[bolthub-verify] FATAL: $DIGESTS_FILE missing or unreadable" >&2
  exit 1
fi

# Optional: verify cosign signature on SHA256SUMS, then check the digests
# file's hash against SHA256SUMS. Only enforced when all of cosign + the
# public key + SHA256SUMS + a signature/bundle are present on the VM, so
# that the basic digest check still works in environments that have not
# rolled the cosign material out yet (e.g. development).
if command -v cosign >/dev/null 2>&1 \
   && [ -r "$COSIGN_PUB" ] \
   && [ -r "$SUMS_FILE" ] \
   && { [ -r "$SUMS_BUNDLE" ] || [ -r "$SUMS_SIG" ]; }; then
  echo "[bolthub-verify] cosign material present, verifying SHA256SUMS"
  if [ -r "$SUMS_BUNDLE" ]; then
    cosign verify-blob \
      --key "$COSIGN_PUB" \
      --bundle "$SUMS_BUNDLE" \
      "$SUMS_FILE" >&2
  else
    cosign verify-blob \
      --key "$COSIGN_PUB" \
      --signature "$SUMS_SIG" \
      "$SUMS_FILE" >&2
  fi

  expected_sum=$(awk '$2 == "image-digests.json" { print $1 }' "$SUMS_FILE")
  actual_sum=$(sha256sum "$DIGESTS_FILE" | awk '{print $1}')
  if [ -z "$expected_sum" ] || [ "$expected_sum" != "$actual_sum" ]; then
    echo "[bolthub-verify] FATAL: $DIGESTS_FILE does not match signed SHA256SUMS" >&2
    echo "[bolthub-verify]   expected $expected_sum" >&2
    echo "[bolthub-verify]   actual   $actual_sum" >&2
    exit 1
  fi
  echo "[bolthub-verify] image-digests.json signature ok"
else
  echo "[bolthub-verify] cosign / SHA256SUMS not installed, skipping signature check"
fi

echo "[bolthub-verify] using $DIGESTS_FILE"

FAILED=0
while IFS=$'\t' read -r ref expected; do
  if [ -z "$ref" ] || [ -z "$expected" ]; then
    continue
  fi

  actual=$(docker image inspect "$ref" --format '{{index .RepoDigests 0}}' 2>/dev/null | awk -F'@' '{print $2}')

  if [ -z "$actual" ]; then
    echo "[bolthub-verify] $ref: no local digest (not pulled?)" >&2
    FAILED=1
    continue
  fi

  if [ "$actual" != "$expected" ]; then
    echo "[bolthub-verify] $ref: MISMATCH" >&2
    echo "[bolthub-verify]   expected $expected" >&2
    echo "[bolthub-verify]   actual   $actual" >&2
    FAILED=1
  else
    echo "[bolthub-verify] $ref: ok ($expected)"
  fi
done < <(jq -r '.images | to_entries[] | "\(.key)\t\(.value.digest)"' "$DIGESTS_FILE")

if [ "$FAILED" -ne 0 ]; then
  echo "[bolthub-verify] FAIL: one or more pinned digests do not match" >&2
  exit 1
fi

echo "[bolthub-verify] OK: all pinned digests match"
