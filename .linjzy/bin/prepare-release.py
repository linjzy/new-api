#!/usr/bin/env python3
"""Apply one reviewed patch series to the exact upstream source. No fuzzy merge."""
import argparse
import hashlib
import subprocess
from pathlib import Path

PATCHES = ("usage-logs-auto-refresh.patch", "sequential-key-mode.patch", "responses-capacity-retry.patch")

def prepare(source, release, upstream, source_ref):
    actual = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip()
    if actual != upstream:
        raise ValueError(f"source commit mismatch: expected {upstream}, got {actual}")
    required = ("relaykit/go.mod", "dto/channel_constraints.go", "model/channel_constraint.go")
    missing = [name for name in required if not (source / name).is_file()]
    if missing:
        raise ValueError("upstream relay architecture requires a reviewed patch port: " + ", ".join(missing))
    if any(c.isspace() for c in release + source_ref):
        raise ValueError("release and source ref must not contain whitespace")
    if (source / "web/default/src/features/usage-logs").is_dir():
        frontend = "web/default"
    elif (source / "web/src/features/usage-logs").is_dir():
        frontend = "web"
    else:
        raise ValueError("unsupported upstream frontend layout")
    for name in PATCHES:
        text = (source / ".linjzy/patches" / name).read_text()
        if frontend == "web/default":
            # Only translate diff paths; never rewrite source text or retry an
            # incompatible patch using a different application strategy.
            text = "".join(line.replace("a/web/src/", "a/web/default/src/").replace("b/web/src/", "b/web/default/src/") if line.startswith(("diff --git ", "--- ", "+++ ")) else line for line in text.splitlines(keepends=True))
        subprocess.run(["git", "apply", "--check", "-"], cwd=source, input=text, text=True, check=True)
        subprocess.run(["git", "apply", "-"], cwd=source, input=text, text=True, check=True)
        print(f"[prepare-release] applied {name} ({frontend})")
    (source / "VERSION").write_text(release + "\n")
    patch_sha = hashlib.sha256("".join(hashlib.sha256((source / ".linjzy/patches" / name).read_bytes()).hexdigest()+"\n" for name in PATCHES).encode()).hexdigest()
    (source / ".linjzy/BUILD-METADATA").write_text(f"upstream_repository=https://github.com/QuantumNous/new-api\nupstream_tag={release}\nupstream_commit={upstream}\npatch_sha256={patch_sha}\nsource_ref={source_ref}\nfrontend={frontend}\n")
    subprocess.run(["git", "diff", "--check"], cwd=source, check=True)

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("source", type=Path)
    parser.add_argument("release")
    parser.add_argument("upstream")
    parser.add_argument("source_ref")
    args = parser.parse_args()
    prepare(args.source.resolve(), args.release, args.upstream, args.source_ref)
