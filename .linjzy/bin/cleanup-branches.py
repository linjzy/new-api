#!/usr/bin/env python3
"""Delete generated release refs after a verified deployment, under the CI lock."""
import argparse
import json
import re
import subprocess
from pathlib import Path

MANAGED_REF = re.compile(r"custom/[0-9][A-Za-z0-9._-]*-(?:custom|autorefresh)-[0-9a-f]{12}-[0-9a-f]{12}")
SHA = re.compile(r"[0-9a-f]{40}")


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def source_from_config(config, repository):
    if config.get("os") != "linux" or config.get("architecture") != "amd64":
        raise ValueError("expected a single linux/amd64 image")
    labels = config.get("config", {}).get("Labels", {})
    source = labels.get("com.linjzy.new-api.source-ref", "")
    commit = labels.get("org.opencontainers.image.revision", "")
    if (labels.get("com.linjzy.new-api.managed") != "true"
            or labels.get("org.opencontainers.image.source") != f"https://github.com/{repository}"
            or not MANAGED_REF.fullmatch(source) or not SHA.fullmatch(commit)):
        raise ValueError("invalid managed image source labels")
    return source, commit


def deletion_plan(heads, default_branch, production, candidate):
    if default_branch != "main" or default_branch not in heads:
        raise ValueError("unexpected default branch; review cleanup configuration")
    for source, commit in (production, candidate):
        if not MANAGED_REF.fullmatch(source) or not SHA.fullmatch(commit) or heads.get(source) != commit:
            raise ValueError(f"protected source is missing or changed: {source}")
    keep = {default_branch, production[0], candidate[0]}
    return {name: sha for name, sha in heads.items() if MANAGED_REF.fullmatch(name) and name not in keep}


def delete_refs(remote, refs):
    if not refs:
        return
    subprocess.run(["git", "push", "--atomic",
                    *[f"--force-with-lease=refs/heads/{name}:{sha}" for name, sha in refs.items()],
                    remote, *[f":refs/heads/{name}" for name in refs]], check=True)


def cleanup(repository, production_image, production_source, production_commit, dry_run=False):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid repository")
    image_repository = f"ghcr.io/{repository.lower()}"
    if not re.fullmatch(re.escape(image_repository) + r"@sha256:[0-9a-f]{64}", production_image):
        raise ValueError("production image must be an immutable digest in this repository")
    remote = command("git", "remote", "get-url", "origin")
    if remote.removesuffix(".git") != f"https://github.com/{repository}":
        raise ValueError("checkout remote does not match the requested repository")
    default = command("gh", "api", f"repos/{repository}", "--jq", ".default_branch")
    candidate_tag = image_repository + ":candidate"
    digest_script = str(Path(__file__).with_name("registry-digest.sh"))
    candidate_digest = command("bash", digest_script, candidate_tag)
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", candidate_digest):
        raise ValueError("candidate is missing; no branches will be deleted")
    candidate_image = image_repository + "@" + candidate_digest
    production = source_from_config(json.loads(command(
        "docker", "buildx", "imagetools", "inspect", production_image, "--format", "{{json .Image}}")), repository)
    if production != (production_source, production_commit):
        raise ValueError("deployment report does not match the production image")
    candidate = source_from_config(json.loads(command(
        "docker", "buildx", "imagetools", "inspect", candidate_image, "--format", "{{json .Image}}")), repository)
    heads = {ref.removeprefix("refs/heads/"): sha for sha, ref in
             (line.split("\t") for line in command("git", "ls-remote", "--heads", "origin").splitlines())}
    refs = deletion_plan(heads, default, production, candidate)
    if command("bash", digest_script, candidate_tag) != candidate_digest:
        raise ValueError("candidate changed during cleanup; no branches will be deleted")
    print(json.dumps({"dry_run": dry_run, "keep": sorted({default, production[0], candidate[0]}),
                      "delete": sorted(refs)}, indent=2), flush=True)
    if not dry_run:
        delete_refs("origin", refs)
        after = {ref.removeprefix("refs/heads/"): sha for sha, ref in
                 (line.split("\t") for line in command("git", "ls-remote", "--heads", "origin").splitlines())}
        if set(refs) & set(after):
            raise ValueError("deleted refs still exist")
        deletion_plan(after, default, production, candidate)
    print(f"{'Would delete' if dry_run else 'Deleted'} {len(refs)} obsolete release branches")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", required=True)
    parser.add_argument("--production-image", required=True)
    parser.add_argument("--production-source", required=True)
    parser.add_argument("--production-commit", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    cleanup(args.repository, args.production_image, args.production_source, args.production_commit, args.dry_run)
