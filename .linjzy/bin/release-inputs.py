#!/usr/bin/env python3
"""Content identities for build inputs and independently versioned validation."""
import argparse
import hashlib
from pathlib import Path

PATCHES = ("usage-logs-auto-refresh.patch", "sequential-key-mode.patch", "responses-capacity-retry.patch")

def digest_files(root, files):
    digest = hashlib.sha256()
    for name in sorted(files):
        data = (root / name).read_bytes()
        digest.update(name.encode() + b"\0" + str(len(data)).encode() + b"\0" + data)
    return digest.hexdigest()

def identities(bundle):
    patch_digest = hashlib.sha256("".join(hashlib.sha256((bundle / "patches" / name).read_bytes()).hexdigest() + "\n" for name in PATCHES).encode()).hexdigest()
    # The upstream commit also participates in the image tag: it identifies the
    # exact Dockerfile, dependency locks, base-image digests and application tree.
    build_files = ["bin/release-inputs.py", "bin/prepare-release.sh", "bin/prepare-release.py"] + ["patches/" + name for name in PATCHES]
    validation_files = ["bin/verify-release.sh", "bin/smoke-test-image.sh"]
    for folder in ("tests", "deploy"):
        validation_files += [str(f.relative_to(bundle)) for f in (bundle / folder).rglob("*")
                             if f.is_file() and "__pycache__" not in f.parts and f.suffix != ".md"]
    return {"patch_sha": patch_digest, "build_sha": digest_files(bundle, build_files), "validation_sha": digest_files(bundle, validation_files)}

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("bundle", type=Path)
    args = parser.parse_args()
    for key, value in identities(args.bundle).items():
        print(f"{key}={value}")
