#!/usr/bin/env bash
set -Eeuo pipefail

SOURCE_DIR="${1:?source directory is required}"
RELEASE_TAG="${2:?release tag is required}"
UPSTREAM_COMMIT="${3:?upstream commit is required}"
SOURCE_REF="${4:?source ref is required}"
USAGE_PATCH_FILE="${5:-$SOURCE_DIR/.linjzy/patches/usage-logs-auto-refresh.patch}"
SEQUENTIAL_PATCH_FILE="${6:-$SOURCE_DIR/.linjzy/patches/sequential-key-mode.patch}"
RESPONSES_CAPACITY_PATCH_FILE="${7:-$SOURCE_DIR/.linjzy/patches/responses-capacity-retry.patch}"

log() {
  printf '[prepare-release] %s\n' "$*"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

usage_customization_present() {
  grep -q 'USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS' \
    "$SOURCE_DIR/web/src/features/usage-logs/constants.ts" &&
    grep -q 'getUsageLogsRefetchInterval' \
      "$SOURCE_DIR/web/src/features/usage-logs/lib/auto-refresh.ts" &&
    grep -q 'onCheckedChange={handleAutoRefreshChange}' \
      "$SOURCE_DIR/web/src/features/usage-logs/index.tsx" &&
    grep -q 'USAGE_LOGS_AUTO_REFRESH_ERROR_TOAST_ID' \
      "$SOURCE_DIR/web/src/features/usage-logs/components/usage-logs-table.tsx" &&
    grep -q 'USAGE_LOGS_AUTO_REFRESH_ERROR_TOAST_ID' \
      "$SOURCE_DIR/web/src/features/usage-logs/components/common-logs-stats.tsx" &&
    grep -q 'const streamErrors' \
      "$SOURCE_DIR/web/src/features/usage-logs/components/dialogs/details-dialog.tsx"
}

sequential_customization_present() {
  grep -q 'MultiKeyModeSequential' \
    "$SOURCE_DIR/constant/multi_key_mode.go" &&
    grep -q 'func SequentialKeyAutoSkip' \
      "$SOURCE_DIR/service/channel.go" &&
    grep -q 'HasSequentialChannel' \
      "$SOURCE_DIR/controller/relay.go" &&
    grep -q 'p.ResetRetryNextTry()' \
      "$SOURCE_DIR/service/channel_select.go" &&
    grep -q 'UpdateChannelStatusByKeyIndex' "$SOURCE_DIR/model/channel.go" &&
    grep -q 'EnableAutoDisabledChannelKey' "$SOURCE_DIR/model/channel.go" &&
    grep -q 'SetupContextForAutoDisabledChannelTestKey' \
      "$SOURCE_DIR/middleware/distributor.go" &&
    grep -q "value: 'sequential'" \
      "$SOURCE_DIR/web/src/features/channels/components/drawers/channel-mutate-drawer.tsx"
}

responses_capacity_customization_present() {
  git -C "$SOURCE_DIR" apply --reverse --check "$RESPONSES_CAPACITY_PATCH_FILE" \
    >/dev/null 2>&1
}

apply_customization() {
  local name="$1"
  local patch_file="$2"
  local verification_function="$3"

  if "$verification_function"; then
    log "$name customization is already present upstream"
    return
  fi

  git -C "$SOURCE_DIR" apply --check "$patch_file" ||
    die "$name patch does not apply directly to $RELEASE_TAG; manual review is required"
  log "applying $name patch"
  git -C "$SOURCE_DIR" apply "$patch_file"
  "$verification_function" || die "$name customization verification failed"
}

git -C "$SOURCE_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1 ||
  die "source directory is not a Git checkout"
[[ -f "$USAGE_PATCH_FILE" ]] || die "customization patch not found: $USAGE_PATCH_FILE"
[[ -f "$SEQUENTIAL_PATCH_FILE" ]] || die "customization patch not found: $SEQUENTIAL_PATCH_FILE"
[[ -f "$RESPONSES_CAPACITY_PATCH_FILE" ]] || die "customization patch not found: $RESPONSES_CAPACITY_PATCH_FILE"
[[ "$RELEASE_TAG" != *[[:space:]]* ]] || die "invalid release tag"
[[ "$SOURCE_REF" != *[[:space:]]* ]] || die "invalid source ref"

ACTUAL_COMMIT="$(git -C "$SOURCE_DIR" rev-parse HEAD)"
[[ "$ACTUAL_COMMIT" == "$UPSTREAM_COMMIT" ]] ||
  die "source commit mismatch: expected $UPSTREAM_COMMIT, got $ACTUAL_COMMIT"

printf '%s\n' "$RELEASE_TAG" >"$SOURCE_DIR/VERSION"

apply_customization \
  'usage-log auto-refresh' \
  "$USAGE_PATCH_FILE" \
  usage_customization_present
apply_customization \
  'sequential multi-key mode' \
  "$SEQUENTIAL_PATCH_FILE" \
  sequential_customization_present
apply_customization \
  'Responses capacity retry before output' \
  "$RESPONSES_CAPACITY_PATCH_FILE" \
  responses_capacity_customization_present
git -C "$SOURCE_DIR" diff --check

PATCH_SHA256="$(
  sha256sum "$USAGE_PATCH_FILE" "$SEQUENTIAL_PATCH_FILE" "$RESPONSES_CAPACITY_PATCH_FILE" |
    awk '{print $1}' |
    sha256sum |
    awk '{print $1}'
)"
{
  printf 'upstream_repository=%s\n' 'https://github.com/QuantumNous/new-api'
  printf 'upstream_tag=%s\n' "$RELEASE_TAG"
  printf 'upstream_commit=%s\n' "$UPSTREAM_COMMIT"
  printf 'patch_sha256=%s\n' "$PATCH_SHA256"
  printf 'source_ref=%s\n' "$SOURCE_REF"
} >"$SOURCE_DIR/.linjzy/BUILD-METADATA"

log "prepared $RELEASE_TAG at $UPSTREAM_COMMIT"
