import os
import subprocess
import tempfile
import unittest
from pathlib import Path

BUNDLE = Path(__file__).resolve().parents[1]
SCRIPT = BUNDLE / "deploy/bin/newapi-custom-upgrade.sh"

class DeploymentContract(unittest.TestCase):
    def run_script(self, body):
        with tempfile.TemporaryDirectory() as tmp:
            env = dict(os.environ, TEST_DIR=tmp, SCRIPT=str(SCRIPT), NEWAPI_CUSTOM_CONFIG_FILE=str(Path(tmp)/"absent.env"))
            prelude = r"""
set -Eeuo pipefail
# Portable inspection helpers for the Linux deployment script's macOS tests.
sha256sum() { python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"; }
readlink() { python3 -c 'import os,sys;print(os.path.realpath(sys.argv[-1]))' "$@"; }
source "$SCRIPT"
STATE_DIR="$TEST_DIR"
CURRENT_STATE_FILE="$TEST_DIR/current.env"
CANDIDATE_STATE_FILE="$TEST_DIR/candidate.env"
SCHEDULED_STATE_FILE="$TEST_DIR/scheduled.env"
JOB_STATE_FILE="$TEST_DIR/job.env"
DEPLOY_DIR="$TEST_DIR"
OVERRIDE_FILE="$TEST_DIR/docker-compose.override.yml"
MAINTENANCE_FLAG="$TEST_DIR/gate"
IMAGE_REPOSITORY=ghcr.io/linjzy/new-api
record() { printf '%s\n' "$*" >> "$TEST_DIR/calls"; }
set_phase() { record "phase:$1"; }
wait_for_app_health() { record health; }
verify_public_entry() { record public; }
cleanup_managed_images() { record cleanup; }
docker() {
  if [[ "$1 $2" == 'image inspect' ]]; then
    [[ "$*" == *--format* ]] && printf 'new\n'
    return 0
  fi
  if [[ "$1" == inspect ]]; then printf '%s\n' "${RUNNING_IMAGE:-old}"; return 0; fi
  record "docker:$*"
}
enable_request_gate() { record gate; }
disable_request_gate() { record ungate; }
wait_for_connection_drain() { record drain; }
wait_for_idle_window() { record idle; }
preflight_deployment() { record preflight; }
ensure_override_manageable() { :; }
backup_database() { record backup; }
deploy_local_and_verify() { record deploy; }
"""
            p = subprocess.run(["bash", "-c", prelude + body], env=env, text=True, capture_output=True)
            calls = (Path(tmp)/"calls").read_text().splitlines() if (Path(tmp)/"calls").exists() else []
            return p,calls

    def test_same_image_never_gates_recreates_or_cleans(self):
        p,calls=self.run_script('RUNNING_IMAGE=new\nactivate_image ghcr.io/linjzy/new-api@sha256:123 v1 upstream patch source commit\n')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(calls,["health","public","phase:unchanged"])

    def test_preflight_failure_never_gates_or_changes_container(self):
        p,calls=self.run_script('preflight_deployment() { record preflight; die invalid-compose; }\nactivate_image ghcr.io/linjzy/new-api@sha256:123 v1 upstream patch source commit\n')
        self.assertNotEqual(p.returncode,0)
        self.assertEqual(calls,["phase:preflight","preflight"])

    def test_upgrade_preflights_before_gate_and_cleans_only_after_success(self):
        p,calls=self.run_script('activate_image ghcr.io/linjzy/new-api@sha256:123 v1 upstream patch source commit\n')
        self.assertEqual(p.returncode,0,p.stderr)
        order=[x for x in calls if x in ("preflight","idle","gate","drain","backup","deploy","public","cleanup")]
        self.assertEqual(order,["preflight","idle","gate","drain","backup","deploy","public","cleanup"])

    def test_idle_wait_does_not_gate_and_still_drains_after_a_new_arrival(self):
        body=SCRIPT.read_text();start=body.index('wait_for_idle_window() {');end=body.index('ensure_override_manageable()',start)
        p,calls=self.run_script(body[start:end]+r'''
date() { echo 100; }
IDLE_WAIT_SECONDS=10
active_app_connections() { echo 0; }
wait_for_idle_window
enable_request_gate
# A request arrived between the idle observation and the gate.
active_app_connections() { [[ -e "$TEST_DIR/drained" ]] && echo 0 || echo 1; }
sleep() { record waiting-after-gate; touch "$TEST_DIR/drained"; }
wait_for_connection_drain test
record switched
''')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(calls,["gate","waiting-after-gate","switched"])

    def test_idle_timeout_falls_back_and_inspection_errors_abort_before_gate(self):
        body=SCRIPT.read_text();start=body.index('wait_for_idle_window() {');end=body.index('wait_for_connection_drain() {',start)
        for mode in ("timeout", "error", "invalid"):
            with self.subTest(mode=mode):
                setup = {
                    "timeout": 'date() { [[ -e "$TEST_DIR/expired" ]] && echo 110 || echo 100; }; active_app_connections() { echo 1; }; sleep() { touch "$TEST_DIR/expired"; }',
                    "error": 'date() { echo 100; }; active_app_connections() { return 1; }',
                    "invalid": 'date() { echo 100; }; active_app_connections() { echo invalid; }',
                }[mode]
                p,calls=self.run_script(body[start:end]+'\nIDLE_WAIT_SECONDS=10\n'+setup+'\nwait_for_idle_window\nenable_request_gate\nwait_for_connection_drain test\n')
                self.assertEqual(p.returncode == 0,mode == "timeout",p.stderr)
                self.assertEqual(calls,["gate","drain"] if mode == "timeout" else [])

    def test_database_backup_refreshes_only_when_upstream_changes_and_never_replaces_on_failure(self):
        body=SCRIPT.read_text();start=body.index('backup_database() {');end=body.index('active_app_connections()',start)
        p,calls=self.run_script(body[start:end]+r"""
DB_BACKUP_FILE="$TEST_DIR/db.dump"
docker() { [[ "$1" == exec ]] && printf 'PGDMP'; }
printf 'CURRENT_UPSTREAM_COMMIT=aaa\n' > "$CURRENT_STATE_FILE"
backup_database aaa && [[ "$(cat "$DB_BACKUP_FILE")" == PGDMP ]] && record created
printf 'old' > "$DB_BACKUP_FILE"
backup_database aaa && [[ "$(cat "$DB_BACKUP_FILE")" == old ]] && record kept
backup_database bbb && [[ "$(cat "$DB_BACKUP_FILE")" == PGDMP ]] && record refreshed
docker() { return 1; }
( backup_database ccc ) 2>/dev/null || record failed
[[ "$(cat "$DB_BACKUP_FILE")" == PGDMP && -z "$(ls "$TEST_DIR"/db.dump.tmp.* 2>/dev/null)" ]] && record preserved
""")
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(calls,["created","kept","refreshed","failed","preserved"])

    def test_cleanup_is_scoped_and_does_not_prune_images_or_build_cache(self):
        p,calls=self.run_script('cleanup_unused_docker_objects\n')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(len(calls),3)
        for line in calls:self.assertIn('--filter label=com.linjzy.new-api.managed=true',line)
        self.assertFalse(any('image prune' in line or 'builder' in line for line in calls))

    def test_failed_connection_inspection_does_not_count_as_empty(self):
        body = SCRIPT.read_text()
        start=body.index('wait_for_connection_drain() {')
        end=body.index('ensure_override_manageable()',start)
        p,calls=self.run_script(body[start:end]+'\nactive_app_connections() { return 1; }\nwait_for_connection_drain test\n')
        self.assertNotEqual(p.returncode,0)
        self.assertIn('cannot inspect active connections',p.stderr)

    def test_empty_drain_does_not_add_fixed_wait(self):
        body=SCRIPT.read_text();start=body.index('wait_for_connection_drain() {');end=body.index('ensure_override_manageable()',start)
        p,calls=self.run_script(body[start:end]+'\nactive_app_connections() { echo 0; }\nsleep() { record unwanted-sleep; return 9; }\nwait_for_connection_drain test\n')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertNotIn('unwanted-sleep',calls)

if __name__ == '__main__':unittest.main()
