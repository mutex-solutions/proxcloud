# C4 code addendum: import reference fence

**Implemented scope:** the cooperative, PRIMARY-database import proof and pin
transitions in [import.go](<../../../internal/registrypins/import.go>). This is not
a claim of runtime/API adoption, all-writer coverage, legacy adoption, live rollout
or GC safety. Both production manifest-delete paths remain on unconditional
`RealDeleteHold`; this change performs no registry or cluster I/O.

## Boundaries

| Component | Responsibility and invariant |
|---|---|
| Application owner | Wins its durable operation/project/owner-generation CAS before authorizing dispatch; retains the same operation UUID after ambiguity, never resends under another ID. Independently reads pod annotations and the durable owner/result binding. |
| `Store.AuthorizeImportDispatch` | Validates exact credential-free immutable source and destination representation, current-own complete PENDING/no-UID receipt set, current ENABLED/DISABLED DB project and absence of a build marker. Inserts one durable operation-PK import marker before returning proof. |
| `ImportProof.Consume` | DB/network-free exact project/source/destination equality. Private identity and the same private `proofState` pointer used by build envelopes make value copies single-use. An import proof is a distinct Go type, not a weakened build proof. |
| `CancelUnusedImportDispatch` | Checks exact operation, owner, generation, host and opaque proof; atomically invalidates its shared once bit **before** the DB CAS to `known_no_effect`. Used/lost/ambiguous proof has no cancel/reissue/clear path. |
| `RecordImportPod` / `ObserveImportPod` | Exact operation/nonce/source/destination plus independently observed UID. The marker advances monotonically to Succeeded/Failed; same-envelope stale Running cannot downgrade it. PENDING receipts retain their reference aliases with the exact pod UID/nonce. |
| `guardImportPin` | Runs alongside the build guard in **every** shared pin-update transaction, including when no build marker exists. Evidence comparisons and pin CAS are atomic under the host lock; wrong-action pod/release methods cannot bypass the import guard. |
| `ObserveBinding` / `ReleaseBinding` | After exact successful terminal pod evidence, transfer the protected destination to independently observed durable owner UID + frozen generation + source digest. The new owner UID may differ from the pod UID; the prior PENDING pod receipt must match the marker. Later release requires the exact transferred owner receipt. |

All public DB methods reject inherited transactions through the existing store
transaction primitive. Isolation is explicitly READ COMMITTED **before** the
PRIMARY probe; acquisition/recording/cancel locks host namespace → project →
marker/receipt rows. Updates use the same host transaction and full receipt CAS.
No public callback, network action, lease, expiry, retry gate or caller boolean is
introduced. Build authorization still requires a **tagged** destination; a narrow
symmetric marker check rejects import/build UUID reuse in both directions.

## Representation determines kind and pin set

```text
managed verify:
  source       = HOST/t/CURRENT_PROJECT/20hex@sha256:A
  destination  = HOST/t/CURRENT_PROJECT/20hex       (same bare repository)
  required pins= { source digest }                 (exactly one)

copy:
  source       = canonical host[:port]/repository@sha256:A
  destination  = HOST/t/CURRENT_PROJECT/NEW_20hex:buildID
  required pins= { destination tag } + { source, if current-own managed }
```

A source in trusted `HOST/t/` with foreign/malformed project scope fails closed;
it never falls back to external. Source URLs, userinfo, queries, fragments,
combined tag+digest, mutable tags and non-canonical/oversized references are
rejected. Fully qualified external/library sources are only syntactic image
references here: `Service.StartImport` must separately apply operator-configured
HTTPS origin, registry URL, DNS and credential policy **before dispatch**. This DB
proof grants no external-network or registry authentication permission and stores
no credentials or URL payloads. Only strict managed current-own refs become pins.

## Lifecycle and positive evidence

```text
PENDING/no UID --AuthorizeImportDispatch--> marker authorized + opaque once proof
    | unused same-proof cancel wins         | exact pod observed
    v                                       v
marker known_no_effect                marker observed; pin stays PENDING + UID/nonce
    | exact pending cleanup                 | exact Succeeded / Failed
    v                                       v
RELEASED                              marker terminal (monotonic)
                                            | Failed: release exact observed pins
                                            | Succeeded: source-only copy pin releases
                                            | Succeeded: destination transfers to BOUND
```

A successful verify's sole source pin **is also its runtime destination** and
cannot be released by `ReleaseImportTerminal`. A successful copy's tagged
PENDING destination likewise cannot release until `ObserveBinding` transfers it.
The canonical result repository must match the frozen destination and its digest
must equal immutable source digest A; phase alone never invents a result, and a
copy destination digest B is rejected without dropping its tag protection.

`MarkUnknown` on an active nonterminal pin marks the durable import operation
UNKNOWN and retains every unreleased operation reference. Outstanding PENDING
siblings cannot cancel/release/bind through this marker. There is no age-based
expiry or clearing API. Terminal/known-no-effect markers reject late UNKNOWN
poisoning; conflicting terminal phases, UIDs, refs or generations fail closed.
Cancellation/timeout/missing pods do not prove no effect.

## Verification scope

[import_test.go](<../../../internal/registrypins/import_test.go>) checks exact
representation/pin sets, malformed and credential-bearing sources, and concurrent
value-copy consumption. [import_postgres_test.go](<../../../internal/registrypins/import_postgres_test.go>)
uses an explicit disposable loopback-only PostgreSQL URL from
`REGISTRYPINS_TEST_POSTGRES`; unset means skipped, not simulated evidence. It
exercises two real pools (one defaults to REPEATABLE READ), durable one-shot mint,
cancel/consume races and canceled DB writes, cross-kind markers, complete pin sets,
current project scope, inherited transaction rejection, monotonic actual pod
annotations, exact terminal release, owner transfer, wrong-digest rejection and
nonexpiring UNKNOWN/outstanding-reference HOLDs. Tests never pull images or
contact registry/Kubernetes/lab services.

This fence is one prerequisite only. No code here authorizes a physical DELETE,
asserts complete writer coverage/adoption, or turns the operator HOLD off.
