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
4. Runs the controller, DTO, service, middleware, relay, model, and standalone
   relaykit tests/build, the capacity retry race tests, and the end-to-end relay
   tests against SQLite plus disposable MySQL 8 and PostgreSQL 15 databases.
5. Builds the upstream Dockerfile for `linux/amd64`; the frontend stage runs
   lint on every customized TypeScript file, the auto-refresh regression test,
   typecheck, and the production build.
6. Runs an isolated container smoke test.
7. Pushes the public source branch and immutable GHCR image.
8. Updates `candidate` only after all checks pass.

The workflow does not contain or use production-server credentials.

## Responses capacity failures

`responses-capacity-retry.patch` recognizes explicit transient overload/capacity
errors inside an HTTP 200 Responses SSE stream. It withholds only empty initial
lifecycle, message/reasoning item and text-part placeholders (at most 16 events /
2 MiB, bounded by the stream timeout) and
defers SSE headers and pings until output starts. Before output, an unbilled
capacity failure returns a retryable 503 to the existing relay loop. The loop
respects `RetryTimes` and channel constraints, excludes attempted channels, and
updates affinity to the channel that succeeds. Exhaustion preserves the capacity
error instead of replacing it with a channel-selection error.

The byte budget covers the instructions and metadata echoed by Codex lifecycle
events; these can exceed 64 KiB before any generated content is available.

Text, populated/encrypted reasoning, tool/unknown events, nonempty output in a
failed response, and nonzero reported usage all prevent replay. Raw item fields
are inspected so encrypted content and unknown extensions cannot disappear
through DTO decoding. Committed streams preserve the upstream failure event
without appending JSON; partial usage is settled once. Non-capacity failures,
malformed/truncated streams, and client cancellation do not use this failover.
Error logs retain `upstream_http_status: 200` and the actual stream failure.

Production needs an enabled alternative channel for the same model/group and
automatic retries enabled. No database migration or configuration change is
required for this patch. Rollback uses the prior immutable app image digest.

## Upstream test fixes

`task-plugin-model-drift-test.patch` makes the final task decoder regression
test independent of JavaScript runtime reuse. The test uses an injected host
clock to return the pinned model on the initial decode and a different model
on the final decode of the same request. A module-local call counter is
unreliable because the engine's `sync.Pool` may return a fresh runtime.
The patch preserves the HTTP 400 and pinned-model rejection assertions and
changes no production code. It is included in the patch hash so builds use a
new immutable source branch and image instead of reusing an older test tree.
