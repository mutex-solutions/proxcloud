# Sandbox startup and builder budgets

## Intentional runtime-clock migration

The earlier custom-template-only 120-second readiness allowance and TTL-from-create
policy are replaced by one provider-controlled phased startup contract for built-in,
project-built, imported and snapshot templates, native create, resume, fork and wake.
This is an intentional product migration, not a regression to the older fallback.

`config.sandbox.startup` has four integer fields, in seconds:

| Field | Default | Meaning |
|---|---:|---|
| `totalSeconds` | 900 | Entire readiness wait; phase transitions cannot reset it |
| `schedulingSeconds` | 120 | Cumulative scheduling/capacity wait |
| `pullSeconds` | 720 | Cumulative immutable image pull wait |
| `runtimeSeconds` | 600 | Cumulative runtime/envd/start/ready command wait |

The hard startup ceiling is 1800 seconds. The sandbox is usable only after readiness
and the first-Ready runtime TTL patch are committed with UID/resourceVersion compare-
and-swap. A finite startup+runtime provisional deadline protects an API crash. The
first Ready clock is immutable across later reads, readiness flaps and pod restarts;
PENDING time is not billed. A failed TTL patch must not produce usable success.

The API retains an outer deadline covering snapshot readiness, sandbox readiness,
cache persistence and final access retrieval. It rechecks cancellation after reads,
so a transport returning a late successful GET cannot produce a late `201`. Cleanup
has its own detached, finite 30-second budget. A disconnected client is not permission
to keep an unobserved create running indefinitely.

A resume lease covers snapshot wait plus the maximum startup budget and cleanup
margin. UNKNOWN or ambiguous resume ownership is not cleared by lease age, `Alive`,
or a missing GET; this prevents duplicate creates from stale claims. Proxy wake is
finite and its client/server request budgets must outlive the configured critical
path. SDK users must configure a sufficiently long request timeout (for default total
startup, e.g. `request_timeout=930`, `retries=0`); uncertain create requests are not safe
to blindly retry because generic create idempotence remains unsupported.

## Independent runtime and builder resources

Template `cpuCount` and `memoryMB` define the future runtime sandbox size. They do not
select the worker's build budget. `buildCpu`, `buildMemoryGiB` and `buildDiskGiB` choose
independent worker resources within `config.sandbox.build` defaults/caps:

```json
{
  "cpu": 2,
  "memoryGiB": 4,
  "diskGiB": 30,
  "maxCpu": 8,
  "maxMemoryGiB": 16,
  "maxDiskGiB": 100,
  "maxConcurrent": 2
}
```

Operator defaults are policy; callers cannot exceed caps. All stages request and
limit ephemeral disk appropriately. Container exit/signal/OOM, resource limits,
scheduling/resource pressure and phase output are retained as sanitized diagnostics.

## Durable logs and compensation

The API records an append-only stage-prefixed bounded log prefix. `logsOffset` is a
stable array index, and `nextLogsOffset` advances without sliding/renumbering old
lines. Container source byte limits, archive truncation, rotated prefixes and
permanent output loss are explicit. A bounded terminal tail preserves final stderr
when the main prefix is full. Terminal output is recaptured until stable or the
bounded retry window records permanent loss; only then can normal worker cleanup run.
Snapshot assembly workers follow the same terminal archive rule, and explicit internal
snapshot teardown removes its worker before its last maintenance owner is deleted.

Worker admission reserves durable capacity before Kubernetes I/O; SQL name/admission
callbacks do not perform new cluster scans or applies. Existing provider-wide rotation
fencing still uses a SQL callback across dispatch as a compatibility exception. Pin
acquisition/authorization happens before that callback and never strips inherited SQL
context. BUILD and IMPORT use distinct immutable operation proofs and shared single-
use state. Request cancellation is durable compensation intent, not evidence of no
effect. Only invalidating an unused proof, or positive terminal evidence for the exact
owned worker, can settle receipts. UNKNOWN is not automatically reopened or released.

All successful/tagged artifact history is retained until an explicit reference-aware
history deletion feature exists. Within one snapshot template, repeated generations
naming the same exact immutable image count once for stored snapshot usage (the maximum
known captured layer-byte size); distinct retained image digests remain additive. This
does not introduce cross-template/project deduplication or claim full image-byte metering. Physical registry deletion remains HOLD globally and
on teardown; this port does not complete legacy/provider/catalog writer coverage.

## Preparation and console contract

`POST /templates/{id}/prepare` admits/refreshes finite warming intent; GET reports
current eligible-node N/M cache coverage. Warming is default-off, requires a trusted
pull-only runtime, and is independent of runtime readiness. Import capabilities
require valid trusted provider policy, a digest-pinned operator worker and durable pin
protection. Missing capabilities are reported honestly rather than offered optimistically.

Console routes require a session with project cloud-resource management rights, reads
included. Runtime/builder defaults and limits come from capabilities. A console build
creates a new name from a ready project base and accepts 1–100 RUN steps only. COPY,
ENV, raw base credentials and foreign principal/project fields are rejected. Safe DTOs
exclude credentials, tokens, import provenance internals, pin receipts and runtime ENV.
