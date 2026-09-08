#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

SCRIPT_PATH="$(readlink -f "${BASH_SOURCE[0]}")"
PACKAGE_DIR="$(cd "$(dirname "$SCRIPT_PATH")/.." && pwd)"
CONFIG_FILE="${NEWAPI_CUSTOM_CONFIG_FILE:-$PACKAGE_DIR/registry.env}"

if [[ -f "$CONFIG_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$CONFIG_FILE"
fi

IMAGE_REPOSITORY="${NEWAPI_IMAGE_REPOSITORY:-}"
SOURCE_REPOSITORY="${NEWAPI_SOURCE_REPOSITORY:-}"
CANDIDATE_TAG="${NEWAPI_CANDIDATE_TAG:-candidate}"

BASE_DIR="${NEWAPI_CUSTOM_BASE_DIR:-$PACKAGE_DIR}"
STATE_DIR="${NEWAPI_CUSTOM_STATE_DIR:-$BASE_DIR/state}"
LOG_DIR="${NEWAPI_CUSTOM_LOG_DIR:-$BASE_DIR/logs}"

DEPLOY_DIR="${NEWAPI_DEPLOY_DIR:-/opt/new-api/deploy}"
OVERRIDE_FILE="${NEWAPI_OVERRIDE_FILE:-$DEPLOY_DIR/docker-compose.override.yml}"
APP_SERVICE="${NEWAPI_APP_SERVICE:-new-api}"
APP_CONTAINER="${NEWAPI_APP_CONTAINER:-new-api}"
APP_PORT="${NEWAPI_APP_PORT:-8899}"
PUBLIC_URL="${NEWAPI_PUBLIC_URL:-https://ai.linjzy.com}"
MAINTENANCE_FLAG="${NEWAPI_MAINTENANCE_FLAG:-/run/newapi-maintenance}"

ROLLBACK_REPO="${NEWAPI_ROLLBACK_IMAGE_REPO:-new-api-rollback}"
ROLLBACK_REF="${ROLLBACK_REPO}:previous"
DB_CONTAINER="${NEWAPI_DB_CONTAINER:-new-api-postgres}"
DB_BACKUP_FILE="${NEWAPI_DB_BACKUP_FILE:-/opt/new-api/backups/new-api-before-upgrade.dump}"
DRAIN_WAIT_SECONDS="${NEWAPI_DRAIN_WAIT_SECONDS:-600}"
HEALTH_TIMEOUT_SECONDS="${NEWAPI_HEALTH_TIMEOUT_SECONDS:-180}"
LOG_RETENTION_DAYS="${NEWAPI_LOG_RETENTION_DAYS:-14}"

LOCK_FILE="${NEWAPI_CUSTOM_LOCK_FILE:-/run/lock/newapi-custom-upgrade.lock}"
OVERRIDE_MARKER="# Managed by newapi-custom-upgrade.sh"

CANDIDATE_STATE_FILE="$STATE_DIR/candidate.env"
CURRENT_STATE_FILE="$STATE_DIR/current.env"
SCHEDULED_STATE_FILE="$STATE_DIR/scheduled.env"
JOB_STATE_FILE="$STATE_DIR/job.env"
SCRIPT_SHA256="$(sha256sum "$SCRIPT_PATH" | awk '{print $1}')"
JOB_PHASE=idle
JOB_STARTED_SECONDS=$SECONDS
PHASE_STARTED_SECONDS=$SECONDS

set_phase() {
  log "phase=$JOB_PHASE duration_seconds=$((SECONDS - PHASE_STARTED_SECONDS))"
  JOB_PHASE="$1"
  PHASE_STARTED_SECONDS=$SECONDS
  write_env_file "$JOB_STATE_FILE" \
    "JOB_PHASE=$JOB_PHASE" "JOB_SCRIPT_SHA256=$SCRIPT_SHA256" \
    "JOB_UPDATED_AT=$(date -Is)"
}

log() {
  printf '[%s] %s\n' "$(date -Is)" "$*"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage:
  newapi-custom-upgrade.sh latest
  newapi-custom-upgrade.sh status
  newapi-custom-upgrade.sh pull [release-tag|latest|image-reference]
  newapi-custom-upgrade.sh deploy
  newapi-custom-upgrade.sh upgrade [release-tag|latest|previous|image-reference]
  newapi-custom-upgrade.sh cleanup

Commands:
  latest    Print the configured GHCR candidate reference.
  status    Show the running container, pulled candidate, deployment state, jobs,
            and disk usage.
  pull      Pull and validate a prebuilt GHCR image without changing service.
  deploy    Deploy the last pulled and validated candidate in a detached job.
  upgrade   Pull, drain connections, and deploy in a detached systemd job.
            "previous" redeploys the registry's previous candidate.
  cleanup   Remove unused New API managed resources and expired task logs.
            Other projects and shared Docker build caches are untouched.

Production hosts do not fetch source code, apply patches, or build Docker
images.
EOF
}

need_command() {
  command -v "$1" >/dev/null 2>&1 ||
    die "required command not found: $1"
}

require_root() {
  [[ "${EUID:-$(id -u)}" -eq 0 ]] || die "run this command as root"
}

require_runtime_commands() {
  local command_name
  for command_name in \
    awk curl df docker find flock grep install mktemp python3 readlink sed ss \
    systemctl systemd-run tee unlink sha256sum
  do
    need_command "$command_name"
  done
}

validate_image_ref() {
  local image_ref="$1"
  [[ "$image_ref" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] ||
    die "invalid Docker image reference: $image_ref"
}

ensure_registry_config() {
  [[ -n "$IMAGE_REPOSITORY" ]] ||
    die "NEWAPI_IMAGE_REPOSITORY is not configured in $CONFIG_FILE"
  [[ -n "$SOURCE_REPOSITORY" ]] ||
    die "NEWAPI_SOURCE_REPOSITORY is not configured in $CONFIG_FILE"
  validate_image_ref "$IMAGE_REPOSITORY"
  [[ "$IMAGE_REPOSITORY" == ghcr.io/* ]] ||
    die "image repository must use ghcr.io"
  [[ "$IMAGE_REPOSITORY" != *:* && "$IMAGE_REPOSITORY" != *@* ]] ||
    die "image repository must not include a tag or digest"
  [[ "$SOURCE_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
    die "invalid source repository: $SOURCE_REPOSITORY"
}

ensure_runtime_dirs() {
  mkdir -p "$BASE_DIR" "$STATE_DIR" "$LOG_DIR"
  [[ -d "$DEPLOY_DIR" ]] ||
    die "deploy directory does not exist: $DEPLOY_DIR"
  [[ -f "$DEPLOY_DIR/docker-compose.yml" ]] ||
    die "docker-compose.yml not found in $DEPLOY_DIR"
}

acquire_lock() {
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec 9>"$LOCK_FILE"
  if [[ "${NEWAPI_INTERNAL_ACTIVATE:-}" == 1 ]]; then
    # The parent holds the lock while handing immutable arguments to systemd.
    flock -w 10 9 || die "another new-api custom upgrade job is already running"
  else
    flock -n 9 || die "another new-api custom upgrade job is already running"
  fi
}

begin_job_log() {
  local job_name="$1"
  local job_log
  job_log="$LOG_DIR/${job_name}-$(date +%Y%m%d%H%M%S).log"
  touch "$job_log"
  log "job log: $job_log"
  exec > >(tee -a "$job_log") 2>&1
}

write_env_file() {
  local target="$1"
  shift
  local tmp entry key value
  tmp="$(mktemp "${target}.tmp.XXXXXX")"
  : >"$tmp"
  for entry in "$@"; do
    key="${entry%%=*}"
    value="${entry#*=}"
    [[ "$key" =~ ^[A-Z0-9_]+$ ]] ||
      die "invalid state key: $key"
    printf '%s=%q\n' "$key" "$value" >>"$tmp"
  done
  chmod 600 "$tmp"
  mv -f "$tmp" "$target"
}

state_value() {
  local file="$1"
  local key="$2"
  [[ -f "$file" ]] || return 0
  (
    # shellcheck disable=SC1090
    source "$file"
    if declare -p "$key" >/dev/null 2>&1; then
      printf '%s' "${!key}"
    fi
  )
}

candidate_reference() {
  ensure_registry_config
  printf '%s:%s' "$IMAGE_REPOSITORY" "$CANDIDATE_TAG"
}

resolve_requested_ref() {
  local requested="${1:-latest}"
  local image_ref

  case "$requested" in
    latest|candidate)
      image_ref="$(candidate_reference)"
      ;;
    */*|*:*|*@*)
      image_ref="$requested"
      ;;
    *)
      image_ref="$IMAGE_REPOSITORY:$requested"
      ;;
  esac

  validate_image_ref "$image_ref"
  if [[ "$image_ref" != "$IMAGE_REPOSITORY:"* &&
    "$image_ref" != "$IMAGE_REPOSITORY@"* ]]
  then
    die "refusing image outside configured repository: $image_ref"
  fi
  printf '%s' "$image_ref"
}

image_label() {
  local image_ref="$1"
  local label_name="$2"
  local value
  value="$(docker image inspect "$image_ref" \
    --format "{{index .Config.Labels \"$label_name\"}}" \
    2>/dev/null || true)"
  [[ "$value" != '<no value>' ]] && printf '%s' "$value"
}

validate_candidate_image() {
  local image_ref="$1"
  local architecture os managed source_url revision

  architecture="$(docker image inspect "$image_ref" --format '{{.Architecture}}')"
  os="$(docker image inspect "$image_ref" --format '{{.Os}}')"
  managed="$(image_label "$image_ref" 'com.linjzy.new-api.managed')"
  source_url="$(image_label "$image_ref" 'org.opencontainers.image.source')"
  revision="$(image_label "$image_ref" 'org.opencontainers.image.revision')"

  [[ "$architecture" == 'amd64' && "$os" == 'linux' ]] ||
    die "candidate platform must be linux/amd64, got $os/$architecture"
  [[ "$managed" == 'true' ]] ||
    die "candidate is missing the managed-image label"
  [[ "$source_url" == "https://github.com/$SOURCE_REPOSITORY" ]] ||
    die "candidate source label does not match $SOURCE_REPOSITORY"
  [[ "$revision" =~ ^[0-9a-f]{40}$ ]] ||
    die "candidate source revision label is invalid"

  PULLED_RELEASE="$(image_label "$image_ref" \
    'com.linjzy.new-api.upstream-tag')"
  PULLED_UPSTREAM_COMMIT="$(image_label "$image_ref" \
    'com.linjzy.new-api.upstream-commit')"
  PULLED_PATCH_SHA256="$(image_label "$image_ref" \
    'com.linjzy.new-api.patch-sha256')"
  PULLED_SOURCE_REF="$(image_label "$image_ref" \
    'com.linjzy.new-api.source-ref')"
  PULLED_SOURCE_COMMIT="$revision"

  [[ -n "$PULLED_RELEASE" && "$PULLED_RELEASE" != *[[:space:]]* ]] ||
    die "candidate upstream release label is invalid"
  [[ "$PULLED_UPSTREAM_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
    die "candidate upstream commit label is invalid"
  [[ "$PULLED_PATCH_SHA256" =~ ^[0-9a-f]{64}$ ]] ||
    die "candidate patch hash label is invalid"
  [[ "$PULLED_SOURCE_REF" == custom/* &&
    "$PULLED_SOURCE_REF" != *[[:space:]]* ]] ||
    die "candidate public source ref label is invalid"
}

pull_candidate() {
  local requested="${1:-latest}"
  local requested_ref repo_digest

  ensure_registry_config
  requested_ref="$(resolve_requested_ref "$requested")"
  log "pulling prebuilt candidate $requested_ref"
  docker pull --platform linux/amd64 "$requested_ref"

  repo_digest="$(
    docker image inspect "$requested_ref" \
      --format '{{range .RepoDigests}}{{println .}}{{end}}' |
      awk -v prefix="${IMAGE_REPOSITORY}@" \
        'index($0, prefix) == 1 {print; exit}'
  )"
  [[ -n "$repo_digest" ]] ||
    die "unable to resolve immutable registry digest for $requested_ref"
  validate_image_ref "$repo_digest"

  validate_candidate_image "$repo_digest"

  PULLED_IMAGE="$repo_digest"
  PULLED_IMAGE_ID="$(docker image inspect "$repo_digest" --format '{{.Id}}')"
  write_env_file "$CANDIDATE_STATE_FILE" \
    "CANDIDATE_REQUESTED_IMAGE=$requested_ref" \
    "CANDIDATE_IMAGE=$PULLED_IMAGE" \
    "CANDIDATE_IMAGE_ID=$PULLED_IMAGE_ID" \
    "CANDIDATE_RELEASE=$PULLED_RELEASE" \
    "CANDIDATE_UPSTREAM_COMMIT=$PULLED_UPSTREAM_COMMIT" \
    "CANDIDATE_PATCH_SHA256=$PULLED_PATCH_SHA256" \
    "CANDIDATE_SOURCE_REF=$PULLED_SOURCE_REF" \
    "CANDIDATE_SOURCE_COMMIT=$PULLED_SOURCE_COMMIT" \
    "CANDIDATE_PULLED_AT=$(date -Is)"

  log "candidate ready: $PULLED_IMAGE"
  log "public source: https://github.com/$SOURCE_REPOSITORY/tree/$PULLED_SOURCE_REF"
}

backup_database() {
  local upstream_commit="$1"
  local tmp
  if [[ -f "$DB_BACKUP_FILE" &&
    "$(state_value "$CURRENT_STATE_FILE" CURRENT_UPSTREAM_COMMIT)" == "$upstream_commit" ]]
  then
    log "upstream version unchanged; keeping $DB_BACKUP_FILE"
    return 0
  fi
  mkdir -p "$(dirname "$DB_BACKUP_FILE")"
  tmp="$(mktemp "$DB_BACKUP_FILE.tmp.XXXXXX")"
  if ! docker exec "$DB_CONTAINER" \
    sh -c 'pg_dump -U "$POSTGRES_USER" -Fc "${POSTGRES_DB:-$POSTGRES_USER}"' >"$tmp" ||
    [[ ! -s "$tmp" ]]
  then
    rm -f "$tmp"
    die "database backup failed; service was not changed"
  fi
  chmod 600 "$tmp"
  mv -f "$tmp" "$DB_BACKUP_FILE"
  log "database backup written to $DB_BACKUP_FILE"
}

active_app_connections() {
  ss -Htn state established "( sport = :${APP_PORT} )" |
    awk 'END {print NR + 0}'
}

disable_request_gate() {
  if [[ -e "$MAINTENANCE_FLAG" ]]; then
    unlink "$MAINTENANCE_FLAG"
    log "public request gate removed"
  fi
}

enable_request_gate() {
  local status_code
  if [[ ! -e "$MAINTENANCE_FLAG" ]]; then
    install -m 644 /dev/null "$MAINTENANCE_FLAG"
    log "new public requests are temporarily gated at nginx"
  fi

  for _ in $(seq 1 10); do
    status_code="$(curl -sS -o /dev/null -w '%{http_code}' \
      --connect-timeout 5 --max-time 10 "$PUBLIC_URL/api/status" || true)"
    [[ "$status_code" == '503' ]] && return 0
    sleep 1
  done

  disable_request_gate
  die "nginx request gate verification failed: expected 503, got $status_code"
}

wait_for_connection_drain() {
  local phase="$1"
  local deadline active_count
  deadline=$(($(date +%s) + DRAIN_WAIT_SECONDS))
  log "waiting for active new-api connections to drain before $phase"

  while (($(date +%s) < deadline)); do
    active_count="$(active_app_connections)" ||
      die "cannot inspect active connections; service was not changed"
    if ((active_count == 0)); then
      log "connection drain confirmed"
      return 0
    fi
    log "$active_count active connection(s); waiting"
    sleep 1
  done

  die "active connections did not drain before $phase; service was not changed"
}

ensure_override_manageable() {
  if [[ -f "$OVERRIDE_FILE" ]] &&
    ! grep -qF "$OVERRIDE_MARKER" "$OVERRIDE_FILE"
  then
    die "refusing to overwrite an unmanaged compose override: $OVERRIDE_FILE"
  fi
}

preflight_deployment() {
  local image_ref="$1" rendered
  ensure_override_manageable
  rendered="$(printf 'services:\n  %s:\n    image: %s\n    stop_grace_period: 180s\n' "$APP_SERVICE" "$image_ref")"
  (cd "$DEPLOY_DIR" && printf '%s\n' "$rendered" |
    docker compose -f docker-compose.yml -f - config --quiet) ||
    die "candidate compose preflight failed; service was not changed"
}

write_override() {
  local image_ref="$1"
  local tmp
  validate_image_ref "$image_ref"
  ensure_override_manageable
  tmp="$(mktemp "$DEPLOY_DIR/.docker-compose.override.yml.XXXXXX")"
  {
    printf '%s\n' "$OVERRIDE_MARKER"
    printf '%s\n' 'services:'
    printf '%s\n' "  ${APP_SERVICE}:"
    printf '    image: %s\n' "$image_ref"
    printf '%s\n' '    stop_grace_period: 180s'
  } >"$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$OVERRIDE_FILE"
}

compose_up_app() {
  (
    cd "$DEPLOY_DIR"
    docker compose config --quiet
    docker compose up -d --no-deps --pull never "$APP_SERVICE"
  )
}

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

wait_for_app_health() {
  local deadline cid health
  deadline=$(($(date +%s) + HEALTH_TIMEOUT_SECONDS))

  while (($(date +%s) < deadline)); do
    cid="$(cd "$DEPLOY_DIR" && docker compose ps -q "$APP_SERVICE")"
    if [[ -n "$cid" ]]; then
      health="$(docker inspect "$cid" \
        --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' \
        2>/dev/null || true)"
      if [[ "$health" == 'healthy' || "$health" == 'running' ]] &&
        curl -fsS --connect-timeout 3 --max-time 10 \
          "http://127.0.0.1:${APP_PORT}/api/status" | json_success
      then
        log "new-api is healthy"
        return 0
      fi
    fi
    sleep 2
  done

  log "new-api did not become healthy in time"
  (cd "$DEPLOY_DIR" &&
    docker compose logs --tail=120 "$APP_SERVICE") || true
  return 1
}

verify_public_entry() {
  local page
  curl -fsS --connect-timeout 5 --max-time 20 \
    "$PUBLIC_URL/api/status" | json_success || return 1
  page="$(curl -fsS --connect-timeout 5 --max-time 20 "$PUBLIC_URL/")" || return 1
  [[ "$page" == *'<html'* && "$page" == *'/static/js/'* ]] || return 1
  log "public API and frontend entry verified"
}

deploy_local_and_verify() {
  local image_ref="$1"
  local expected_image_id="$2"
  local running_image_id

  write_override "$image_ref"
  compose_up_app || return 1
  wait_for_app_health || return 1

  running_image_id="$(docker inspect "$APP_CONTAINER" --format '{{.Image}}')"
  [[ "$running_image_id" == "$expected_image_id" ]] || {
    log "running image mismatch: expected $expected_image_id, got $running_image_id"
    return 1
  }
}

rollback_to_image() {
  local rollback_ref="$1"

  log "rolling back to $rollback_ref"
  enable_request_gate
  wait_for_connection_drain 'rolling back the live container'
  write_override "$rollback_ref"
  if ! compose_up_app || ! wait_for_app_health; then
    disable_request_gate
    return 1
  fi

  disable_request_gate
  verify_public_entry
}

cleanup_managed_images() {
  local image_id running_id candidate_id previous_id

  running_id="$(docker inspect "$APP_CONTAINER" \
    --format '{{.Image}}' 2>/dev/null || true)"
  candidate_id="$(state_value "$CANDIDATE_STATE_FILE" CANDIDATE_IMAGE_ID)"
  previous_id="$(docker image inspect "$ROLLBACK_REF" \
    --format '{{.Id}}' 2>/dev/null || true)"

  # Keep the running image, a pulled candidate and one previous image.
  while read -r image_id; do
    [[ -n "$image_id" ]] || continue
    case "$image_id" in
      "$running_id" | "$candidate_id" | "$previous_id") continue ;;
    esac
    log "removing unused managed image $image_id"
    docker image rm --force "$image_id" >/dev/null 2>&1 || true
  done < <(docker image ls --no-trunc --format '{{.ID}}\t{{.Repository}}' |
    awk -F '\t' -v app="$IMAGE_REPOSITORY" -v rollback="$ROLLBACK_REPO" \
      '$2 == app || $2 == rollback {print $1}' | sort -u)
}

rotate_managed_files() {
  find "$LOG_DIR" -maxdepth 1 -type f -name '*.log' \
    -mtime "+$LOG_RETENTION_DAYS" -delete 2>/dev/null || true
}

cleanup_unused_docker_objects() {
  # Only explicitly managed disposable resources qualify. Database volumes and
  # other Compose projects never carry this label.
  docker container prune --force --filter 'label=com.linjzy.new-api.managed=true' >/dev/null
  docker network prune --force --filter 'label=com.linjzy.new-api.managed=true' >/dev/null
  docker volume prune --force --filter 'label=com.linjzy.new-api.managed=true' >/dev/null
}

safe_cleanup() {
  disable_request_gate
  existing_background_job || rm -f "$SCHEDULED_STATE_FILE"
  cleanup_managed_images
  cleanup_unused_docker_objects
  rotate_managed_files
  log "disk usage after cleanup"
  docker system df || true
  df -h "$BASE_DIR" || true
}

activate_image() {
  local image_ref="$1"
  local release_tag="$2"
  local upstream_commit="$3"
  local patch_sha="$4"
  local source_ref="$5"
  local source_commit="$6"
  local candidate_id previous_id

  validate_image_ref "$image_ref"
  docker image inspect "$image_ref" >/dev/null 2>&1 ||
    die "candidate image is not present locally: $image_ref"
  candidate_id="$(docker image inspect "$image_ref" --format '{{.Id}}')"
  previous_id="$(docker inspect "$APP_CONTAINER" --format '{{.Image}}')"

  if [[ "$candidate_id" == "$previous_id" ]]; then
    log "candidate image is already running; recording immutable reference"
    wait_for_app_health || die "running image failed local verification"
    verify_public_entry ||
      die "running candidate failed public verification"
    rm -f "$CANDIDATE_STATE_FILE" "$SCHEDULED_STATE_FILE"
    set_phase unchanged
    return 0
  fi

  set_phase preflight
  preflight_deployment "$image_ref"
  set_phase gate
  enable_request_gate
  set_phase drain
  wait_for_connection_drain 'replacing the live container'
  set_phase backup
  backup_database "$upstream_commit"
  set_phase activate
  ensure_override_manageable
  docker tag "$previous_id" "$ROLLBACK_REF"
  log "activating $image_ref; previous image retained as $ROLLBACK_REF"
  if ! deploy_local_and_verify "$image_ref" "$candidate_id"; then
    log "candidate local verification failed"
    if rollback_to_image "$ROLLBACK_REF"; then
      log "automatic rollback succeeded"
    else
      log "CRITICAL: automatic rollback verification failed"
    fi
    rm -f "$CANDIDATE_STATE_FILE"
    cleanup_managed_images
    die "deployment failed and rollback was attempted; inspect the job log"
  fi

  disable_request_gate
  if ! verify_public_entry; then
    log "candidate public verification failed"
    if rollback_to_image "$ROLLBACK_REF"; then
      log "automatic rollback succeeded"
    else
      log "CRITICAL: automatic rollback verification failed"
    fi
    rm -f "$CANDIDATE_STATE_FILE"
    cleanup_managed_images
    die "deployment failed and rollback was attempted; inspect the job log"
  fi

  write_env_file "$CURRENT_STATE_FILE" \
    "CURRENT_IMAGE=$image_ref" \
    "CURRENT_IMAGE_ID=$candidate_id" \
    "CURRENT_RELEASE=$release_tag" \
    "CURRENT_UPSTREAM_COMMIT=$upstream_commit" \
    "CURRENT_PATCH_SHA256=$patch_sha" \
    "CURRENT_SOURCE_REF=$source_ref" \
    "CURRENT_SOURCE_COMMIT=$source_commit" \
    "CURRENT_DEPLOYED_AT=$(date -Is)"
  rm -f "$CANDIDATE_STATE_FILE" "$SCHEDULED_STATE_FILE"

  set_phase complete
  log "deployment succeeded"
  # Only the running image and the retained previous image stay local.
  cleanup_managed_images
}

existing_background_job() {
  systemctl list-units --type=service --state=running,activating \
    --no-legend 'newapi-custom-*' 2>/dev/null |
    grep -q .
}

schedule_internal_job() {
  local action="$1"
  local target="$2"
  local unit timestamp
  existing_background_job &&
    die "a new-api custom background job is already running; inspect status first"

  timestamp="$(date +%Y%m%d%H%M%S)"
  unit="newapi-custom-${action}-${timestamp}"

  case "$action" in
    upgrade)
      systemd-run --quiet --collect \
        --unit "$unit" \
        --no-block \
        /usr/bin/env NEWAPI_INTERNAL_ACTIVATE=1 \
        "NEWAPI_CUSTOM_CONFIG_FILE=$CONFIG_FILE" \
        "$SCRIPT_PATH" _upgrade "$target"
      ;;
    activate)
      local release_tag upstream_commit patch_sha source_ref source_commit
      release_tag="$(state_value "$CANDIDATE_STATE_FILE" CANDIDATE_RELEASE)"
      upstream_commit="$(state_value "$CANDIDATE_STATE_FILE" \
        CANDIDATE_UPSTREAM_COMMIT)"
      patch_sha="$(state_value "$CANDIDATE_STATE_FILE" \
        CANDIDATE_PATCH_SHA256)"
      source_ref="$(state_value "$CANDIDATE_STATE_FILE" CANDIDATE_SOURCE_REF)"
      source_commit="$(state_value "$CANDIDATE_STATE_FILE" \
        CANDIDATE_SOURCE_COMMIT)"
      systemd-run --quiet --collect \
        --unit "$unit" \
        --no-block \
        /usr/bin/env NEWAPI_INTERNAL_ACTIVATE=1 \
        "NEWAPI_CUSTOM_CONFIG_FILE=$CONFIG_FILE" \
        "$SCRIPT_PATH" _activate \
        "$target" "$release_tag" "$upstream_commit" \
        "$patch_sha" "$source_ref" "$source_commit"
      ;;
    *)
      die "unknown scheduled action: $action"
      ;;
  esac

  write_env_file "$SCHEDULED_STATE_FILE" \
    "SCHEDULED_UNIT=$unit" \
    "SCHEDULED_ACTION=$action" \
    "SCHEDULED_TARGET=$target" \
    "STARTED_AT=$(date -Is)"
  log "started detached $action job as ${unit}.service"
}

show_status() {
  printf '%s\n' 'Registry configuration:'
  if [[ -n "$IMAGE_REPOSITORY" ]]; then
    printf '  image_repository=%s\n' "$IMAGE_REPOSITORY"
    printf '  candidate=%s:%s\n' "$IMAGE_REPOSITORY" "$CANDIDATE_TAG"
    printf '  source=https://github.com/%s\n' "$SOURCE_REPOSITORY"
  else
    printf '%s\n' '  not configured'
  fi

  printf '%s\n' 'Current container:'
  docker inspect "$APP_CONTAINER" \
    --format '  configured={{.Config.Image}} image_id={{.Image}} status={{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' \
    2>/dev/null || printf '%s\n' '  not running'

  if [[ -f "$CANDIDATE_STATE_FILE" ]]; then
    printf '%s\n' 'Pulled candidate:'
    sed 's/^/  /' "$CANDIDATE_STATE_FILE"
  fi
  if [[ -f "$CURRENT_STATE_FILE" ]]; then
    printf '%s\n' 'Managed deployment state:'
    sed 's/^/  /' "$CURRENT_STATE_FILE"
  fi
  if [[ -f "$SCHEDULED_STATE_FILE" ]]; then
    printf '%s\n' 'Background job state:'
    sed 's/^/  /' "$SCHEDULED_STATE_FILE"
  fi

  if [[ -f "$JOB_STATE_FILE" ]]; then
    printf '%s\n' 'Last job result:'
    sed 's/^/  /' "$JOB_STATE_FILE"
  fi
  printf 'Retained previous image: %s\n' \
    "$(docker image inspect "$ROLLBACK_REF" --format '{{join .RepoDigests " "}}' 2>/dev/null || echo none)"
  if [[ -f "$DB_BACKUP_FILE" ]]; then
    printf 'Database backup: %s (%s)\n' "$DB_BACKUP_FILE" "$(date -r "$DB_BACKUP_FILE" -Is)"
  fi
  printf 'Deployment script: %s\n' "$SCRIPT_SHA256"
  printf '%s\n' 'Background jobs:'
  systemctl list-units --all --no-legend 'newapi-custom-*' 2>/dev/null || true
  printf '%s\n' 'Disk usage:'
  df -h "$BASE_DIR" 2>/dev/null || df -h "$DEPLOY_DIR"
  docker system df 2>/dev/null || true
}

finish_internal_job() {
  local rc="$1"
  disable_request_gate
  write_env_file "$JOB_STATE_FILE" \
    "JOB_PHASE=$JOB_PHASE" "JOB_EXIT_CODE=$rc" \
    "JOB_DURATION_SECONDS=$((SECONDS - JOB_STARTED_SECONDS))" \
    "JOB_SCRIPT_SHA256=$SCRIPT_SHA256" "JOB_FINISHED_AT=$(date -Is)"
  log "phase=$JOB_PHASE exit_code=$rc total_seconds=$((SECONDS - JOB_STARTED_SECONDS))"
  if ((rc != 0)); then
    rm -f "$SCHEDULED_STATE_FILE"
  fi
}

run_internal_upgrade() {
  local requested="$1"
  [[ "${NEWAPI_INTERNAL_ACTIVATE:-}" == '1' ]] ||
    die "internal upgrade may only be started by the detached systemd job"
  require_root
  require_runtime_commands
  ensure_registry_config
  ensure_runtime_dirs
  acquire_lock
  begin_job_log upgrade
  trap 'finish_internal_job $?' EXIT
  set_phase pull
  pull_candidate "$requested"
  activate_image \
    "$PULLED_IMAGE" "$PULLED_RELEASE" "$PULLED_UPSTREAM_COMMIT" \
    "$PULLED_PATCH_SHA256" "$PULLED_SOURCE_REF" "$PULLED_SOURCE_COMMIT"
}

run_internal_activate() {
  local image_ref="$1"
  local release_tag="$2"
  local upstream_commit="$3"
  local patch_sha="$4"
  local source_ref="$5"
  local source_commit="$6"
  [[ "${NEWAPI_INTERNAL_ACTIVATE:-}" == '1' ]] ||
    die "internal activation may only be started by the detached systemd job"
  require_root
  require_runtime_commands
  ensure_runtime_dirs
  acquire_lock
  begin_job_log activate
  trap 'finish_internal_job $?' EXIT
  set_phase inspect
  activate_image \
    "$image_ref" "$release_tag" "$upstream_commit" \
    "$patch_sha" "$source_ref" "$source_commit"
}

main() {
  local command_name="${1:-status}"
  shift || true

  case "$command_name" in
    latest)
      [[ $# -eq 0 ]] || die "latest does not accept arguments"
      candidate_reference
      printf '\n'
      ;;
    status)
      [[ $# -eq 0 ]] || die "status does not accept arguments"
      need_command docker
      show_status
      ;;
    pull)
      require_root
      require_runtime_commands
      ensure_registry_config
      ensure_runtime_dirs
      acquire_lock
      pull_candidate "${1:-latest}"
      ;;
    deploy)
      require_root
      require_runtime_commands
      ensure_registry_config
      ensure_runtime_dirs
      acquire_lock
      local candidate_image
      candidate_image="$(state_value "$CANDIDATE_STATE_FILE" CANDIDATE_IMAGE)"
      [[ -n "$candidate_image" ]] ||
        die "no pulled and validated candidate image is available"
      [[ $# -eq 0 ]] || die "deploy does not accept arguments"
      schedule_internal_job activate "$candidate_image"
      ;;
    upgrade)
      require_root
      require_runtime_commands
      ensure_registry_config
      ensure_runtime_dirs
      acquire_lock
      [[ $# -le 1 ]] || die "upgrade accepts at most one image selector"
      local requested="${1:-latest}"
      schedule_internal_job upgrade "$requested"
      ;;
    cleanup)
      require_root
      require_runtime_commands
      ensure_runtime_dirs
      acquire_lock
      [[ $# -eq 0 ]] || die "cleanup does not accept arguments"
      safe_cleanup
      ;;
    _upgrade)
      [[ $# -eq 1 ]] || die "invalid internal upgrade arguments"
      run_internal_upgrade "$1"
      ;;
    _activate)
      [[ $# -eq 6 ]] || die "invalid internal activation arguments"
      run_internal_activate "$@"
      ;;
    help|-h|--help)
      usage
      ;;
    *)
      usage >&2
      die "unknown command: $command_name"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
