import os
import json
import shlex
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
MAINTENANCE_STATE_FILE="$TEST_DIR/maintenance.env"
DEPLOY_DIR="$TEST_DIR"
OVERRIDE_FILE="$TEST_DIR/docker-compose.override.yml"
MAINTENANCE_FLAG="$TEST_DIR/gate"
DRAIN_CONTAINER_ID=old-container
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
begin_app_drain() { :; }
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

    def test_application_drain_validates_ownership_counters_and_readiness(self):
        body=SCRIPT.read_text();start=body.index('wait_for_connection_drain() {');end=body.index('ensure_override_manageable()',start)
        valid={"protocol":1,"job_id":"test-job","draining":True,"http_inflight":0,"ws_busy":0,"ws_idle":0,"ws_closing":0,"ready":True}
        cases=[(valid,True),({**valid,"job_id":"other"},False),({**valid,"ws_busy":1},False),({**valid,"http_inflight":-1},False),({**valid,"ws_idle":False},False),({**valid,"protocol":2},False),({k:v for k,v in valid.items() if k!="ws_closing"},False)]
        for payload,accepted in cases:
            with self.subTest(payload=payload):
                setup='\nJOB_ID=test-job\nAPP_DRAIN_ACTIVE=1\nDRAIN_PAYLOAD='+shlex.quote(json.dumps(payload))+'\ndocker() { if [[ "$1" == inspect ]]; then echo true; else printf "%s\\n" "$DRAIN_PAYLOAD"; fi; }\n'
                p,calls=self.run_script(body[start:end]+setup+'wait_for_connection_drain test\nrecord switched\n')
                self.assertEqual(p.returncode==0,accepted,p.stderr)
                self.assertEqual(calls,["switched"] if accepted else [])

    def test_busy_application_timeout_never_forces_a_container_restart(self):
        body=SCRIPT.read_text();start=body.index('wait_for_connection_drain() {');end=body.index('ensure_override_manageable()',start)
        p,calls=self.run_script(body[start:end]+r'''
JOB_ID=test-job
APP_DRAIN_ACTIVE=1
DRAIN_WAIT_SECONDS=1
date() { [[ -e "$TEST_DIR/expired" ]] && echo 102 || echo 100; }
docker() {
  if [[ "$1" == inspect ]]; then echo true; return; fi
  printf '%s\n' '{"protocol":1,"job_id":"test-job","draining":true,"http_inflight":0,"ws_busy":1,"ws_idle":0,"ws_closing":0,"ready":false}'
}
sleep() { touch "$TEST_DIR/expired"; }
wait_for_connection_drain test
record switched
''')
        self.assertNotEqual(p.returncode,0)
        self.assertIn('did not drain',p.stderr)
        self.assertEqual(calls,[])

    def test_legacy_capability_probe_never_invokes_the_old_binary(self):
        body=SCRIPT.read_text();start=body.index('begin_app_drain() {');end=body.index('# Opportunistic idle wait',start)
        p,calls=self.run_script(body[start:end]+r'''
docker() { if [[ "$1" == inspect ]]; then echo '<no value>'; else record unexpected-exec; return 1; fi; }
begin_app_drain
[[ "$APP_DRAIN_ACTIVE" == 0 ]] && record legacy
''')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertEqual(calls,["legacy"])

    def test_failed_activation_and_rollback_leave_the_gate_closed(self):
        p,calls=self.run_script(r'''
enable_request_gate() { GATE_OWNED=1; record gate; }
deploy_local_and_verify() { LIVE_CONTAINER_CHANGED=1; record deploy; return 1; }
rollback_to_image() { record rollback-failed; return 1; }
trap 'finish_internal_job $?' EXIT
activate_image ghcr.io/linjzy/new-api@sha256:123 v1 upstream patch source commit
''')
        self.assertNotEqual(p.returncode,0)
        self.assertIn('maintenance gate retained',p.stdout)
        self.assertNotIn('ungate',calls)

    def test_gate_release_requires_ownership_cancel_and_local_health(self):
        body=SCRIPT.read_text();start=body.index('disable_request_gate() {');end=body.index('enable_request_gate()',start)
        for scenario in ('healthy','unhealthy','foreign'):
            with self.subTest(scenario=scenario):
                p,calls=self.run_script(body[start:end]+r'''
JOB_ID=owner
GATE_OWNED=1
APP_DRAIN_ACTIVE=1
printf '%s\n' owner > "$MAINTENANCE_FLAG"
docker() { if [[ "$1" == inspect ]]; then echo old-container; else record cancel; fi; }
curl() { printf '%s\n' '{"success":true}'; }
'''+({'healthy':'','unhealthy':'curl() { return 1; }\n','foreign':'printf "%s\\n" foreign > "$MAINTENANCE_FLAG"\n'}[scenario])+r'''
if disable_request_gate; then record released; else record retained; fi
[[ -e "$MAINTENANCE_FLAG" ]] && record gate-exists || record gate-removed
''')
                self.assertEqual(p.returncode,0,p.stderr)
                expected={"healthy":["cancel","released","gate-removed"],"unhealthy":["cancel","retained","gate-exists"],"foreign":["retained","gate-exists"]}[scenario]
                self.assertEqual(calls,expected)

    def test_gate_probe_requires_maintenance_marker_on_both_listener_families(self):
        body=SCRIPT.read_text();start=body.index('enable_request_gate() {');end=body.index('begin_app_drain()',start)
        for marked in (True,False):
            with self.subTest(marked=marked):
                p,calls=self.run_script(body[start:end]+r'''
docker() { printf '%s\n' old-container; }
sleep() { :; }
curl() {
  local headers="" address=""
  while (($#)); do
    case "$1" in
      -D) headers="$2"; shift ;;
      --resolve) address="$2"; shift ;;
    esac
    shift
  done
  record "probe:$address"
  printf '%s\r\n' 'HTTP/1.1 503 Service Unavailable' > "$headers"
'''+('  printf "%s\\r\\n" "X-Newapi-Maintenance: 1" >> "$headers"\n' if marked else '')+r'''
  printf 503
}
enable_request_gate
record gated
''')
                self.assertEqual(p.returncode==0,marked,p.stderr)
                if marked:
                    self.assertEqual(calls,["probe:ai.linjzy.com:443:127.0.0.1","probe:ai.linjzy.com:443:[::1]","gated"])
                else:
                    self.assertNotIn('gated',calls)

    def test_container_network_inspection_propagates_errors(self):
        body=SCRIPT.read_text();start=body.index('active_app_connections() {');end=body.index('disable_request_gate()',start)
        p,calls=self.run_script(body[start:end]+r'''
docker() { echo 123; }
nsenter() { record "namespace:$*"; printf '%s\n' connection-one connection-two; }
[[ "$(active_app_connections)" == 2 ]] && record counted
nsenter() { return 1; }
if active_app_connections >/dev/null; then record incorrect-empty; else record inspection-failed; fi
''')
        self.assertEqual(p.returncode,0,p.stderr)
        self.assertIn('counted',calls)
        self.assertIn('inspection-failed',calls)
        self.assertNotIn('incorrect-empty',calls)

    def test_abandoned_maintenance_recovery_checks_the_recorded_owner(self):
        body=SCRIPT.read_text();start=body.index('recover_maintenance() {');end=body.index('run_internal_upgrade()',start)
        for scenario in ('owned','foreign','unhealthy'):
            with self.subTest(scenario=scenario):
                p,calls=self.run_script(body[start:end]+r'''
printf '%s\n' owner > "$MAINTENANCE_FLAG"
printf '%s\n' MAINTENANCE_JOB_ID=owner > "$MAINTENANCE_STATE_FILE"
docker() {
  if [[ "$*" == *drain-protocol* ]]; then echo 1
  elif [[ "$1" == inspect ]]; then echo live-container
  else printf '%s\n' '{"protocol":1,"job_id":"owner","draining":true}'
  fi
}
disable_request_gate() { record recovered; }
'''+({'owned':'','foreign':'printf "%s\\n" foreign > "$MAINTENANCE_FLAG"\n','unhealthy':'wait_for_app_health() { record unhealthy; return 1; }\n'}[scenario])+'recover_maintenance\n')
                self.assertEqual(p.returncode==0,scenario=='owned',p.stderr)
                self.assertEqual(calls,{"owned":["health","recovered","public"],"foreign":[],"unhealthy":["unhealthy"]}[scenario])

if __name__ == '__main__':unittest.main()
