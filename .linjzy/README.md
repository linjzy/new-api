# Custom release image pipeline

This public fork builds a small, auditable customization bundle on top of
published new-api releases. Production hosts pull prebuilt images from GHCR
and do not clone source code or run Docker builds.

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
3. Applies every reviewed patch in `.linjzy/patches/` without a fallback merge.
4. Runs the controller, DTO, service, middleware, relay, model, and standalone relaykit tests.
5. Builds the upstream Dockerfile for `linux/amd64`; the frontend stage runs
   lint on every customized TypeScript file, the auto-refresh regression test,
   typecheck, and the production build.
6. Runs an isolated container smoke test.
7. Pushes the public source branch and immutable GHCR image.
8. Updates `candidate` only after all checks pass.

The workflow does not contain or use production-server credentials.

## Upstream test fixes

`task-plugin-model-drift-test.patch` makes the final task decoder regression
test independent of JavaScript runtime reuse. The test uses an injected host
clock to return the pinned model on the initial decode and a different model
on the final decode of the same request. A module-local call counter is
unreliable because the engine's `sync.Pool` may return a fresh runtime.
The patch preserves the HTTP 400 and pinned-model rejection assertions and
changes no production code. It is included in the patch hash so builds use a
new immutable source branch and image instead of reusing an older test tree.
