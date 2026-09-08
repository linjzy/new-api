import importlib.util
import os
import shutil
import tempfile
import unittest
from pathlib import Path

BUNDLE=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location("release_inputs", BUNDLE/"bin/release-inputs.py")
inputs=importlib.util.module_from_spec(spec)
spec.loader.exec_module(inputs)

class ReleaseIdentity(unittest.TestCase):
    def test_build_changes_invalidate_image_but_test_changes_only_revalidate(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            bundle=root/".linjzy"
            shutil.copytree(BUNDLE,bundle,ignore=shutil.ignore_patterns("__pycache__"))
            workflow=root/".github/workflows/custom-image.yml"
            workflow.parent.mkdir(parents=True)
            shutil.copy2(Path(os.environ.get("CUSTOM_WORKFLOW_FILE", str(BUNDLE.parent/".github/workflows/custom-image.yml"))),workflow)
            original=inputs.identities(bundle)
            f=bundle/"tests/frontend/auto-refresh-errors.test.tsx"
            f.write_text(f.read_text()+"\n// Additional regression\n")
            verification=inputs.identities(bundle)
            self.assertEqual(original['build_sha'],verification['build_sha'])
            self.assertNotEqual(original['validation_sha'],verification['validation_sha'])
            f=bundle/"bin/prepare-release.py"
            f.write_text(f.read_text()+"\n# Build change\n")
            build=inputs.identities(bundle)
            self.assertNotEqual(original['build_sha'],build['build_sha'])
            self.assertEqual(original['patch_sha'],build['patch_sha'])
            deployment=bundle/'deploy/bin/newapi-custom-upgrade.sh'
            deployment.write_text(deployment.read_text()+'\n# Deployment behavior change\n')
            self.assertEqual(build['build_sha'],inputs.identities(bundle)['build_sha'])
            self.assertNotEqual(build['validation_sha'],inputs.identities(bundle)['validation_sha'])
            workflow.write_text(workflow.read_text()+"\n# Pipeline change\n")
            self.assertNotEqual(build['build_sha'],inputs.identities(bundle)['build_sha'])

if __name__=='__main__':unittest.main()
