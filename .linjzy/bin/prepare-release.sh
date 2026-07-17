#!/usr/bin/env bash
set -Eeuo pipefail

SOURCE_DIR="${1:?source directory is required}"
RELEASE_TAG="${2:?release tag is required}"
UPSTREAM_COMMIT="${3:?upstream commit is required}"
SOURCE_REF="${4:?source ref is required}"
PATCH_FILE="${5:-$SOURCE_DIR/.linjzy/patches/usage-logs-auto-refresh.patch}"

log() {
  printf '[prepare-release] %s\n' "$*"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

customization_present() {
  grep -q 'USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS' \
    "$SOURCE_DIR/web/default/src/features/usage-logs/constants.ts" &&
    grep -q 'onCheckedChange={handleAutoRefreshChange}' \
      "$SOURCE_DIR/web/default/src/features/usage-logs/index.tsx" &&
    grep -q 'refetchIntervalInBackground: false' \
      "$SOURCE_DIR/web/default/src/features/usage-logs/components/usage-logs-table.tsx" &&
    grep -q 'USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS' \
      "$SOURCE_DIR/web/default/src/features/usage-logs/components/common-logs-stats.tsx"
}

git -C "$SOURCE_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
  die "source directory is not a Git checkout"
[[ -f "$PATCH_FILE" ]] || die "customization patch not found: $PATCH_FILE"
[[ "$RELEASE_TAG" != *[[:space:]]* ]] || die "invalid release tag"
[[ "$SOURCE_REF" != *[[:space:]]* ]] || die "invalid source ref"

ACTUAL_COMMIT="$(git -C "$SOURCE_DIR" rev-parse HEAD)"
[[ "$ACTUAL_COMMIT" == "$UPSTREAM_COMMIT" ]] ||
  die "source commit mismatch: expected $UPSTREAM_COMMIT, got $ACTUAL_COMMIT"

printf '%s\n' "$RELEASE_TAG" >"$SOURCE_DIR/VERSION"

if customization_present; then
  log "usage-log auto-refresh customization is already present upstream"
else
  git -C "$SOURCE_DIR" apply --check "$PATCH_FILE" ||
    die "patch does not apply directly to $RELEASE_TAG; manual review is required"
  log "applying usage-log auto-refresh patch"
  git -C "$SOURCE_DIR" apply "$PATCH_FILE"
fi

customization_present || die "customization verification failed"
git -C "$SOURCE_DIR" diff --check

PATCH_SHA256="$(sha256sum "$PATCH_FILE" | awk '{print $1}')"
{
  printf 'upstream_repository=%s\n' 'https://github.com/QuantumNous/new-api'
  printf 'upstream_tag=%s\n' "$RELEASE_TAG"
  printf 'upstream_commit=%s\n' "$UPSTREAM_COMMIT"
  printf 'patch_sha256=%s\n' "$PATCH_SHA256"
  printf 'source_ref=%s\n' "$SOURCE_REF"
} >"$SOURCE_DIR/.linjzy/BUILD-METADATA"

log "prepared $RELEASE_TAG at $UPSTREAM_COMMIT"
