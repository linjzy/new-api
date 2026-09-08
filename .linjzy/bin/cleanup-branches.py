#!/usr/bin/env python3
"""Remove every non-main branch at the start of the serialized release job."""
import argparse
import json
import re
import subprocess


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def cleanup(repository):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("invalid repository")
    remote = command("git", "remote", "get-url", "origin")
    if remote.removesuffix(".git") != f"https://github.com/{repository}":
        raise ValueError("checkout remote does not match the requested repository")
    if command("gh", "api", f"repos/{repository}", "--jq", ".default_branch") != "main":
        raise ValueError("expected main as the default branch")
    heads = {ref: sha for sha, ref in
             (line.split("\t") for line in command("git", "ls-remote", "--heads", "origin").splitlines())}
    if "refs/heads/main" not in heads:
        raise ValueError("main is missing; no branches will be deleted")
    refs = {ref: sha for ref, sha in heads.items() if ref != "refs/heads/main"}
    print(json.dumps({"keep": ["main"], "delete": sorted(ref.removeprefix("refs/heads/") for ref in refs)},
                     indent=2), flush=True)
    if refs:
        subprocess.run(["git", "push", "--atomic",
                        *[f"--force-with-lease={ref}:{sha}" for ref, sha in refs.items()],
                        "origin", *[f":{ref}" for ref in refs]], check=True)
    remaining = [line.split("\t")[1] for line in
                 command("git", "ls-remote", "--heads", "origin").splitlines()]
    if remaining != ["refs/heads/main"]:
        raise ValueError("remote branches changed during cleanup; build stopped")
    print(f"Deleted {len(refs)} branches before building; kept main")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", required=True)
    cleanup(parser.parse_args().repository)
