#!/usr/bin/env python3
"""Keep the versions tagged candidate or previous and the current image; delete other registry versions and their custom/* refs."""
import argparse
import json
import subprocess

POINTERS = ("candidate", "previous")


def gh(*args):
    return subprocess.check_output(["gh", "api", *args], text=True)


def image_tags(version):
    return [tag for tag in version["tags"] if tag not in POINTERS and not tag.startswith("verified-")]


def plan(versions, current_tag):
    keep = [v for v in versions if current_tag in v["tags"] or any(p in v["tags"] for p in POINTERS)]
    keep_tags = {tag for v in keep for tag in image_tags(v)}
    return keep_tags, [v for v in versions if v not in keep]


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--owner", required=True)
    parser.add_argument("--package", required=True)
    parser.add_argument("--current", required=True, help="image tag built or reused by this run")
    args = parser.parse_args()
    package = f"users/{args.owner}/packages/container/{args.package}"
    versions = [json.loads(line) for line in gh(f"{package}/versions?per_page=100", "--paginate", "--jq",
                                                 ".[] | {id, created_at, tags: .metadata.container.tags} | @json").splitlines() if line]
    keep_tags, delete = plan(versions, args.current)
    if args.current not in keep_tags:
        raise SystemExit(f"{args.current} is missing from the registry listing; nothing deleted")
    heads = subprocess.check_output(["git", "ls-remote", "--heads", "origin", "refs/heads/custom/*"], text=True)
    stale = [line.split("\t")[1] for line in heads.splitlines()
             if line.split("\t")[1].removeprefix("refs/heads/custom/") not in keep_tags]
    print(json.dumps({"keep": sorted(keep_tags), "delete_versions": [v["tags"] or v["id"] for v in delete],
                      "delete_branches": stale}, indent=2), flush=True)
    for version in delete:
        gh("-X", "DELETE", f"{package}/versions/{version['id']}")
    if stale:
        subprocess.run(["git", "push", "origin", *[f":{ref}" for ref in stale]], check=True)
