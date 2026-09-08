#!/usr/bin/env bash
set -Eeuo pipefail
IMAGE_REF="${1:?image reference is required}"
if result="$(docker buildx imagetools inspect "$IMAGE_REF" 2>&1)"; then
  digest="$(awk '/^Digest:/ {print $2; exit}' <<< "$result")"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    printf 'registry returned no valid digest for %s\n' "$IMAGE_REF" >&2
    exit 1
  }
  printf '%s\n' "$digest"
else
  # Only an explicit missing manifest is a cache miss. Transport/auth failures
  # must stop the job rather than rebuild and overwrite an immutable tag.
  case "$result" in
    *MANIFEST_UNKNOWN*|*'manifest unknown'*|*'manifest not found'*|*"$IMAGE_REF: not found"*) ;;
    *) printf '%s\n' "$result" >&2; exit 1 ;;
  esac
fi
