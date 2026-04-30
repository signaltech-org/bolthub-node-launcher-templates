#!/usr/bin/env bash
# Refresh image-digests.json for the latest version directory by querying the
# Docker Hub registry. Run before cutting a new template release.
#
# Usage:
#   ./scripts/refresh-digests.sh templates/v1/image-digests.json
#
# Requires: jq, curl
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "usage: $0 <path/to/image-digests.json>" >&2
  exit 1
fi

FILE="$1"
TMP=$(mktemp)
cp "$FILE" "$TMP"

mapfile -t IMAGES < <(jq -r '.images | keys[]' "$FILE")

for ref in "${IMAGES[@]}"; do
  # Split into name + tag (assume single ':' on tag)
  name="${ref%:*}"
  tag="${ref##*:}"
  # Library images need /library/ prefix
  if [[ "$name" != */* ]]; then
    repo_path="library/${name}"
  else
    repo_path="$name"
  fi

  echo "[refresh] querying $ref ..." >&2
  resp=$(curl -fsSL "https://hub.docker.com/v2/repositories/${repo_path}/tags/${tag}")

  index_digest=$(echo "$resp" | jq -r '.digest')
  if [ -z "$index_digest" ] || [ "$index_digest" = "null" ]; then
    echo "[refresh] FATAL: no .digest for $ref" >&2
    exit 1
  fi

  amd64_digest=$(echo "$resp" | jq -r '.images[] | select(.architecture=="amd64" and .os=="linux" and (.variant==null or .variant=="")) | .digest' | head -n1)
  arm64_digest=$(echo "$resp" | jq -r '.images[] | select(.architecture=="arm64" and .os=="linux") | .digest' | head -n1)

  jq --arg ref "$ref" \
     --arg idx "$index_digest" \
     --arg amd "$amd64_digest" \
     --arg arm "$arm64_digest" \
     '.images[$ref].digest = $idx
      | .images[$ref].platforms = [
          {os:"linux", architecture:"amd64", digest:$amd},
          {os:"linux", architecture:"arm64", digest:$arm}
        ]' "$TMP" > "${TMP}.next"
  mv "${TMP}.next" "$TMP"
done

mv "$TMP" "$FILE"
echo "[refresh] $FILE updated"
