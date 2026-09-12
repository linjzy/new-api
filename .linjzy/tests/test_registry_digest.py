import os
import subprocess
import unittest
from pathlib import Path

SCRIPT=Path(__file__).resolve().parents[1]/'bin/registry-digest.sh'

class RegistryDigest(unittest.TestCase):
    def inspect(self, mode):
        env=dict(os.environ, SCRIPT=str(SCRIPT), MODE=mode)
        script='''
set -Eeuo pipefail
docker() {
  case "$MODE" in
    found) printf 'Digest: sha256:%064d\\n' 1 ;;
    absent) printf 'ERROR: manifest unknown\\n' >&2; return 1 ;;
    auth) printf 'ERROR: unauthorized\\n' >&2; return 1 ;;
    network) printf 'ERROR: connection reset\\n' >&2; return 1 ;;
  esac
}
export -f docker
bash "$SCRIPT" ghcr.io/example/new-api:test
'''
        return subprocess.run(['bash','-c',script],env=env,text=True,capture_output=True)

    def test_missing_manifest_is_the_only_cache_miss(self):
        found=self.inspect('found')
        self.assertEqual(found.returncode,0,found.stderr)
        self.assertRegex(found.stdout.strip(),r'^sha256:[0-9a-f]{64}$')
        absent=self.inspect('absent')
        self.assertEqual(absent.returncode,0,absent.stderr)
        self.assertEqual(absent.stdout,'')
        for mode in ['auth','network']:
            with self.subTest(mode=mode): self.assertNotEqual(self.inspect(mode).returncode,0)

if __name__=='__main__':unittest.main()
