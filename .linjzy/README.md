# Custom release image pipeline

This public fork builds a small, auditable customization bundle on top of
published new-api releases. The bundle contains exactly three business patches:
usage-log auto-refresh, sequential multi-key mode, and Responses capacity retry.
Production hosts pull prebuilt images from GHCR
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
respects `RetryTimes`, affinity retry policy and channel constraints, and excludes
attempted channels. Affinity changes to the successful channel only when
`switch_on_success` is enabled; otherwise it retains the initial channel.
Exhaustion preserves the capacity error instead of replacing it with a
channel-selection error.

Sequential key rotation does not spend the channel failover budget, including
when a capacity failure occurs before or after retiring invalid keys. The
integration matrix exercises both orders with and without the memory cache.

The byte budget covers the instructions and metadata echoed by Codex lifecycle
events; these can exceed 64 KiB before any generated content is available.

Text, populated/encrypted reasoning, tool/unknown events, nonempty output in a
failed response, and nonzero reported usage all prevent replay. Raw item fields
are inspected so encrypted content and unknown extensions cannot disappear
through DTO decoding. Committed streams preserve the upstream failure event
without appending JSON; partial usage is settled once. Failed streams without
billable tokens or tool calls refund the precharge and create only an error log,
without a duplicate zero-usage consume log. Non-capacity failures,
malformed/truncated streams, and client cancellation do not use this failover.
Error logs retain `upstream_http_status: 200` and the actual stream failure.

Replay checks inspect raw usage, including detail-only counts and unknown
provider billing fields, even when the shared DTO has no token total. Usage
snapshots from earlier lifecycle events are retained if a final failure omits
usage. This prevents replaying upstream work and losing previously reported
usage.

Production needs an enabled alternative channel for the same model/group and
automatic retries enabled. No database migration or configuration change is
required for this patch. Rollback uses the prior immutable app image digest.
