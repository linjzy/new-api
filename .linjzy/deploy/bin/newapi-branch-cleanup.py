#!/usr/bin/env python3
"""Dispatch cleanup with an Actions-only token; never give the host Git write access."""
import argparse
import json
import os
import re
import stat
import sys
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

WORKFLOW = "custom-cleanup.yml"


def save_state(path, data):
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    with os.fdopen(fd, "w") as output:
        json.dump(data, output)
        output.write("\n")
    os.replace(tmp, path)


class CleanupDispatch:
    def __init__(self, repository, token_file, state_file):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("invalid source repository")
        info = token_file.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077:
            raise ValueError("GitHub token file must be private (mode 600)")
        self.token = token_file.read_text().strip()
        if not self.token or any(c.isspace() for c in self.token):
            raise ValueError("GitHub token file is empty or invalid")
        self.repository = repository
        self.state_file = state_file

    def api(self, path, data=None):
        request = urllib.request.Request(
            f"https://api.github.com/repos/{self.repository}/{path}",
            data=None if data is None else json.dumps(data).encode(),
            headers={"Authorization": f"Bearer {self.token}", "Accept": "application/vnd.github+json",
                     "Content-Type": "application/json", "X-GitHub-Api-Version": "2026-03-10"})
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)

    def state(self):
        return json.loads(self.state_file.read_text()) if self.state_file.exists() else {}

    def check(self, same_image=None):
        state = self.state()
        if state.get("phase") == "submitting":
            raise ValueError("cleanup dispatch result is unknown; inspect GitHub Actions before another deployment")
        if state.get("phase") != "submitted":
            return True
        run_id = state.get("run_id")
        if not isinstance(run_id, int) or run_id <= 0:
            raise ValueError("invalid pending cleanup run ID")
        run = self.api(f"actions/runs/{run_id}")
        if run.get("path") != f".github/workflows/{WORKFLOW}" or run.get("event") != "workflow_dispatch":
            raise ValueError("pending run does not match the cleanup workflow")
        if run.get("status") != "completed":
            if same_image is not None and state.get("image") == same_image:
                print(f"Cleanup already submitted: run {run_id}")
                return False
            raise ValueError(f"cleanup run {run_id} is still {run.get('status')}; deploy after it finishes")
        state.update(phase="finished", conclusion=run.get("conclusion"))
        save_state(self.state_file, state)
        print(f"Previous cleanup run {run_id}: {run.get('conclusion')}")
        return True

    def submit(self, image, source, commit):
        if not re.fullmatch(re.escape(f"ghcr.io/{self.repository.lower()}") + r"@sha256:[0-9a-f]{64}", image):
            raise ValueError("invalid production image digest")
        if (not re.fullmatch(r"custom/[0-9][A-Za-z0-9._-]*-(?:custom|autorefresh)-[0-9a-f]{12}-[0-9a-f]{12}", source)
                or not re.fullmatch(r"[0-9a-f]{40}", commit)):
            raise ValueError("invalid production source")
        if not self.check(same_image=image):
            return
        state = {"phase": "submitting", "image": image, "source": source, "commit": commit}
        save_state(self.state_file, state)
        try:
            result = self.api(f"actions/workflows/{WORKFLOW}/dispatches", {
                "ref": "main", "inputs": {"production_image": image, "production_source": source,
                                            "production_commit": commit, "dry_run": False}})
        except urllib.error.HTTPError as error:
            if 400 <= error.code < 500 and error.code != 408:
                state.update(phase="rejected", http_status=error.code)
                save_state(self.state_file, state)
            raise ValueError(f"cleanup dispatch failed with HTTP {error.code}") from None
        run_id = result.get("workflow_run_id")
        if not isinstance(run_id, int) or run_id <= 0:
            raise ValueError("cleanup dispatch returned no run ID; inspect GitHub Actions before another deployment")
        state.update(phase="submitted", run_id=run_id)
        save_state(self.state_file, state)
        print(f"Branch cleanup submitted: https://github.com/{self.repository}/actions/runs/{run_id}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", required=True)
    parser.add_argument("--token-file", required=True, type=Path)
    parser.add_argument("--state-file", required=True, type=Path)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("check")
    submit = sub.add_parser("submit")
    submit.add_argument("image")
    submit.add_argument("source")
    submit.add_argument("commit")
    args = parser.parse_args()
    try:
        dispatch = CleanupDispatch(args.repository, args.token_file, args.state_file)
        if args.command == "check":
            dispatch.check()
        else:
            dispatch.submit(args.image, args.source, args.commit)
    except (OSError, ValueError, urllib.error.URLError) as error:
        print(f"Branch cleanup: {error}", file=sys.stderr)
        sys.exit(1)
