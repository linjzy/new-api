#!/usr/bin/env bash
set -Eeuo pipefail

SOURCE_DIR="${1:?source directory is required}"
RELEASE_TAG="${2:?release tag is required}"
UPSTREAM_COMMIT="${3:?upstream commit is required}"
SOURCE_REF="${4:?source ref is required}"
USAGE_PATCH_FILE="${5:-$SOURCE_DIR/.linjzy/patches/usage-logs-auto-refresh.patch}"
ANTHROPIC_PATCH_FILE="${6:-$SOURCE_DIR/.linjzy/patches/anthropic-buffered-nonstream.patch}"
CHANNEL_TEST_PATCH_FILE="${7:-$SOURCE_DIR/.linjzy/patches/channel-test-responses-policy.patch}"

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
    grep -q 'onCheckedChange={handleAutoRefreshChange}' \
      "$SOURCE_DIR/web/src/features/usage-logs/index.tsx" &&
    grep -q 'refetchIntervalInBackground: false' \
      "$SOURCE_DIR/web/src/features/usage-logs/components/usage-logs-table.tsx" &&
    grep -q 'USAGE_LOGS_AUTO_REFRESH_INTERVAL_MS' \
      "$SOURCE_DIR/web/src/features/usage-logs/components/common-logs-stats.tsx"
}

anthropic_customization_present() {
  [[ -f "$SOURCE_DIR/relay/channel/claude/buffered_stream.go" ]] &&
    grep -q 'func ClaudeBufferedStreamHandler' \
    "$SOURCE_DIR/relay/channel/claude/buffered_stream.go" &&
    grep -q 'return ClaudeBufferedStreamHandler' \
      "$SOURCE_DIR/relay/channel/claude/adaptor.go" &&
    grep -q 'StopSequence.*stop_sequence' \
      "$SOURCE_DIR/dto/claude.go"
}

channel_test_customization_present() {
  grep -q 'ShouldChatCompletionsUseResponsesGlobal' \
    "$SOURCE_DIR/controller/channel-test.go" &&
    [[ -f "$SOURCE_DIR/controller/channel_test_endpoint_test.go" ]] &&
    grep -q 'TestNormalizeChannelTestEndpointUsesResponsesCompatibilityPolicy' \
      "$SOURCE_DIR/controller/channel_test_endpoint_test.go"
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
[[ -f "$ANTHROPIC_PATCH_FILE" ]] || die "customization patch not found: $ANTHROPIC_PATCH_FILE"
[[ -f "$CHANNEL_TEST_PATCH_FILE" ]] || die "customization patch not found: $CHANNEL_TEST_PATCH_FILE"
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
  'Anthropic buffered non-stream' \
  "$ANTHROPIC_PATCH_FILE" \
  anthropic_customization_present
apply_customization \
  'channel-test Responses policy' \
  "$CHANNEL_TEST_PATCH_FILE" \
  channel_test_customization_present
git -C "$SOURCE_DIR" diff --check

PATCH_SHA256="$(
  sha256sum "$USAGE_PATCH_FILE" "$ANTHROPIC_PATCH_FILE" "$CHANNEL_TEST_PATCH_FILE" |
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
