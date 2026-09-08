# Custom release image pipeline

This public fork builds a small, auditable customization bundle on top of
published new-api releases. The bundle contains exactly three business patches:
usage-log auto-refresh (including stream error display deduplication), sequential
multi-key mode, and Responses capacity retry. Regression tests are maintained separately in `.linjzy/tests/`; business patches
contain no test additions or test modifications.
Production hosts pull prebuilt images from GHCR
and do not clone source code or run Docker builds.

## Release mapping

Each successful build has three immutable identifiers:

- Upstream release tag and commit.
- Public immutable source branch in this fork:
  `custom/<release>-custom-<build-sha>-<upstream-sha>`.
- GHCR image tag:
  `<release>-custom-<build-sha>-<upstream-sha>`.

The generated source branch contains the exact patched source tree, the patch,
the preparation and smoke-test scripts, and `.linjzy/BUILD-METADATA`.

Build identity covers the patch bytes, preparation scripts, identity algorithm
and workflow. The upstream commit covers the exact Dockerfile, pinned base-image
digests and dependency lockfiles. Verification has a separate identity covering
its scripts, regression cases, deployment package and workflow. Changing only
verification reuses the image and reruns the current checks. A `verified-*` tag
records the validation identity and check profile against the same image digest.
Both source and the matching verification digest must exist before work is skipped.

The moving `candidate` image tag is only a discovery pointer. Deployment
scripts must resolve it to an immutable registry digest before changing the
running Compose service.

## Automation

`.github/workflows/custom-image.yml` runs every six hours, on changes to the
customization bundle in `main`, and through manual dispatch. It:

1. Resolves a published, non-draft release and its exact upstream commit.
2. Computes build and verification identities and checks existing artifacts
   before preparing source or installing Go/Bun.
3. Applies the three patches directly to that release. Frontend paths are
   selected explicitly from `web/src` or `web/default/src`; source-content or
   relay-architecture incompatibility fails before building, without a fuzzy merge.
4. Builds standalone relaykit and verifies Responses retry/settlement and exact
   multi-key recovery with SQLite and a disposable PostgreSQL database.
5. Runs MySQL compatibility and race checks for backend changes, scheduled
   builds, or explicitly requested extended checks.
6. Runs frontend error/auto-refresh regressions, lint and typecheck. The Docker
   build produces the production frontend.
7. Runs isolated image startup/status/frontend checks, then publishes the exact
   source, immutable image and verification digest.
8. Updates `candidate` after a scheduled build, or a manual build that explicitly
   requests promotion. A push validates/builds without moving `candidate`.

The release preparation is verified against rc.34 and rc.35. The current fork
`main` has a different relay architecture in addition to moving the frontend;
its code is not a supported release preimage. A future release with that
architecture requires an explicitly reviewed port rather than automatic patch
rewrites. The preflight names the missing architecture files.

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
when a capacity failure occurs before or after retiring invalid keys.

Before output, provider `keepalive` events containing only their type and numeric
sequence/timestamp metadata are discarded. They do not commit SSE or consume the
lifecycle buffer; the absolute prelude timeout still applies. After output they
are forwarded normally. Populated or unknown heartbeat fields prevent replay.

Multi-key retirement persists synchronously before the next selection, while
notifications run asynchronously. Recovery updates the affected channel and
routing membership, preserves polling cursors, and refreshes pricing only when
channel availability changes.

Log-list and statistics requests delegate error notifications to their query
components, keeping authentication refresh and redirects in the shared client.

Usage-log details display the terminal error once and show only distinct
additional stream errors. Stored error data and occurrence counts are preserved.

The byte budget covers the instructions and metadata echoed by Codex lifecycle
events; these can exceed 64 KiB before any generated content is available.

Text, populated/encrypted reasoning, tool/unknown events, nonempty output in a
failed response, and nonzero reported usage all prevent replay. Raw item fields
are inspected so encrypted content and unknown extensions cannot disappear
through DTO decoding. Committed streams preserve the upstream failure event
without appending JSON; partial usage is settled once, including cancellation
between upstream usage receipt and the first client write. Both the HTTP request
layer and stream scanner defer Responses headers and pings until output. Failed streams without
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
required for this patch. Deployment retains only the existing one-job automatic recovery behavior;
there is no retained previous image, backup, or manual rollback history. See
[deployment instructions](deploy/README.md).

## Local verification

Prepare an isolated checkout at a supported upstream commit, copy `.linjzy/`
into it, then run:

```bash
bash .linjzy/bin/prepare-release.sh SOURCE RELEASE UPSTREAM_COMMIT SOURCE_REF
bash .linjzy/bin/verify-release.sh SOURCE all
bash .linjzy/bin/verify-release.sh SOURCE race
python3 -m unittest discover -s .linjzy/tests -p 'test_*.py'
actionlint .github/workflows/custom-image.yml
shellcheck .linjzy/bin/*.sh .linjzy/deploy/bin/*.sh
```

For database compatibility, pass `CUSTOM_TEST_SQL_DRIVER=postgres` or `mysql`
and `CUSTOM_TEST_SQL_DSN` to `verify-release.sh SOURCE database`. These tests
create and delete channel/ability fixtures and require a disposable database.
Never point them at production.
