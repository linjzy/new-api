#!/usr/bin/env bash
set -Eeuo pipefail
SOURCE_DIR="$(cd "${1:?source directory is required}" && pwd)"
MODE="${2:-all}"
BUNDLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -d "$SOURCE_DIR/web/default/src" ]]; then
  FRONTEND="$SOURCE_DIR/web/default"
else
  FRONTEND="$SOURCE_DIR/web"
fi
case "$MODE" in
  go|race|frontend|all) ;;
  *) printf 'unknown verification mode: %s\n' "$MODE" >&2; exit 1 ;;
esac
if [[ "$MODE" != frontend ]]; then
  cp -R "$BUNDLE_DIR/tests/go/." "$SOURCE_DIR/"
  cd "$SOURCE_DIR"
  if [[ "$MODE" == race ]]; then
    GOWORK=off go test -race ./controller ./service ./relay ./relay/channel ./relay/channel/openai -run 'TestCustom|TestOaiResponses|TestNewTaskAPIRequestInheritsClientCancellation' -count=1
  else
    GOWORK=off go -C relaykit build ./...
    GOWORK=off go test -v ./controller ./relay ./relay/channel ./relay/channel/openai ./model ./service -run 'TestCustom|TestOaiResponses|TestResponsesUsage|TestApplyResponsesUsage|TestNewTaskAPIRequestInheritsClientCancellation' -count=1
  fi
fi
if [[ "$MODE" == frontend || "$MODE" == all ]]; then
  mkdir -p "$FRONTEND/src/features/usage-logs/custom-tests"
  cp "$BUNDLE_DIR"/tests/frontend/* "$FRONTEND/src/features/usage-logs/custom-tests/"
  cd "$SOURCE_DIR/web"
  bun install --frozen-lockfile
  cd "$FRONTEND"
  bun run typecheck
  # Check the reviewed patch surface; upstream feature directories contain
  # unrelated files with independent lint debt.
  changed_ts=()
  while IFS= read -r path; do changed_ts+=("$path"); done < <(python3 - "$BUNDLE_DIR" <<'PYFILES'
import sys
from pathlib import Path
files = set()
for patch in (Path(sys.argv[1]) / 'patches').glob('*.patch'):
    for line in patch.read_text().splitlines():
        if line.startswith('+++ b/web/src/'):
            name = line.removeprefix('+++ b/web/')
            if name.endswith(('.ts', '.tsx')):
                files.add(name)
print('\n'.join(sorted(files)))
PYFILES
  )
  bun x oxlint -c .oxlintrc.json "${changed_ts[@]}" src/features/usage-logs/custom-tests
  bun run test src/features/usage-logs/custom-tests src/features/channels/components/__tests__/astra-iq-status.test.tsx
  if [[ "${BUILD_FRONTEND:-true}" == true ]]; then bun run build; fi
fi
