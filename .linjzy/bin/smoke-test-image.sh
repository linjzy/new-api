#!/usr/bin/env bash
set -Eeuo pipefail

IMAGE_REF="${1:?image reference is required}"
RELEASE_TAG="${2:?release tag is required}"
CONTAINER_NAME="new-api-smoke-$(date +%s)-$$"

# shellcheck disable=SC2329
cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

json_success() {
  python3 -c '
import json
import sys

try:
    payload = json.load(sys.stdin)
except Exception:
    raise SystemExit(1)
raise SystemExit(0 if payload.get("success") is True else 1)
'
}

VERSION_OUTPUT="$(docker run --rm "$IMAGE_REF" --version 2>&1)"
[[ "$VERSION_OUTPUT" == *"$RELEASE_TAG"* ]] || {
  printf 'image version mismatch: expected %s, got %s\n' \
    "$RELEASE_TAG" "$VERSION_OUTPUT" >&2
  exit 1
}

docker run -d \
  --name "$CONTAINER_NAME" \
  --tmpfs /data:rw,nosuid,size=256m \
  --tmpfs /app/logs:rw,nosuid,size=64m \
  -e TZ=Asia/Shanghai \
  -p 127.0.0.1::3000 \
  "$IMAGE_REF" --log-dir /app/logs >/dev/null

for _ in $(seq 1 90); do
  PORT="$(docker port "$CONTAINER_NAME" 3000/tcp 2>/dev/null |
    sed -n 's/.*://p' | head -n 1)"
  if [[ -n "$PORT" ]] &&
    curl -fsS --connect-timeout 2 --max-time 5 \
      "http://127.0.0.1:${PORT}/api/status" | json_success &&
    curl -fsS --connect-timeout 2 --max-time 5 \
      "http://127.0.0.1:${PORT}/" | grep -q '/static/js/'
  then
    printf 'smoke test passed for %s\n' "$IMAGE_REF"
    exit 0
  fi

  if [[ "$(docker inspect "$CONTAINER_NAME" \
    --format '{{.State.Running}}' 2>/dev/null || true)" != 'true' ]]
  then
    break
  fi
  sleep 2
done

docker logs --tail=100 "$CONTAINER_NAME" || true
printf 'smoke test failed for %s\n' "$IMAGE_REF" >&2
exit 1
