# DBaaS read endpoint / replica invariant

## Implemented boundary

A configured read-only endpoint requires PostgreSQL, MySQL or MariaDB and at
least two declared instances. CREATE and SET_READ_ENDPOINT retain their existing
400 validation messages. SCALE_REPLICAS now checks the **current Application
callback values**, not the cloud-resource cache or the earlier engine read:

1. Recheck the current engine's action capability and provider replica offer.
2. Read its current explicit boolean `network.readEndpoint` policy.
3. If that reader is enabled and the proposed count is below two, return **409**:
   `Disable the read-only endpoint before scaling below two instances`.
4. Only after validation assign `instances` and re-apply the full existing spec.

Disabling the reader is an explicit customer action. Scaling does not silently
change its network policy, CIDRs, chart pin, resource sizing or credentials.
Unrelated repair actions remain usable on legacy invalid values: there is no
new global validator in PatchDatabaseValues.

## C4 component and sequence view

```text
Authorized existing database action
  -> project.dbaasAction
       -> first live engine/capability read
       -> dbaas.Service.PatchDatabaseValues
            -> fresh owned Application JSON
            -> current engine + offer + reader/replica validation
            -> refusal: 400/409, no Apply or Secret write
            -> success: existing full-spec Application Apply

Read-only database sync
  -> current engine + strict declared count + exact boolean reader policy
  -> effective reader flags and optional reader-Service lookup
  -> cache/billing input projection (no desired-state or money write)
```

The shared pure ValidateReadEndpoint rule is in the DBaaS config component.
EffectiveReadEndpoint additionally requires an exact `true` boolean and a valid
positive integral declared count. Generated integers and JSON numbers are
accepted; missing/string/fractional/non-finite/overflow values are not coerced
into a valid reader configuration.

## Legacy projection and pricing limits

For legacy one-instance, unsupported-engine or malformed reader configuration,
sync reports `read_endpoint=false`, `read_endpoint_lb=false` and an empty
`read_endpoint_host`. It skips an unnecessary reader-Service lookup and ignores
any stray previously supplied reader host. It does **not** rewrite desired
Application values or automatically disable a policy.

For valid PostgreSQL configurations, the configured reader-LB pricing flag keeps
its existing meaning even before the reader ingress address is assigned. It is
not physical LB existence/readiness or successful replica-routing evidence.
MySQL/MariaDB reader configuration still uses the existing proxy rather than a
second chargeable LB. The existing writer endpoint/compute billing eligibility
contract is unchanged; an empty reader host is not a compute-charge waiver.
There is no historical monetary reconciliation in this change.

## Bounded verification and non-goals

- Deep-copy fake Application reads exercise stale-cache contradictions, changes
  between the initial engine read and mutation callback, offered-count checks,
  enabled-reader downscale refusal with zero Apply/Secret writes and complete
  Application equality, explicit disable then scale-to-one, and valid counts.
- Pure/projection tests cover supported/unsupported engines, exact booleans,
  malformed counts and preserved desired JSON. Existing billing-resource input
  tests confirm only structurally impossible reader flags are excluded.
- The cached Helm executable renders the unchanged PostgreSQL/MariaDB chart at
  one/three instances to show why impossible readers must not be advertised.
  Rendering is not an operator, LB, routing or engine acceptance test.

The existing read/full-spec server-side Apply path is **not** a transaction,
compare-and-swap or serialized lost-update solution. An external writer can still
race after validation; this fix does not add backend operation fencing or alter
global SSA behavior. No chart/operator/frontend/schema/pricing implementation,
financial posting, credentials, live database, registry hold or saved-runtime
capability is changed. Full DBaaS engine/day-2 acceptance remains separate.
