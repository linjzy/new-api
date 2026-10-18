import importlib.util
import shutil
import tempfile
import unittest
from pathlib import Path

BUNDLE = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("release_inputs", BUNDLE / "bin/release-inputs.py")
inputs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inputs)


class ReleaseIdentity(unittest.TestCase):
    def test_build_changes_invalidate_image_but_validation_changes_only_revalidate(self):
        with tempfile.TemporaryDirectory() as tmp:
            bundle = Path(tmp) / ".linjzy"
            shutil.copytree(BUNDLE, bundle, ignore=shutil.ignore_patterns("__pycache__"))
            original = inputs.identities(bundle)
            test = bundle / "tests/frontend/auto-refresh-errors.test.tsx"
            test.write_text(test.read_text() + "\n// Additional regression\n")
            verification = inputs.identities(bundle)
            self.assertEqual(original["build_sha"], verification["build_sha"])
            self.assertNotEqual(original["validation_sha"], verification["validation_sha"])
            deployment = bundle / "deploy/bin/newapi-custom-upgrade.sh"
            deployment.write_text(deployment.read_text() + "\n# Deployment behavior change\n")
            deployed = inputs.identities(bundle)
            self.assertEqual(verification["build_sha"], deployed["build_sha"])
            self.assertNotEqual(verification["validation_sha"], deployed["validation_sha"])
            readme = bundle / "deploy/README.md"
            readme.write_text(readme.read_text() + "\nDocumentation only.\n")
            self.assertEqual(deployed, inputs.identities(bundle))
            prepare = bundle / "bin/prepare-release.py"
            prepare.write_text(prepare.read_text() + "\n# Build change\n")
            build = inputs.identities(bundle)
            self.assertNotEqual(original["build_sha"], build["build_sha"])
            self.assertEqual(original["patch_sha"], build["patch_sha"])


if __name__ == "__main__":
    unittest.main()
