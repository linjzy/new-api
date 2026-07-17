# Custom release image pipeline

This public fork builds a small, auditable customization on top of published
new-api releases. Production hosts pull prebuilt images from GHCR and do not
clone source code or run Docker builds.

## Release mapping

Each successful build has three immutable identifiers:

- Upstream release tag and commit.
- Public immutable source branch in this fork:
  `custom/<release>-autorefresh-<patch-sha>-<upstream-sha>`.
- GHCR image tag:
  `<release>-autorefresh-<patch-sha>-<upstream-sha>`.

The generated source branch contains the exact patched source tree, the patch,
the preparation and smoke-test scripts, and `.linjzy/BUILD-METADATA`.

The moving `candidate` image tag is only a discovery pointer. Deployment
scripts must resolve it to an immutable registry digest before changing the
running Compose service.

## Automation

`.github/workflows/custom-image.yml` runs on a schedule and can also be
started manually. It:

1. Selects the newest published, non-draft upstream release unless an explicit
   release is requested.
2. Checks out the exact upstream tag commit.
3. Applies `.linjzy/patches/usage-logs-auto-refresh.patch` without a fallback
   merge.
4. Builds the upstream Dockerfile for `linux/amd64`.
5. Runs an isolated container smoke test.
6. Pushes the public source branch and immutable GHCR image.
7. Updates `candidate` only after all checks pass.

The workflow does not contain or use production-server credentials.
