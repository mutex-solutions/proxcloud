# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Sandboxes on the managed platform are offered** (#483, `deploy/platform/platform-bom.yaml`,
  [managed-services.md](docs/managed-services.md#sandboxes-on-the-platform)). The platform BOM names the sandbox
  chart, `proxcloud-sandbox` 0.1.0 at its published digest, as the component `sandbox` (`feature: sandbox`), and
  moves the platform's own cluster chart from `proxmox-kamaji-cluster` 0.1.16 to 0.1.19, the chart the tenants are
  on already and the first pin with a node group's own pool, address range and DNS servers. The code of #420, #422,
  #423, #432 and #436 was in the build and switched off; with these pins an installation whose settings hold
  `spec.sandbox` gets its sandbox machines (a node group of the platform cluster, or the cluster `pc-sandbox`), the
  sandbox chart and a sandbox provider that is registered disabled and goes live only through its release gates.
  The install page and the settings of a ready platform show the **Sandboxes** block with the texts of #436: Off
  is where every installation starts, and a shared node group is saved only with the tick under what it gives up.
  - **An installation without the block plans and renders as before**, and a run makes no pool, ACL row, token,
    machine, Application or provider for sandboxes (`TestApplyWithoutSandboxWritesNothingOfIt`, now against the
    real BOM; `TestSandboxRenderGolden`).
  - **A platform that an earlier release installed gets nothing until its operator asks.** It stays on its BOM,
    where the block is refused (`400 invalid_installation`; the answer now names the release to upgrade to) and
    the page shows no block. Its upgrade to this BOM has two rows more than the release: `seed/pc-platform`, class
    chart (0.1.16 to 0.1.19), and `workload/sandbox`, class added, which nothing renders without the block. No
    machines row. Then the block is taken, and an apply builds the group beside the existing ones
    (`TestUpgradeFromTheLastReleaseBuildsNoSandbox`, the real BOM of 0.11.1 to this one on fakes;
    `TestPreviousBOMsMoveCertManager` states both rows for each of the fourteen previous BOMs an upgrade is
    allowed from).
  - **The move of the cluster chart replaces no machine.** `TestUpgradeRollsNoMachine` compares those fourteen
    BOMs. A render of the published charts by hand, every object and not only the machine objects: for the
    thirteen BOMs on 0.1.16 (0.10.1-rc.2 to 0.11.1) chart 0.1.19 renders the same objects, with that release's
    values and with this build's; for 0.10.1-rc.1 (chart 0.1.15) the one difference is the node deletion timeout
    of 0.1.16, which the run's check leaves out as before and no machine is replaced for.
  - **`TestSandboxIsDark` and `TestSandboxRunIsDark` are gone on purpose.** `TestSandboxIsOffered` states the
    opposite (this BOM names the chart after every other component of the cluster, both placements are taken on
    it, every previous BOM is refused with the upgrade named), and `TestSandboxRunRefusedOnABOMWithoutTheChart`
    keeps the run's refusal for a platform still on the BOM of 0.11.1. The tests of what a run builds, plans,
    checks and refuses for sandboxes (#432, #436) no longer run against a synthetic BOM: they run against this
    one.
  - **Nothing of it ran on a lab**: Proxmox, ArgoCD, Cluster API, CAPMOX, Kamaji and the clusters are fakes in
    these tests, and the charts are rendered, not installed. The release candidate's drill is the first run
    ([Not verified on a lab yet](docs/managed-services.md#not-verified-on-a-lab-yet)).

### Changed

- **The built-in sandbox template images are those of the Actions build of 2026-11-23** (`deploy/templates/images.yaml`,
  run 45167180286 of `sandbox-templates` on commit `57e85c5`; the commit of #443, carried here so that the checks run
  on it). No file under `deploy/templates` or `cmd/mcp-gateway` changed between that commit and this one. On the
  managed platform these digests are the sandbox provider's templates and the chart's prepull, pulled from ghcr.io,
  and they move with a release and no other way; on the installer path the next `make upgrade` mirrors them
  ([managed-sandbox.md](docs/managed-sandbox.md), section 6).

- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1.yaml`, sha256
  `b04cdabce8ffd004309fbb2e34ffe1a9871648db103553b4badc5bd17be69172`, the file of the tag byte for byte). The
  `v0.11.1` release commit left the BOM at `0.11.1`, and the release gate refuses only `-dev`. For an upgrade: a
  platform installed by `v0.11.1` or moved to its BOM is not refused (`bom_unsupported`) under a later build while
  the file is carried. Its pins were those of `main` until the sandbox pins above, so the plan from it now has
  three changes: the release (record only), the platform's cluster chart (class chart) and the sandbox chart
  (class added); no seed part, no machine replaced. `TestPreviousBOMsMoveCertManager` states that answer, and
  `TestUpgradeRollsNoMachine` compares fourteen of the twenty-three previous BOMs with the BOM on `main`, none of
  them rolling. No move to that BOM and no apply on such a platform was run on a lab.
- **Fourteen dependency updates (November)**, the open Dependabot pull requests taken together. Go modules:
  `aws-sdk-go-v2/service/s3` 1.116.0, `aws-sdk-go-v2/service/ecr` 1.69.0, `aws-sdk-go-v2/credentials` 1.21.3,
  `golang.org/x/crypto` 0.59.0 (with `x/sys` 0.50.0 and `x/text` 0.44.0) and `golang.org/x/sync` 0.25.0; the module
  minimum stays `go 1.26.0`. Customer console (`web/client`): `recharts` 3.11.0, `@tanstack/react-query` 5.108.2,
  `@vitejs/plugin-react` 6.3.0 and `@axe-core/playwright` 4.14.0. Operator console (`web/admin`): `recharts` 3.11.0,
  `@tanstack/react-query` 5.108.2, `react-router-dom` 7.18.6 and `lucide-react` 1.77.0. Workflows:
  `oras-project/setup-oras` v2.0.4. What changes with them:
  - **An `ssh-dss` public key is refused.** `x/crypto` 0.59.0 removed DSA from its `ssh` package, so a key pair
    import and the SSH keys of a platform installation refuse such a key, where they accepted it before. The key
    pair import answers 400 "ssh-dss (DSA) keys are no longer accepted: use an ed25519, ecdsa or rsa key"
    (`TestKeypairImportRefusesDSA`, which generates a DSA key; the same test passes the key on `x/crypto` 0.58.0);
    a platform installation answers that the entry "is not an SSH public key". OpenSSH itself no longer accepts
    these keys.
  - **`setup-oras` downloads the ORAS CLI without a token.** Since v2.0.4 the action sends the job token with the
    download by default; the two workflows that publish and verify a VM artifact pass `token: ''`, and their tests
    assert the whole step (`images/tests/test_vm_export_workflow.py`, `images/tests/test_vm_publish_workflow.py`).
    The action runs on node24, which the hosted runners already forced on the node20 build of v1.2.4.
  - **`@axe-core/playwright` 4.14.0 declares Node 22.12 or newer** (a development dependency of the customer
    console: its accessibility specs). Builds and CI use Node 24.22.2.
  - **`@vitejs/plugin-react` 6.3.0 in `web/client` needed `npm install --force`**: npm refuses it alone over an
    optional peer that is not installed (`@rolldown/plugin-babel` 0.3 wants `@babel/core` 8, `shadcn` keeps 7). The
    lockfile is the one Dependabot produced, and `web/admin` already had the same pair.
  - **Not taken: `tailwind-merge` 3.7.1 in `web/admin`.** It keeps an earlier `leading-*` class when a font size
    follows, so every card title that passes a text size renders at line-height 1 and the content under it moves up
    8 px: 75 of the 202 screenshots of the operator console differed by more than 20000 pixels, and none of the
    twenty sampled ones once this package was put back. The customer console would have stayed on 3.7.0.
  - **Not taken: `@airwallex/components-sdk` 1.37.1 in `web/client`.** Since 1.37.0 the SDK loads its script from
    `checkout.openpay.com` and `static.openpay.com` when the Airwallex host does not answer. That `openpay.com` is
    Airwallex's own could not be established from public sources: its registrant is hidden behind a privacy
    service, no Airwallex or OpenPay page names the domain (OpenPay, which Airwallex bought in 2025, publishes
    under `getopenpay.com`), and the only source is the SDK's own changelog. A script that takes card data is not
    loaded from such a host on that. The console stays on 1.36.0 until the owner has decided.
  Not verified on a lab: no VM artifact was exported with the new `setup-oras` pin (neither workflow runs on a
  pull request).

### Fixed

- **A cluster's load balancer controller lets a deleted Service go under a rolled-back portal whose state is near
  64 KiB, and 50 deleted Services with long keep-address values keep their wishes** (`cmd/lb-controller`,
  `internal/cloud/lbgw/annotations.go`; the portal's code is not changed). Two cases of a request that names
  Services being deleted (`gone`, from `v0.11.1-rc.3`):
  - **`413` is followed like `400`.** A portal of `v0.11.0` or of a candidate before `v0.11.1-rc.3` measures the
    whole body against the state's 64 KiB before it reads a field. With a state within a few KiB of that limit,
    the request with `gone` was answered `413` and not the `400` of a field it does not know, and the controller
    stepped down only on `400`: every Service got the Warning `TooLarge`, no change reached the balancer, and the
    Service that was being deleted stayed `Terminating` for as long as the portal was rolled back. The controller
    now sends the request without `gone` after a `413` too, and the request of protocol 1 after a `413` from a
    portal that names an older protocol (a state that the annotations of protocol 2 put over `v0.11.0`'s limit).
    A `413` to a request that carried neither is one request and is reported as before. Against the portal of
    this release, a state that is really over 64 KiB costs one more request a turn while a Service is being
    deleted.
  - **The keep-address value of a deleted Service is sent short.** The controller sent the value of
    `proxcloud.io/lb-keep-address` as written, up to 257 characters. 50 Services deleted together with long
    values (which are no booleans) were over the gateway's 16 KiB for the Services being deleted: the gateway
    answered `400`, the controller sent the request without `gone`, and the Services among them with a valid value
    lost their word too (their addresses then went by the state the portal last stored). The value now goes
    without the spaces around it and with 8 characters at most. A boolean has 5 at most and goes whole; a longer
    value is none and is sent as 7 characters and a `~`, so that the cut never reads as a boolean (`true    x`
    cut after 8 characters would read as `true`).
  For an upgrade: **the controller image carries both**, so a cluster gets them with the offered controller update
  (the cluster's update to the provider's chart version, or "Enable load balancer controller" again) and behaves
  as before until then. Nothing changes for a balancer, an address or a stored state, and a Service with a valid
  keep-address value is read as before. Tests: `TestGatewayPutWithoutGoneAfter413` and
  `TestGatewayPutStepsDownAfter413` (`cmd/lb-controller/gone_test.go`, the second against a handler that reads
  the body as `v0.11.0` does) and `TestGoneAnnotationsAreShort` (`internal/cloud/lbgw/annotations_test.go`,
  through the gateway's handler). Not run on a lab.
- **A run no longer overwrites a provider's `config.sandbox` of another owner that names no pool**
  (`internal/platform/admin/platform_sandbox.go`). Such a block (what is left of an installer's sandbox cluster, a
  registry or an entry appliance) is inert for the sync, and it was replaced by the installation's own; the write
  now refuses it like a block with a pool, and the run stops at `sandbox_refused`. Found in the review of #436.
- **The load balancer gateway bounds a cluster's calls** (`internal/cloud/lbgw`, `Handler.ServeHTTP`; Known issues
  of `v0.11.1`: "The gateway has no rate limit per cluster"). The gateway took every `PUT /lb/v1/state` a cluster
  sent, and since the controller watches (chart 0.1.19) a tenant whose Service flaps sends about one a second, each
  one a credential lookup, a body of up to 64 KiB and a write of the state. Each cluster now has 30 calls at once
  and one more every 2 s (30 a minute sustained; fixed, no setting). A call over that is answered `429`
  `TooManyRequests` with `Retry-After`, after the credential is accepted and before the body is read: nothing is
  stored, no converge starts, and the last call is not written as seen. A refused credential takes none of a
  cluster's calls, so nobody who names a cluster can use up its limit. The API says a cluster over the limit in
  one log line (`lb gateway: rate limit`) and one audit event of its project, at most once per cluster a minute.
  The count is kept per API process: two API replicas take twice as many, and a restart starts every cluster at
  30. `TestClusterRateLimit` runs it on the tests' clock (30 calls taken, the 31st refused with the portal not
  called, one more 2 s later, the whole 30 after a quiet minute, a wrong secret before each call, one log line and
  one event in a minute of refusals); its control is another cluster's call in the same instant, which is taken.
  With the check taken out the 31st call is answered `200`; counted at a refused credential the 16th good call is
  refused; with one count for all clusters the other cluster is refused.
  `TestStateRefusedAtTheGatewaysLimitStillLands` (`cmd/lb-controller`) runs the controller with its watch against
  the gateway itself: a Service that flaps until one of its states is answered `429`, then nothing more; the state
  that was refused, the newest, is at the portal once a call has come back, with no further change in the cluster,
  and the Service keeps its address. Without the limit no state is refused (200 in a row are taken); with a controller
  that takes the `429` for an answer the portal stays one state behind and the test fails.
  **For an upgrade:** nothing to do, and no controller changes. A turn of the controller is one call, two when a
  certificate is asked for or a refused state is asked once more without its Services that are being deleted, and
  a quiet cluster sends one per 30 s, so a cluster in normal use never meets the limit. A cluster that does (Services, Nodes or TLS Secrets changing about once a second for a minute) keeps its
  balancer and its addresses as they are: the controller of `v0.11.1` (chart 0.1.19) reports the refusal as a
  Warning Event `TooManyRequests` on each of its Services, takes no address from that turn and sends its whole
  state again at the next change or, with none, after its interval (10 s), so the newest state arrives up to about
  10 s late and none is lost. A certificate the portal asks for is the second call of a turn, which a cluster at
  the limit has refused, and the controller sends one Secret at most once a minute: a new or renewed certificate
  of such a cluster is served up to a minute after its changes stop, and the old one until then. While the
  changes go on, a Service's Events go between that Warning and `EnsuredLoadBalancer`. A controller of `v0.11.0`
  (a turn every 10 s) cannot reach the limit. Not run on a lab.
- **A node selector term with an empty key matches no node** (`cmd/lb-controller`, `parseSelector`). A term of
  `proxcloud.io/lb-node-selector` with `=` and no key (`=edge`, `=`) was dropped, so a selector made only of such
  terms was read as no selector and every node of the cluster was put behind the Service: a typo in the one
  annotation that narrows a Service's nodes failed open. The term is now kept under the key `""`, which no node
  label has, so the selector selects no node, alone or next to a term that matches (`pool=edge,=x`), as the
  upstream controller reads it. The Service is sent with an empty node list, has no member and gets the Warning
  Event `UnAvailableLoadBalancer`, which is what the guide says of a selector that matches none. Unchanged: a term
  with a key (the spaces around `=` dropped), a term without `=`, an empty value, an empty term (a trailing comma,
  `a=b,,c`), and an annotation that is empty or only commas and spaces, which is no selector. Tests:
  `TestParseSelector`, `TestSelectorSelects` and `TestEmptySelectorKeyOfProtocol2`, all three failing on the
  parser as it was. The portal is not changed; the controller image carries the fix.
  - **Upgrade note: a Service whose selector has a term without a key loses its members when the cluster's
    controller is updated.** A Service annotated `proxcloud.io/lb-node-selector: "=edge"` was served by every node
    of its cluster. From the first turn of the new controller it is sent with no node, as a Service whose selector
    matches none: no member answers and the Service gets `UnAvailableLoadBalancer`. A Service with `pool=edge,=x`
    was served by the nodes labelled `pool=edge` and has no member either. Write the key (`pool=edge`) or remove
    the annotation and the members are back at the next turn. To find such a Service before the upgrade:
    `kubectl get svc -A -o json | jq -r '.items[] | select((.metadata.annotations["proxcloud.io/lb-node-selector"] // "") | test("(^|,)\\s*=")) | "\(.metadata.namespace)/\(.metadata.name)"'`.
    Every other Service gets nothing new.
- **A cluster Service whose health check is off no longer reads "0 of N up".** The Workers column of a cluster's
  Services (on the cluster's page and on its load balancer's page of the customer console) counted a worker as up
  only when one of its members read `UP`. A Service with `proxcloud.io/lb-enable-health-monitor: "false"` has pools
  without a check: HAProxy reports each worker as `no check` and sends to every one, so the row read "0 of 3 up" for
  a Service that served on all three. Such a row now reads "3, not checked". A worker with at least one checked
  member counts by those, as before, and a row with both kinds of worker reads "2 of 2 up, 1 not checked". For an
  upgrade: only the console changes; no load balancer is touched and the API answers as before. A UDP port whose
  check is off is not affected: its members are read from the kernel's table and read `UP` while they are in it.
  `web/client/e2e/kubernetes-lb-unchecked.spec.ts` fails on the page as it was, with "0 of 3 up".

## [0.12.0-rc.1] - 2026-11-26

The first release candidate of 0.12.0, published as a prerelease and never as `latest`. It is not the release:
`v0.11.1` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under Unreleased above: sandboxes on the managed platform, switched on in the BOM
(#483; the code of #422, #423, #432 and #436 was in `v0.11.1`, switched off), the fourteen dependency updates (#484),
the kept BOM of 0.11.1 (#482) and the Workers column of a Service with its health check off in the customer console
(#493): what `main` held at `65c24cc0`, where its notes were closed. A pull request merged after that commit and
before the tag has its entry under Unreleased above and is not in the notes. It exists so that the lab can run the
drill of this code: nothing of it had run on a lab when it was tagged. The entries above are not repeated here. Not
for a production site.

### Changed

- **The platform BOM's release is `0.12.0-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had:
  those of `0.11.1` but the platform's own cluster chart, `proxmox-kamaji-cluster` 0.1.19
  (`sha256:476950b5e841c9460c47960b1841ceac17998b5c06c7358c9966585f6602d5fa`, from 0.1.16), and the component
  `sandbox`, chart `proxcloud-sandbox` 0.1.0
  (`sha256:04300d336186e3b4c56e9213cf90e90a851b8c37b0da92cc911fb05dcb1bf2c7`, `feature: sandbox`). No chart of the
  product differs from its newest tag
  (`git diff proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`,
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and
  `git diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed.
  It is a release BOM, and the release gate passes on it and on the twenty-three previous BOMs.
- **The plan from each previous BOM** (`PlanFor`, one control plane machine and one machine per group). From 0.11.1
  and its five candidates three changes: the release (record only), `seed/pc-platform` 0.1.16 to 0.1.19 (class chart,
  stage `cluster`, wave 3) and `workload/sandbox` to 0.1.0 (class added, stage `workload-platform`, wave 4); allowed,
  with no part, no seed row, no machines row and no seed backup; phrase `upgrade <name> to 0.12.0-rc.1`. From 0.11.0,
  0.11.0-rc.1 and 0.11.0-rc.2 four: those and `tenantCharts.kubernetes` 0.1.17 to 0.1.19, record only. From 0.10.2
  and 0.10.2-rc.1 the same four (the tenant chart from 0.1.16). From 0.10.1, 0.10.1-rc.1 and 0.10.1-rc.2 six: those
  and `cert-manager` v1.20.2 to v1.21.3 on the seed and on the platform cluster (class chart, wave 1; from
  0.10.1-rc.1 the tenant chart and `pc-platform` move from 0.1.15). From the nine BOMs on Kubernetes 1.35.10 a
  refusal by the pair gate (22 changes from 0.10.0 and 0.10.0-rc.1, 23 from the four of 0.9, 26 from the three of
  0.8). This BOM carries no unfinished replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What the platform's upgrade does, and how it proves that no machine is replaced.** In stage `cluster` the run
  applies the Application `pc-platform` at chart 0.1.19 without automated sync, waits for ArgoCD's comparison and
  reads it (`rolloutGate`, `internal/platformlife/cluster.go`): a machine or bootstrap template that is new or would
  change, or a control plane that would change, fails the run with `unexpected_rollout`, with nothing synced, every
  machine as before and the Application left at the new version without automated sync; otherwise the stage's
  receipt records `rolloutChecked` and the Application is applied as always. Without `spec.sandbox` the run makes no
  pool, ACL row, token, sandbox Application or sandbox provider (`TestUpgradeFromTheLastReleaseBuildsNoSandbox`).
- **What the platform's upgrade does not do.** It gives no existing platform sandboxes. A platform on an earlier BOM
  is refused the block (`400 invalid_installation`, naming this release), so asking is possible only after the
  upgrade, and only an apply after that save writes anything on Proxmox.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.1`.

### Verified on a lab

Nothing of this code. Before the code was written, the method was tried by hand on a throwaway tenant cluster of the
lab (the spike of 2026-11-21: Kubernetes 1.36.6 on the platform's node image, workers of 2 vCPU and 2 GiB): runsc
20261116.0 installed in 33 s and a first gVisor pod was Ready in 3.0 s; a cold start of `base` took 127 s (the image
pull) and a warm one 3 s; a node of 2 vCPU took one sandbox of one vCPU, and an idle sandbox cost about 35 to 50 MiB;
with the planned policies a sandbox reached the internet on 443 and 587, not on 25, and no in-cluster or private
target; an egress cap of 10 Mbit/s measured 9.4 Mbit/s; an admission policy saw `exec` and refused it outside the
project namespaces. It is a spike of the method, not a test of this code, and it covered neither the platform
cluster as the place of the group, nor the pool and its access, the VM firewall, the provider and its gates, the
separate placement or the upgrade.

### Not verified

- Everything of this candidate, until its drill: the portal at the candidate on a platform of 0.11.1 (no block, the
  refused save); the move to the BOM `0.12.0-rc.1` with every VM id, Machine, template name and address as before;
  the rollout check on a real ArgoCD; `shared` from Check to a sandbox started with the E2B SDK (the pool and the ACL
  row by the portal token, the chart and the nodes, the provider disabled, the fence, the four gates, the packets
  from a fenced machine and from inside a sandbox, the identities and the admission policies, go-live, off);
  `separate` the same; a switch of placement; a machine-replacing upgrade refused; a new install; Remove.
- Any move to the BOM `0.12.0-rc.1` (plans read, not run), and the candidate's images and its tag's workflows: they
  do not exist before the tag.
- That Proxmox accepts the `shared` ACL row from the portal token: the tests' Proxmox evaluates no permission.
- The new `setup-oras` pin (#484): no VM artifact was exported with it.
- The sandbox invoice line on a sent bill.

### Known issues

- With `shared`, code that escapes a sandbox is root on a machine of the platform cluster, beside every tenant's
  control plane and database; the pod network is open to it. `separate` limits the same escape to the sandbox
  cluster. The install page prints the list and saves `shared` only with a tick.
- A machine of 2 cores holds one sandbox of one vCPU, and a template of 2 vCPU (`code-interpreter-v1`,
  `mcp-gateway`) is not expected to fit it (read in the code, not run).
- No sandbox registry, no custom templates, no snapshots; pause and resume are held. No entry appliance: the way
  from the internet to the proxy address is the operator's, and with `separate` that address takes any source on
  443. A self-signed certificate only. Prices are placeholders until set.
- The pool name `proxcloud-sbx` and the token name `capmox@pve!pc-sandbox` are fixed: two portals on one Proxmox
  cluster collide.
- Sandboxes cannot be asked for in the same step as the BOM move: the save is refused until the upgrade is done.
- The sandbox chart pins its runtime at the tag `dev-d26edd2` and its proxy at `v0.11.0-rc.1`, both by digest; with
  `separate` the sandbox chart inside `pc-sandbox` is named by version, not by digest.
- The provider sync fences every VM it finds in pool `proxcloud-sbx`, also one of another owner: closed, never
  opened.
- The known issues of 0.11.1 stand: no change merged up to `65c24cc0` answers one of them.
- The full list is in `docs/releases/v0.12.0-rc.1.md`.

## [0.11.1] - 2026-11-26

The release after `v0.11.0`. It completes layer 4 of the load balancer: UDP listeners, TLS termination with
certificates and the layer 4 options, for a tenant's own balancers and for the Services of a managed Kubernetes
cluster. It was prepared by the release candidates `v0.11.1-rc.1`, `v0.11.1-rc.2`, `v0.11.1-rc.3`, `v0.11.1-rc.4` and
`v0.11.1-rc.5`, whose sections below are kept; everything they held is in this section, and the code is that of
`v0.11.1-rc.5`: `git diff v0.11.1-rc.5` over the product's files (everything but `docs`, this file,
`deploy/platform/previous` and `internal/platformlife/testdata/previous`) shows the version lines of this release and
the test lines that the kept BOM of `0.11.1-rc.5` needed, and nothing else. The entries under Upgrade notes, Added,
Changed and Fixed were written when each change merged, before or between the five lab drills: where one says "not run
on a lab", "not measured on a lab" or names a candidate as the thing to come, the lists below are the current state.

- **Five listener protocols** (Added): `tcp`, `udp`, `tls`, `http` and `https`, where `v0.11.0` served `tcp` and
  `http`.
- **UDP on a pair** (Added). The kernel's IPVS balances the datagrams on the appliance that holds the address, next to
  HAProxy. A balancer on one VM refuses UDP.
- **TLS ends on the balancer** (Added). A project uploads certificates with their keys; a listener presents up to ten
  and picks by SNI. TLS 1.2 and 1.3 only.
- **The layer 4 options** (Added): session persistence by source address, PROXY protocol 1 or 2, five health check
  types with their own timing, three timeouts and a connection limit per listener.
- **Kubernetes Services get the same** (Added): UDP ports, TCP and UDP on one address, session affinity, TLS from a
  `kubernetes.io/tls` Secret, the `proxcloud.io/lb-*` annotations, and an address a Service leaves to its cluster and
  takes again. The controller of cluster chart 0.1.19 watches the cluster and sends a change about a second after it
  happens.
- **No request is lost at a reload or a node change** (Fixed, #430), which `v0.11.0` listed as a known issue. On the
  lab a client loop counted 31365 requests and 0 failed across the upgrade from `v0.11.0`. A node change under load
  was not run there.
- **Eight fixes that the drills of the candidates and the tests of the release asked for** (Fixed): a pair with a UDP
  listener knows which appliance holds its address (#447), the first appliance of a pair serves alone (#446), a
  damaged certificate file is noticed and healed (#448), an address annotated just before its delete is kept (#452),
  an appliance takes a change while the image's package manager runs (#453), a changed UDP connection limit reaches
  the kernel (#454), a UDP client that was sending before its listener existed is answered (#473), and a UDP client
  that keeps sending while the UDP service of an appliance stops and starts is answered again (#477). None of these
  faults is in `v0.11.0`: each is in code that is new in this release.
- **An existing cluster gets none of this by the portal's upgrade.** It keeps the controller it was made with until it
  is moved to chart 0.1.19.
- **The sandbox platform code of `v0.12.0` is in the build and switched off**: no BOM names the sandbox chart
  (`TestSandboxIsDark`).

The platform BOM of this release is `0.11.1`. It pins what the five candidates pin, and the six BOMs differ in the
`release` line alone: the pins of `0.11.0` but `tenantCharts.kubernetes`, cluster chart `proxmox-kamaji-cluster`
0.1.19 (`sha256:476950b5e841c9460c47960b1841ceac17998b5c06c7358c9966585f6602d5fa`) for new tenant clusters. No k3s,
Kubernetes version, node image, third-party chart, Cluster API provider or clusterctl image moves, and the platform
cluster stays on chart 0.1.16. The release carries the BOMs of `0.8.0` to `0.11.1-rc.5` (twenty-two) for the platforms
those builds installed.

Upgrading from `v0.11.0`. From an older release, read the sections of `v0.11.0`, `v0.10.2` and `v0.10.1` as well:
their steps still apply. The release notes (`docs/releases/v0.11.1.md`) give the steps in order.

- **Billing: an address without a `FLOATING_IP` row gets one, billed from its allocation time.** An address that a
  cluster's balancer holds for a Service, or that a cluster keeps, and that has no row gets one at the first pass of
  that balancer after the portal's upgrade (its controller's next request, within 30 s, or the services sync). The row
  starts at the allocation's time, and its bill item is counted from that time or from the start of the open bill's
  cycle, whichever is later (`getLastRateTime`). It is the row a services sync writes: before the upgrade, run a
  services sync and read each project's floating IPs and its open bill. An address that is given back before the
  upgrade gets no row. A tenant's own balancers and the addresses of servers are not touched. On the lab six addresses
  got their row with the allocation time at the upgrade from rc.2 to rc.3; what a charge run then bills was read in
  the code only.
- **Every load balancer is pushed once**, at its first pass after the upgrade, and nothing needs a maintenance window.
  The HAProxy config of every balancer changed (a state file, a retry on another member, a connect timeout of 2 s,
  backends named by pool), and a pair's keepalived files changed: one HAProxy reload per appliance and one keepalived
  reload per appliance of a pair, the standby first. A reload is not a restart. On the lab, from `v0.11.0` to rc.1, a
  client loop counted 31365 requests and 0 failed, the standby was pushed 15 s before the other appliance, the
  cluster's pair within a minute and the single balancer at the first services sync. This reload is the last one that
  starts every member as up: a balancer with a dead member, or a Service with `externalTrafficPolicy: Local`, loses
  the known 2 to 4 seconds once more. The push masks sshd on every appliance. Run a services sync right after the
  upgrade on a site without a scheduler.
- **A cluster gets UDP, TLS from a Secret, keep-address and the annotations when it is moved to chart 0.1.19**, and
  not before: the portal's upgrade and the platform run change no existing cluster (K12 of the first drill). The
  tenant's platform update, the operator's Bump of the chart version, or "Enable load balancer controller" again move
  it; only the controller's pod rolls. After the move a Service with a UDP port that stood refused gets a public
  address, which is billed and counts in the floating IP quota. A cluster that is on chart 0.1.19 already under
  `v0.11.1-rc.1` or `v0.11.1-rc.2` is offered no update by the tenant's console and is skipped by "Bump all": the
  operator's "Apply settings" or "Enable load balancer controller" again set its controller's image, which the
  controller's part of #452 needs. A cluster whose controller was set under `v0.11.1-rc.3` or later needs nothing.
- **`proxcloud.io/lb-internal` is read now.** `v0.11.0` did not read it. A Service that carries it with `"true"`,
  `"yes"` or an empty value loses its address when its cluster gets the new controller: set it to `"false"` or remove
  it where the address must stay. On the lab two such Services lost their address within half a minute of their
  cluster's update.
- **Before a rollback of the portal to `v0.11.0`:** release the kept addresses on the cluster's page (`v0.11.0` saves
  the cluster's balancer without the list, and they then stay allocated, billed and unlisted until the cluster is
  deleted), take `proxcloud.io/lb-tls-secret` off the Services whose address must stay, remove UDP and TLS listeners,
  and do not move a cluster's chart while the portal is rolled back. An older build does not carry the BOM `0.11.1`
  and refuses such a platform (`bom_unsupported`). No rollback ran on a lab.
- **On the wire: a UDP port with no listener is silently dropped.** On a pair with a UDP listener, a datagram for a
  port that has no UDP listener gets no ICMP "port unreachable"; a monitor that reads that answer, or a UDP traceroute
  to the address, times out. No balancer of `v0.11.0` has a UDP listener.
- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.11.1`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. `compose.yaml` did not change since
  `v0.11.0`: on the lab the upgrades between the candidates took 25 to 32 s each.
- **The platform run is record only**, phrase `upgrade <name> to 0.11.1`, one action with no parts. From `0.11.0`,
  `0.11.0-rc.1` and `0.11.0-rc.2` the plan has two rows, the release and `tenantCharts.kubernetes` 0.1.17 to 0.1.19:
  it records the release and writes the chart on the Kubernetes provider, so clusters created after it get the new
  chart. It replaces no machine, syncs nothing and writes nothing on Proxmox, and no tenant cluster changes chart. On
  the lab the same two rows took 44 s (to the first candidate) and the one-row runs between the candidates 38, 8.5,
  8.2 and 8.5 s, with no VM restarted. The run to `0.11.1` itself was read as a plan, not run.
- **From other BOMs** (`Classify`, on the release commit): from `0.11.1-rc.1`, `0.11.1-rc.2`, `0.11.1-rc.3`,
  `0.11.1-rc.4` and `0.11.1-rc.5` one change, the release, record only; from `0.10.2` and `0.10.2-rc.1` two, the
  release and the tenant chart from 0.1.16; from `0.10.1` and `0.10.1-rc.2` four, those and `cert-manager` `v1.20.2`
  to `v1.21.3` on the seed and on the platform cluster (class chart; back up the cert-manager resources first, as the
  section of 0.10.2 says); from `0.10.1-rc.1` five, the tenant chart from 0.1.15 and also `pc-platform` 0.1.15 to
  0.1.16, no machine replaced; from the nine BOMs on Kubernetes 1.35.10 refused by the pair gate on both cert-manager
  rows.
- **From a candidate.** From `v0.11.1-rc.5` nothing is pushed: every file of an appliance is rendered the same to the
  byte. From `v0.11.1-rc.4` a pair with a UDP listener gets `/usr/local/sbin/proxcloud-lvs-net`, the script of its UDP
  service, and one reload of `proxcloud-lvs.service` per appliance, the standby first; no unit file is written and the
  unit is not restarted. From `v0.11.1-rc.3` also `lvs.nft`, with one WARN line per appliance ("load balancer: an
  appliance's UDP table is not as its configuration has it; pushing it again": the state read finding the table of
  rc.3); from `v0.11.1-rc.2` also `lvs.conf` and the unit's drop-in, with one `daemon-reload`, and a UDP connection
  limit that was stale in the kernel is put right; from `v0.11.1-rc.1` also keepalived's five files once per appliance
  of a pair. Each of these ran on the lab between two neighbouring candidates. The upgrade frees no UDP client that a
  candidate left unanswered: it stays so until it is silent for more than 30 s or sends from another source port. The
  upgrade notes below say it in full.
- The app chart is `0.11.1` with appVersion `v0.11.1`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.11.1`. The installer's k3s and Kubernetes pins stay at 1.35.10.

Verified on a lab. One lab of three Proxmox nodes, every appliance on one node, Ubuntu 26.04 appliances of one vCPU,
the scheduler off (every sync started by hand); five drills, on `v0.11.1-rc.1` (2026-11-23), `v0.11.1-rc.2`
(2026-11-23 and 2026-11-24), `v0.11.1-rc.3` (2026-11-24), `v0.11.1-rc.4` (2026-11-24) and `v0.11.1-rc.5` (2026-11-25),
dates in UTC, each checked afterwards by a read-only audit that recomputed its figures. Nothing ran on the images of
this release.

- On rc.1, not run again: the upgrade from `v0.11.0` under five loops (31365 requests, 0 failed); one push per
  balancer, the standby first by 15 s, the single balancer at the first services sync; the platform run of two
  record-only rows (44 s); a cluster on the `v0.11.0` controller unchanged by both (K12); the cluster's update without
  a machine roll; the `lb-internal` and `loadBalancerIP` cases; a UDP Service answering from outside.
- On rc.1, TLS on a single balancer: uploads, SNI, the versions, HTTP/2, PROXY version 1 and 2, a rotation under load
  (6450 requests, 0 failed), the bundle of 16 KiB, a reboot (56 s), and the search for the private key (289 strings, 0
  hits outside the sealed row and the 11 certificate files).
- On rc.2, the upgrade from rc.1: 25 s with its backup first; four loops of 24627, 24627, 24627 and 12314 requests, 0
  failed. A pair got keepalived's files once with one keepalived reload per appliance, the standby first by 8 s, no
  HAProxy reload, no VRRP transition; the single balancer got nothing. The pair rc.1 left `DEGRADED` read `ACTIVE` at
  the first pass. The platform run of one row took 38 s; 18 VMs kept their process.
- On rc.2, Kubernetes on the moved cluster: ports added, changed and removed; UDP alone and beside TCP; policy Local;
  each annotation by its name (18 cases); affinity; TLS from a Secret with SNI, a renewal served after 17 and 21 s
  with 0 failed of 4749, bad Secrets held with `InvalidCertificate`; keep-address, the limit of 5 and the release; the
  gateway down and back; 64355 TCP and 32178 UDP requests, 0 failed.
- On rc.2, a new cluster: chart 0.1.19 and protocol 2 from creation; the first appliance served TCP and UDP alone 1
  min 57 s before the second was ready, the row `BUILD`, `DEGRADED`, `ACTIVE`, never `ERROR`; the exclude label, a
  cordon, a node's first Ready; a replace with TLS and UDP (one window of 0.7 s); a planned failover 0.7 s, a dead
  master 3.2 s; the cluster's delete left nothing.
- On rc.2, a tenant's own pair: UDP beside TCP on one port, DNS at 200 queries a second for 5 minutes with none of
  59983 lost, algorithms, persistence, checks, source ranges, answers of 4 KiB; TLS with SNI, TLS 1.1 refused, HTTP/2,
  a rotation of 2052 requests with 0 failed; six failovers (planned 0.7 to 0.8 s, a dead master 3.5 to 3.6 s); a
  damaged certificate file healed in one pass; UDP files edited by hand put back; about 55 config changes with 0
  failed requests; 44 packets routed toward a tenant's address, none arrived; the pair deleted in 17 s.
- On rc.3, the upgrade from rc.2: 31 s; eight loops, 183773 requests in 47 minutes, 0 failed. No HAProxy config and no
  keepalived file pushed on four appliances. A pair with a UDP listener got `lvs.conf` and the drop-in once with one
  reload of the UDP service, the standby first by 10 to 11 s. A stale limit (kernel 5, listener 50) was put right on
  both appliances. Six addresses got their floating IP row with the allocation time. The platform run took 8.5 s; 19
  VMs kept their process. A cluster on chart 0.1.19 was offered no update and was moved by "Apply settings".
- On rc.3, the fixes of the rc.2 drill: a UDP limit, algorithm, persistence and weight in the kernel of both
  appliances at the first read (#454); a replace that lost 0.7 s of TCP and 0.8 s of UDP (#454); a change taken at
  once while `apt-get` ran on the standby, the holder and both, and held with "A change waits" while dpkg worked on
  `haproxy`, landed by the platform's own retry 10 and 20 s after dpkg's end, never `DOWN` or `ERROR` (#453);
  `GET_HEALTH` right 5, 15 and 30 s after a holder's shutdown (#453); sshd masked after a reboot and certificate files
  0600 in every sample (#453); annotate and delete in one line kept the address twice, five kept and the sixth given
  back (#452); the rows written and archived by the pass, a rotation without a Warning (#452). Clean-up: 15
  allocations gave 15 archived rows.
- On rc.4, the upgrade from rc.3: 28.5 s; six loops, four of them UDP from one socket each, 35710 requests in 15
  minutes, 0 failed. A pair with a UDP listener got one file, `lvs.nft`, and one reload of its UDP service, the
  standby first by 7 to 8 s, the cluster's pair 22 s after the upgrade; no other file, no HAProxy reload and no VRRP
  transition on four appliances; 0 of 4464 datagrams lost on each of four running flows. The platform run took 8.2 s;
  19 VMs kept their process.
- On rc.4, the fix of #473: a listener added while its client sends from one socket was answered within 0.02 s of the
  first new client (under rc.3 on the same lab: 0 of 962); a listener removed and added again beside the pair's other
  UDP listeners (440 unanswered while it was gone, answered 0.06 s after); a UDP Service made again on its kept
  address while clients send (answered 0.06 and 0.18 s after the first new client); a UDP port added to a Service. A
  failover with one-socket flows lost 0.8 s, a replace 0.7 s of TCP and 0.8 s of UDP. After a failover the first
  `GET_HEALTH` that reached the started appliance serving wrote the row `ACTIVE`, with no pass in between.
- On rc.4, one try that showed less than the notes of rc.4 said: a restart of `nftables.service` on the appliance that
  holds the address, where three one-socket flows lost 7 datagrams each (1.6 s) and were then answered. Looped on the
  same lab the next day, still under rc.4, 8 of 40 such restarts left a client unanswered for as long as it sent: 12
  of 160 clients at 500 datagrams a second, none of 240 at 5 and none of 240 at 50. In real guests on a test host it
  was about 1 client in 8 at 5 a second and 3 in 4 at 50.
- On rc.5, the upgrade from rc.4, on one pair with one UDP listener: 32 s with its backup first; twelve one-socket UDP
  clients, four each at 5, 50 and 500 datagrams a second, and a TCP loop counted 1,461,283 datagrams and 6,590
  requests across it and the ten minutes after, none unanswered. The pair got one file, the script, and one reload of
  its UDP service per appliance, the standby first by 15 s, at the services sync; the service kept its process and its
  start time; no other file, no HAProxy reload, no VRRP transition; the reload loaded `nf_conntrack_netlink`. In that
  minute 1 of 133,169 datagrams went unanswered, half a second after the standby's reload; the cause was not found.
  The platform run of one record-only row took 8.5 s; every running VM kept its process.
- On rc.5, the fix of #477, looped on the appliance that holds the address with the clients sending throughout: the
  last UDP listener removed and added again 40 times, the service's keepalived killed 60 times, `systemctl restart
  nftables` 40 times, `systemctl restart proxcloud-lvs` 20 times: after each, every client was answered within 0.2 s
  of the first, 0 of 1,920 left unanswered; UDP was unanswered meanwhile for 27 to 36 s (the listener away), 3.2 to
  3.8 s, 1.2 to 1.6 s and 1.1 to 1.6 s. A start alone, 30 times: 0 of 360. The same pair under rc.4 the same day: 4 of
  20, 10 of 52 and 8 of 40 of the first three events left a client unanswered, 27 of 448 clients at 500 datagrams a
  second, 1 of 448 at 50 and 0 of 448 at 5; at 5 and 50 a second this lab cannot tell the fix from rc.4, at 500 it
  can, and it has no rc.4 figure for a start alone. The journal had "the connection table was emptied" at each of 261
  starts that found no table, never "could not be emptied". HAProxy through it: 56,890 requests, 0 failed, no VRRP
  transition. The appliance's own traffic after such a start, tried once: 13 name lookups answered, `apt-get update`
  ended with 0, the clock in sync. Two failovers lost 0.6 to 0.8 s; a replace took 14 min 48 s and its switch lost 0.6
  to 1.0 s of UDP and 0.7 s of TCP; no client was left unanswered. The clean-up left no row.

Not verified on a lab:

- The images of this release, its tag's workflows, and the move of a platform to the BOM `0.11.1` (the lab is on
  `0.11.1-rc.5`). The upgrade from `v0.11.0`, rc.1, rc.2 or rc.3 straight to the release: each step between two
  neighbours ran.
- A balancer without a UDP listener at the upgrade from rc.3 or rc.4.
- The fix of #477 on Debian 13 appliances (kernel 6.12) and on an appliance with more than one vCPU, where rc.4 left a
  client unanswered far more often; the same fix at 5 and 50 datagrams a second on a lab, and for a start alone: rc.4
  is nearly clean there on this lab too, and those rates rest on runs in real guests on a test host.
- `systemctl reload nftables`; a clean exit of the service's keepalived; a flush that fails; systemd's start limit.
- A client caught before the upgrade: that it stays caught through the reload, and that the next stop and start, or a
  replace, frees it. The event on a cluster's pair (its only UDP Service deleted and made again). The connection
  table's entry count before and after a start, and the appliance's own traffic after a start beyond one try.
- A listener whose servers are all down ("port unreachable"); a first datagram from outside for a port with no
  listener at the appliance (the node's firewall dropped it first); the connection entry itself.
- The health fix's one exception, a change made while the appliance was down. A table changed by hand and loaded again
  at the next pass. Every member out and back for a one-socket flow.
- An upgrade with a held push; a tenant's change inside a new pair's first self-started apt run; a real
  unattended-upgrades upgrade of `haproxy`; `needrestart`.
- A Service deleted while pending or refused; a wrong Secret's Warning after 2 minutes; an old controller deleting a
  Service under the new portal; more than 50 Services deleted at once; what a charge run bills for the rows a pass
  wrote.
- pveproxy down during `GET_HEALTH`; a cluster's pair with one appliance held while the other is built; the upgrade of
  a pair with one appliance and of a tenant's own pair with TLS listeners.
- A node change under load with two workers or more (the lab had no room), a drain, policy Local among several nodes,
  affinity with two clients. A project's delete, and every pass a scheduler would run by itself.
- Two nodes, the consoles in a browser, a production site with a public network, the installer path. cert-manager
  itself, `CertificateExpired`, a Secret over 16 KiB, 16 Secrets on one cluster, a changed encryption key, a restart
  of the tenant's API server, a rollback of the portal.
- The appliance's own forward-drop rule (the node's firewall dropped first). The search for the private key with
  needles under rc.2 to rc.5. The sandbox invoice line on a sent bill, and anything of the sandbox platform code.
- Everything the section of 0.11.0 lists as not verified.

Where an entry below says the BOM on `main` is `0.12.0-dev`, that was the state at that merge: this release's BOM is
`0.11.1`, and the change after its tag sets `main` back to a `-dev` one.

### Upgrade notes

- **From v0.11.1-rc.4, a pair with a UDP listener gets one file and one reload of its UDP service on each
  appliance; every other balancer gets nothing.** This is what the fix "no UDP client is left unanswered when the
  UDP service stops and starts" does to a balancer that exists.
  - *A balancer without a UDP listener* (a tenant's pair or single VM, a cluster's pair with TCP Services only): no
    file is written and nothing is reloaded.
  - *A pair with a UDP listener*, a tenant's or a cluster's, at its first pass after the upgrade (a cluster's
    controller's next request, the services sync, or the next change), the standby first and then the appliance that
    holds the address. **Written:** `/usr/local/sbin/proxcloud-lvs-net`, the script the unit runs before it starts
    and at a reload. **Not written:** `lvs.conf`, `lvs.nft`, `lvs.sysctl`, `proxcloud-lvs.service` and its drop-in,
    which are the same to the byte. **Reloaded:** `proxcloud-lvs.service`, once. The unit is **not** restarted,
    no unit file is written, no package is installed, and HAProxy and the VRRP keepalived are not touched, so no
    address moves. The reload runs the new script, which finds the nftables table loaded, loads it again in one
    transaction as before and **empties nothing**; it also loads the kernel module `nf_conntrack_netlink` when
    that is not loaded yet (`modprobe`), which nothing uses until the unit next starts. In real guests of both
    images, 100 such pushes each onto the script of v0.11.1-rc.4, twice, under eight clients that never stop: the
    unit kept its process and its start time every time, no client was left unanswered and none of 40,000 new
    sockets was. A datagram without an answer within its client's interval was counted 2 to 9 times in 33,000 at
    50 a second and 0 to 3 times in 3,400 at 5 a second over the 100 reloads on a busy workstation, and not once
    in 60 reloads on a quieter one; a reload of the release before read the same there (0 to 7 in 33,000). Until
    that pass the appliance serves as it did before the upgrade.
  - *A client that is already caught stays caught.* The upgrade's reload finds the table, so it frees no one: a
    client that was left unanswered before the upgrade stays so until it is silent for more than 30 s or sends from
    another source port, as before. From then on a stop and start of the UDP service frees it instead of catching
    it.
  - *From then on, at every start of the UDP service that finds no table* (a boot, the first UDP listener, a
    restart of the unit or of `nftables.service`, a reload of `nftables.service`): the appliance's whole connection
    table is emptied once, right after the nftables table is loaded, and the unit's journal has one line, "the
    nftables table was not loaded before: the connection table was emptied". TCP is not tracked while the table is
    loaded, so HAProxy's connections and the appliance's own (a package mirror) do not notice: 0 of 800 new
    connections and of 4,000 exchanges on one connection failed over 50 restarts, with the flush as without. An
    answer that is on its way to a UDP question the appliance itself asked in that instant (DNS, NTP) is dropped
    once, as a lost datagram is; the next question is answered.
  - *Coming from v0.11.1-rc.3 or before*, the same one reload carries this file with the files of the notes below.
- **From v0.11.1-rc.3: nothing is pushed or reloaded because of the fix "an appliance the platform started reads up
  once it serves again".** A pair whose row still reads `DEGRADED` with "its VM is stopped; starting it" or "it does
  not answer" for an appliance that is back is put right by the first health read of it (the balancer's page, a
  `GET_HEALTH` call) or by its next pass, whichever comes first; by the pass alone when the balancer was changed
  while that appliance was down, and on a pair with a UDP listener, whose row records the UDP files of
  v0.11.1-rc.3 until the push of the next note. No row gets a new field.
- **From v0.11.1-rc.3, a pair with a UDP listener gets one file and one reload of its UDP service on each
  appliance; every other balancer gets nothing.** This is what the fix "a UDP client that was sending before its
  listener existed is answered" does to a balancer that exists.
  - *A balancer without a UDP listener* (a tenant's pair or single VM, a cluster's pair with TCP Services only): no
    file is written and nothing is reloaded.
  - *A pair with a UDP listener*, a tenant's or a cluster's, at its first pass after the upgrade (a cluster's
    controller's next request, the services sync, or the next change), the standby first and then the appliance that
    holds the address. **Written:** `/etc/proxcloud-lb/lvs.nft`, which gains the chain `lb-input`. **Not written:**
    `lvs.conf`, `lvs.sysctl`, `proxcloud-lvs-net`, `proxcloud-lvs.service` and its drop-in, which are the same to
    the byte. **Reloaded:** `proxcloud-lvs.service`, once: its script loads the nftables table again in one
    transaction, the new table in place of the old, so there is **no moment without the table** or without the
    forward chain's drop, and keepalived's check process reads its config again. Members that are up stay in the
    kernel's table, no connection entry is dropped (in real guests of both images a client that sent five datagrams
    a second from one socket lost none across the reload), the unit is **not** restarted, no unit file is written,
    no package is installed, and HAProxy and the VRRP keepalived are not touched, so no address moves. Until that
    pass the appliance serves as it did before the upgrade.
  - *On the wire, from that reload on.* A UDP datagram to an address of such a balancer for a port that has no
    UDP listener, or whose listener's pool has no server at all, is dropped: **the client gets no ICMP "port
    unreachable" for it any more** (a UDP traceroute to the address ends without its last hop). A listener whose
    servers are all down answers "port unreachable" as before, and TCP and ping are as before.
  - *A client that is caught at that moment stays caught.* The chain keeps a caught flow from being made and frees
    none: a client that was already unanswered before the upgrade is answered once it was silent for more than
    30 s, as before, and is not caught again.
  - No row gets a new field. A portal rolled back to v0.11.1-rc.3 writes its own `lvs.nft` back at its first pass
    (one file, one reload).
- **From v0.11.1-rc.2, a pair with a UDP listener gets two files and one reload of its UDP service on each
  appliance; every other balancer gets nothing.** This is what the change "a changed UDP connection limit is
  applied, and UDP changes are taken quicker" does to a balancer that exists.
  - *A balancer without a UDP listener* (a tenant's pair or single VM, a cluster's pair with TCP Services only): no
    file is written and nothing is reloaded. Its HAProxy config, `keepalived.conf` and keepalived's support files
    are the same to the byte, and nothing of the UDP service is on it.
  - *A pair with a UDP listener*, a tenant's or a cluster's, at its first pass after the upgrade (a cluster's
    controller's next request, the provider sync, or the next change), the standby first and then the appliance that
    holds the address. **Written:** `/etc/keepalived/lvs.conf`, which gains one line, `warmup 0`, in every virtual
    server that has a health check (a pair whose UDP pools all have the check switched off gets the drop-in alone,
    with the same reload), and the new `/etc/systemd/system/proxcloud-lvs.service.d/nftables.conf`. **Not
    written:** `lvs.nft`, `lvs.sysctl`, `proxcloud-lvs-net` and `proxcloud-lvs.service`, which are the same to the
    byte and are no longer written when they are. **Reloaded:** systemd's unit files (`daemon-reload`, for the
    drop-in) and `proxcloud-lvs.service`, once: its script loads the nftables table again in one transaction and
    keepalived's check process reads the config again. Members that are up stay in the kernel's table through it
    (measured on both images: 3 of 3 at every sample), no connection entry is dropped, the unit is **not**
    restarted, and HAProxy and the VRRP keepalived are not touched, so no address moves.
  - *The same push puts the kernel's connection limits right.* A UDP listener whose `connectionLimit` was added,
    changed or removed while its members were in the table has had the old figure in the kernel since (R3-F2
    below). At this push each member gets the limit the listener shows: a limit that was never enforced is
    enforced from then on, and a removed one stops refusing flows. A listener whose limit never changed is not
    edited.
  - *nftables.service.* The drop-in does nothing at the upgrade. From then on a restart or a stop of
    `nftables.service` on such an appliance restarts or stops its UDP service with it, forwarding off first; UDP
    on that appliance is unanswered for the second or two until the members have passed a first check again.
  - No row gets a new field. A portal rolled back to v0.11.1-rc.2 writes its own `lvs.conf` back at its first pass
    (all five files, one reload); the drop-in stays on the appliance, where that portal neither sums nor removes
    it, and keeps doing what it does.
- **From v0.11.1-rc.2: the upgrade pushes no file to a load balancer and reloads none because of the change "a
  load balancer takes a change while the image's package manager runs".** Nothing an appliance holds changed: the
  HAProxy config the platform renders, `keepalived.conf`, keepalived's track and notify scripts, the sysctl file,
  the unit drop-in and the files of the UDP service are the same to the byte, so every recorded hash still matches
  and the first pass after the upgrade writes nothing on a balancer that serves its config. What changed is the
  commands the portal sends, and an appliance meets them at its next push, whenever that is (a change of its
  config, or a service the pass finds not running): the same reload as before, and on that push the appliance's
  `ssh.service` and `ssh.socket` are masked and stopped. Until that push an existing appliance is as it was: on the
  Ubuntu image sshd listens again after a reboot until the next HAProxy reload, behind the VM firewall, which
  drops the port. To mask it everywhere at once, change any balancer's config, or wait for its next change. The
  balancer's row gets two fields, written without a push: `heldSince` while a change waits, and for a cluster's
  balancer `served` on each Service, at the first pass after the upgrade. A portal rolled back to v0.11.1-rc.2
  ignores both and leaves a masked sshd masked.
- **What an existing cluster balancer gets from the fixes for Services that are deleted (#452): no file, no
  reload.** No config the platform renders changed, so the upgrade pushes nothing to an appliance for them: no
  HAProxy, keepalived or LVS file is written, nothing is reloaded, and no address moves. What changes is in the
  portal's rows, at each cluster balancer's first pass after the upgrade (its controller's next request, within
  30 s, or the provider sync): every address the balancer holds that has no `FLOATING_IP` row yet gets one, billed
  from the time the address was allocated, which is what the next provider sync would have written. On a site
  whose sync runs on schedule that is at most the addresses taken since its last run; on a site that ran none it
  is every Service and kept address, which were in the quota but neither listed nor billed until now. A tenant's
  own balancers are untouched: their addresses' rows are the sync's, as before.
- **A cluster keeps the controller it has, and with it the old answer to an annotation set just before a delete.**
  The portal's upgrade restarts no controller. Until a cluster is moved to the portal's controller image (the
  tenant's platform update, the operator's **Bump**, or "Enable load balancer controller" again), a Service
  annotated `proxcloud.io/lb-keep-address: "true"` and deleted within about a second still gives its address back,
  and a credential rotation still puts one Warning `GatewayRefused` on each Service. The move rolls the
  controller's pod only: no machine, no appliance, no balancer config. A controller of this release against a
  portal rolled back to an earlier v0.11.1 build works: that portal refuses the new field, is sent the request
  without it in the same turn, and goes by its stored state as it did.
- **UDP and TLS from Services need the cluster on chart 0.1.19.** The platform BOM now pins cluster chart
  `proxmox-kamaji-cluster` 0.1.19 for new tenant clusters. On an installed platform that is a record-only step of
  the upgrade run, from every previous BOM: it writes the version on the Kubernetes provider and touches no cluster,
  VM or balancer. A cluster created afterwards serves UDP ports, TLS from a Secret, the `proxcloud.io/lb-*`
  annotations and kept addresses from its first Service. A cluster made before v0.11.1 keeps the controller it was
  made with until it is moved to the provider's chart (the tenant's platform update, the operator's **Bump**, or
  "Enable load balancer controller" again): until then its UDP Services stay `UDPNotSupported` and no annotation
  but `proxcloud.io/lb-proxy-protocol` is read. Both consoles now say which controller a cluster has. The move sets
  the controller's image to the portal's, rolls no machine and changes no balancer config
  ([managed-k8s.md](docs/managed-k8s.md), "Upgrade note for v0.11.1"). Not run on a lab.
- **Chart 0.1.19 of a Kubernetes cluster needs the controller image of this release.** The chart starts the
  cluster's load balancer controller with `--watch=true`, which the image of v0.11.0 does not know and exits at.
  Moving a cluster to the chart sets the controller's image to the portal's, so an upgrade is safe. The one way
  into the wrong pair is a portal rolled back to v0.11.0 that is then asked to "Enable load balancer controller"
  again, or to set the chart version, on a cluster that is on 0.1.19: do neither while the portal is rolled back.
  The Services of such a cluster stay as they were last served until the controller runs again.
- **A cluster's unchanged load balancer state reaches the portal every 30 s, where it was every 10 s.** This holds
  for every cluster whose controller runs this release's image, on any chart. The console's "the controller last
  reported" therefore reads up to a minute and a half.

- **A Service whose `proxcloud.io/lb-internal` is not false loses its public address when its cluster gets the
  controller of this release.** `v0.11.0` did not read the annotation, so a Service that carries
  `proxcloud.io/lb-internal: "true"`, `"yes"` or an empty value is served there on a public address. From this
  release only a false value or no annotation gives one: the Service is refused (`InternalNotSupported` for a true
  value, `InvalidAnnotation`, which names the value, for one that is no boolean) and its address goes back. The
  portal's upgrade alone takes no address: the controller of `v0.11.0` sends no annotation but
  `proxcloud.io/lb-proxy-protocol`, so the portal does not see this one until the cluster is moved to the provider's
  chart version or given "Enable load balancer controller" again. Before that move, set the annotation to `"false"`
  or remove it on the Services whose address must stay.
- **A Service that ends TLS with a Secret loses its address if the portal is rolled back to v0.11.0.** That portal
  ends no TLS and would hand the port to the pods without it, so the cluster's controller does not send it such a
  Service: the address goes back, the Service says `TLSNotAvailable`, and it gets a new address when the portal is
  on v0.11.1 again. Before a rollback, take `proxcloud.io/lb-tls-secret` off the Services whose address must stay.
- **Kept addresses are lost from view if the portal is rolled back to v0.11.0.** That portal does not know the
  list of kept addresses (`proxcloud.io/lb-keep-address`) and saves the cluster's balancer without it. The addresses
  stay allocated, billed and in the floating IP quota, no page lists them, also after the portal is on v0.11.1
  again, and they go back to the pool only with the cluster. Before a rollback, release every kept address on the
  cluster's page.
- **UDP from a Kubernetes Service needs the controller of this release.** Upgrading the portal alone changes no
  Service: a cluster made before v0.11.1 keeps answering a UDP Service `UDPNotSupported` (the message now ends "For
  UDP, upgrade the cluster"), so no Service that stood pending gets a public address, which is billed and counts in
  the floating IP quota, without its owner's action. The cluster gets UDP, the annotations' Events and the new node
  rule when it is moved to the provider's chart version or given "Enable load balancer controller" again. A new
  cluster has them from its creation. The annotations themselves are read by the portal, but a controller of
  v0.11.0 sends none but `proxcloud.io/lb-proxy-protocol: "true"`.
- **Every load balancer is reloaded once after the upgrade.** The HAProxy config the platform renders changed (see
  "No request is lost" under Fixed), for a single appliance too, whose config had not changed since its first release,
  and so did the pair's keepalived track script. The first services sync after the upgrade (and, for a cluster's
  balancer, the controller's next PUT) pushes each balancer once: one HAProxy reload per appliance and one keepalived
  reload per appliance of a pair. That reload is the last one that starts every member as up: the config it loads
  names its backends differently, so the state saved from the old one matches nothing. A Service with
  `externalTrafficPolicy: Local`, or a balancer with a dead member, sees the known 2 to 4 seconds once more. Open
  connections are kept (a reload, not a restart). **Run a services sync right after the upgrade**, at a time of your
  choosing, on a site without a scheduler.
- **The push is safe to fail.** The standby gets the config first, and a config its HAProxy does not take is not sent
  to the appliance that holds the address (new in this release: it was sent there too, where the apply's own check
  put the old file back). On each appliance the apply checks the file (`haproxy -c`), reloads, proves that HAProxy
  listens on every bind of it, and puts the old file back otherwise; a hash is recorded only for a config that is
  served. The new directives are checked against the HAProxy of both appliance images (3.0 on Debian 13, 3.2 on
  Ubuntu 26.04) by the container test, not on a lab.
- **From v0.11.1-rc.1: every pair gets keepalived's files once more, and nothing else.** Two files an existing
  pair runs changed since that candidate: keepalived's track script `/usr/local/sbin/proxcloud-lb-check` (it asks
  HAProxy's master again during a reload) and its notify script `/usr/local/sbin/proxcloud-lb-notify` (it writes
  the state by a rename). `keepalived.conf`, the sysctl file and the unit drop-in are the same to the byte and are
  written again with them. `haproxy.cfg` is the same to the byte and is not pushed, so HAProxy is not reloaded,
  and the files of a pair's UDP listeners are the same and are not pushed either. The first pass after the upgrade
  (the first services sync, or the controller's next call for a cluster's pair) gives each appliance of a pair
  those five files and one `systemctl reload keepalived`, the standby first. A reload keeps keepalived's state, so
  no address moves (the same reload at the upgrade to rc.1 made no VRRP transition on the lab). A single appliance
  runs no keepalived and gets nothing. A pair that has one appliance at the upgrade is the other case: that
  appliance gets keepalived's files as its own master, for the first time where it never had them (see Fixed,
  "The first appliance of a pair"). Upgrading from v0.11.0, this is part of the one push described above. A
  pair with a UDP listener that read `DEGRADED` "Neither appliance holds the address yet" under rc.1 reads `ACTIVE`
  after that pass, and its cluster's pending Services become ready (see Fixed). Not run on a lab.
- **Until its push, `GET_HEALTH` of a balancer lists its members as `NOT LOADED`**: the stored config names its
  backends by pool, and the appliance still runs the names of before. The listeners are not affected.
- **The controller's two fixes need the controller of this release.** A cluster keeps the controller image it has
  until it is moved to the provider's chart version (the cluster's update) or given "Enable load balancer
  controller" again, which also replaces its credential. Until then it sends a node before it is Ready and writes a
  Service's address when it is allocated, as in `v0.11.0`; what the balancer does (state kept over a reload,
  redispatch) applies from its push on. The portal works with either controller: the protocol of `/lb/v1` did not
  change.
- **Pool names of a cluster's balancer changed** from `s<service>-<port>` by position to `s<digest of the Service's
  UID>-<node port>`, in the row and in `GET_HEALTH`. Nothing reads them but people.

### Added

- **The consoles show a cluster's UDP and TLS Services, and which controller the cluster has**
  (`internal/cloud/lbgw`, `internal/platform/project/cloud_lb_cluster_pve.go`, both consoles).
  - **The tenant's cluster page** shows each port of a Service with its protocol (`443/HTTPS`, `53/UDP`), under a
    port that ends TLS the Secret its certificate came from, the names it holds and when it expires (a warning from
    30 days before), and under a UDP port what its health check does not see. The cluster balancer's own page shows
    the protocols and the UDP workers' health too.
  - **A controller of an earlier release is named.** The gateway keeps the protocol a controller names with its
    last call (written at once when it changes), and the cluster's load balancer view answers it next to the
    portal's own (`controllerProtocol`, `protocol`). The tenant's page then says what is not served (UDP ports, the
    annotations, TLS from a Secret, kept addresses) and that the platform update on the page brings it; the
    operator's **Load balancer** dialog says "It speaks protocol 1 of 2" and names the bump or the rotation.
  - **The view lists the cluster's certificates** (`certificates`: id, Secret, subject, names, both dates, public
    fingerprint). They are rows of the cluster's balancer that no project's **Certificates** page lists. Nothing
    of a key and not the hash of a Secret is in it.
  - **The guides.** The tenant's Kubernetes guide no longer says that UDP is refused: it covers UDP ports and their
    health check, the options (the annotation table), TLS from a Secret and where the key rests, keeping an
    address, the seventeen deliberate differences from the upstream cloud controller manager and how to rename a
    manifest written for it. The load balancer guide no longer says that TLS termination is not available. The
    operator's guide has "UDP, TLS and the controller of an earlier release".
  - Tested: the gateway's record (`TestControllerProtocolIsRecorded`), this image's controller naming the portal's
    protocol against the real gateway handler (`TestControllerNamesThePortalsProtocol`), the view against Postgres
    (`TestLBClusterStateConverges` and its `TLSFromSecret`: the protocol, the certificate, and nothing of a key in the
    JSON), and the pages on
    the mock apps (Playwright). Not run on a lab.

- **A Service can leave its public address to its cluster, and a Service can ask for a kept address**
  (`internal/cloud/lbgw`, `internal/platform/project/cloud_lb_cluster_pve.go`, `cmd/lb-controller`; "Keeping a
  Service's address" in [managed-k8s.md](docs/managed-k8s.md)). What OCCM does with `keep-floatingip` and
  `spec.loadBalancerIP`, under the product's own name.
  - `proxcloud.io/lb-keep-address: "true"`: when the Service goes, its address leaves the appliances and stays the
    cluster's. **A kept address is billed and counts in the floating IP quota** like any public address, until it
    is released. A cluster keeps 5 at most; with 5 kept, the Service is told `KeepAddressLimit` and its address is
    given back when it goes.
  - `spec.loadBalancerIP` that names an address the Service's own cluster keeps: the Service gets it, with no new
    address and no quota asked. Any other value (another cluster's or project's address, a free one, one in use,
    no IPv4 address) is not obeyed: the Service gets an address of the pool and the Warning `LoadBalancerIPIgnored`,
    in the same words whichever it was. A Service that has an address keeps it.
  - The cluster's page lists the kept addresses with a "Release" button each (cluster action
    `RELEASE_LOAD_BALANCER_ADDRESS`); the operator's cluster dialog shows them. They are released with the cluster
    too.
  - Gateway protocol 2 gains one optional field, `loadBalancerIP` on a Service. It needs the controller of this
    release: a cluster on v0.11.0's controller is served as before, and its `spec.loadBalancerIP` is still ignored
    with the Event. No chart change.
- **The load balancer controller watches the cluster: a change of a Service or a node is at the portal within 2
  seconds** (`cmd/lb-controller`, `internal/cloud/kamajik8s/watch.go`, cluster chart `proxmox-kamaji-cluster` 0.1.19;
  "The controller" in [managed-k8s.md](docs/managed-k8s.md)). Until now the controller listed the cluster every 10 s
  and sent its whole state each time. With chart 0.1.19 it watches the Services, the Nodes and the TLS Secrets its
  Services name, and takes its turn one second after a change.
  - **The watch is only a trigger.** A turn is what it was: list everything, send the whole state. An event that is
    lost or a stream that is down costs time, never a wrong state, and a state that did not change is still sent
    every 30 s.
  - **Fewer requests.** A state equal to the one the portal last answered is not sent again before those 30 s: a
    kubelet's heartbeat is a list on the cluster and nothing at the portal. While the portal is building, the
    controller asks every 10 s as before, and a state that changed is sent once more 10 s later: the portal
    answers a new state before its pass over the appliances, so the second request brings what the pass made of
    it (the address of a Service that was refused and put right) and starts the pass a running one skipped. This
    part also holds for clusters on charts 0.1.17 and 0.1.18, which keep polling every 10 s.
  - **No library.** A raw watch over HTTP on the product's own cluster client: streams of 5 minutes that go on
    from the last resourceVersion, a new start after `410 Gone`, a wait that grows to 30 s while the API server is
    down.
  - **Secrets.** A Secret is watched by its name, only while a Service names it, at most 16 per cluster; the
    controller still never lists Secrets and reads one only with a GET of its name. A renewed certificate is on the
    balancer seconds after cert-manager wrote it.
  - **The chart.** 0.1.19 adds the argument `--watch=true` to the controller and nothing else: no value, no RBAC,
    no NetworkPolicy rule. It is in the source and not offered yet: the provider's chart version follows in this
    release.
  - Not run on a lab. Tested against an API server over HTTP that streams as kube-apiserver does (events for the
    controller's own writes, bookmarks, `410 Gone`, streams cut off), not against a real one.

- **Kubernetes Services: TLS ends on the cluster's load balancer, with the certificate of a Secret**
  (`internal/cloud/lbgw/tls.go`, `cmd/lb-controller`, `internal/platform/project/cloud_lb_cluster_tls.go`;
  "TLS from a Service's Secret" in [managed-k8s.md](docs/managed-k8s.md)).
  - `proxcloud.io/lb-tls-secret: <name>[,<name>...]` names one to four `kubernetes.io/tls` Secrets of the
    Service's own namespace. Every TCP port of the Service then ends TLS on the balancer, or only the ports of
    `proxcloud.io/lb-tls-ports`. The nodes are spoken to in HTTP, with `X-Forwarded-For` and
    `X-Forwarded-Proto: https`, or in plain TCP with `proxcloud.io/lb-backend-protocol: tcp`. Several Secrets are
    chosen by the name the client asks for; the first one is the default. TLS 1.2 and 1.3 with the suites of the
    project certificates.
  - The cluster's controller reads only the Secrets its Services name: a GET of one name in the Service's
    namespace, never a list or a watch, and nothing of another type. It sends the hash of each one every turn and
    the Secret itself once, when the portal asks for that hash; a changed Secret (a renewal) is sent once more and
    reaches both appliances as a new file and one reload.
  - The portal checks a Secret as an uploaded certificate, seals it with its encryption key in a row that
    belongs to the cluster's balancer and that no project route reaches, and keeps only hashes in the cluster's
    state. No log line, audit event, Event or error text holds anything of a certificate or a key; the audit log
    names the Secret and the certificate's public fingerprint.
  - A Secret that is deleted, of another type, incomplete or refused by the check never takes TLS off a port and
    never costs a Service its address: the Service stays exactly as it was served and a Warning Event
    `InvalidCertificate` says why. A Service that was never served gets no address until its Secret is good. An
    expired certificate is served, with a Warning Event `CertificateExpired`.
  - Whoever may write a Service in a namespace can have any TLS Secret of that namespace served, as with an
    Ingress.
  - No chart value and no RBAC in the tenant cluster is new: the controller is the portal image's and uses the
    kubeconfig it already has. Not run on a lab yet.
- **Kubernetes Services: UDP, session affinity and the layer 4 annotations on the cluster's load balancer**
  (`internal/cloud/lbgw`, `cmd/lb-controller`, `internal/platform/project/cloud_lb_cluster_pve.go`; the full table
  and the differences from OCCM: [managed-k8s.md](docs/managed-k8s.md)). The gateway speaks protocol 2, which
  only adds to protocol 1 (a Service's annotations, affinity and selected nodes in the request, its `ipMode` and
  Events in the answer); a controller of v0.11.0 is served as before.
  - A Service's UDP ports are served by the pair's LVS, and a TCP and a UDP port of one number share the Service's
    address (the listener's key is protocol and port). SCTP refuses the Service with `ProtocolNotSupported`.
  - `sessionAffinity: ClientIP` keeps a client on one node for the Service's timeout (TCP and UDP).
  - `externalTrafficPolicy: Local` checks UDP ports too over HTTP on `healthCheckNodePort`.
  - Annotations, read by the portal: `proxcloud.io/lb-proxy-protocol` (now also `v1`, `v2`), `lb-method`,
    `lb-connection-limit`, `lb-timeout-client-data`, `lb-timeout-member-connect`, `lb-timeout-member-data`,
    `lb-x-forwarded-for`, `lb-enable-health-monitor`, `lb-health-monitor-delay`, `-timeout`, `-max-retries`,
    `-max-retries-down`, `lb-node-selector`. Every other annotation of the prefix `proxcloud.io/lb-` is named in
    one Warning Event `AnnotationNotSupported`; a value that is not understood is named in `InvalidAnnotation` and
    the default holds; `proxcloud.io/lb-internal: "true"` refuses the Service (`InternalNotSupported`), so it never
    gets a public address. `lb-keep-address` keeps the Service's address for the cluster (its own entry above).
  - **Only `proxcloud.io/lb-*` is read: the product defines its own annotation names.** What a knob does follows
    OCCM, but OCCM's annotation names are not aliases: on a Service here they do nothing, are not sent to the portal
    and raise no Event. A manifest written for OCCM is renamed first; the guide has the table by OCCM's short key.
    **A Service that asks for an internal balancer with OCCM's annotation alone gets a public address** until the
    annotation is renamed to `proxcloud.io/lb-internal`. `proxcloud.io/lb-proxy-protocol: "true"` is version 2, as
    in `v0.11.0` (OCCM's `true` is version 1: write `v1`).
  - An annotation that cannot be read never refuses a Service, so a Service that `v0.11.0` served keeps its address
    over the cluster's upgrade whatever annotations were left on it: a value over 256 characters is not obeyed and
    named in `InvalidAnnotation`, and any number of annotations the table does not know is named (the first 32) and
    counted in `AnnotationNotSupported`. The forwarded-for header asked for next to the PROXY header is not added
    and named in `InvalidAnnotation`; the PROXY header holds. The internal annotation is the one that takes an
    address back. The controller sends only the annotations of the prefix `proxcloud.io/lb-`, a value over 256
    characters cut.
  - A state over the gateway's 64 KiB is refused as a whole and changes nothing: the balancer and every address stay
    as they are until the state fits, and each Service gets a Warning Event `TooLarge` that says so.
  - A node selector's terms may have spaces around `=` (`pool = edge`); OCCM drops them around a term only and
    matches no node with such a term.
  - A controller that finds an older portal (a portal rolled back to `v0.11.0`) sends it the request of protocol 1
    in the same turn, after that portal's refusal, and remembers nothing: the first request a portal that came back
    gets is the whole one. While the portal is rolled back, such a turn is two requests and the old portal audits
    the refused one each turn.
  - The controller says the Events of Kubernetes' own service controller: `UpdatedLoadBalancer`,
    `UnAvailableLoadBalancer`, `SyncLoadBalancerFailed`, `DeletingLoadBalancer`, `DeletedLoadBalancer`.
  - Not run on a lab: tested on fakes of the tenant cluster and of Proxmox, and against the HAProxy and keepalived
    of both appliance images in containers.
- **Managed platform: the sandbox provider, and the fence of its machines on the platform network (not offered yet:
  no BOM pins the sandbox chart).** With `spec.sandbox`, a run now does the portal side as well
  (`internal/platformlife/sandbox_provider.go`, `docs/managed-services.md`, "The sandbox provider"):
  - Stage `cluster` writes the sandbox range (`config.network.sandboxCidrs`) and the sandbox pool
    (`config.sandbox`) on the installation's Proxmox provider before a sandbox machine exists; stage `register`
    adds the firewall identity's kubeconfig and registers "\<name> Sandboxes", **DISABLED**, with this release's
    template images by digest (`deploy/templates/images.yaml`, now embedded). A run never sets the provider live.
  - The provider sync fences every VM of pool `proxcloud-sbx`, and no other, on the platform's bridge and tag
    (no vnet of their own). A `shared` group gets a rule set of its own: the Cilium tunnel, health and ICMP with
    the cluster's machines, TCP 6443 to the API address and to every machine of the cluster (the `kubernetes`
    Service is rewritten to a control plane machine's address inside the VM), the kubelet from the cluster alone,
    no MetalLB and no proxy entry. The platform prefix is denied in both placements.
  - On a `shared` cluster the fence gate first requires the RuntimeClass to select the sandbox group and tolerate
    the sandbox taint alone, then asks for that group's Nodes only; the network guard gate counts only them; the
    portal's identity binds its namespaced role in each `sb-<project>` namespace and lists through them, so a
    stolen token reads nothing outside `sb-*`.
  - A platform's sandbox provider is refused "live" while the job scheduler is off (`409`), and the Check's
    `scheduler` row says why. `placement: off` is refused while the provider is live (`409
    sandbox_provider_live`); off and Remove take the provider's sandbox settings and the pool's firewall groups
    and IP sets away.
  - `separate`: the proxy's certificate is written inside `pc-sandbox`. The Check's `sandbox` row also fails for a
    datacenter firewall that is off and for a `proxyAddress` a Service already holds; a sandbox pool of another
    owner on the provider fails the save; the `capacity` row warns for small `kamaji` machines.
  - Console: a Sandboxes block in the settings (off, shared node group, separate cluster; machines, range,
    proxy address, domain) for a release whose BOM has the chart. `shared` prints what it gives up in full and is
    saved only with a tick. The Ready card shows the provider, reads its release gates and offers the certificate.
  - Nothing of this ran on a lab: the drill list is in `docs/managed-services.md`, "Not verified on a lab yet".

- **Load balancers: TLS and HTTPS listeners, and a project's certificates** (`internal/cloud/pve/lbtls.go`,
  `internal/platform/project/cloud_lb_certs.go`, the tenant console's Certificates page and load balancer page).
  A listener takes `protocol: "tls"` (TLS ends on the load balancer, TCP to the servers) or `"https"` (TLS ends on
  the load balancer, HTTP to the servers), on one VM and on a pair, with `certificates`: one to ten of the project's
  certificates. They were refused with "TLS termination is not available" until now.
  - **Certificates.** `GET`, `POST` and `DELETE` under `/api/v1/project/{id}/certificates`: upload a PEM chain and
    its PEM key; read what the certificate says (names, dates, SHA-256 fingerprint, key kind, the load balancers
    using it); delete one no listener names (`409` otherwise). The upload checks that the key belongs to the first
    certificate, that the chain is in order (at most 5), that the key is RSA of 2048 to 8192 bits or ECDSA P-256 or
    P-384 without a passphrase, and 16 KiB in all. An expired certificate is taken with a warning. There is no
    update: a replacement is a new certificate, named on the listeners in the old one's place.
  - **Which certificate a client gets.** The one whose names hold the server name it asks for (SNI); the first of
    the listener's list for any other name and for none.
  - **The TLS policy is fixed.** TLS 1.2 and 1.3; the three TLS 1.3 suites, and for TLS 1.2 six suites with ECDHE
    and AES-GCM or ChaCha20-Poly1305, for RSA and for ECDSA certificates. No RSA key exchange, no CBC, no DHE.
    Shown by real handshakes against the HAProxy of both appliance images (3.0 on Debian 13, 3.2 on Ubuntu 26.04):
    TLS 1.1 and 1.0 refused, each suite of the policy accepted, the others refused.
  - **HTTPS** offers HTTP/2 and HTTP/1.1, speaks HTTP/1.1 to the servers, and sets `X-Forwarded-For` and
    `X-Forwarded-Proto: https`, replacing what a client sent in the second. TLS to the servers is not offered.
  - **The private key.** Stored as one blob encrypted with the portal's encryption key (AES-256-GCM), bound to its
    project and certificate id; no route returns it, and no log line or audit event holds it (the audit event of an
    upload and of a delete holds the name and the fingerprint). On a portal whose encryption key is missing or
    shorter than 32 characters an upload answers `503` and nothing is stored. Deleted with the project.
  - **On the appliances.** Each certificate is one file in `/etc/haproxy/certs` (root, mode 0700; files 0600),
    named by the SHA-256 of its chain. The directory is made private and checked before anything is written into
    it, because the guest agent creates files readable by all. Files go before the config that names them, on the
    standby first; a new config HAProxy refuses leaves the certificate in use in place (a file of the config in
    use that is damaged on the appliance is another matter, see "A damaged certificate file" under Fixed); files
    no config names are removed after the new config is served. A key is never in cloud-init. The container
    test searches every
    file of the guest for the key, on both images: it is in the certificate files in use and nowhere else, after
    the first push, after three changes of certificate, and in no file once the last TLS listener is gone.
  - **A change of certificate closes no connection**: it is one HAProxy reload. In the container test, over 300
    handshakes made across three reloads with a changed certificate all succeeded, and a connection open since
    before them still worked after.
  - **Console.** A Certificates page (upload, what each certificate says, who uses it, delete) and, on a TLS or
    HTTPS listener, the certificates it presents in order. Both warn from 30 days before a certificate expires.
  - **Operators: where tenant keys now rest.** In the database, encrypted; **in `backups/`**, where
    `proxcloud-up.sh backup` (run by every upgrade) writes the database dump and a copy of `.env` with
    `ENCRYPTION_KEY` side by side, so whoever reads that directory reads every tenant's private key; and **in clear
    on each appliance's disk**, so in any Proxmox backup, snapshot, clone or replica of an appliance VM. The
    Compose README's Backup section, the cloud integration guide and the tenant guide say so.
  - **No balancer is reloaded by this change**: a config without a TLS listener is the bytes it was.
  - Not run on a lab by this change. A cluster's Services cannot ask for TLS yet: that is the controller's change.
- **Load balancers: UDP listeners, on a load balancer with high availability** (`internal/cloud/pve/lblvs.go`,
  `internal/platform/project/cloud_lb_pair_pve.go`, the tenant console's load balancer page). A listener and its
  pool take `protocol: "udp"`; a TCP and a UDP listener may share a port number.
  - **How it is served.** HAProxy balances no generic UDP: the kernel's IPVS does, in NAT mode, filled by a second
    keepalived that runs only health checks, in a unit of its own (`proxcloud-lvs.service`). An nftables rule
    masquerades the datagrams on the project NIC, so the servers answer the appliance on their own subnet and need
    no change; the answer leaves the public NIC with the load balancer's address as its source. The VRRP keepalived
    is not touched: a server that comes or goes reloads the UDP service alone.
  - **What the servers see:** the appliance's private address, never the client's (UDP has no PROXY protocol).
  - **Options:** round robin, least connections or source address with weights; session persistence by source
    address; a connection limit that counts **per server**; source ranges (one `udp` firewall rule per address and
    source list).
  - **Health checks:** `udp-connect` (default: a datagram to the port and a ping; a stopped server leaves in about
    10 s), with an optional `payload` and `requireReply` for a check that sees a closed port; `tcp` or `http` on a
    port of your choice; `none`. A server enters the pool at its first good check and leaves after 2 to 10 failed
    ones. **Read the caveat:** without a payload the check sends random bytes to the service, and without a
    required reply a closed port on a running server is not seen while the port has traffic. The page and the
    guide say so.
  - **The appliance is no router.** Forwarding is on only while an nftables chain with policy drop is loaded that
    lets through nothing but the answers of balanced flows; the unit loads the table first and switches forwarding
    off when it stops or fails to start. TCP is not connection-tracked, so HAProxy's traffic cannot fill the table.
  - **Only where it is used.** A load balancer without a UDP listener gets no file, no package and no service of
    this, and its recorded hashes do not move. The first UDP listener installs `nftables` where the image lacks it (that package alone: nothing already on a serving appliance is upgraded);
    removing the last one takes the service, the table and the files off again.
  - **Not on one VM.** A load balancer without high availability answers `400` "UDP needs a high-availability load
    balancer". A Kubernetes Service with a UDP port is still refused (`UDPNotSupported`) until the cluster
    gateway maps it.
  - **After a failover** the other appliance spreads clients afresh (the appliances do not sync their connections).
  - Tested in real guests of both appliance images (Debian 13 and Ubuntu 26.04 containers with systemd, keepalived,
    nftables and HAProxy, on the CI runner's kernel) with network namespaces for client, servers and gateway: the
    reply path, the dead server, the dropped forwarding, the untracked TCP, the refused config. The method was
    run by hand on the lab's Ubuntu 26.04 appliances in the spike of 2026-11-22; this code has run on no lab pair
    yet: an Ubuntu 26.04 pair and a Debian 13 pair are part of the release drill.

- **Managed platform: the code that builds sandboxes from the install settings, not offered yet**
  (`internal/platformlife/sandbox.go`, `sandbox_run.go`). `spec.sandbox` takes `placement` `shared`, `separate` or
  `off`, the machines, their own address range, the proxy address and the domain. **No release's BOM names the
  sandbox chart, so every installation is refused the block** (`400 invalid_installation`, "has no sandbox chart");
  nothing changes for a site until the release that pins it.
  - `shared` is a node group `sandbox` of the platform cluster, added through one function that the render, the
    roll, the capacity row and every machine count read, so it is ordered and gated like the other groups and is the
    last unit of a roll. `separate` is a cluster of its own, `pc-sandbox`, with a Kamaji control plane on the `kamaji`
    group. Both put their machines into the new Proxmox pool `proxcloud-sbx`, with addresses from `nodeRange`.
  - `nodeRange` is one aligned IPv4 CIDR inside the platform network, apart from every other range and from the
    tenant pool, the blocked, management and public networks: the provider fences it as a sandbox network, and a
    range its isolation refuses would break every tenant's network create.
  - **No new Proxmox privilege and no second root action.** The portal token makes the pool and writes one ACL row
    for the platform's own CAPMOX token (`shared`) or mints the sandbox cluster's token as it mints a tenant
    cluster's (`separate`), with the rights the two setup scripts already grant for tenant pools. The new Check row
    `sandbox` reads those rights and, where they are missing, says to run the managed-services script again as root
    ([docs/proxmox-permissions.md](docs/proxmox-permissions.md#sandboxes-on-the-managed-platform)).
  - The capacity row fails, also on an installed platform, for sandbox machines no run has built on a node without
    the room. The plan lists the machines, the pool and the ACL row or token before an apply, says how many sandboxes
    a machine holds by CPU (a machine of 2 cores holds one), and what `off` deletes.
  - A PUT without the block keeps the stored one; `off` is explicit. Built, `placement` changes only to `off`, and
    `nodeRange` and `proxyAddress` not at all. `tls.mode: secret` is refused at the save: the run makes a
    self-signed certificate for `*.<domain>`, and no route stores an operator's own yet.
  - The sandbox chart is the one component an upgrade may add to an installed platform (plan class `added`): a chart
    of our own marked with a feature, rendered only for settings that ask for it. Every other added or removed
    component stays refused. An upgrade that replaces the platform's machines is refused while `pc-sandbox` exists
    (it is not upgraded in place: `off`, the upgrade, then `separate` again) and while a `shared` group is asked for
    and not built.
  - Not in this change: the sandbox provider's registration, the VM firewall of the sandbox machines and its gates,
    the provider's `sandboxCidrs` entry, the proxy's Secret inside `pc-sandbox`, an operator's own certificate, and
    the console's form. Run on fakes and against the two charts' renders; on no lab. The figures for what a machine
    holds are from the spike of 2026-11-21 on a throwaway tenant cluster.
- **Load balancers: a protocol per listener and the layer 4 options** (`internal/cloud/pve/lb.go`,
  `internal/platform/project/cloud_lb_pve.go`, the tenant console's load balancer page). `UPDATE_CONFIG` and the
  page take, on a single appliance and on a pair:
  - per listener `protocol` (what clients are spoken to in), `connectionLimit` (1 to 20000, HAProxy `maxconn` of
    the frontend; 20000 is the appliance's whole budget) and `timeoutClientData` (milliseconds, `timeout client`);
  - per pool `sessionPersistence` `{type: source_ip, timeout}` (`stick-table type ip` with `stick on src`; 10800 s
    when no time is named), `proxyVersion` `1` or `2` next to `proxyProtocol` (`send-proxy` or `send-proxy-v2`),
    `timeoutMemberConnect` and `timeoutMemberData` (milliseconds, `timeout connect` and `timeout server`);
  - per health check the types `https` (`check-ssl verify none`), `tls-hello` (`option ssl-hello-chk`) and `none`
    next to `tcp` and `http`, and the figures `interval`, `timeout` (seconds), `rise` and `fall` (it was fixed at
    3 s, 2 and 2).
  - **The protocol moved to the listener.** The model knows `tcp`, `udp`, `tls`, `http` and `https` listeners and
    which pool protocol each needs (`tcp` and `tls`: `tcp`; `http` and `https`: `http`; `udp`: `udp`), and one port
    holds one listener per transport. **Served in this release: `tcp` and `http`.** A `udp`, `tls` or `https`
    listener answers `400` with the reason ("UDP is not available on a load balancer yet", "TLS termination is not
    available on a load balancer yet") until the releases that serve them; `certificates` on a listener is reserved
    for the last two.
  - **No existing balancer changes.** A stored listener without a protocol reads its pool's, a request from before
    (the protocol on the pool only) is read as it was, and a field at its default writes no line: the three config
    goldens of before are byte for byte the same, so the upgrade pushes nothing because of this.
  - Limits: source persistence is kept by each HAProxy process, so it starts afresh after a change of the
    configuration and after the other appliance takes over; a TLS hello check takes any TLS answer for healthy and
    looks at no certificate; a pool with check `none` never takes a server out, and its backend reads no saved
    state at a reload (`load-server-state-from-file none`): a server that was down when its check was taken away
    would otherwise stay down for good, as nothing brings it back. A health check's `timeout` is HAProxy's
    `timeout check`, the time the answer may take once the check is connected; with one set, the connection itself
    must come within the pool's connect timeout (2 s when none is named) or the interval, whichever is shorter, so
    on a `tcp` check it only shortens the wait. A Kubernetes cluster's balancer does not take these options from
    its Services yet.
  - Tests: the validation table (the matrix and every range), a line-by-line test of what each option adds to the
    config, three new goldens, and the HAProxy of both appliance images (3.0 on Debian 13, 3.2 on Ubuntu 26.04)
    running `haproxy -dW -c` on every golden and then a rendered config against members in the same container: a
    PROXY v1 header at the member, one member per client with persistence and both without, HTTPS and TLS hello
    checks that take a TLS server for up and a plain one for down, `no check`, and a listener's limit as the
    frontend's. Not run on a lab.
- **Chart `proxcloud-sandbox` 0.1.0: the sandbox runtime of the managed platform** (`deploy/charts/proxcloud-sandbox`,
  published on a `proxcloud-sandbox-v*` tag; not pinned by the BOM yet, so nothing installs it). Mode `shared` runs it
  on a node group `sandbox` of the platform cluster, mode `separate` on the platform-owned sandbox cluster. node-setup
  (runsc 20261116.0 from the pinned package file, the network guard), telemetry and the template prepull run on the
  sandbox nodes only (shared), so no platform node gets runsc or a containerd restart. RuntimeClass `gvisor` carries
  `scheduling`: every gVisor pod lands on a sandbox node, tolerating `proxcloud.io/sandbox` only (never
  `proxcloud.io/unsecured`). A CiliumNodeConfig gives the sandbox nodes alone the network guard's CNI chain
  (`custom-cni-conf`, `write-cni-conf-when-ready` present and empty), the bandwidth manager and host-only socket LB.
  agent-sandbox v1.0.9 and sandbox-proxy (its MetalLB address from `proxy.address`) run on the system side; the
  release namespace gets no placement. In shared mode the portal's identity holds cluster-wide only namespaces,
  RoleBindings and `bind` on one namespaced ClusterRole, which it binds in each `sb-*` namespace, and four
  ValidatingAdmissionPolicies narrow it: namespaces `sb-*` with `proxcloud.io/tenant=true`, RoleBindings of that role
  to itself only, the firewall identity's node writes on sandbox nodes only, and every pod of a tenant namespace
  (both modes) on RuntimeClass `gvisor` with no privilege, added capability, host namespace, `nodeName` at create or
  hostPath but `/opt/proxcloud-sandbox/bin` read-only, no hostPort, no volume type outside Pod Security restricted and
  no toleration of `proxcloud.io/unsecured` or of every taint, ephemeral containers included. In shared mode the chart
  refuses to render with an empty sandbox node selector or RuntimeClass scheduling. No Pod Security label: baseline
  refuses the runtime hostPath of every sandbox. The firewall identity may get RuntimeClass `gvisor` (the fence gate),
  the network guard and the portal may read CiliumNodeConfigs. Images by digest (the runtime built from `main` at
  `d26edd2e`, the first with the network guard's per-node Cilium read, below; the proxy of `v0.11.0-rc.1`; busybox,
  pause). Tests: goldens of both modes, the node-setup script, guardrail, telemetry, the network guard's RBAC and
  agent-sandbox equal to `deploy/sandbox-cluster`, and the policies run through the API server's CEL against the pods
  the portal's `BuildCR` makes.
- **Cluster chart `proxmox-kamaji-cluster` 0.1.18: a node group's own pool, address range and DNS servers, and the
  sandbox chart as an add-on.** A node group may set `pool`, `addresses` (an `InClusterIPPool` `<release>-<group>`,
  `net0` on it with `defaultIPv4: false`, so the machines take no address from the cluster's range) and `dnsServers`;
  `addons.sandbox` installs chart `proxcloud-sandbox` in mode separate through CAAPH. A group without the keys renders
  what 0.1.17 rendered: every values fixture of this and every previous release renders the same control plane,
  machine and bootstrap templates as the pinned chart, and the tenant goldens are unchanged.

### Changed

- **The tests pull each published chart once per machine, not at every call.** `go test ./internal/platformlife`
  ran `helm pull` thirteen times at every run, in CI and on every workstation: the platform's cluster chart
  `proxmox-kamaji-cluster` eleven times (once per call of the test helper, so once per previous BOM that may still
  be upgraded from) and the mirrored cert-manager twice. Each was a download the package page counts, which is
  where most of the downloads of `charts/proxmox-kamaji-cluster` came from. The tests now keep what they read in
  `proxcloud-test-charts` under the user's cache directory (or the directory `PROXCLOUD_TEST_CHART_CACHE` names),
  by the BOM's digest: four reads of the registry on a machine that has none of the charts, none after that, and
  in CI only what a changed BOM added (`actions/cache`, keyed by the BOMs). The digest check is stricter than
  before: the cluster chart was pulled by its tag and not compared with the BOM, and is now read by the pinned
  manifest digest and hashed, with the archive, at every use, cached or not. Nothing in the product changes
  ([ci-release.md](docs/ci-release.md), "The test chart cache").
- **New tenant clusters get cluster chart `proxmox-kamaji-cluster` 0.1.19** (`tenantCharts.kubernetes`, digest
  `sha256:476950b5e841c9460c47960b1841ceac17998b5c06c7358c9966585f6602d5fa`, published by the tag
  `proxmox-kamaji-cluster-v0.1.19` and compared with the registry by `bom_verify.py`), and with it the load
  balancer controller that watches. The platform cluster `pc-platform` stays on 0.1.16: the plan did not ask for
  its move, and nothing it would gain (a node group's own pool, the sandbox add-on) is used by this release. No
  component of the sandbox feature is in the BOM (`TestSandboxIsDark`, unchanged). The upgrade plan from 0.11.0,
  0.11.0-rc.1 and 0.11.0-rc.2 has two rows, the release and `tenantCharts.kubernetes` 0.1.17 to 0.1.19, both record
  only; from 0.10.2 and 0.10.2-rc.1 the same two; from 0.10.1 and 0.10.1-rc.2 four, from 0.10.1-rc.1 five, as
  before; every BOM on Kubernetes 1.35.10 stays refused (`TestPreviousBOMsMoveCertManager`, which now also asserts
  the row's class and both versions). The goldens of every previous BOM are byte for byte what they were;
  `render-providers.golden.yaml` names 0.1.19. The controller image a new, re-enabled or bumped cluster gets is the
  portal's own, as before (`PROXCLOUD_LB_CONTROLLER_IMAGE`): the one that speaks protocol 2 and knows `--watch`.
- **Which nodes are behind a cluster's Services follows Kubernetes' own rule** (`cmd/lb-controller`, `readNodes`;
  `k8s.io/cloud-provider` `stableNodeSetPredicates`). The label
  `node.kubernetes.io/exclude-from-external-load-balancers` is read as a boolean: `false` (and `0`, `f`) now keeps
  the node in, where any value took it out. A node with the cluster autoscaler's taint
  `ToBeDeletedByClusterAutoscaler` is taken out. A node new to the balancer still waits for its first Ready, and a
  member still stays through NotReady. Both changes need the controller of this release.
- **A Service with an SCTP port is refused with `ProtocolNotSupported`** (it was `UDPNotSupported`).
- **A load balancer's config files are compared with what was applied, at every pass.** The state read of an
  appliance now also answers what `haproxy.cfg`, `keepalived.conf` and the UDP files hash to; an appliance whose
  file is not the one the platform recorded (edited by hand, or lost) is pushed again, on a single appliance and on
  a pair. Before, only "HAProxy listens on every bind" was asked, and a file changed by hand stayed.
- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1-rc.5`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1-rc.5.yaml`, sha256
  `a28b54343f999674a2ca0e53b255312845cb190a78f5570dbaeaadcc7e8d9000`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.1-rc.5`, and the release gate refuses only `-dev`. For an upgrade: a
  platform moved to that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while
  the file is carried, and its pins are those of `main`, so the plan from it has one change, the release, record
  only: no seed part, no machine replaced. `TestPreviousBOMsMoveCertManager` states that answer, and
  `TestUpgradeRollsNoMachine` compares thirteen of the twenty-two previous BOMs with the BOM on `main`, none of them
  rolling. No move to that BOM and no apply on such a platform was run on a lab.
- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1-rc.4`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1-rc.4.yaml`, sha256
  `60c0281bd9723cb15585e92cc9087e05680798943c538d95044af77257f51222`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.1-rc.4`, and the release gate refuses only `-dev`. For an upgrade: a
  platform moved to that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while
  the file is carried, and its pins are those of `main`, so the plan from it has one change, the release, record
  only: no seed part, no machine replaced. `TestPreviousBOMsMoveCertManager` states that answer, and
  `TestUpgradeRollsNoMachine` compares twelve of the twenty-one previous BOMs with the BOM on `main`, none of them
  rolling. No move to that BOM and no apply on such a platform had run on a lab when the file was kept; the
  candidate's drill then moved one lab's platform to it, from 0.11.1-rc.3.
- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1-rc.3`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1-rc.3.yaml`, sha256
  `b9d119c59162ce75956bfb43a251915aaac6d8eee577944292be5434bbe3d581`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.1-rc.3`, and the release gate refuses only `-dev`. A platform moved to
  that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while the file is carried.
  Its pins are those of `main`, so the plan from it has one change, the release, record only.
  `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares eleven of the
  twenty previous BOMs with the BOM on `main`, none of them rolling. No move to that BOM and no apply on such a
  platform had run on a lab when the file was kept; the candidate's drill then moved one lab's platform to it,
  from 0.11.1-rc.2.
- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1-rc.2`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1-rc.2.yaml`, sha256
  `b66f6e7f3f96efa724393abc2f070fedbf4c2fcf1ef79ce6411d933a38944073`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.1-rc.2`, and the release gate refuses only `-dev`. A platform moved to
  that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while the file is carried.
  Its pins are those of `main`, so the plan from it has one change, the release, record only.
  `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares ten of the
  nineteen previous BOMs with the BOM on `main`, none of them rolling. No move to that BOM and no apply on such a
  platform had run on a lab when the file was kept; the candidate's drill then moved one lab's platform to it,
  from 0.11.1-rc.1.
- **The platform BOM on `main` is `0.12.0-dev` again, and the build carries the BOM of `0.11.1-rc.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.1-rc.1.yaml`, sha256
  `2d96e6c616ec26f0263e94b190f7d10936645fa6d184bb789d9870518b4248ee`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.1-rc.1`, and the release gate refuses only `-dev`. A platform moved to
  that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while the file is carried.
  Its pins are those of `main`, so the plan from it has one change, the release, record only.
  `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares nine of the
  eighteen previous BOMs with the BOM on `main`, none of them rolling. No apply on such a platform was run on a lab
  by this change.
- **The platform BOM on `main` is `0.12.0-dev`, and the build carries the BOM of `0.11.0`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.0.yaml`, sha256
  `d8e37db2fd204a6a58a4d6357f4c54bb6dccc82de52430340f2c8c7ca0e0d7ff`, the file of the tag byte for byte). The `v0.11.0`
  release commit left the BOM at `0.11.0`, and the release gate refuses only `-dev`. A platform installed by `v0.11.0`
  or moved to its BOM is not refused (`bom_unsupported`) under a later build while the file is carried. Its pins were
  those of `main` then; with the tenant chart at 0.1.19 (above) the plan from it has two changes, the release and
  that chart, both record only. `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares eight of the seventeen previous BOMs with the BOM on
  `main`, none of them rolling. No apply on such a platform was run on a lab by this change.
- **The platform BOM's release is `0.11.1`** (`deploy/platform/platform-bom.yaml`), with the pins of `0.11.1-rc.5`:
  the two files differ in the `release` line alone. No chart of the product differs from its newest tag (`git diff
  proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`, `git diff database-cluster-v0.5.2 --
  deploy/charts/database-cluster`, `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and `git
  diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed. The
  release gate passes on it and on the twenty-two previous BOMs. No component of the sandbox feature is in it
  (`TestSandboxIsDark`). The app chart is `0.11.1`, appVersion `v0.11.1`, and the installer's ProxCloud pin and
  `DEFAULT_VERSION` of `proxcloud-up.sh` name `v0.11.1`. This BOM carries no unfinished replacement
  (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **No product change against `v0.11.1-rc.5`.** Between that tag and the release commit the files outside `docs`, this
  file and the kept previous BOMs differ in the BOM's `release` line and the two goldens that carry it, in the version
  lines a final moves (the app chart, the installer's pin, `DEFAULT_VERSION`, the documents that name the current
  stable release, the release-contract test) and in the three test files that name the kept BOM of `0.11.1-rc.5`. No
  Go source outside tests, no chart template, no workflow and no appliance file differs.
- **The guides give what the five lab drills showed** (docs/cloud-integration.md, docs/managed-services.md,
  docs/ci-release.md, docs/release-readiness.md, deploy/compose/README.md): what ran on which candidate, what the
  drill of `v0.11.1-rc.5` can and cannot tell apart on appliances of one vCPU, and that nothing ran on the images of
  the release.

### Fixed

- **A unit test of a pair's push order asked for an order the product does not define.**
  `TestLBPairWithUDPKnowsItsHolder` compared the loads of a change that adds a TCP and a UDP port as one list:
  HAProxy before the UDP service on each appliance. An appliance takes the two at once since "a UDP port added with
  a TCP port is served sooner" (#454), so either may be loaded first, and the test failed whenever the UDP service
  was (CI on the release pull request of v0.11.1-rc.5, the unit step of `go-test`). The order the product does
  promise, the standby before the appliance that holds the address, held in that run and in every failing run
  here. The test now asks for that and nothing more: each appliance loads each of the two once, and the standby has
  loaded both before the other appliance is given either. The assertion as it was failed 56 of 500 runs on two
  CPUs that six busy loops share, 0 of 500 on an idle workstation under the race detector, and 20 of 20 with the
  fake appliance holding HAProxy's requests back by 0.6 s; as it is now it passed 500 of 500 in each of the first
  two and 20 of 20 with either push held back. No other test of `internal/platform/project` or
  `internal/cloud/pve` asks for an order between two pushes that are started together: the 51 load balancer tests
  of `internal/platform/project` pass with either push held back. Nothing in the product changes.
- **No UDP client is left unanswered when the UDP service stops and starts.** On an appliance pair a UDP client
  that kept sending from one source port while `proxcloud-lvs.service` stopped and started on the appliance holding
  the address could get no answer afterwards, for as long as it kept sending, while the balancer read `ACTIVE`, its
  members `UP`, and a new client was answered (CI on the release pull request of v0.11.1: the real-guest test of
  the fix "a UDP client that was sending before its listener existed is answered", below, failed its step
  "nftables.service restarted" about one run in twenty). The product caused it by
  itself: a balancer's last UDP listener removed and a UDP listener added again while a client sends (also a UDP
  Service made again), and the unit's keepalived dying, which systemd answers with a stop and a start. By hand:
  `systemctl restart nftables` or `systemctl restart proxcloud-lvs` on the appliance that holds the address, and
  `systemctl reload nftables` for a client whose first datagrams arrive in those milliseconds. A push that keeps a
  UDP listener only reloads the unit and never caused it.
  - *Cause.* The unit's stop empties IPVS, which removes each flow's good connection entry, and then deletes the
    nftables table, the chain `lb-input` of that fix with it. Connection tracking stays on for about 12 ms
    after the table is gone. A datagram in that moment is delivered and tracked without the source rewrite, and
    once the unit is back every later datagram of that client refreshes the entry (30 s), as in that fix's
    finding. A second, far shorter moment sits inside the table's load at a start: tracking is on before the rules
    act.
  - *Fix.* The unit's script notes whether the nftables table was loaded before it loads it, and at a start that
    found none it empties the kernel's connection table right after the load: one netlink message, what
    `conntrack -F` sends, from perl-base, which both images have (neither has `conntrack`); no package is added.
    After the load no such entry can be made, since `lb-input` drops what IPVS does not take and what IPVS takes
    is rewritten, so the flush leaves none. The kernel's answer is read and the unit's journal says which way it
    went, in one line. A flush that fails does not keep the unit from starting: an appliance that serves with the
    defect of before is better than one that does not serve, and the journal says "the connection table could
    not be emptied".
  - *Measured* in real guests of Debian 13 and Ubuntu 26.04, 200 stops and starts per event and image, with four
    one-socket clients at 5 datagrams a second and four at 50. Clients left unanswered before the fix, of 800 at
    each rate, Debian and then Ubuntu: the last UDP listener removed and added again 37 and 465, 51 and 500; the
    unit's keepalived killed 43 and 462, 46 and 481; `nftables.service` restarted 49 and 442, 41 and 447. With the
    fix: 0 of 800 in every one of them. The moment inside the load, under 1,000 sockets that send 5,000 datagrams
    a second with no listener: 80 and 79 of 200 starts left an entry without the fix, 0 of 200 with it. A
    reload of `nftables.service` by hand, 100 each: 179 and 178 of 27,000 new sockets left unanswered before, 0
    with the fix.
  - *What it does not do.* It frees no client at the upgrade (see the upgrade note), and it does not shorten the
    stop and start itself: a client's datagrams during it (about 1.2 s at a restart) are not answered.
  - *Not run on a lab.* Everything above ran in containers on one workstation's kernel (7.0); the appliance VMs'
    own kernels (6.12 on Debian 13, 7.0 on Ubuntu 26.04) carry the module the flush needs
    (`nf_conntrack_netlink`, in both images' kernel packages), which the script loads by name. The lab drill of
    the release is to show the journal line and an answered client on a real pair of each image.
- **An appliance the platform started reads up once it serves again.** After a failover, the appliance whose VM
  was stopped and which the platform started read `down` "its VM is stopped; starting it", and the balancer
  `DEGRADED`, until the next pass after it was back: on a tenant's own pair the next services sync, long after the
  appliance served again as the standby (lab drill of v0.11.1-rc.3, observation T2-o1). The health read already
  took back its own mark "it cannot be reached" when it reached the appliance again; it now takes back the pass's
  two marks for an appliance it could not read as well ("its VM is stopped; starting it", "it does not answer"),
  in its answer and in the row, when the appliance answers, HAProxy serves its configuration, keepalived is master
  or backup, and, on a pair with a UDP listener, the UDP service serves. That last condition is new for "it cannot
  be reached" too. A pass's mark stays on an appliance whose row does not record the balancer's configuration as
  it is now: it was down while a pass loaded a change on the other appliance, and reads down until a pass has
  loaded the change on it too. An appliance that is down for a reason the pass found (HAProxy not running, keepalived in
  fault, UDP listeners not served) stays so until a pass says otherwise.
- **A UDP client that was sending before its listener existed is answered.** On an appliance pair a UDP client
  that was already sending from one source port when the listener for its port was added got no answer for as long
  as it kept sending (lab drill of v0.11.1-rc.3, finding T2-F1: 0 of 2584 datagrams in the 8 minutes after the
  listener was live; answered again only after more than 30 s of silence). The balancer read `ACTIVE`, its members
  `UP`, and a new client was answered, so nothing showed it. It held for every client that sends from one port and
  is never silent for 30 s (a syslog, flow or metrics exporter, a VPN peer), in three ways: a UDP listener or a
  Service port added while clients send, a restart of `nftables.service` on the appliance that holds the address,
  and a UDP Service made again on a kept address, which is on the appliance some seconds before its listener.
  - *Cause.* A datagram that reached the address before its virtual server existed was tracked as a flow without
    the source rewrite the UDP path needs, and every later datagram of that flow refreshed the entry: once the
    listener was there, the server got the client's own address as the source and sent its answer by the tenant
    gateway, where it was lost.
  - *Fix.* The appliance's nftables table has one more chain, `lb-input`, in the input hook after IPVS: a first
    UDP datagram from outside that IPVS did not take is dropped there before its connection entry is confirmed, so
    no such flow is made, and the first datagram after the listener is served is answered. The kernel's own table
    decides, so nothing has to follow the listeners, and no package is added.
  - *Measured* in real guests of Debian 13 and Ubuntu 26.04 with a client that sends five datagrams a second from
    one socket: without the chain, 0 answers in 62 s; with it, the first answer 0.6 to 0.8 s after the push that
    added the listener, 0.0 to 0.2 s after the members were back in the table at a restart of `nftables.service`,
    and 0.7 to 0.8 s after the push for an address that came 7 s before its listener. A client of a listener that
    was there first lost nothing when the listener was added, at a reload of the UDP service, at a member's weight
    change and when the second address came.
  - *Weighed and left out.* A set of the listeners' ports that keeps other UDP untracked: it fails in the same way
    when a port enters the set before its virtual server exists, which is the order of a push and of a start, and
    otherwise costs a starting service's flows a datagram or two. Removing the entries with `conntrack`: it works
    and needs a package neither image has, on every existing appliance.
  - The state read notices a table without the chain (the one of a release before, or one changed by hand): such
    an appliance reads up with a change to load, and the pass loads the file again.
  - *Not changed, said plainly.* The chain frees no client that is caught already (upgrade notes). A UDP datagram
    for a port without a listener gets no "port unreachable" any more (upgrade notes). Nothing of this ran on a
    lab yet: the lab re-drill is to show it on an appliance VM with the image's own kernel.
- **A changed connection limit of a UDP listener reaches the kernel.** A `connectionLimit` added to, raised on or
  removed from a UDP listener that already served was written to `lvs.conf` on both appliances, the row read
  `ACTIVE`, and the kernel kept the figure of before: keepalived 2.3.x rewrites a member at a reload only when its
  weight changed. On the lab a limit of 5 that was added limited nothing (40 of 40 flows answered), a raise to 50
  left 5, and after its removal 20 of 30 new flows got no answer. The UDP apply now compares the kernel's table
  with the file after the reload and edits what differs with `ipvsadm`, which keeps every flow; and the state read
  compares each member's limit and weight and each listener's algorithm and persistence timeout with the file, so
  a table that differs is pushed again: such an appliance answers UDP with the figure of before, so it reads up
  with "a change is not loaded on it yet" and the row says that a change waits, where a listener the table lacks
  still reads "not served" and puts the appliance down. It is pushed as a change is, the standby first, and not
  while the standby holds a push. Weight, algorithm and persistence were
  checked for the same fault on both images: keepalived writes those itself. Run in real guests of Debian 13 and
  Ubuntu 26.04 on the CI runner's kernel; not run on a lab.
- **A UDP change is live sooner.** On the lab a change was live after 14 to 44 s where the plan aims at 15 s: a UDP
  port added with a TCP port answered after 33 s, changes that followed each other after up to 44 s. Four waits
  are gone. A push of the UDP service writes the files that differ, which for a member change is one where it was
  five. A new member's first check is made at once: it waited a random time of up to the check interval, 4 to 6 s
  measured, and is now in the table 0.6 to 0.8 s after the push that names it; one whose port is closed still
  never enters. On each appliance HAProxy's push and the UDP push are made at once, and the two appliances are
  read at once; the standby still takes every change before the appliance that holds the address. And a Service
  change that arrives while the portal is at the last pass of a converge starts the next converge when that one
  ends, where it waited for the controller's next send. Counted without a lab: guest calls and file writes of a
  change. The times are the lab's to show: HAProxy's own reload, 4 to 5 s on each appliance there, is unchanged.
- **A replace does not move the address to an appliance without UDP members.** The new appliance took the address
  as soon as its UDP service held the listeners, before a member had passed a first check: UDP was lost for 2.4 s
  where TCP was lost for 0.8 s, and one port had no member for 5.2 s. The planned switch now waits until the
  appliance that takes the address holds every UDP member the other one holds, 30 s at most, after which it
  switches all the same and the API log says so. Tested against the fake pair; not run on a lab.
- **A restart of nftables.service does not leave an appliance forwarding without its filter.** `nftables.service`
  empties the whole ruleset, and with it the table that lets an appliance with a UDP listener forward only the
  answers of its own flows; forwarding stayed on until the next pass put the table back. The UDP unit is now part
  of `nftables.service`: it stops before it, forwarding off first, and starts after it with the table loaded
  before forwarding; a reload of `nftables.service` is followed by the unit's own. A start of a stopped
  `nftables.service` by hand is still seen by the next pass only. Run in real guests of both images.
- **A load balancer takes a change while the image's package manager runs.** An appliance answered "busy" to every
  change whenever a process named `apt-get`, `apt` or `dpkg` ran in it, also long after its own packages were
  installed, and Ubuntu's own timers run on every appliance: on the lab 4 to 7 minutes after first boot, for 2 to
  6.5 minutes, and then every day. A tenant's change that was answered 200 was not served for 3 min 28 s, and only
  because the tenant sent it again: the standby had it, the appliance that holds the address did not, nothing
  tried again before the next services sync and nothing was logged. Now an appliance whose packages are installed
  takes the config check, the swap and the reload while apt runs. It waits in one case: dpkg is at work on a
  package the appliance itself runs (`haproxy`, `socat`, `keepalived`, `nftables`), as in an upgrade by
  unattended-upgrades, which ends with the package's own restart of its service. dpkg's own record says when that
  is, and it lasts seconds. The first install waits for the package manager as before
  ([cloud-integration.md](docs/cloud-integration.md), "A change while the package manager runs"). Not run on a lab.
- **A pair that serves is never `DOWN`, and its Services stay ready.** An appliance that served the config its row
  records and could not take a new one read `down`, "it runs an older configuration"; with both appliances in
  that state the row read `DOWN` "No appliance answers" for 29 s on the lab while both served, and every Service
  of the cluster went back to "building". Such an appliance is now `up` with the reason, the row stays `ACTIVE`
  and its message says what waits, on which appliance and why. A cluster's Service that the appliance holding the
  address serves as it is stays ready while that appliance waits for a change of something else; a Service that
  is new or changed is ready when the holder serves it, as before.
- **The standby takes every change first, also when it cannot take one now.** An appliance that was held was
  skipped and the next one pushed, so the appliance that holds the address got a change 21 s before the standby
  on the lab. Now the appliances after a held one wait with it, for 100 s; after that the holder goes ahead, as it
  does beside a standby that is down.
- **A change that waits is tried again by the platform itself.** 20 s after the pass that was held and every 20 s
  for two minutes, for a tenant's pair, a load balancer on one VM and a cluster's pair. Until now it waited for
  the next services sync, which a site without a scheduler does not have.
- **The log shows each push.** One INFO line per push an appliance took (`load balancer: push taken`: balancer,
  VM, `haproxy`, `keepalived`, `udp` or `certificates`, keepalived's role, the place in the order) and one WARN per
  push held or refused, with the class of the reason (`package-manager`, `installing`, `guest-agent`, `refused`).
  The order of a push could only be read from Proxmox's access log. No line holds a config, a key or a digest.
- **`GET_HEALTH` and the row are right after a failover.** For about 70 s after a role change, until the next
  pass, `GET_HEALTH` of a pair answered 409 "the active appliance did not answer" or listed two masters (six of
  six failovers on the lab): it kept the row's role for an appliance it could not read and asked the first listed
  holder. It now reads each appliance's role at the call, lists an appliance that cannot be read as `down`, "it
  cannot be reached", asks the appliance that holds the address now, and corrects the row's roles by the same
  read when they differ. "It cannot be reached" in the row is that read's own mark: a later `GET_HEALTH` that
  reaches the appliance and finds it serving takes it back, so a tenant's own pair does not read it until the next
  services sync after an appliance's reboot. A `GET_HEALTH` that reaches no appliance at all lists them as that
  and leaves the row as it is: one call Proxmox or the guest agents did not answer does not make a pair that
  serves read `DOWN`.
- **sshd stays off on an appliance after a reboot.** The units were disabled, and on the Ubuntu image cloud-init
  starts `ssh.service` at every boot all the same: sshd listened from the reboot until the next HAProxy reload
  (about 18 min on the lab), behind the VM firewall and with no authorized key. `ssh.service` and `ssh.socket` are now
  masked, by the install of a new appliance and by every push on an existing one. The sandbox entry appliance,
  which is pushed by the same command, gets the same.
- **A certificate file is never readable by others, not for a moment.** The guest agent creates a file with mode
  0666, and the chmod came a command later: a new certificate file, which holds the private key, had that mode for
  the time between the two, inside a directory only root can enter. The file is now made empty with mode 0600
  before the agent writes into it, and the agent leaves the mode of a file that exists. One more command through
  the guest agent per push that writes a certificate file, none for a push whose files are in place.
- **A Service's address was listed and billed only after a provider sync, and the cluster page followed a delete
  a whole pass late.** The `FLOATING_IP` row of a public address is what lists it for the tenant and bills it. For
  the addresses of a cluster's Services it was written by the provider sync alone: on the lab 12 addresses were in
  use with 4 rows until a sync ran, and two addresses taken and given back in between never had one (drill R2a-o2
  of v0.11.1-rc.2); on a site with the scheduler the wait was the sync's interval. Now the pass that allocates an
  address writes its row, billed from the allocation's time, and the pass that gives it back archives it, as
  before. The row is the one the sync makes, so a sync after it changes nothing, and a kept address keeps its
  Service's row. Separately, the cluster's row was saved at the end of a pass, after both appliances were pushed:
  the page's kept list and `quota-usage` showed a deleted Service's address as the Service's for 44 s after the
  delete, 40 s after it had stopped answering (drill R2a-o1). Now the row is saved as soon as the pass has decided,
  before its first call to Proxmox: the Services, each one's address, the kept addresses and the ones on their way
  back. A Service that was ready reads ready meanwhile, as it did; what the appliances serve is still said at the
  pass's end. The quota count itself never depended on either. Tests on PostgreSQL through the gateway's own
  handler, with the pass held before its first call to Proxmox; each fails without its fix. Not run on a lab.
- **A credential rotation put a Warning on every Service of the cluster.** The operator's "Enable load balancer
  controller" on a cluster that has a controller replaces its credential: the old token stops at once, and the
  kubelet writes the new one into the controller's Secret volume at its next sync of the pod. The call in between
  was refused, and the controller said `Warning GatewayRefused "authentication failure"` on each Service, ten
  seconds before the Normal Event (drill R2b-o5 of v0.11.1-rc.2), for something no Service's owner can act on. Now
  a refused credential is not said on the Services while it is younger than two minutes: the turn fails and is
  logged, the next one comes after the interval and reads the token again, and with the new token the Services hear
  nothing new. A refusal that lasts (a wrong Secret, a released cluster) is said as before once the two minutes are
  over. Two minutes, not one retry: the kubelet may take a minute and a half, and the lab's ten seconds were the
  short end of that. Test: the controller against the gateway's own handler with the credential replaced under it;
  it fails without the fix. Not run on a lab.
- **A Service annotated to keep its address just before its delete lost the address.** `kubectl annotate ...
  proxcloud.io/lb-keep-address=true` followed at once by `kubectl delete` gave the address back to the pool, twice on
  the lab (0.31 and 0.36 s between the two; drill R2a-2 of v0.11.1-rc.2), and the guide said the opposite. The portal
  read the wish from the last state it had stored, and the cluster's controller takes its turn a second after a
  change: an annotation and a delete within that second were one turn, in which the Service was already left out,
  so the portal never saw the annotation. Now the controller names the Services that are being deleted beside the
  state, each with what its object says at its deletion (the finalizer `proxcloud.io/load-balancer` keeps the object
  readable until then), and lets a Service go only after a request that named it. The portal goes by that word, and
  by its stored state only for a controller that sends none. The other order is right too: an annotation removed
  just before the delete gives the address back, where the stored state would have kept it. The limit of five kept
  addresses stands, and a Service that never had an address keeps none. The field (`gone`) is an addition within
  protocol 2 of `/lb/v1`: a controller of v0.11.0 or of an earlier v0.11.1 build is served as before, and a portal
  that does not know the field (rolled back) refuses it and is sent the request without it in the same turn. Up to
  50 Services being deleted are named per request, outside the state's 64 KiB; more wait for the next turn. Tests:
  the controller against an API server that sends the events in a real one's order, through the gateway's own
  handler, and the portal on PostgreSQL through the same handler; each fails without its half of the fix. Not run
  on a lab.
- **The first appliance of a pair could not serve until the second existed.** A cluster's appliance pair is built
  one appliance per attempt, and an attempt that fails (no room on a node, a failed clone) is tried again five
  minutes later. keepalived was pushed only to an appliance that had a peer, and keepalived's files are what let
  HAProxy bind an address and what hold it: the appliance that was alone refused its HAProxy config ("cannot bind
  socket"), the row read `ERROR` for a pass, the refusal was in no log, and the Services showed an address that
  answered nobody until the second appliance had joined (9 min 14 s after the Services were made on the lab; without
  end where no node has room). The same held for a tenant's pair left with one appliance by a replace whose new
  clone failed. Now an appliance with no peer gets keepalived too and is its own master: it holds the addresses
  and serves TCP, TLS and UDP alone, follows every change of the row, and the row reads `DEGRADED` with no standby
  (a cluster's pair says that the platform builds the second one). Its adverts meanwhile are addressed to
  `127.0.0.1` and reach no other appliance: one a second goes to the tenant network's gateway alone, which
  drops it. When the second appliance comes, the one that served is told of its peer first and keeps the
  addresses, and the new one starts as its standby: no address moves. HAProxy is never pushed before keepalived,
  and a push an appliance refuses is logged at WARN with the balancer and the appliance (the reason stays in the
  row's message). The config of a pair that has both appliances did not change by a byte, so no existing pair is
  pushed because of this. Found by the drill of v0.11.1-rc.1 (Q0b-1). In a real guest of both appliance images
  and in the wire fake; not run on a lab ([cloud-integration.md](docs/cloud-integration.md), "One appliance of a
  pair").
- **A damaged certificate file on a load balancer went unnoticed until HAProxy's next start, which then took every
  listener down.** HAProxy reads a certificate file when it starts or reloads and serves from memory after that.
  A file truncated or deleted on an appliance therefore changed nothing that the platform asked about: the row
  read `ACTIVE`, the same request sent again and a services sync pushed nothing. HAProxy refuses its whole config
  when one file of it cannot be loaded, so the next reboot or restart of the appliance would have left the plain
  listeners down with the TLS ones, for up to the 15 minutes of the next services sync. Now the state read of
  every pass also asks for the certificate files the config names: each must be there, hold the bytes the portal
  would write, and be root's alone (mode 0600 in a directory of mode 0700). One that is not is written again in
  that pass, while HAProxy still serves from memory, at the cost of one reload and no connection; a mode or an
  owner that is wrong is put right the same way, and the pass logs at WARN which file and what was wrong. On a
  pair only the appliance concerned is pushed, the standby first, and an appliance that has a change of config
  still to take is asked after it took it: a certificate new to it is no damage, and a config the standby
  refuses still reaches no other appliance. The read is the command the pass already sent:
  a balancer without a TLS listener runs the command of before and is asked nothing more. What the appliance
  answers of a file is its SHA-256, which the portal compares; nothing of a certificate or a key, and not that
  digest either, is in a command line, a log line, an error or a status text. Found by the drill of v0.11.1-rc.1
  (Q3-1). In a real guest of both appliance images (a file truncated, deleted and opened to all, then a restart
  of HAProxy) and in the wire fake; not run on a lab ([cloud-integration.md](docs/cloud-integration.md), "The
  files are proven on every pass").
- **Every change of a load balancer with TLS wrote every certificate file again.** A write through the guest
  agent took about 0.6 s on the lab and happens inside the tenant's request, so a change of anything (a member, a
  limit) answered after 8.8 s on a balancer with two certificates and after 13.5 s on one with eleven. A push now
  asks the appliance what its certificate directory holds, in the command that makes the directory private, and
  writes only the files that are missing, hold other bytes or are not root's alone: none for a change that
  touches no certificate, the new file alone for a change of certificate. Certificates still reach an appliance
  before the config that names them, and the config's check, swap and roll back are unchanged. Found by the
  drill of v0.11.1-rc.1 (Q3-o2). The calls are counted against the wire fake and in a real guest of both
  appliance images; the time saved was not measured on a lab.
- **A project could not see which public addresses its quota counts.** The `floatingIps` quota counts every
  public address of a project: a server's public NIC, a load balancer's address, each Service address of a
  Kubernetes cluster and each address a cluster keeps. `GET /api/v1/project/{id}/quota-usage` gave the limit with
  no figure in use, and no list showed the addresses of balancers and Services, so a project at its limit read
  "Quota exceeded for floatingIps: 5 in use" and could not tell which five. The quota read now answers `network`:
  `floatingIps` and `networks` as used and limit, by the count the quota check itself uses, and `addresses`, one
  row per address with what holds it (the server, the balancer, the Service with its cluster, a kept address, one
  being given back). The tenant dashboard's **Quota & usage** card shows a "Public addresses" tile and the list
  under it. Found by the drill of v0.11.1-rc.1 (Q0b-2) ([api.md](docs/api.md), "Quota usage").
- **Docs: a certificate file damaged on an appliance does not give a refused config.** The integration notes
  said that the config's hash covers the certificates and that the applied-state check needs no term for them,
  and the review list of TLS expected a file damaged by hand to end in a refused config with the last good one
  kept. Neither holds. The hash covers the files' names, not their bytes; and a push brings every file its config
  names in place before the config is checked, so a change made after the damage is applied and the file is whole
  again (drill of v0.11.1-rc.1, Q3-2: shown twice). A refused config needs a new config that names a file HAProxy
  cannot load, which the container test holds. The notes now tell the two cases apart and say what ran on a lab
  and what did not ([cloud-integration.md](docs/cloud-integration.md), "A refused config and a damaged file are
  two things").
- **Guides: only the last `X-Forwarded-For` value is the load balancer's.** An `http` or `https` listener adds the
  client's address to `X-Forwarded-For` after whatever the client sent in that header, as HAProxy's `option
  forwardfor` and other balancers do. The API reference and the tenant guide read as if a server could take the
  header as it comes: a server that reads the first value is told whatever the client wrote. They now say that
  the last value is the one the balancer wrote, that earlier values come from the client and prove nothing, and
  that the PROXY protocol is the way to get an address a client cannot forge. The behaviour did not change, and
  the real-guest test now holds it. Found by the drill of v0.11.1-rc.1 (Q3-o1).
- **Guides: with the PROXY protocol on, health checks carry the header too.** The API reference, the tenant guide
  and the integration notes described `proxyProtocol` for the traffic alone. HAProxy sends the PROXY header on a
  health check as well when the check goes to the member's own port, and sends none on a check with a port of its
  own (`healthCheck.port`), the same port number included. So a server that accepts connections only with the
  header is checked correctly as long as the check has no port of its own, and a health endpoint on its own port
  must answer a plain connection. The texts say so now; nothing in the config changed. Seen by the drill of
  v0.11.1-rc.1 (Q3-o3) on HAProxy 3.2.25 and, for both cases, on 3.0.11 in a container.
- **`proxcloud.io/lb-internal` with a value that is no boolean gave the Service a public address.** A value such as
  `"yes"` was named in `InvalidAnnotation` and the Service was served, so the one annotation that guards an address
  failed open over a typing mistake. It fails closed now: a Service gets a public address only when the annotation
  is absent or false (`false`, `0`, `f`). Every other value refuses it: `"true"` with `InternalNotSupported` as
  before, a value that is no boolean (also an empty one, or one too long to read) with `InvalidAnnotation`, and the
  Event names the value. The cluster's controller follows the same rule for a portal that reads no annotations (one
  rolled back to `v0.11.0`): such a Service is not sent to it. Found in the review of #435.
- **An empty `proxcloud.io/lb-tls-secret: ""` opened the Service's ports without TLS, and said nothing.** An
  annotation that is there and empty, as a template that filled nothing in leaves it, was read as "no TLS". It
  now asks for TLS like any other value and is held like a Secret that cannot be used: a Service that was never
  served gets no address, one that is served stays exactly as it was, and one Warning Event `InvalidCertificate`
  says that the annotation is empty. Removing the annotation is still the way to a plain port. The controller does
  not send such a Service to a portal of protocol 1 either. Found in the review of #438.
- **The second of two quick changes to a cluster's Services waited about 11 s for the controller's next send.** A
  pass over a cluster's balancer applies the state it read at its start; a state the controller stored while that
  pass was at work started no pass of its own and was applied only when the controller sent it again. The pass
  that ran now looks at the stored state once more before it ends and applies it when it changed, so the second of
  two changes 2 to 3 s apart no longer waits for that send. The layer 4 plan asks for 15 s; the time on a pair of
  appliances is not measured yet and is a drill item. It looks once: a Service that keeps
  changing does not keep a pass running, and one pass at a time per cluster still holds. Found in the review of
  #439.
- **The console took "Down after" 1 on a UDP pool, which the API refuses.** The save came back with the API's
  reason. The load balancer page now holds the API's rule itself: for a UDP pool the field starts at 2, a 1 shows
  why under the pool ("must be 2 to 10 for a UDP pool: the load balancer tries a failed check again at least
  once"), and "Save configuration" waits until it is corrected. Found in the review of #433.
- **Sandboxes worth less than half a cent each were invoiced 0.00.** Every sandbox is a bill item of its own, and a
  sent bill rounds each item to two decimals, so 10,000 one-minute sandboxes at 0.0002 accrued 2.00 and were
  invoiced nothing. At the send, what the sandbox items that round to 0.00 accrued is now summed per project and
  added as one item, "Sandbox runs below 0.005 each (n)", rounded once (`pricing.ScaleUpItems`). The items
  themselves stay on the bill at 0.00 with their minutes. A bill without such an item is sent as before; it applies
  to bills sent from now on and never to one already sent.

- **The sandbox network guard accepts Cilium's CNI hand-over per node** (`cmd/sandbox-netguard`,
  `internal/ciliumcni`, `internal/cloud/sandbox/netguard.go`, `deploy/sandbox-cluster/telemetry.yaml`). `node-setup`'s
  `sandbox-netguard -install` and the network-guard gate read only the cluster-wide `cilium-config`, so on a cluster
  where Cilium keeps the CNI config everywhere except on the sandbox nodes (a `CiliumNodeConfig` selecting them, the
  platform's shared mode) node-setup would never become Ready and the gate never pass. Both now compute each node's
  Cilium config the way the agent does (the ConfigMap, then every `CiliumNodeConfig` of `kube-system` whose selector
  matches the node, in name order) and require `custom-cni-conf` `"true"` and an empty `write-cni-conf-when-ready`:
  a `CiliumNodeConfig` must set that key to `""`, because left out the ConfigMap's path stays and Cilium rewrites the
  conflist without the netguard chain. The gate checks it per schedulable node, beside the heartbeat. netguard's
  service account may now get Nodes and get/list `ciliumnodeconfigs`; the portal's sandbox identity gets the same
  read through the existing `kube-system` Role. With the older RBAC (403) or no CRD (404) the ConfigMap alone decides,
  as before. Ships in the next sandbox runtime image built by CI from main. Not run on a lab.
- **No request is lost at a reload or when a node changes** (`internal/cloud/pve/lb.go`,
  `internal/platform/project/cloud_lb_cluster_pve.go`, `cmd/lb-controller`). `v0.11.0` shipped with this as a known
  issue (drill N3-8): a scale-up lost 22 of 44 requests in 4.4 s, a scale-down 52 in 10.3 s, and a Service with
  `externalTrafficPolicy: Local` lost 11 to 12 requests in 2.2 to 2.4 s at every change of any other Service of its
  cluster. Three causes, each fixed:
  - *A reload started every member as up.* The config now names a state file (`server-state-file
    /var/lib/haproxy/server-state` in `global`, `load-server-state-from-file global` in `defaults`), and the apply
    writes `show servers state` to it just before the reload, so a member that was down is down from the first
    moment of the new process. HAProxy finds a member again by the names of its backend and of the server
    (`src/server_state.c`: the key is `be_name srv_name`; the address in the file is not applied to a server the
    config gives an address). Backends were named by position (`p0`, `p1`): a port added to one Service renamed the
    backends of every Service after it, which is the very change the drill measured. A backend is now named by its
    pool (`p-<pool name>`), and a cluster's pool by its Service and node port. A HAProxy that does not answer has no
    state to save, and the file is removed. `init-state down` is not used: it needs HAProxy 3.1, and Debian 13 ships
    3.0.
  - *The controller sent a node before it was Ready.* It sends a node from its first turn Ready on, and then for as
    long as the node exists, Ready or not. Kubernetes' own service controller hands a balancer every node that is
    not deleted, not excluded and not tainted for removal, Ready or not (`k8s.io/cloud-provider`
    `controllers/service/controller.go`, `stableNodeSetPredicates`, v0.34.0; KEP-3458), so that a flapping Ready
    condition does not reconfigure every balancer. The second half is kept: a member that goes NotReady stays, and
    its health check decides. The first half is not, because HAProxy before 3.1 starts a member new to its config as
    up. The memory of what was sent is the controller process's: a controller that starts takes every node it finds
    for a member, Ready or not, so its restart takes no member out of a balancer (a node that joins in that very
    turn is sent before it is Ready).
  - *A member that just went away kept its share of the connections until two checks had failed.* `option redispatch
    1` with `retries 3` and `timeout connect 2s` (it was 5 s): a connection a member does not take is tried on
    another member at once. A member that stopped costs a client the connect timeout, 2 seconds, not its answer.
    Only the connect is tried again, never a request that was sent. An open connection to a VM that stops is still
    cut, as with any balancer.

  What is left: a member new to a config still starts as up, so a node that is Ready and does not forward yet (or
  has no local endpoint of a `Local` Service) gets connections until its first check fails, each of which waits the
  connect timeout and is then answered by another member. Not run on a lab: the figures above are those of
  `v0.11.0`, and the fix is shown by tests, on real HAProxy 3.0 and 3.2 in the container test.
- **A Service's address is written when it is served** (`cmd/lb-controller`,
  `internal/platform/project/cloud_lb_cluster_pve.go`; drill N3-7). The controller wrote
  `status.loadBalancer.ingress` as soon as the portal had allocated an address, 3 min 12 s before the first Service
  of a cluster answered: `helm --wait`, Argo CD's health and external-dns took the Service for reachable. It writes
  it when the portal answers `ready`, keeps an address already written while a later change is building, and
  removes it when the portal names none. `ready` now means that the appliance holding the address serves the
  Service's config: one appliance was enough, and every config reaches the standby first. Kubernetes' own service
  controller does the same: it patches the status with what the provider's `EnsureLoadBalancer` returned, and a
  provider returns it once its balancer is provisioned (`k8s.io/cloud-provider`
  `controllers/service/controller.go`, `syncLoadBalancerIfNeeded`, v0.34.0).
- **A change the API was applying when it stopped goes live a minute later, not three** (`cloud_lb_pve.go`,
  `cloud_lb_cluster_pve.go`, `cloud_lb_pair_pve.go`; drill N3-9: 190 to 209 s). The balancer's own lock
  (`lb-<externalId>`) was taken for three minutes whatever happened to its holder. It is a lease of one minute that
  its holder extends while it works (`ShedLock.Hold`, as the cluster's converge lock since `v0.11.0`), at all seven
  places that take it. The locks are not released when the API is told to stop: the bound is the lease.
- **A paused VM is a known state** (`internal/cloud/pve/vm_inventory.go`; drill N3-5). Proxmox lists a paused VM as
  `paused`, and the VM inventory accepted `running` and `stopped` only: the project's SERVER sync was held back, and
  with it the provider's seed sweep and sandbox sweep, for as long as the pause lasted, which is every VM of a
  project an operator suspends. The row reads `PAUSED`, and the project stays trusted. The check is the one of
  `v0.10.2`; the fakes listed a paused VM as `running`, and now list it as Proxmox does.
- **A pair with a paused appliance names the pause** (`cloud_lb_pair_pve.go`; drill N3-6). The row of a pair whose
  former master was paused read "Both appliances hold the address: they cannot hear each other", from what the
  paused one held before the pause. A paused appliance holds nothing, and the row reads "One appliance carries the
  traffic, with no standby (appliance N: its VM is paused). A paused VM serves again when it is resumed in Proxmox;
  Replace appliances builds a new one." The platform still does not resume a paused appliance.
- **An appliance whose HAProxy has no listeners gives up the address** (`internal/cloud/pve/lbpair.go`; drill N3-3:
  it kept the address for 25 s, until the next pass). keepalived's track script asked only whether HAProxy's master
  has a worker. It also asks whether the first address the config binds is listened on (`ss`, one bind, no process
  names: it runs every second), except for the ten seconds after an apply put a file in place, while HAProxy loads
  it. Only a hand edit makes that state.
- **Three messages** (drills N3-1, N3-2, N3-4). During Replace appliances the row showed the appliance that carries
  the traffic with its role of before the switch ("BACKUP", no address) for as long as the build took: it is read
  again once the old one is out. `GET_HEALTH` of a balancer whose HAProxy is stopped answered 409 "The load
  balancer's VM did not answer": it says that HAProxy is not running and that the next pass starts it. A pair whose
  address holder runs the config of before a change it could not take yet read "One appliance carries the traffic,
  with no standby", with the healthy standby as the one listed up: it reads "The appliance that holds the address is
  the one that is not in order (...); the other one is ready, as its standby".
- **A pair with a UDP listener did not know which appliance holds its address.** The state read's script keeps
  its answer to "holds the address" in the shell variable `v`, and the check of the UDP service, which the same
  script calls, loops over the virtual servers with a variable of the same name: a shell function's variables are
  its caller's. With any UDP listener the read said "holds nothing" for both appliances. The row read `DEGRADED`
  "Neither appliance holds the address yet" for as long as the listener stood, every Service of a cluster made or
  changed afterwards stayed pending in its status although it answered, a pass's pushes went to the appliances in
  the row's order, which may be the holder first, and `GET_HEALTH` found no appliance to ask and answered no
  member. Nothing was wrong on the appliances: the addresses, HAProxy and the UDP listeners were served throughout.
  Now every function a script of the product's defines for another script to call runs in a subshell (the checks
  of HAProxy's listeners, of the UDP service and of HAProxy's worker), and the read calls them in one. A pass also
  orders its pushes by what the appliances say at that pass, not by what the row said at the pass before. **A pair
  that reads `DEGRADED` by this heals at the first pass after the upgrade**, with nothing to do: the script of the
  read is sent with every read, so the first pass names the holder, the row is `ACTIVE` and its Services are
  ready, and the controller writes their address into their status at its next call after that pass. In
  v0.11.1-rc.1 only: v0.11.0 has no UDP listener. Found by the drill of v0.11.1-rc.1 (Q1-2), and by the container
  test of the first-appliance fix above, which reads the state of an appliance that serves UDP. The
  tests that let it through answered the read from what it should say; now the script's text is run, in dash and
  bash, with and without UDP listeners (`TestVRRPStateScriptWithAndWithoutUDP`), the wire fake of every apply test
  runs it too, and the real-guest test reads the holder, the push order and the members' health while a UDP
  listener is served. Not run on a lab ([cloud-integration.md](docs/cloud-integration.md), "What the drill of
  v0.11.1-rc.1 found").
- **Both consoles said that a cluster's update changes no Service that is served.** It does, in two cases the
  upgrade notes name: a Service whose `proxcloud.io/lb-internal` is not `"false"` loses its public address when its
  cluster gets the controller of this release, and one whose `proxcloud.io/lb-tls-secret` names no usable Secret
  stays as it is served and takes no change until the Secret is right. The hint on the tenant's cluster page, the
  one in the operator's load balancer dialog and the tenant guide now say so, and that every other Service that is
  served stays as it is. Text only. Found by the drill of v0.11.1-rc.1 (Q1-1), where two such Services lost their
  address 20 s after the update.
- **A Service that named an address its cluster does not keep got two Warnings.** `LoadBalancerIPIgnored` said
  "is not an address this cluster keeps" until the Service had its address of the pool, and from then on "the
  Service has ...: an address is taken when the Service gets its address, not after", which reads as if the
  address had been named too late. It is one Warning now, in the same words before and after, and it names no
  address the Service has. The second text stays for the case it is true of: an address the cluster does keep,
  named by a Service that already has another. Found by the drill of v0.11.1-rc.1 (Q1-3).
- **The log said "an appliance's keepalived config is not the one its row records" at every planned change.** A
  pass compared the file on the appliance with the config it was about to push, so each Service that came or went
  on a cluster's pair, and each address a tenant's pair gained or lost, was logged at WARN as a config changed
  behind the product (16 lines in 18 minutes on the lab). The file is now compared with the config the row records
  (a pair's row keeps the hash of `keepalived.conf` itself, `keepalivedConfHash`, beside the one of the config with
  its support files): the line means a hand edit or a lost file, and only that, and such a config is pushed again as
  before. A row of before this release needs nothing: its hash is filled in by its next pass. Log only: no push
  was added or left out. Found by the drill of v0.11.1-rc.1 (Q1-4).
- **A reload of HAProxy could cost the appliance that holds the address a failed run of keepalived's track
  script.** At a reload HAProxy's master replaces itself and its socket refuses connections for a moment (about
  10 ms with HAProxy 3.2 and 70 ms with 3.0 on an idle config). The track script asked it once, so a push that met
  that moment was one failed run on the master, and after two in a row keepalived gives the addresses up: one
  more failed run would have moved them at a push. The script now asks up to four times, 0.2 s apart: a reload that
  is over within 0.6 s is no failed run. An HAProxy that is really gone is seen at most 0.6 s later than before,
  less than keepalived's interval of one second. Found by the drill of v0.11.1-rc.1 (Q1-o1). In a real guest of
  both appliance images: 20 reloads in a row on the appliance that holds the address, with the script also run
  without a pause meanwhile (over 1000 runs), no failed run and no state change, and a stopped HAProxy still gives
  the address up. Not run on a lab.
- **A file of an appliance could be read empty or half written during a push.** keepalived's support files (its
  track and notify scripts, the sysctl file, the unit drop-in) and the UDP service's (the nftables table, its
  sysctls, its network script) were written onto their place through the guest agent, which empties a file and
  then fills it and creates it with mode 0666: the track script, which keepalived runs every second, was empty
  for a moment at every push (seen twice on the lab by a read every 4 s) and writable by anyone until the apply set
  its mode. Every file of a push is now written beside its place, as `<path>.new`, given its owner and mode there,
  and moved in by a rename: a reader sees the file of before or the new one. keepalived's notify script writes the
  state the portal reads the same way. HAProxy's config and the certificate files already went in so. Found by
  the drill of v0.11.1-rc.1 (Q1-o2). The fakes and the real-guest tests now refuse any write of the agent's that
  names a file's place.

### Known issues

The release notes (`docs/releases/v0.11.1.md`) hold this list with its figures. It is the list of `v0.11.1-rc.5`,
whose code this is, less what the drill of that candidate settled.

- **UDP.** Only on a pair: a balancer on one VM answers `400`. A UDP server never sees the client's address (no PROXY
  protocol for UDP). The default check sends a datagram and a ping: a server that answers no ping from the appliance
  is taken out, and without `requireReply` a closed port on a running server is not seen while the port has traffic.
  The connection limit counts per server. After a failover, and after every change of the configuration, source
  persistence starts afresh. `leastconn` counts the flows of the last five minutes.
- **A UDP port without a listener is silent** on a pair with a UDP listener: no ICMP "port unreachable", also for a
  listener whose pool has no server at all. A listener whose servers are all down answers it as before.
- **A stop and start of the UDP service leaves UDP unanswered on that appliance for its seconds**: 1.1 to 1.6 s at a
  restart of `proxcloud-lvs.service` or of `nftables.service`, 3.2 to 3.8 s when the service's keepalived is killed,
  and 27 to 36 s when a balancer's last UDP listener is removed and one is added again (the lab, under rc.5).
  Afterwards every client is answered again.
- **The lab evidence of the fix of #477 has limits.** The lab's appliances have one vCPU, where the candidate before
  the fix left a client unanswered almost only at 500 datagrams a second (27 of 448 clients, against 2 of 688 at 50
  and 1 of 688 at 5): the lab tells the fix from that candidate at 500 datagrams a second and not at 5 or 50, where
  the evidence is the runs in real guests on a test host. Debian 13 appliances, and appliances with more than one
  vCPU, did not run on a lab.
- **A flush that fails is said in the journal of `proxcloud-lvs.service` alone**, at info priority: the balancer reads
  `ACTIVE`, and a client that sent through that stop and start can stay unanswered for as long as it sends. An image
  of the operator's own without `perl` or without the kernel module `nf_conntrack_netlink` ends there. Each start of
  the UDP service that finds no table empties the appliance's whole connection table: an answer on its way to a UDP
  question of the appliance's own is lost once. More than five starts within 10 s meet systemd's start limit.
- **Under a candidate.** A UDP client that a candidate left unanswered stays so through the upgrade, until it is
  silent for more than 30 s or sends from another source port. The first datagrams after a listener is served can be
  lost until its first member passes its check. A ruleset of the operator's own that tracks connections while the
  product's table is away can still catch a client until the next start of the UDP service, and a masquerade or
  `notrack` rule taken out of the loaded table by hand is not noticed.
- **Where private keys rest.** In the database, encrypted with the portal's encryption key; in `backups/`, where the
  dump and a copy of `.env` with that key lie side by side; and in clear on each appliance's disk (root only), so in
  any Proxmox backup, snapshot, clone or replica of an appliance VM. A Service's Secret rests the same way. Whoever
  may write a Service in a namespace can have any TLS Secret of that namespace served.
- **TLS.** No update of a certificate (upload, name, delete). The policy is fixed: no TLS 1.1, no RSA key exchange, no
  CBC suite, no TLS to the servers. A certificate with a signature the appliance's HAProxy refuses is taken by the
  upload and refused at the push. While a Secret is unusable its whole Service is frozen as it was served. Only the
  last `X-Forwarded-For` value is the balancer's.
- **Annotations.** Only `proxcloud.io/lb-*` is read: a Service that asks for an internal balancer with another cloud's
  annotation alone gets a public address. `proxcloud.io/lb-proxy-protocol: "true"` is version 2.
- **Existing clusters.** A cluster is not upgraded for you and keeps protocol 1 until its chart is moved; on the
  `v0.11.0` controller a Service that adds `proxcloud.io/lb-tls-secret` is served plain, with no Event. Chart 0.1.19
  with a `v0.11.0` controller image does not run (`--watch=true`). A cluster on the controller of `v0.11.1-rc.1` or
  `v0.11.1-rc.2` still gives back an address annotated within a second of its delete and still gets `GatewayRefused`
  at a rotation, until "Apply settings".
- **A portal rolled back to `v0.11.0`** takes the address of a TLS Service and drops the list of kept addresses, which
  then stay allocated, billed and unlisted until the cluster is deleted.
- **The gateway.** A cluster's state over 64 KiB is refused as a whole (`TooLarge`). No rate limit per cluster: a
  Service that flaps causes about one call a second. With more than 50 Services deleted at once, the address of a
  Service beyond the first 50 is decided from the stored state in that turn. A real credential failure is said on the
  Services two minutes late. With two API replicas, a Service change that arrives during a converge on the other
  replica waits for the controller's next send.
- **A change still waits in two cases**: while dpkg is at work on a package the appliance itself runs, and during the
  first install of a new appliance. The platform's own retries are six, 20 s apart, and the balancer says "the
  platform tries again within half a minute" also when they are over: on a site without a scheduler nothing tries
  again until the next change. A package upgrade that restarts HAProxy in the moment of a change can make a good
  config read `ERROR` until the next pass; `needrestart` during a change is not handled.
- **After a failover**, an appliance the product had to start reads `down` "its VM is stopped; starting it", and the
  balancer `DEGRADED`, until the first health read after it is back or the next pass. An appliance that was down
  through a change stays one change behind until the next services sync or the next change. In a pass, an appliance
  whose state cannot be read twice is still marked down.
- **Pairs.** An appliance behind a held standby still gets a new config first when its own HAProxy is not serving or
  one of its certificate files is faulty; a change made while an appliance joins or is replaced reaches the holder
  first. When a replace's wait for UDP members ends at 30 s the address moves all the same. A lone appliance sends one
  VRRP advert a second to its network's gateway, and one whose public network was taken from the project stays
  `BUILD`. A reload that keeps HAProxy's master socket closed for more than 0.6 s is one failed run of keepalived's
  track script; two in a row move the addresses. A support file of keepalived edited by hand is not seen by a pass. An
  existing appliance runs sshd after a reboot until its next push, on the Ubuntu image, behind its firewall.
- **Timings on the lab.** A change is live in 16 to 33 s (one TCP port 16 to 20 s, one UDP port 19 to 23 s, both
  together 26 to 30 s; changes 3 s apart 50 to 55 s). A planned failover loses 0.7 to 0.8 s, a dead master 3.2 to 3.6
  s, a replace 0.7 s of TCP and 0.6 to 1.0 s of UDP. A new certificate is served 15.8 to 21 s after its change. A new
  cluster's first answer came 8 min 42 s after its Services.
- **Rows.** A services sync that lists the addresses in the moment before a pass takes one can archive the row the
  pass just wrote; the next pass or sync writes it again, from the allocation time. A Service can be refused
  `QuotaExceeded` for a minute or two after other Services gave their addresses back. A deleted cluster's worker stays
  in the tenant's lists until the next services sync. With the scheduler off, `DELETE /project/{id}` answers `200`
  `SCHEDULED_FOR_DELETION` though nothing will delete it.
- **Still true from 0.11.0.** A member new to a config starts as up. One pair per cluster, five public addresses by
  default, in-cluster clients do not pass the pair, one node is not node HA. The other known issues of 0.11.0 still
  hold, except "no UDP listeners and no TLS termination" and the requests lost at a reload or a node change, which
  this release answers by its code.

## [0.11.1-rc.5] - 2026-11-25

The fifth release candidate of 0.11.1, published as a prerelease and never as `latest`. It is not the release:
`v0.11.0` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.1 above: what `v0.11.1-rc.4` held and the one pull request merged
since (#477). One more pull request merged since changes a unit test and nothing in the product (#479): the test of
a pair's push order no longer asks for an order between the two pushes of one appliance. The drill of `v0.11.1-rc.4`
ran every part of its plan and found no defect: the upgrade from rc.3 did what its notes said, and the two fixes of
rc.4 held in every case it tried. This candidate exists because CI on the release pull request of v0.11.1 then found
a defect that all four candidates have, in a case the drill had tried once: a UDP client that keeps sending while
the UDP service of an appliance stops and starts can stay unanswered for as long as it sends. Nothing of its fix had
run on a lab when it was tagged. The entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.11.1-rc.5`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.11.1-rc.1`, `0.11.1-rc.2`, `0.11.1-rc.3` and `0.11.1-rc.4`: the files differ in the release line alone. No
  chart of the product differs from its newest tag (`git diff proxmox-kamaji-cluster-v0.1.19 --
  deploy/charts/proxmox-kamaji-cluster`, `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`, `git
  diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and `git diff proxcloud-sandbox-v0.1.0 --
  deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed. It is a release BOM, and the release
  gate passes on it and on the twenty-one previous BOMs.
- **No component of the sandbox feature is in this BOM** (`TestSandboxIsDark`). The sandbox code is in the build and
  cannot be turned on, as in `v0.11.1-rc.4`.
- **The plan from each previous BOM** (`PlanFor`). From 0.11.1-rc.4, 0.11.1-rc.3, 0.11.1-rc.2 and 0.11.1-rc.1 one
  change, the release, record only, allowed, with no part, no seed row and no machine replaced; phrase `upgrade
  <name> to 0.11.1-rc.5`. From 0.11.0, 0.11.0-rc.1 and 0.11.0-rc.2 two changes, the release and
  `tenantCharts.kubernetes` 0.1.17 to 0.1.19, both record only, allowed. From 0.10.2 and 0.10.2-rc.1 the same two
  rows (the chart from 0.1.16); four from 0.10.1 and 0.10.1-rc.2 (those and `cert-manager` on the seed and on the
  platform cluster); five from 0.10.1-rc.1 (the tenant chart from 0.1.15, and also `pc-platform` 0.1.15 to 0.1.16);
  and a refusal by the pair gate from the nine BOMs on Kubernetes 1.35.10. This BOM carries no unfinished
  replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What the portal's upgrade does to the balancers, from `v0.11.1-rc.4`.** See the first upgrade note above. A pair
  with a UDP listener gets `/usr/local/sbin/proxcloud-lvs-net` (the script of its UDP service) and one reload of
  `proxcloud-lvs.service` per appliance, the standby first; the other five files of the UDP service are the same to
  the byte, the unit is not restarted and systemd is not reloaded. The reload finds the nftables table and empties
  nothing. Every other balancer gets nothing. The only code outside tests that changed since the tag is
  `internal/cloud/pve/lblvs.go`, and in it that script alone; no file under `internal/cloud/pve/testdata` changed,
  and one was added, the script of the tag. Read again from the code on this commit.
- **A UDP client that is unanswered before the upgrade stays so** until it is silent for more than 30 s or sends
  from another source port. The upgrade's reload frees no one.
- **From `v0.11.1-rc.3`** that and `lvs.nft`, in one push with one reload of the UDP service per appliance; the
  operator's log has one WARN line per appliance for it, "load balancer: an appliance's UDP table is not as its
  configuration has it; pushing it again", which is the state read finding the table of rc.3 (seen on the lab at the
  upgrade from rc.3 to rc.4). **From `v0.11.1-rc.2`** also `lvs.conf` and the drop-in of `proxcloud-lvs.service`,
  with one `daemon-reload`. **From `v0.11.1-rc.1`** also keepalived's five files once per appliance of a pair, with
  one keepalived reload. **From `v0.11.0`** the one push of every balancer that the first candidate's upgrade made;
  a balancer of `v0.11.0` has no UDP listener.
- **Billing at the upgrade from `v0.11.0`, `v0.11.1-rc.1` and `v0.11.1-rc.2`** is what the section of `0.11.1-rc.3`
  below says. A site that runs `v0.11.1-rc.3` or `v0.11.1-rc.4` has these rows.
- **Nothing the controller does changed since `v0.11.1-rc.3`.** A cluster that is on chart 0.1.19 under rc.1 or rc.2
  still needs "Apply settings" or "Enable load balancer controller" again for the controller's part of #452.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.0`.

### Verified on a lab

Nothing on this candidate, and nothing of its fix. On one lab (Ubuntu 26.04 appliances, all on one node, the
scheduler off), by candidate:

- On `v0.11.1-rc.1`, on `v0.11.1-rc.2` and on `v0.11.1-rc.3`, not run again as a whole: what the sections of
  `0.11.1-rc.4` and `0.11.1-rc.3` below list.
- On `v0.11.1-rc.4` (2026-11-24, UTC), the upgrade from `v0.11.1-rc.3`: six client loops, four of them UDP from one
  socket each, 35710 requests in 15 minutes, 0 failed; a pair with a UDP listener got `lvs.nft` alone and one reload
  of the UDP service, the standby first by 7 to 8 s, with no HAProxy reload and no VRRP transition on four
  appliances; none of 4464 datagrams lost on each of four running one-socket flows when the table was replaced; the
  platform run of one record-only row, 8.2 s, 19 VMs kept their process.
- On `v0.11.1-rc.4`, its two fixes: a UDP listener added while its client sends was answered within 0.02 s of the
  first new client (0 of 962 under rc.3 on the same lab); a listener removed beside the pair's other UDP listeners
  left 440 datagrams unanswered while it was gone and was answered 0.06 s after it was served again; a UDP Service
  made again on its kept address, and a UDP port added to a Service, were answered within 0.18 s of the first new
  client; a failover with one-socket flows running lost 0.8 s, a replace 0.7 s of TCP and 0.8 s of UDP; after a
  failover the first `GET_HEALTH` that reached the started appliance serving answered up and wrote the row `ACTIVE`.
- On `v0.11.1-rc.4`, one try that showed less than its notes said: a restart of `nftables.service` on the appliance
  that holds the address, where three one-socket flows lost 1.6 s and were then answered. The notes of rc.4 said
  that such a client is answered once the members are back. That is not what rc.4 does: in real guests of both
  images such a restart left about 1 client in 8 unanswered at 5 datagrams a second and 3 in 4 at 50 (14 and 13 of
  100 clients, 72 and 78 of 100), for as long as it kept sending. It is the defect this candidate fixes. The section
  of `0.11.1-rc.4` below and the notes of that candidate stay as they were published.

### Not verified

- Everything of this candidate on a lab, and its fix: the flush on an appliance VM with the image's own kernel (the
  tests ran each image's userland on the kernel of a test host and of the CI runner, where `nf_conntrack_netlink`
  was loaded already, so neither the kernel loading it at the first message nor the script's `modprobe` was
  exercised, and Debian 13's kernel 6.12 never ran the flush); the events on a real pair with a one-socket client
  that never stops (the last UDP listener removed and one added again, the service's keepalived killed, a restart
  and a reload of `nftables.service`); a reboot of the appliance that holds the address and a real failover; the
  appliance's own DNS, time sync and apt after such a start; and a client caught before the upgrade, that it stays
  caught through the reload and is freed by the next stop and start.
- The upgrade to this candidate from any earlier build, and any move to the BOM `0.11.1-rc.5` (plans read, not run;
  the lab moved to `0.11.1-rc.4`).
- What the audit of the rc.4 drill lists: a balancer without a UDP listener at the upgrade from rc.3, a reload of
  `nftables.service`, a listener whose servers are all down, a first datagram for a port without a listener at the
  appliance (the node's firewall dropped it first), the health fix's one exception, a table changed by hand, every
  member out and back for a one-socket flow, and what the section of `0.11.1-rc.4` below takes from the audit of the
  rc.3 drill, Debian 13 appliances on a lab among it.
- The candidate's images and its tag's workflows: they do not exist before the tag.

### Known issues

- Those of `v0.11.1-rc.4`, `v0.11.1-rc.3`, `v0.11.1-rc.2` and `v0.11.1-rc.1`, below, but the ones this candidate
  fixes.
- The upgrade frees no UDP client, and until a pair's first pass after the upgrade it serves as rc.4 did.
- A flush that fails is said in the journal of `proxcloud-lvs.service` alone, at info priority: the state read still
  says UDP is served, and the appliance serves as rc.4 did. An image of the operator's own without `perl` or without
  the kernel module `nf_conntrack_netlink` ends there too.
- Each start of the UDP service that finds no table empties the appliance's whole connection table: an answer on its
  way to a UDP question of the appliance's own is lost once.
- A stop and start of the UDP service still leaves UDP unanswered on that appliance for a second or two, and more
  than five starts within 10 s meet systemd's start limit.
- The times of a change, of a replace and of a failover were measured on `v0.11.1-rc.3` and `v0.11.1-rc.4`; nothing
  was measured on this candidate.
- The full list is in `docs/releases/v0.11.1-rc.5.md`.

## [0.11.1-rc.4] - 2026-11-25

The fourth release candidate of 0.11.1, published as a prerelease and never as `latest`. It is not the release:
`v0.11.0` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.1 above: what `v0.11.1-rc.3` held and the one pull request its lab
drill asked for (#473). The drill of `v0.11.1-rc.3` ran every part of its plan and found no regression against
`v0.11.0` or `v0.11.1-rc.2`: the upgrade from rc.2 did what its notes said, and the three fixes of rc.3 held. This
candidate exists so that one of its findings does not reach the release (a UDP client that was already sending
when the listener for its port was added got no answer for as long as it kept sending); it carries one smaller fix
of the same drill too (an appliance the platform started after a failover reads up once it serves again). Nothing
of the two had run on a lab when it was tagged. The entries above are not repeated here. Not for a production
site.

### Changed

- **The platform BOM's release is `0.11.1-rc.4`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.11.1-rc.1`, `0.11.1-rc.2` and `0.11.1-rc.3`: the files differ in the release line alone. No chart of the
  product differs from its newest tag
  (`git diff proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`,
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and
  `git diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed.
  It is a release BOM, and the release gate passes on it and on the twenty previous BOMs.
- **No component of the sandbox feature is in this BOM** (`TestSandboxIsDark`). The sandbox code is in the build and
  cannot be turned on, as in `v0.11.1-rc.3`.
- **The plan from each previous BOM** (`PlanFor`). From 0.11.1-rc.3, 0.11.1-rc.2 and 0.11.1-rc.1 one change, the
  release, record only, allowed, with no part, no seed row and no machine replaced; phrase
  `upgrade <name> to 0.11.1-rc.4`. From 0.11.0, 0.11.0-rc.1 and 0.11.0-rc.2 two changes, the release and
  `tenantCharts.kubernetes` 0.1.17 to 0.1.19, both record only, allowed. From 0.10.2 and 0.10.2-rc.1 the same two
  rows (the chart from 0.1.16); four from 0.10.1 and 0.10.1-rc.2 (those and `cert-manager` on the seed and on the
  platform cluster); five from 0.10.1-rc.1 (the tenant chart from 0.1.15, and also `pc-platform` 0.1.15 to 0.1.16);
  and a refusal by the pair gate from the nine BOMs on Kubernetes 1.35.10. This BOM carries no unfinished
  replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What the portal's upgrade does to the balancers, from `v0.11.1-rc.3`.** See the first two upgrade notes above.
  A pair with a UDP listener gets `/etc/proxcloud-lb/lvs.nft` (the table gains the chain `lb-input`) and one reload
  of `proxcloud-lvs.service` per appliance, the standby first; the other five files of the UDP service are the same
  to the byte, the unit is not restarted and systemd is not reloaded. Every other balancer gets nothing, and the
  fix of the health read pushes no file: no golden under `internal/cloud/pve/testdata` changed since the tag, and
  the only code outside tests that changed is `internal/cloud/pve/lblvs.go` and
  `internal/platform/project/cloud_lb_pair_pve.go`. Read again from the code on this commit.
- **On the wire, for a pair with a UDP listener, from that reload on.** A UDP datagram for a port without a UDP
  listener, or for a listener whose pool has no server at all, is dropped without an ICMP "port unreachable". A
  client that is unanswered at the upgrade stays so until it was silent for more than 30 s.
- **From `v0.11.1-rc.2`** that and what the third candidate's upgrade did, in one push: `lvs.conf`, the drop-in of
  `proxcloud-lvs.service` and `lvs.nft`, with one `daemon-reload` and one reload of that service per appliance.
  **From `v0.11.1-rc.1`** also keepalived's five files once per appliance of a pair, with one keepalived reload.
  **From `v0.11.0`** the one push of every balancer that the first candidate's upgrade made; a balancer of
  `v0.11.0` has no UDP listener.
- **Billing at the upgrade from `v0.11.0`, `v0.11.1-rc.1` and `v0.11.1-rc.2`** is what the section of
  `0.11.1-rc.3` below says: an address of a cluster's balancer without a `FLOATING_IP` row gets one at the first
  pass, with the allocation's time as its start. A site that runs `v0.11.1-rc.3` has these rows.
- **Nothing the controller does changed since `v0.11.1-rc.3`.** A cluster that is on chart 0.1.19 under rc.1 or
  rc.2 still needs "Apply settings" or "Enable load balancer controller" again for the controller's part of #452.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.0`.

### Verified on a lab

Nothing on this candidate, and nothing of its two fixes. On one lab (Ubuntu 26.04 appliances, all on one node,
the scheduler off), by candidate:

- On `v0.11.1-rc.1` (2026-11-23) and on `v0.11.1-rc.2` (2026-11-23 and 2026-11-24), not run again: what the
  section of `0.11.1-rc.3` below lists.
- On `v0.11.1-rc.3` (2026-11-24), the upgrade from `v0.11.1-rc.2`: eight client loops, 183773 requests in 47
  minutes, 0 failed; no HAProxy config, no keepalived file, no HAProxy reload and no VRRP transition on four
  appliances; a pair with a UDP listener got `lvs.conf` and the drop-in once with one reload of the UDP service,
  the standby first by 10 to 11 s; a stale limit (kernel 5, listener 50) put right by that push; six addresses got
  their `FLOATING_IP` row with the allocation's time as start; the platform run of one record-only row, 8.5 s, 19
  VMs kept their process; a cluster on chart 0.1.19 moved by "Apply settings", only the controller's pod rolled.
- On `v0.11.1-rc.3`, its three fixes: a UDP limit, algorithm, persistence and weight in the kernel of both
  appliances at the first read (#454), a replace with UDP that lost 0.7 s of TCP and 0.8 s of UDP, forwarding
  never on without the table at a restart of nftables (0 of 87 samples); a change taken at once while `apt-get`
  ran, and held with "A change waits" while dpkg worked on `haproxy`, landed by the platform's own retry, never
  `DOWN` or `ERROR` (#453), `GET_HEALTH` right at three reads in the 30 s after a holder's shutdown, sshd masked after a
  reboot, a new certificate file 0600 in every sample; annotate and delete in one command line kept the address,
  twice, the limit of five held, a rotation raised no Warning (#452); and the clean-up left nothing billed.
- On `v0.11.1-rc.3`, the finding this candidate fixes: 2584 of 2584 datagrams of a one-socket client unanswered
  after its listener was added, and 4933 after a restart of `nftables.service`.

### Not verified

- Everything of this candidate on a lab, and both fixes since `v0.11.1-rc.3`: the new table on an appliance VM
  with the image's own kernel (the tests ran each image's userland on the test host's kernel), a one-socket sender
  before its listener, a restart and a reload of `nftables.service` on the holder, a real failover and a Service
  made again on a kept address while a client sends, the appliance's own DNS, time sync and apt under the chain;
  the health read after a failover where the platform starts the appliance (its tests use a fake guest and a fake
  Proxmox), and with a change made while it was down.
- The upgrade to this candidate from any earlier build, and any move to the BOM `0.11.1-rc.4` (plans read, not
  run; the lab moved to `0.11.1-rc.3`).
- What the audit of the rc.3 drill lists: an upgrade with a held push, a real unattended-upgrades upgrade of
  `haproxy`, a Service deleted while pending or refused, more than 50 Services deleted at once, what a charge run
  bills, pveproxy down during `GET_HEALTH`, a node change under load with two workers or more, a project's delete
  and every pass a scheduler would start by itself, Debian 13 appliances on a lab, two nodes, the consoles in a
  browser, cert-manager itself, a portal rollback, and any production site with a public network.
- The candidate's images and its tag's workflows: they do not exist before the tag.

### Known issues

- Those of `v0.11.1-rc.3`, `v0.11.1-rc.2` and `v0.11.1-rc.1`, below, but the ones this candidate fixes.
- A UDP client that is caught at the upgrade stays caught until it was silent for more than 30 s; the first
  datagrams after a listener is served can be lost until its first member passes its check (in real guests the
  first answer came 0.6 to 0.8 s after the push that added the listener).
- A UDP port without a listener answers no ICMP "port unreachable"; a masquerade or TCP `notrack` rule taken out
  of the loaded table by hand is not noticed, as under rc.3.
- An appliance that was down through a change stays one change behind, and reads down "its VM is stopped;
  starting it", until the next services sync or the next change: nothing queues a pass after the one that started
  its VM.
- The platform's own retries are six, 20 s apart.
- The times of a change (16 to 33 s), of a replace (0.7 s of TCP and TLS, 0.8 s of UDP) and of a planned failover
  (0.7 to 0.8 s) were measured on `v0.11.1-rc.3`; nothing was measured on this candidate.
- The full list is in `docs/releases/v0.11.1-rc.4.md`.

## [0.11.1-rc.3] - 2026-11-24

The third release candidate of 0.11.1, published as a prerelease and never as `latest`. It is not the release:
`v0.11.0` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.1 above: what `v0.11.1-rc.2` held and the three fixes its lab drill
asked for (#452, #453, #454). The drill of `v0.11.1-rc.2` ran every part of its plan and found no regression
against `v0.11.0`; this candidate exists so that three of its findings do not reach the release (a change waited
while the image's package manager ran on an appliance, a changed connection limit of a UDP listener did not reach
the kernel, an address annotated to be kept and deleted within a second was given back). Nothing of the three
fixes had run on a lab when it was tagged. The entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.11.1-rc.3`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.11.1-rc.1` and `0.11.1-rc.2`: the files differ in the release line alone. No chart of the product differs from
  its newest tag (`git diff proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`,
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and
  `git diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed.
  It is a release BOM, and the release gate passes on it and on the nineteen previous BOMs.
- **No component of the sandbox feature is in this BOM** (`TestSandboxIsDark`). The sandbox code is in the build and
  cannot be turned on, as in `v0.11.1-rc.2`.
- **The plan from each previous BOM** (`PlanFor`). From 0.11.1-rc.2 and from 0.11.1-rc.1 one change, the release,
  record only, allowed, with no part, no seed row and no machine replaced; phrase `upgrade <name> to 0.11.1-rc.3`.
  From 0.11.0, 0.11.0-rc.1 and 0.11.0-rc.2 two changes, the release and `tenantCharts.kubernetes` 0.1.17 to 0.1.19,
  both record only, allowed. From 0.10.2 and 0.10.2-rc.1 the same two rows (the chart from 0.1.16); four from 0.10.1
  and 0.10.1-rc.2 (those and `cert-manager` on the seed and on the platform cluster); five from 0.10.1-rc.1 (also
  `pc-platform` 0.1.15 to 0.1.16); and a refusal by the pair gate from the nine BOMs on Kubernetes 1.35.10. This BOM
  carries no unfinished replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **Billing at the upgrade, from every earlier build.** At each cluster balancer's first pass after the portal's
  upgrade, every address it holds for a Service or keeps that has no `FLOATING_IP` row gets one, with the
  allocation's time as its start: the bill item is counted from that time, or from the start of the open bill's
  cycle where the address is older (`getLastRateTime`). It is the row the sync would have written. A site that ran
  no sync has every such address billed from its allocation in one step; see the first upgrade note of #452 above
  and "Billing first" in `docs/releases/v0.11.1-rc.3.md`, which says what to check before the upgrade.
- **What the portal's upgrade does to the balancers, from `v0.11.1-rc.2`.** See the three upgrade notes above. A
  pair with a UDP listener gets `lvs.conf` (one line more, `warmup 0`, per virtual server with a check) and the new
  drop-in of `proxcloud-lvs.service`, with one `daemon-reload` and one reload of that service per appliance, the
  standby first; a kernel connection limit that was stale is put right by that push. Every other balancer gets
  nothing: the HAProxy config, `keepalived.conf` and keepalived's support files are the same to the byte (no golden
  under `internal/cloud/pve/testdata` changed since the tag but the four `lvs-*.conf`, which gain `warmup 0`). The
  fix for the package manager (#453) pushes no file. Read again from the code on this commit.
- **From `v0.11.1-rc.1`** that and what the second candidate's upgrade did: keepalived's five files once per
  appliance of a pair, with one keepalived reload, the standby first; no HAProxy push. **From `v0.11.0`** the one
  push of every balancer that the first candidate's upgrade made; a balancer of `v0.11.0` has no UDP listener.
- **The controller's part of #452 reaches a cluster only with the controller's image.** A cluster that is on chart
  0.1.19 under an earlier candidate is offered no update by the tenant's console (the chart version is the
  provider's already) and is skipped by "Bump all"; the operator's "Apply settings" on its row, or "Enable load
  balancer controller" again, sets the image (`SetChartVersion`, `EnableLBController`).
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.0`.

### Verified on a lab

Nothing on this candidate, and nothing of its three fixes. On one lab (Ubuntu 26.04 appliances, all on one node,
the scheduler off), by candidate:

- On `v0.11.1-rc.1` (2026-11-23), not run again: the upgrade from `v0.11.0` under five client loops (31365
  requests, 0 failed), one push per balancer with the standby first, the platform run of two record-only rows, a
  cluster on the `v0.11.0` controller unchanged, the cluster's update with no machine rolled, the `lb-internal`
  and `loadBalancerIP` cases, a UDP Service answering from outside; and TLS on a single balancer (SNI, versions,
  HTTP/2, PROXY version 1 and 2, a rotation with 0 failed of 6450, the 16 KiB bundle, the search for the private
  key, a reboot).
- On `v0.11.1-rc.2` (2026-11-23 and 2026-11-24): the upgrade from `v0.11.1-rc.1` (four loops, 0 failed;
  keepalived's files once per appliance of a pair, the standby first, no HAProxy reload, no VRRP transition; the
  pair rc.1 left `DEGRADED` read `ACTIVE` at the first pass; the platform run of one record-only row, 38 s);
  Kubernetes on the moved cluster and on a new one (UDP, mixed ports, the annotations, TLS from a Secret with a
  renewal, kept addresses, the first appliance serving alone, replace and failover on the cluster's pair, the
  cluster's delete); a tenant's own pair with UDP, TLS and faults (six failovers, a replace, reboots, a damaged
  certificate file healed, a lone appliance); 44 packets routed toward a tenant's address, none arrived.

### Not verified

- Everything of this candidate on a lab, and each of the three fixes since `v0.11.1-rc.2`: a change taken while
  the package manager runs and the platform's own retries, a changed UDP limit in the kernel and a stale one put
  right at the upgrade, the keep-address annotation set just before a delete, the `FLOATING_IP` rows a pass writes
  and what they bill, the health view after a failover, the planned switch that waits for UDP members, sshd after
  a reboot, the mode of a new certificate file, the restart of `nftables.service`.
- The upgrade from `v0.11.1-rc.2` (the one push of a pair with a UDP listener), and any move to the BOM
  `0.11.1-rc.3` (plans read, not run; the lab moved to `0.11.1-rc.2`).
- What the audit of the drill lists: a node change under load with two workers or more, a project's delete and
  every pass a scheduler would start by itself, the upgrade of a tenant's own pair with UDP or TLS listeners,
  Debian 13 appliances on a lab, two nodes, the consoles in a browser, cert-manager itself, a portal rollback,
  and any production site with a public network.
- The candidate's images and its tag's workflows: they do not exist before the tag.

### Known issues

- Those of `v0.11.1-rc.2` and `v0.11.1-rc.1`, below, but the ones this candidate fixes.
- A change still waits while dpkg is at work on a package the appliance itself runs, and during a first install;
  the balancer says "the platform tries again within half a minute" also when the six retries are over.
- A cluster on an earlier controller still gives back an address annotated just before its delete; with more than
  50 Services deleted at once the ones beyond the first 50 are decided from the stored state.
- After a failover the balancer's row names the old roles until the next pass or the next health read.
- One busy UDP port hides a closed one on the same server; `leastconn` on UDP counts the flows of the last five
  minutes.
- The times of a change (14 to 44 s) and of a failover (0.7 to 0.8 s planned, 3.2 to 3.6 s with a dead master)
  were measured on `v0.11.1-rc.2`; nothing was measured on this candidate.
- The full list is in `docs/releases/v0.11.1-rc.3.md`.

## [0.11.1-rc.2] - 2026-11-24

The second release candidate of 0.11.1, published as a prerelease and never as `latest`. It is not the release:
`v0.11.0` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.1 above: what `v0.11.1-rc.1` held, the three fixes its lab drill
asked for (#446, #447, #448), and a change of the repository's own tests, which pull each published chart once
(#449; no product code). It exists so that the drill can go on with the defect that stopped it fixed. Nothing
of the three fixes had run on a lab when it was tagged. The entries above are not repeated here. Not for a
production site.

### Changed

- **The platform BOM's release is `0.11.1-rc.2`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.11.1-rc.1`: the two files differ in the release line alone. No chart of the product differs from its newest tag
  (`git diff proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`,
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and
  `git diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed.
  It is a release BOM, and the release gate passes on it and on the eighteen previous BOMs.
- **No component of the sandbox feature is in this BOM** (`TestSandboxIsDark`). The sandbox code is in the build and
  cannot be turned on, as in `v0.11.1-rc.1`.
- **The plan from each previous BOM** (`PlanFor`). From 0.11.1-rc.1 one change, the release, record only, allowed,
  with no part, no seed row and no machine replaced; phrase `upgrade <name> to 0.11.1-rc.2`. From 0.11.0,
  0.11.0-rc.1 and 0.11.0-rc.2 two changes, the release and `tenantCharts.kubernetes` 0.1.17 to 0.1.19, both record
  only, allowed. From 0.10.2 and 0.10.2-rc.1 the same two rows (the chart from 0.1.16); four from 0.10.1 and
  0.10.1-rc.2 (those and `cert-manager` on the seed and on the platform cluster); five from 0.10.1-rc.1 (also
  `pc-platform` 0.1.15 to 0.1.16); and a refusal by the pair gate from the nine BOMs on Kubernetes 1.35.10. This BOM
  carries no unfinished replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What the portal's upgrade does to the balancers, from `v0.11.0`.** What the first candidate's upgrade did:
  the HAProxy config and a pair's keepalived files differ from the ones `v0.11.0` wrote, so every balancer is pushed
  once at its next pass, the standby first. This candidate renders the HAProxy config of `v0.11.1-rc.1` to the byte
  (no config golden changed since that tag).
- **What the portal's upgrade does to the balancers, from `v0.11.1-rc.1`.** See "From v0.11.1-rc.1" under Upgrade
  notes above: keepalived's five files once per appliance of a pair, with one keepalived reload, the standby first;
  no HAProxy push; nothing on a single appliance; a pair that read `DEGRADED` by the defect of the holder read is
  `ACTIVE` after that pass; the certificate files a config names are checked at every pass from now on. Read
  again from the code on this commit.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.0`.

### Verified on a lab

Nothing on this candidate. On `v0.11.1-rc.1`, on one lab on 2026-11-23 (Ubuntu 26.04 appliances), two parts of its
drill ran:

- The upgrade from `v0.11.0`: the portal upgrade under five client loops (31365 requests on the Service that
  stayed served through the part, 0 failed); one push per balancer, the pair's standby first, with no VRRP
  transition and no new package; the platform run of two record-only rows, which replaced no machine; a cluster on
  the `v0.11.0` controller unchanged by both; the cluster's update with no machine rolled; the `lb-internal`,
  keep-address and `loadBalancerIP` cases; a UDP Service answering from outside the cluster (50 of 50 datagrams).
- TLS on a single balancer: SNI, the TLS versions and suites, HTTP/2, PROXY version 1 and 2, a rotation under load
  (0 failed of 6450), a bundle of 16 KiB, and a search for the private key that found it only sealed in the
  database and in the root-only certificate files of the appliance.

### Not verified

- Everything of this candidate on a lab, and each of the three fixes since `v0.11.1-rc.1`: the holder read on a
  pair with a UDP listener and the healing of a degraded pair, a lone appliance of a pair, the check and the heal
  of a certificate file, the push of only the files that differ, the quota list.
- The upgrade from `v0.11.1-rc.1` (the one keepalived reload per appliance of a pair), and any move to the BOM
  `0.11.1-rc.2` (plans read, not run).
- UDP and TLS on a tenant's own pair, TLS on any pair, a failover under UDP, Debian 13 appliances on a lab, TLS
  from a Service's Secret on a lab, the watch timings, the consoles in a browser, and any production site with a
  public network.
- The candidate's images and its tag's workflows: they do not exist before the tag.

### Known issues

- Those of `v0.11.1-rc.1`, below.
- A lone appliance of a pair sends one VRRP advert a second, addressed to `127.0.0.1`, to its network's gateway.
- A second appliance that joins can hold the addresses beside the first for one pass, when the push that tells
  the first of its peer fails.
- A reload that keeps HAProxy's master socket closed for more than 0.6 s is still one failed run of keepalived's
  track script.
- While an appliance holds a change it refused, its certificate files are not checked; a single balancer whose
  heal cannot be pushed reads `DOWN` although HAProxy still serves.
- The full list is in `docs/releases/v0.11.1-rc.2.md`.

## [0.11.1-rc.1] - 2026-11-23

The first release candidate of 0.11.1, published as a prerelease and never as `latest`. It is not the release:
`v0.11.0` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.1 above: the complete layer 4 load balancer (#430, #431, #433, #434),
its Kubernetes side (#435, #438, #439, #440, #441, #442) and, switched off, the sandbox platform work of 0.12.0
(#422, #423, #432, #436). It exists so that the lab can run the drill of this code: nothing of it had run on a lab
when it was tagged. The entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.11.1-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins of `0.11.0`
  but `tenantCharts.kubernetes`, cluster chart `proxmox-kamaji-cluster` 0.1.19
  (`sha256:476950b5e841c9460c47960b1841ceac17998b5c06c7358c9966585f6602d5fa`). No chart of the product differs from
  its newest tag (`git diff proxmox-kamaji-cluster-v0.1.19 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster`,
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` and
  `git diff proxcloud-sandbox-v0.1.0 -- deploy/charts/proxcloud-sandbox` print nothing), so no chart tag is needed.
  It is a release BOM, and the release gate passes on it and on the seventeen previous BOMs.
- **No component of the sandbox feature is in this BOM** (`TestSandboxIsDark`). The sandbox code of #422, #423, #432
  and #436 is in the build and cannot be turned on: an installation that asks for the block is refused, and no run
  installs the sandbox chart.
- **The plan from each previous BOM** (`PlanFor`, one control plane machine and one machine per group). From 0.11.0,
  0.11.0-rc.1 and 0.11.0-rc.2 two changes, the release and `tenantCharts.kubernetes` 0.1.17 to 0.1.19, both record
  only, allowed, with no part, no seed row and no machine replaced; phrase `upgrade <name> to 0.11.1-rc.1`. From
  0.10.2 and 0.10.2-rc.1 the same two rows (the chart from 0.1.16); four from 0.10.1 and 0.10.1-rc.2 (those and
  `cert-manager` on the seed and on the platform cluster); five from 0.10.1-rc.1 (also `pc-platform` 0.1.15 to
  0.1.16); and a refusal by the pair gate from the nine BOMs on Kubernetes 1.35.10 (21, 22 or 25 changes). This BOM
  carries no unfinished replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What the portal's upgrade does to the balancers.** The HAProxy config rendered for a balancer of `v0.11.0` is not
  the one this build renders (the state file, `retries` and `option redispatch`, `timeout connect 2s`, backends named
  by pool: #430), and a pair's keepalived track script changed, so the recorded hashes differ and every balancer is
  pushed once at its next pass, the standby first (`lbPushOrder`); a config the standby refuses reaches no other
  appliance in that pass. Read again from the code for the move from 0.11.0 on this commit.
- **What the platform's upgrade does not do.** It moves no cluster. A cluster made before this release keeps its
  controller (gateway protocol 1) until it is moved to chart 0.1.19: its UDP Services stay `UDPNotSupported`, so no
  Service gets an address and a bill without its owner's action.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.11.0`.

### Verified on a lab

Nothing of this code. Before the code was written, the method was tried by hand on the lab's two Ubuntu 26.04
appliances (the spike of 2026-11-22): UDP through IPVS with a masquerade, a second keepalived for the UDP service
(20 reloads, no switch of the address, 0 of 415 datagrams lost), the UDP check with a ping beside it, and two
certificates on one HAProxy bind. It is a spike of the method, not a test of this code.

### Not verified

- Everything of this candidate, until its drill: UDP and TLS by the product on a lab pair, on either appliance image
  (Debian 13 was not in the spike); the layer 4 options on a lab; the Kubernetes side on a real cluster (UDP and
  mixed Services, the annotations, TLS from a Secret, kept addresses, the watch); the consoles.
- The one push of every balancer at the upgrade, and the fix for requests lost at a reload (#430), on a lab.
- An existing cluster across the upgrade, its move to chart 0.1.19, and a rollback of the portal to `v0.11.0`.
- Any move to the BOM `0.11.1-rc.1` (plans read, not run), and the candidate's images and its tag's workflows: they
  do not exist before the tag.
- The sandbox invoice line on a sent bill, and anything of the sandbox platform code.

### Known issues

- UDP is served on a pair only. A UDP server sees the appliance's address, never the client's. The default UDP check
  pings the server (it needs ICMP from the appliance), sends random bytes without a payload, and does not see a
  closed port under traffic without a required reply.
- Private keys of certificates rest encrypted in the database, in `backups/` next to the copy of `.env` that opens
  them, and in clear on each appliance's disk.
- Only `proxcloud.io/lb-*` annotations are read. A Service that asks for an internal balancer under another cloud's
  annotation name gets a public address until the annotation is renamed to `proxcloud.io/lb-internal`.
- Chart 0.1.19 with the controller image of `v0.11.0` does not run (`--watch=true`): do not enable the controller
  again or set the chart version on such a cluster while the portal is rolled back to `v0.11.0`.
- A portal rolled back to `v0.11.0` drops a cluster's list of kept addresses: they stay billed and in the quota,
  and no page lists them, until the cluster is deleted. Release them before a rollback.
- A bill with sandbox items that round to 0.00 gets one summed item, "Sandbox runs below 0.005 each (n)".
- The full list is in `docs/releases/v0.11.1-rc.1.md`.

## [0.11.0] - 2026-11-22

The release after `v0.10.2`. It brings highly available load balancers: a load balancer runs on a pair of appliance
VMs that share one public address, and a managed Kubernetes cluster can give its Services of type LoadBalancer a
public address on such a pair. It was prepared by the release candidates `v0.11.0-rc.1` and `v0.11.0-rc.2`, whose
sections below are kept; everything they held is in this section, and the code is that of `v0.11.0-rc.2`. The entries
under Added, Fixed and Changed were written when each change merged, before or between the two lab drills: where one
says "Not run on a lab", the two lists below are the current state.

- **A load balancer is a pair** (Added). Two appliance VMs, one public address moved by keepalived, the same HAProxy
  config on both. On the lab a dead appliance cost 2.8 to 3.5 seconds, a planned switch 0.7 to 0.8.
- **What HA means on one node.** The pair survives the failure of an appliance, not the loss of the node both run on.
  Both land on one node on a one-node site, on a tenant network in a `simple` zone, and where the appliance image is
  on one node only. Two nodes were not run on a lab.
- **What a pair costs.** Two servers, their disks and one public address, as ordinary rows of the project. A tenant
  can ask for one VM (`ha: false`); a balancer made before this release stays one VM.
- **A load balancer controller for managed Kubernetes** (Added). A Service of type LoadBalancer gets its own public
  address on the cluster's pair; listeners and members follow the Services' ports and the nodes by themselves. On the
  lab a port added to a live Service answered after 16.8 seconds, a changed node port was followed in 12.7, a removed
  port lost its firewall rule after 9.5 and left the config after 14.4, a second Service had its own address after 32
  and gave it back 33.6 after its delete, and a new worker was a member 14 seconds after its Node appeared. Through
  the listener and Service changes the first port lost 0 requests and a long-lived connection survived six reloads
  (`externalTrafficPolicy: Cluster`, healthy nodes). Node changes lose requests for seconds (Known issues).
- **Existing clusters keep MetalLB** until the operator enables the controller for them, in class-only mode: no
  MetalLB address moves.
- **Docker Compose can serve the gateway listener** the controllers call (`--cluster-gateway <address>`, off by
  default; Added).
- **Sandbox nodes install gVisor again**: the node setup installs runsc `20261116.0` from the release's own package,
  pinned by SHA256, where it asked apt for a version the suite no longer lists (Fixed).
- **The first candidate's two faults are fixed and shown** (Fixed). Replace appliances took an address down for 458
  seconds on `v0.11.0-rc.1`, and a stopped master appliance was started again in 1 of 6 tries. On `v0.11.0-rc.2` three
  replaces under load cost one switch of 0.7 to 0.8 seconds each, and a stopped appliance was started by the next sync
  three times of three.
- **Not in this release:** UDP listeners and TLS termination. Both are planned for the next release, "complete layer
  4" (Known issues).

The platform BOM of this release is `0.11.0`. It pins what the candidates `0.11.0-rc.1` and `0.11.0-rc.2` pin, and the
three BOMs differ in the `release` line alone: the pins of `0.10.2` but `tenantCharts.kubernetes`, cluster chart
`proxmox-kamaji-cluster` 0.1.17 (`sha256:dfdccf67de36238f4ae7e7914035f99f394fb6f45ed80540d6e1a2bf5076e914`) for new
tenant clusters. No k3s, Kubernetes version, node image, third-party chart, Cluster API provider or clusterctl image
moves, and the platform cluster stays on chart 0.1.16. The release carries the BOMs of `0.8.0`, `0.8.1`, `0.8.2`,
`0.9.0-rc.1`, `0.9.0-rc.2`, `0.9.0`, `0.9.1`, `0.10.0-rc.1`, `0.10.0`, `0.10.1-rc.1`, `0.10.1-rc.2`, `0.10.1`,
`0.10.2-rc.1`, `0.10.2`, `0.11.0-rc.1` and `0.11.0-rc.2` for the platforms those builds installed.

Upgrading from `v0.10.2`. From an older release, read the sections of `v0.10.2` and `v0.10.1` as well: their steps
still apply.

- **A platform on a BOM before `0.10.1`** (0.8.0 to 0.10.0 and their candidates, all on Kubernetes 1.35.10) is refused
  by this build, by the pair gate on both cert-manager rows. Move it to `0.10.1` with the `v0.10.1` build first, both
  parts.
- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.11.0`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. `compose.yaml` did not change since
  `v0.10.2`: on the lab the upgrade between the two candidates took 27.6 seconds and PostgreSQL, Keycloak and Traefik
  kept their containers. From now on a new load balancer is a pair (two servers, their disks, one address). Existing
  balancers keep their one VM, config and firewall rules, and are not pushed at the upgrade: each pass asks every
  appliance whether HAProxy serves its config and pushes only where it does not (shown for balancers made on a
  candidate; for one made on `v0.10.2` it rests on tests).
- **The platform run is record only**, phrase `upgrade <name> to 0.11.0`, one action with no parts. From `0.10.2` or
  `0.10.2-rc.1` the plan has two rows, the release and `tenantCharts.kubernetes` 0.1.16 to 0.1.17: it records the
  release and writes the chart on the Kubernetes provider, so clusters created after it get the new chart. It replaces
  no machine, syncs nothing and writes nothing on Proxmox; no tenant cluster changes chart and none loses MetalLB. On
  the lab the same two rows took 39.6 seconds (to the first candidate), and the one-row run between the candidates 8.5
  seconds with no VM restarted. The run to `0.11.0` itself was read as a plan, not run.
- **From other BOMs** (`Classify`): from `0.11.0-rc.1` and `0.11.0-rc.2` one change, the release, record only; from
  `0.10.1` and `0.10.1-rc.2` four, the two above and `cert-manager` `v1.20.2` to `v1.21.3` on the seed and on the
  platform cluster (back up the cert-manager resources first, as the section of 0.10.2 says); from `0.10.1-rc.1` five,
  also `pc-platform` 0.1.15 to 0.1.16, no machine replaced; from the nine BOMs on 1.35.10 refused. A platform that
  stands in the replacement of its machines toward `0.10.1` or one of its candidates goes on to that BOM with a Retry
  first.
- **What a site needs for load balancers.** An untagged public bridge on every node: a public network is a bridge name
  and nothing more, the appliance's NIC is plugged on it with no VLAN tag, and the bridge must exist under that name
  on every node an appliance can land on (a VLAN on a trunk gets a bridge of its own on every node; an SDN vnet is
  refused for a pair). The appliance image (`config.lbImage`, default `debian-13`) on every node, with `haproxy`,
  `socat`, `keepalived` and `iproute2` from apt or baked in. For cluster balancers a route from the platform network
  to the portal host on port 8444, where the controllers call the gateway listener (Compose: `./proxcloud-up.sh
  install --cluster-gateway <address>`, 14 to 19 seconds on the lab, only the API container recreated; keep the volume
  `gateway`), and the provider settings: a public network with a gateway on the compute provider
  (`config.network.publicNetworks`) and `config.cluster.loadBalancer` on the Kubernetes provider (that public network
  and the flavor of the appliances). Without them every cluster keeps MetalLB alone.
- **New clusters get the controller in default mode** when every gate holds (a gateway URL and the controller image,
  chart 0.1.17 on the provider, the project's own workers, a public network the project may use): a Service without a
  class gets a public address, MetalLB serves only class `proxcloud.io/internal`, and NGINX Gateway Fabric takes one
  public address per Gateway. **An existing cluster** keeps MetalLB as its default until the operator runs **Enable
  load balancer controller** for it (class-only: only Services of class `proxcloud.io/lb`).
- **A load balancer pair made on `v0.11.0-rc.1`**: run a services sync right after the portal upgrade. On the lab such
  a pair refused every connection for about 8 minutes while its row read `ACTIVE`, and the first sync on the corrected
  code repaired it in 5 seconds.
- The app chart is `0.11.0` with appVersion `v0.11.0`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.11.0`. The installer's k3s and Kubernetes pins stay at 1.35.10.

Verified on a lab. One lab, every appliance on one node, Ubuntu 26.04 appliances; the first drill on `v0.11.0-rc.1`
(2026-11-21), the second on `v0.11.0-rc.2` (2026-11-22), each checked afterwards by a read-only audit:

- On rc.1, not run again: pair create and its firewall rules; 15 failovers 0.7 to 3.5 s; a deleted vrrp rule restored
  by the sync; stop / hard reboot of an appliance refused; five Services on five addresses of one pair; refusals as
  Events (UDP, no node port, loadBalancerIP, quota); class internal on MetalLB; PROXY protocol v2; an allowed source
  range; policy Local health checks; idle TCP 330 s; foreign Nodes refused and audited; suspended project; gateway
  down during a delete; enable on an existing cluster; one floating IP row per address, traffic counted once.
- Upgrade rc.1 to rc.2: portal 27.6 s; platform run record only, 8.5 s, no VM restarted; serving balancers not pushed
  by the upgrade or by two syncs; an appliance rc.1 left not serving repaired by the first sync.
- Replace appliances under 10 requests a second, three times: one switch of 0.7 to 0.8 s each (rc.1: 458 s), 12.5 to
  13.5 min, never ACTIVE with one appliance.
- A new pair and a new single balancer answer before their first ACTIVE; config files 644, scripts 755.
- Stopped keepalived or HAProxy started by the next sync; HAProxy on another config reported and loaded again by the
  next sync; a config change held while apt runs on the master, applied after, 0 requests lost.
- A stopped master, three times: failover 2.9 to 3.4 s, started by the next sync, ACTIVE one sync later, the sync's
  other passes ran. A paused master: failover 3.0 s, ACTIVE one sync after the resume.
- A cluster's controller: port added (answers after 16.8 s), targetPort changed (0.5 s), nodePort changed (12.7 s),
  port removed (rule gone 9.5 s, out of the config 14.4 s), second Service on its own address (32 s) and given back
  (33.6 s); 0 requests lost on the first port and its long-lived connection kept through six reloads.
- Workers 1 to 2 and 2 to 1: the member follows 14 s after the Node (with the loss under Known issues).
- Converge lock about 58 s; a change cut by an API stop is live after 190 to 209 s (rc.1: 24.7 min). Controller idle:
  18.6 Proxmox calls a minute (rc.1: 132). A dead master of a cluster's pair: 3.5 s, started by the product 31 s
  later.
- A refused credential: one log line with cluster and reason, one audit row. Clean-up: no object, no quota, the
  Proxmox cluster as before.

Not verified on a lab:

- UDP listeners and TLS termination: not in v0.11.0, planned next.
- Two nodes: VRRP across nodes, placement on two nodes, loss of a node.
- The consoles: no page was opened; every step went through the API.
- A production site with a public network: one lab, a test range on a plain bridge.
- The default Debian 13 appliance image (container test only).
- A balancer made before v0.11.0 across the upgrade (tests only; the lab's were made on rc.1).
- The scheduler: the lab runs none, every sync was started by hand.
- Request loss at node changes was measured once per direction, policy Local twice; a dead node across a reload not
  run.
- A real unattended-upgrades run of minutes; a VM stopped from inside the guest; an appliance out of the token's
  sight; a worker firewall write during a faulted sync.
- The refusal of a foreign source address, NGINX Gateway Fabric in default mode, a cluster made on chart 0.1.16 then
  enabled, a project delete with a balancer, the installer path, the sandbox fix (#420), the sandbox entry appliance.
- The upgrade to the final BOM 0.11.0 (the lab moved to 0.11.0-rc.2).

Where an entry below says the BOM on `main` is `0.11.0-dev`, that was the state at that merge: this release's BOM is
`0.11.0`, and the change after its tag sets `main` back to a `-dev` one.

### Added

- **A load balancer may run on two appliance VMs, and a new one does by default** (`data.ha`, default `true`;
  `internal/platform/project/cloud_lb_pair_pve.go`, `internal/cloud/pve/lbpair.go`). keepalived (VRRPv3, unicast
  over the project network with `check_unicast_src`, both `BACKUP` with `nopreempt`, a track script on HAProxy)
  moves the public address between them; both run the same HAProxy config, bound to the address, and the standby
  gets every config first. The address belongs to the balancer (`lb:<id>`), so a pair costs one public address, two
  servers and their disks, with no new billing type or quota key. The second VM goes to another node when it can
  (soft spread group `lb-<id>`) and to the first one's node on a `simple` zone, whose traffic stays on its node; where
  both are on one node the page says the pair survives an appliance failure, not the loss of the node. Status
  `ACTIVE`, `DEGRADED` (one appliance, or both holding the address, with the reason) or `DOWN`; **Replace
  appliances** rebuilds both from the current image, the standby first, with one planned switch. A public network
  that is an SDN vnet is refused for a pair. Existing balancers are not changed: their install line, readiness
  check, config and firewall rules stay as they are. Measured on the lab spike: 3.1 to 3.5 s lost for a dead
  appliance, under 1 s for a stopped one. Not run on a lab with a public network, across nodes, or with a node lost.
- **The gateway accepts a Kubernetes cluster's load balancer state** (`PUT /lb/v1/state` on the gateway listener,
  `internal/cloud/lbgw`, `internal/platform/project/cloud_lb_cluster_pve.go`; docs/managed-k8s.md). A cluster's load
  balancer controller presents its own credential (`lb@proxcloud!<clusterId>`, stored as a SHA-256, one call for its
  own cluster, minting again rotates it) and sends the cluster's Services of type LoadBalancer and its nodes. The
  portal checks each Service (TCP and IPv4 only, a node port in range, source ranges that fit a firewall rule), keeps
  as members only the cluster's own worker addresses, stores the state and converges in the background: one pair per
  cluster, built with the first Service and kept until the cluster is deleted; one public address per Service (a
  `floatingIps` of the quota each), listeners on that address only, pools in mode tcp on the node port, an HTTP check
  on `healthCheckNodePort` for policy Local, an idle timeout of one hour. A Service removed gives its address back as
  soon as no appliance holds it, with no day of grace. A suspended or deleting project changes nothing. A tenant's
  edit or delete of the cluster's balancer answers 409. The cluster's release revokes the credential first, then
  removes the pair, its addresses and the row, without the controller. The Kubernetes provider names the public
  network and flavor in `config.cluster.loadBalancer`, checked on save. Nothing calls the gateway yet: the controller
  comes with a later change. Not run on a lab.
- **A load balancer controller for managed Kubernetes clusters** (`cmd/lb-controller`, cluster chart
  `proxmox-kamaji-cluster` **0.1.17**; docs/managed-k8s.md). One per cluster, on the platform cluster next to its
  control plane: every 10 s it sends the cluster's Services of type LoadBalancer and its nodes to the gateway with the
  cluster's own credential, holds each Service with the finalizer `proxcloud.io/load-balancer`, writes its address in
  `status.loadBalancer` (`ipMode: Proxy` with PROXY v2) and reports refusals (UDP, no node port, source ranges, quota)
  as Events, one per change. It is a second binary of the `proxcloud-api` image, with no image of its own, and its
  NetworkPolicy lets it reach DNS, its namespace's control planes and the gateway only. A new cluster gets it in
  default mode when every gate holds (a gateway URL, `PROXCLOUD_LB_CONTROLLER_IMAGE`, chart 0.1.17, the project's own
  workers, the provider's `config.cluster.loadBalancer` on a public network the project may use): Services without a
  class get a public address and MetalLB serves only class `proxcloud.io/internal`. An existing cluster keeps MetalLB
  as its default; **Enable load balancer controller** (`POST /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/load-balancer`)
  adds the controller in class-only mode (class `proxcloud.io/lb`), and again rotates the credential. NGINX Gateway
  Fabric's Service class is never changed. The controller's image is set at create, enable and chart bump, never by the
  sync. Compose sets the image with `--cluster-gateway`, the installer chart with `api.csiGateway`. The BOM pins
  chart 0.1.17 for new tenant clusters (Changed). Not run on a lab.
- **The consoles show a cluster's load balancer** (docs/managed-k8s.md, the Kubernetes and load balancer guides).
  The tenant's cluster page says which Services of type LoadBalancer get a public address (default, class only, or
  private addresses only), what the pair of appliances and each address cost, and in default mode that NGINX Gateway
  Fabric takes a public address per Gateway (the add-on dialog says it too); it shows when the controller last
  reported and the balancer: its appliances, each Service's address, ports, workers up and state, with refusals such
  as `UDPNotSupported`. On the load balancer pages a cluster's balancer is read-only and links to its cluster. The
  operator's Kubernetes provider form edits `config.cluster.loadBalancer` (public network and flavor, both or
  neither), and the provider's clusters card shows each cluster's mode with a **Load balancer** dialog: the
  controller's last call, the balancer, and **Enable load balancer controller** or, on a cluster that has one, a
  rotation. New reads: the cluster action `GET_LOAD_BALANCER` and
  `GET /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/load-balancer`; `load_balancer` on the cluster row and
  `loadBalancer` in the operator's cluster list. Not run on a lab.
- **Docker Compose can serve the gateway listener** (`proxcloud-up.sh install --cluster-gateway <address>`,
  `deploy/compose/compose.cluster-gateway.yaml`): published on `<address>:8444` only, with an ECDSA key and a
  self-signed certificate the API makes on its first start (`PROXCLOUD_CSI_GATEWAY_SELF_SIGNED`) in a volume of its
  own. Opt-in, v0.11.0 or later. Block volumes stay off on Compose: no gateway token is registered there.
- **Health checks, PROXY v2 and allowed sources on every load balancer**: per pool an HTTP check on a path and port
  (`healthCheck`) and `proxyProtocol`; per listener `sourceRanges` (IPv4, at most 512 characters), written as the
  source of the listener's firewall rule. Listener rules hold at most 15 ports each, Proxmox's limit, and the writer
  no longer deletes a rule it did not write.

### Fixed

- **The services sync keeps running when a load balancer's appliance is down** (`internal/cloud/pve/vm_inventory_data.go`,
  `syncprovider.go`, `write.go`, `internal/cloud/syncjob/job.go`). The drill of `0.11.0-rc.1` stopped the master
  appliance of a pair and the sync started it again in 1 of 6 tries. The sync had read the pair's public address from
  the master's guest agent into its SERVER row. A stopped VM has no agent, so the address counted as lost, the
  project's inventory as unproved, and the sync left out its load balancer pass, the one that starts a stopped
  appliance; the row went on saying `ACTIVE`, "MASTER up". The same skip left out the isolation rules, worker
  firewalls, start at boot and sandbox pass of every tenant of the provider, and the sync of every project after the
  failed one, for as long as the VM stayed down. Now:
  - an appliance's SERVER row holds the address the platform configured and nothing its guest reports; a row that
    holds a balancer's address is put right by the next sync, whatever state the VM is in;
  - a VM that Proxmox reports stopped or paused is known to have no agent: its row goes back to its configured
    addresses, as after a stop through the portal, and its project is synced. This holds for any server, not only an
    appliance: a tenant's VM that had reported a container bridge and was then shut down from inside kept its project
    unsynced until it was started. A running VM whose agent does not answer keeps its addresses, as before;
  - a project whose inventory cannot be proved holds back its own rows, isolation and cleanup, the provider's seed
    sweep and the sandbox pass's sweep of old firewall generations, and nothing else. The other projects, the load
    balancer pass, the provider's isolation rules, worker firewalls, start at boot and the fencing of sandbox workers
    run ([docs/vm-inventory-safety.md](docs/vm-inventory-safety.md#what-an-unproved-inventory-holds-back));
  - the load balancer pass, which now runs in every sync, proves the one absence it acts on: an appliance missing
    from the VM list is gone only when its VMID is free in the cluster. One the platform's token cannot see is
    `down`, "the platform cannot see its VM", and the row no longer says "The load balancer's VM is gone. Delete
    this load balancer" or "Replace appliances" for a VM that is only out of sight.
  Tested against Postgres and a fake Proxmox that stops and pauses VMs and whose stopped VMs have no agent
  (`TestSyncRepairsAStoppedApplianceWhateverElseIsUnknown`, `TestVMInventoryApplianceRowNeverHoldsTheBalancersAddress`,
  `TestVMInventoryStoppedVMIsKnownToHaveNoAgent`, `TestLBApplianceOutOfSightIsNotGone`). Not run on a lab.
- **An API restart no longer holds a cluster's load balancer for 20 minutes** (`internal/platform/lock/shedlock.go`,
  `internal/platform/project/cloud_lb_cluster_pve.go`). The converge of a cluster's balancer took its lock for the
  longest a pass can take, and on the drill an API restarted in the middle of one left the lock held: for 24.7
  minutes every state the controller sent was stored and none applied, so a deleted Service kept its address. The
  lock now lasts one minute and its holder extends it while it works (`ShedLock.Hold`); a holder that died loses it a
  minute later. A pair also refuses a third appliance, should two passes ever build at once.
- **A controller's unchanged state starts no pass over the balancer** (`cloud_lb_cluster_pve.go`). Every PUT, one
  every 10 seconds per cluster, ran a full pass: about 16 Proxmox calls with two guest commands, 96 calls a minute per
  cluster on the drill. A PUT now starts a pass when the state changed, when the balancer is not settled (building,
  `DEGRADED`, an address to give back), or when the last look at a settled balancer is a minute old; it is answered
  with each Service's state as before. An appliance stopped behind a settled balancer is therefore started again
  within about 70 seconds, where the drill measured 2 to 6; the other appliance takes the address in seconds either
  way.
- **A refused load balancer credential says whose and why** (`internal/cloud/lbgw/gateway.go`). The log line of a
  refused credential named neither the cluster nor the reason, and no audit event was written, where the guide said
  refusals were audited. The line now carries the cluster the header claimed and `malformed`, `unknown cluster`,
  `revoked` or `wrong secret`; a revoked credential and a wrong secret for a cluster that has one are `LB_GATEWAY` /
  `DENY` audit events of its project, at most one per cluster and reason a minute. The caller still gets the same
  401, and the secret is in neither.
- **A load balancer's config is recorded only when its appliance serves it** (`internal/cloud/pve/lb.go`,
  `lbpair.go`, `internal/platform/project/cloud_lb_pve.go`, `cloud_lb_pair_pve.go`). The drill of `0.11.0-rc.1`
  replaced a pair's appliances and the address refused every connection for 458 seconds while the row read
  `ACTIVE`, or `DEGRADED` with "One appliance carries the traffic" while the second appliance was built: the
  first config push met the haproxy package's own first start. The readiness check passed as soon as
  the binaries existed, the apply found HAProxy "activating", joined that start and answered success, and HAProxy
  kept the package's config; the hash was stored, so nothing was pushed again. A single appliance had the same
  check and the same apply. Now:
  - an appliance is ready when dpkg reports every package `install ok installed`, no `apt-get`, `apt` or `dpkg`
    runs and the units exist;
  - the apply starts HAProxy when it does not run and reloads it through its master socket in every case, wants
    `Success=1`, and then wants every `bind` of the pushed file to be a socket HAProxy listens on. If any of it
    fails the old file goes back, the row says `ERROR` with the reason, no hash is recorded and the next pass
    pushes again. keepalived's apply wants keepalived running after the change;
  - a recorded hash is checked on every pass: each appliance is asked whether HAProxy listens on every bind of its
    config. One that does not is not `up` (the pair is `DEGRADED` or `DOWN`, a single appliance `DOWN`, with the
    reason) and is pushed again in that pass. A pair's own hash, by which a cluster's Services are called ready,
    is recorded only when an appliance is `up` with the config: two appliances that both hold the address read
    `DEGRADED` whatever they run;
  - `GET_HEALTH` says so too: a member the stored config holds and the running HAProxy does not has status
    `NOT LOADED` (it was `""`, which reads as nothing wrong), and the answer carries `notLoaded` and `message`.

  A single appliance that is in line is asked once per pass and not pushed; its config and its hash are what they
  were. `iproute2` joins the install line (the listen check uses `ss`; stock cloud images have it).
  `test/integration/lb_appliance_test.go` runs the product's pushes and reads against a guest of Debian 13 and of
  Ubuntu 26.04 with the distribution's systemd, dpkg, HAProxy and keepalived, including a push while the package's
  start is under way, where the apply of `0.11.0-rc.1` answers success with nothing listening. Not run on a lab.
- **A stopped keepalived or HAProxy on a running appliance is started by the next pass**, and the message of a
  `DEGRADED` pair names the cause and what puts it right. It said "Replace appliances builds the missing one again"
  for a stopped service, a stopped VM and an appliance still installing, where nothing is missing; that sentence
  now appears only when an appliance is gone.
- **Files written on an appliance through the guest agent are root's, mode 0644, the two scripts 0755** (they were
  0666: the agent creates a file with that mode). The apply sets and checks the mode. An appliance built before
  gets it with its next config change or with Replace appliances.
- **During Replace appliances the row no longer reads `ACTIVE` with one appliance listed**: it is `DEGRADED`, "One
  appliance carries the traffic while the other is being replaced", until both are up.
- **A load balancer appliance whose build failed no longer stays behind**: a VM the failed build could not remove
  is destroyed before the next try (pair create, replace, a cluster's pair).
- **The VM inventory no longer treats an address owned by a load balancer as an orphan**: such a row (no VM lists
  it) would have stopped the project's floating IP reconcile, so its address was never billed and no other address
  change of the project reached its rows (`orphanIPs`, `ReleaseIfUnchanged`). **A pair's traffic is counted once**:
  one counter per (appliance, balancer) on the appliance's public NIC, never one per address.
- **A new sandbox node installs gVisor again** (`deploy/sandbox-cluster/node-setup.yaml`, docs/managed-sandbox.md
  §1.1). The installer's `node-setup` DaemonSet asked the gVisor apt suite `release` for runsc 20261102.0, which the
  suite no longer lists (it carries the newest release only), so the install failed on every new node. It now
  downloads the release's own package from its versioned pool path, checks its SHA256 and installs it with
  `dpkg -i`: runsc **20261116.0** (the newest release), `RUNSC_SHA256` `495796a5…caeb1e`, both set together. It
  removes the apt source earlier versions wrote. Nodes keep the IPv6 refusal, the drop-in containerd config and
  stage 55's edits of the runtime image and `RUNSC_ALLOW_SUID`. The docs no longer say the node image ships the
  `gvisor` handler: the drop-in writes it. Not run on a lab.

### Changed

- **New tenant clusters get cluster chart `proxmox-kamaji-cluster` 0.1.17** (`tenantCharts.kubernetes`, digest
  `sha256:dfdccf67de36238f4ae7e7914035f99f394fb6f45ed80540d6e1a2bf5076e914`, published from the tag
  `proxmox-kamaji-cluster-v0.1.17`), and with it the load balancer controller where every gate holds. The platform
  cluster (`pc-platform`) stays on 0.1.16. On an installed platform the move is record only: the upgrade run writes
  the chart on the Kubernetes provider, and an existing tenant cluster keeps its chart until its own update. From
  the BOMs `0.10.1-rc.2` to `0.10.2` the plan gains that one record-only row (`TestPreviousBOMsMoveCertManager`).
- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.10.2`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.2.yaml`, sha256
  `78dbc9a79150ea71d77d460e8679ba94d1d7484c82e6f6bdcbcbf4560cdf3dee`, the file of the tag byte for byte). The `v0.10.2` release
  commit left the BOM at `0.10.2`, and the release gate refuses only `-dev`. A platform installed by `v0.10.2` or
  moved to its BOM is not refused (`bom_unsupported`) under a later build while the file is carried. Its pins are
  those of `main`, so the plan from it has one change, the release, record only. `TestPreviousBOMsMoveCertManager`
  states that answer, and `TestUpgradeRollsNoMachine` compares five of the fourteen previous BOMs with the BOM on
  `main`, none of them rolling. No apply on such a platform was run on a lab by this change.
- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.11.0-rc.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.0-rc.1.yaml`, sha256
  `ca2313a651ccc9c99b37fd1c6d54d1ce09eb9220b3be74c4a70aa930f740a26d`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.11.0-rc.1`, and the release gate refuses only `-dev`. A platform moved to
  the candidate's BOM (its drill) is not refused (`bom_unsupported`) under a later build while the file is carried.
  Its pins are those of `main`, so the plan from it has one change, the release, record only.
  `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares six of the fifteen
  previous BOMs with the BOM on `main`, none of them rolling. No apply on such a platform was run on a lab by this
  change.
- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.11.0-rc.2`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.11.0-rc.2.yaml`, sha256
  `1c31634d18e5888d0974cf34e6b06c7bc6563c0d9035b326cc0e70ac41e72705`, the file of the tag byte for byte). The
  second candidate's commit left the BOM at `0.11.0-rc.2`, and the release gate refuses only `-dev`. A platform
  moved to that BOM (the candidate's drill) is not refused (`bom_unsupported`) under a later build while the file is
  carried. Its pins are those of `main` and of `0.11.0-rc.1`, so the plan from it has one change, the release,
  record only. `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares seven
  of the sixteen previous BOMs with the BOM on `main`, none of them rolling. No apply on such a platform was run on a
  lab by this change.
- **The platform BOM's release is `0.11.0`** (`deploy/platform/platform-bom.yaml`), with the pins of `0.11.0-rc.2`:
  the two files differ in the `release` line alone. No chart of the product changed since its tag (`git diff
  proxmox-kamaji-cluster-v0.1.17 -- deploy/charts/proxmox-kamaji-cluster`, `git diff database-cluster-v0.5.2 --
  deploy/charts/database-cluster` and `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print
  nothing), so no chart tag is needed. The release gate passes on it and on the sixteen previous BOMs. The app chart
  is `0.11.0`, appVersion `v0.11.0`, and the installer's ProxCloud pin and `DEFAULT_VERSION` of `proxcloud-up.sh` name
  `v0.11.0`. This BOM carries no unfinished replacement (`TestRollTowardAKeptCandidateGoesOn` passes under it
  unchanged).
- **The guides give what the two lab drills showed** (docs/cloud-integration.md, docs/managed-k8s.md,
  docs/managed-services.md, deploy/compose/README.md): the figures of `v0.11.0-rc.2` next to those of `v0.11.0-rc.1`,
  what the corrections of #424 and #425 did on a lab, and what neither drill ran.

### Known issues

- **No UDP listeners and no TLS termination.** A UDP port of a Service is refused with the Event `UDPNotSupported`, a
  listener is plain TCP or HTTP, and TLS passes through to the members. Both are planned for the next release,
  "complete layer 4".
- **Layer 4 and plain HTTP only**: no routing by host or path. **IPv4 only**: an IPv6 Service is refused, with an
  Event.
- **One appliance pair per cluster**, shared by all its Services, each with its own address.
- **A worker added or removed loses requests for seconds.** A new worker is a member as soon as its Node exists,
  before it is Ready, and HAProxy takes a new member for healthy until two checks fail: on the lab a scale-up lost 22
  of 44 requests in 4.4 seconds and a scale-down 52 in 10.3 seconds. The same happens at every worker replacement (a
  machine roll, a Kubernetes upgrade of the workers). Measured once per direction.
- **A member that is down gets traffic again after every reload**, until its checks fail: a Service with
  `externalTrafficPolicy: Local` on a cluster where a node has no local endpoint (NGINX Gateway Fabric's chart
  default), and any Service while a node is dead. On the lab such a Service lost 11 to 12 requests in 2.2 to 2.4
  seconds at each change of another Service of the same cluster.
- **In-cluster clients do not pass the pair.** A pod that calls a Service's public address is answered inside the
  cluster (`ipMode: VIP`): it sees neither a pair outage nor the source ranges. Only a PROXY protocol Service
  (`ipMode: Proxy`) goes out through the pair.
- **Five public addresses by default** (`floatingIps: 5`): in default mode every Service of type LoadBalancer and
  every Gateway takes one, and the sixth waits with `QuotaExceeded` until the project's quota or the provider's
  default quota is raised.
- **What backups hold.** A Proxmox backup or snapshot of an appliance holds its disk: the HAProxy config with the
  listeners and the members' addresses, and the keepalived config. The platform writes no secret to an appliance in
  this release, so such a backup holds none of the portal's. Restoring an appliance from one was not tested: its
  config comes from the portal, and for a pair Replace appliances builds fresh ones. The portal's `backup` writes
  the database dump and a copy of `.env` side by side: the dump holds every balancer's row and a hash of each
  cluster's gateway secret, and `.env` holds the key that opens the provider credentials in the dump. Whoever has
  both files has the portal's secrets. Neither holds the volume `gateway`.
- **A cluster's first Service shows its address before it answers**: 3 minutes 12 seconds earlier on the lab. Tools
  that wait for the address (`helm --wait`, Argo CD health, external-dns) take the Service for ready too early; the
  Event stays `EnsuringLoadBalancer` until it serves. The first Service waits for the pair (6 minutes 37 seconds on
  the lab until it answered, with packages from apt); a second Service of the same cluster answered after 32.
- **Builds and replaces take minutes** (7 to 13.5 on the lab): a stock image runs its own first-boot upgrade and the
  platform waits for the package manager. A config change also waits while the package manager runs on an appliance;
  the other appliance takes it first and the row reads `DEGRADED` until both have it.
- **A tenant's pair is repaired by the services sync**, not at once: a stopped appliance or service is started at the
  next sync, and the other appliance carries the traffic meanwhile. A single appliance has no other one. A stopped
  appliance of a cluster's pair is started within about 70 seconds (31 in the lab's one run).
- **A paused VM makes its project's inventory unproved** for as long as it is paused, which includes every project the
  operator suspends: the project's own rows are frozen and the provider's seed sweep and sandbox sweep are held back
  (the load balancer pass and the provider's other passes run). Not new in this release. A paused appliance is not
  resumed by the platform, and the row of a pair whose former master is paused says "Both appliances hold the address"
  instead of the cause.
- **A change cut by an API stop waits about 3 minutes** (190 to 209 seconds on the lab; traffic was not touched). A
  Replace's mark can outlive a dead API for up to 60 minutes.
- **One node is not node HA**, and a switch cuts open connections (no connection state sync between the appliances).
- An address released before a sync saw it is not billed. A new cluster's first MetalLB address can take about 10
  minutes (not new). A cluster's state over 64 KiB is refused whole (around 45 Services with several ports or long
  source ranges). A gateway URL that is a host name leaves the controller's egress port open to any address. Enable
  during a cluster's delete can mint its credential again. A Service deleted and created again gets a new address.
  Cluster appliances are not checked against the server quota.
- The known issues of 0.10.2 still hold: a platform on a BOM before `0.10.1` cannot take this move, from the BOM
  `0.10.1` there is no way back (cert-manager), the installer path is not drilled, a seed backup on an NFS store can
  be readable by everyone, Cilium is outside its own support matrix on 1.36, and the tenant cluster chart's
  cert-manager addon stays at `v1.20.2`.

## [0.11.0-rc.2] - 2026-11-22

The second release candidate of 0.11.0, published as a prerelease and never as `latest`. It is not the release:
`v0.10.2` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.0 above: the corrections of the full lab drill of `v0.11.0-rc.1`
(#424, #425) and the sandbox node fix (#420) next to what that candidate held (#415 to #418). It exists so that the
lab can run the drill again on the corrected code: none of the corrections had run on a lab when it was tagged. The
entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.11.0-rc.2`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.11.0-rc.1`: those of `0.10.2` but `tenantCharts.kubernetes`, cluster chart `proxmox-kamaji-cluster` 0.1.17
  (`sha256:dfdccf67de36238f4ae7e7914035f99f394fb6f45ed80540d6e1a2bf5076e914`). No chart of the product changed
  since its tag (`git diff proxmox-kamaji-cluster-v0.1.17 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing), so no chart tag is
  needed. It is a release BOM, and the release gate passes on it and on the fifteen previous BOMs.
- **The plan from each previous BOM** (`PlanFor`, one control plane machine and one machine per group). From
  0.11.0-rc.1 one change, the release, record only, allowed, with no part, no seed row and no machine replaced;
  phrase `upgrade <name> to 0.11.0-rc.2`. From the fourteen older BOMs the answers of `0.11.0-rc.1`: two changes
  from 0.10.2 and 0.10.2-rc.1 (the release and `tenantCharts.kubernetes` 0.1.16 to 0.1.17, both record only), four
  from 0.10.1 and 0.10.1-rc.2 (those and `cert-manager` on the seed and on the platform cluster), five from
  0.10.1-rc.1 (also `pc-platform` 0.1.15 to 0.1.16), and a refusal by the pair gate from the nine BOMs on
  Kubernetes 1.35.10 (21, 22 or 25 changes). This BOM carries no unfinished replacement
  (`TestRollTowardAKeptCandidateGoesOn` passes under it unchanged).
- **What rc.1's drill showed** (one lab, 2026-11-21; a nested three-node Proxmox cluster, every appliance on one
  node, a public test range on a plain bridge, Ubuntu 26.04 appliances with packages from apt through the lab's
  router). It passed: a pair made by a tenant (`ACTIVE` 8 min 16 s after the request), 15 failovers of that pair,
  every one under its target (guest shutdown 0.8 s; VM stopped hard 2.8 to 3.2 s; keepalived stopped 0.7 s; HAProxy
  stopped 2.2 to 2.7 s; paused and resumed 3.2 to 3.5 s), the `vrrp` rule written back by the sync, one floating IP
  row per address and one traffic counter per appliance, `ha: false`, a new cluster in default mode (ready after
  7 min 42 s; first Service's address after 4 min 22 s, answering from outside after 6 min 7 s), class
  `proxcloud.io/internal` on MetalLB, UDP, no node port, `loadBalancerIP` and the sixth address refused or warned
  with Events, PROXY v2, policy Local, members following the workers (8 s for a new one), a dead master of the
  cluster's pair (3.1 to 3.4 s), an idle connection of 330 s, a node outside the workers and a rotated credential
  refused, a suspended project, the gateway down during a Service's delete, the clean-up at a Service's and a
  cluster's delete, and the enable of an existing cluster in class-only mode with its MetalLB address unchanged. It
  failed twice. Replace appliances left the address refusing every connection for 458 s while the row read
  `ACTIVE`: the first config push met the HAProxy package's own first start. And a stopped master appliance was
  started again by the services sync in 1 of 6 tries, with the row still `ACTIVE`: the sync had cached the pair's
  address in the appliance's SERVER row, and left out its load balancer pass and the provider passes of every
  tenant while the VM was down. It also found the 20-minute cluster lock (24.7 minutes without a converge after an
  API restart), a full pass on every controller call (96 Proxmox calls a minute per cluster), stopped services
  never started, files of mode 0666, and refused credentials without a trace.
- **What rc.2 changes** (in detail under 0.11.0, Fixed): a config is recorded only when the appliance serves
  it, a recorded hash is checked on every pass, stopped services are started, file modes are set, the row is
  stated during a Replace (#424); the services sync keeps running when an appliance is down, the cluster lock is
  a one-minute lease, an unchanged controller call starts no pass, a refused credential is logged and audited
  (#425). One consequence: a stopped appliance of a cluster's pair is started within about 70 seconds, where the
  drill measured 2 to 6.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION`
  stay on `v0.10.2`.

### Verified on a lab

On `v0.11.0-rc.1` only, as listed above and in `docs/releases/v0.11.0-rc.2.md`; nothing was run again on this
candidate. Also on rc.1: the portal upgrade from `v0.10.2-rc.1` (28 s), the platform run 0.10.2-rc.1 to 0.11.0-rc.1
(two record-only rows, 39.6 s, no Proxmox write) and `--cluster-gateway` on and off (14 s each, six switches).

### Not verified

- The corrections of this candidate (#424, #425), on a lab: tested against the distributions' systemd, dpkg,
  HAProxy and keepalived in a container and against a fake Proxmox. The second drill comes next.
- A port added to or removed from a live Service; an outside address refused by a listener's source ranges.
- Two nodes: VRRP between nodes, placement on two nodes, the loss of a node, open connections across a switch.
- The consoles (the drill used the API), a production site, the default Debian 13 appliance image on a lab, a
  balancer made before 0.11.0 under this code, NGINX Gateway Fabric in default mode, the installer path, the
  sandbox node fix (#420).
- Any move to the BOM `0.11.0-rc.2` (plans read, not run), and the candidate's images and its tag's workflows:
  they do not exist before the tag.

### Known issues

- A UDP port is refused and no listener terminates TLS: both are planned for the next release, "complete layer 4".
- One pair per cluster, shared by all its Services.
- An in-cluster client of a Service with `ipMode: VIP` is answered inside the cluster and never passes the pair.
- The built-in project quota is five public addresses (`floatingIps: 5`): the sixth Service or Gateway waits with
  `QuotaExceeded` until the quota is raised.

## [0.11.0-rc.1] - 2026-11-21

The first release candidate of 0.11.0, published as a prerelease and never as `latest`. It is not the release:
`v0.10.2` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.11.0 above (#415, #416, #417, #418): a load balancer on a pair of
appliances, the load balancer controller for managed Kubernetes clusters, the Compose gateway listener and the two
billing fixes. It exists so that a lab can run the drill of those: none of them has run on a lab on this code. The
entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.11.0-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had:
  those of `0.10.2` but `tenantCharts.kubernetes`, cluster chart `proxmox-kamaji-cluster` 0.1.17
  (`sha256:dfdccf67de36238f4ae7e7914035f99f394fb6f45ed80540d6e1a2bf5076e914`, published from the tag
  `proxmox-kamaji-cluster-v0.1.17`). No chart of the product changed since its tag
  (`git diff proxmox-kamaji-cluster-v0.1.17 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing), so no chart tag is
  needed. It is a release BOM, and the release gate passes on it and on the fourteen previous BOMs.
- **The plan from each previous BOM** (`PlanFor`, one control plane machine and one machine per group). From 0.10.2
  and 0.10.2-rc.1, two changes, allowed, with no part, no seed row and no machine replaced: the release and
  `tenantCharts.kubernetes` 0.1.16 to 0.1.17, both record only (the run writes the chart on the Kubernetes provider;
  new tenant clusters get it, an existing one keeps its chart). Phrase `upgrade <name> to 0.11.0-rc.1`. From 0.10.1
  and 0.10.1-rc.2, four: those and `cert-manager` on the seed and on the platform cluster (chart, wave 1), as the
  move to 0.10.2 had. From 0.10.1-rc.1, five: those and `pc-platform` 0.1.15 to 0.1.16. From the nine BOMs on
  Kubernetes 1.35.10 (21 changes from 0.10.0 and 0.10.0-rc.1, 22 from 0.9.0, 0.9.1 and the candidates of 0.9.0, 25
  from the three 0.8.x BOMs) the upgrade is refused: both cert-manager rows carry the pair gate's sentence. Such a
  platform upgrades to 0.10.1 with the `v0.10.1` build first, then to 0.10.2 or to this BOM.
- **This BOM carries no unfinished replacement**, as 0.10.2's: an installation on 0.10.0 that stands in the
  replacement of its machines toward 0.10.1-rc.1, 0.10.1-rc.2 or 0.10.1 goes on to that BOM with a Retry (this build
  carries the files); `TestRollTowardAKeptCandidateGoesOn` passes under it unchanged.
- **What the upgrade does not do.** The platform cluster stays on cluster chart 0.1.16, no tenant cluster changes
  chart, mode or MetalLB, and no load balancer of a tenant is rebuilt. The pair and the controller come from the
  portal's code: the pair for new balancers as soon as the portal runs this release, the controller for clusters
  created after the platform's upgrade on a Kubernetes provider with `config.cluster.loadBalancer`, and by the
  operator's **Enable load balancer controller** (class-only) on an existing one.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION` stay
  on `v0.10.2`. `--cluster-gateway` takes `v0.11.0-rc.1` (the script compares the version without its `-rc.N`).

### Verified on a lab

Only the spike that preceded the code, not this candidate: on 2026-11-21, on the lab portal of `v0.10.2-rc.1`, two
tenant servers on one node set up by hand as a pair (keepalived over SSH, the VRRP rule and the address's filter
entries written by hand and removed after) measured the failover the code is built on: a stopped master 0.7 to
0.8 s, a hard-reset (dead) master 3.1 to 3.5 s, keepalived stopped 0.7 to 0.9 s, HAProxy stopped and left down 2.0
to 2.6 s, a killed HAProxy restarted by systemd with no switch; adding an address with a keepalived reload and three
HAProxy reloads lost no request.

### Not verified

- Everything in this candidate's code, on a lab: the pair as the product builds it (create, status, apply, replace,
  delete), the VRRP rule and filter entries the product writes, the gateway, the controller, chart 0.1.17 on a
  tenant cluster, the enable action, the Compose gateway listener, the consoles.
- A pair on a public network, across two nodes, through the loss of a node, and open connections across a switch.
- The two billing fixes on a real site: covered by tests only.
- Any upgrade to the BOM `0.11.0-rc.1` (plans read, not run).
- The candidate's images and its tag's workflows: they do not exist before the tag.

## [0.10.2] - 2026-11-21

The release after `v0.10.1`. Its BOM moves cert-manager from `v1.20.2` to `v1.21.3` on the seed and on the platform
cluster of a managed-services platform, by one upgrade run that replaces no machine, and nothing else. It was
prepared by the release candidate `v0.10.2-rc.1`, whose section below is kept; everything it held is in this
section. The entries under Added and Changed were written when each change merged, before the drill: where one says
"Not run on a lab", the two lists below are the current state.

- **cert-manager moves from `v1.20.2` to `v1.21.3`** on the seed and on the platform cluster, from the BOM `0.10.1`:
  the first third-party chart an upgrade moves (Added, Changed). There is no way back.
- **A security fix.** `v1.20.2` is affected by GHSA-8rvj-mm4h-c258 (high: a direct ACME Challenge could bypass an
  Issuer's DNS01 solver policy and use a ClusterIssuer's DNS credentials), fixed upstream since `v1.20.3`; `v1.21.3`
  has the fix.
- **Inside its support table again.** cert-manager 1.21 lists Kubernetes 1.33 to 1.36, which covers the platform's
  1.36.6 and the installer's 1.35.10.
- **A light run.** No machine is replaced, nothing is written on Proxmox and no seed backup is asked for. On the lab
  the run took 3 minutes 8 seconds and issued no Certificate again.
- **The Check row `certificates`** names a cert-manager Deployment that is not available or a Certificate that is not
  Ready. A warning only (Added).
- **The installer path moves too** (`cert_manager_chart: v1.21.3`): stage 40 at `make upgrade`, stage 50 at the next
  `make install`. Not drilled.
- **The guide and the notes give what a production site showed on `v0.10.1`**: databases made before database chart
  0.5.2 must be moved to it before a platform part, the figures of a one-node roll, a transient "Error deleting Node"
  and the mode of a seed backup on an NFS store (Changed).

The platform BOM of this release is `0.10.2`. It pins what the candidate `0.10.2-rc.1` pins, and the two BOMs differ
in the `release` line alone: the pins of `0.10.1` but cert-manager, `v1.21.3` on the seed and on the platform cluster
at the chart mirror's digest `sha256:0f159f17f9ed152737a96aa113f042c43ce1b28061660d14aef7d5c6c2a88600`, with
`upgradeFrom: ["v1.20.2"]`. No k3s, Kubernetes version, node image, chart of the product, Cluster API provider or
clusterctl image moves. The release carries the BOMs of `0.8.0`, `0.8.1`, `0.8.2`, `0.9.0-rc.1`, `0.9.0-rc.2`,
`0.9.0`, `0.9.1`, `0.10.0-rc.1`, `0.10.0`, `0.10.1-rc.1`, `0.10.1-rc.2`, `0.10.1` and `0.10.2-rc.1` for the platforms
those builds installed.

Upgrading from `v0.10.1`. From an older release, read the sections of `v0.10.1` and `v0.10.0` as well: their steps
still apply.

- **A platform on a BOM before `0.10.1`** (0.8.0 to 0.10.0 and their candidates, all on Kubernetes 1.35.10) is
  refused by this build: both cert-manager rows say to finish an upgrade to a release with this one's Kubernetes
  version, k3s and node image first. Keep the portal on `v0.10.1` and move the platform to `0.10.1`, both parts,
  first. What a production site of one Proxmox node showed on that move (2026-11-20): a database made before
  database chart 0.5.2 (one instance, with a PodDisruptionBudget) blocks the part "platform" with `drain_blocked`;
  move it to 0.5.2 first with the operator route `POST /api/v1/admin/service/{id}/db-clusters/{clusterId}/bump-chart`
  (on CloudNativePG 1.30.1 it did not restart the database). With the node image already a published template there
  was no import; each clone took 6 to 8 seconds on NVMe; the platform API was away 67 seconds at the hand-over, the
  tenant API 104 seconds in the `kamaji` unit and the database 75 seconds in the `dbaas` unit. Cluster API's text
  "DeletingNode: Error deleting Node" showed for about 50 seconds during the control plane's hand-over and cleared
  by itself. A seed backup (`vzdump`) to an NFS store came out mode 777: `chmod 600` the archive and its log.
- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.10.2`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. `compose.yaml` did not change since
  `v0.10.1`: on the lab the upgrade to the candidate took 24 seconds and PostgreSQL, Keycloak and Traefik kept their
  containers. The platform stays on its BOM until an operator starts the move.
- **Before the move:** back up the cert-manager resources on the seed and on the platform cluster (upstream's
  advice; there is no way back): `kubectl get certificates,issuers,clusterissuers -A -o yaml > cert-manager-backup.yaml`
  with each cluster's kubeconfig. Read the Check row `certificates`: it should be `ok`.
- **The run**, phrase `upgrade <name> to 0.10.2`, one action with no parts. From `0.10.1` or `0.10.1-rc.2` the plan
  has three rows: the release (record only) and `cert-manager` on the seed (stage `seed-platform`) and on the platform
  cluster (stage `workload-platform`), class chart, wave 1. ArgoCD applies the CRDs in place (`v1` stays the only
  version), removes the Role and RoleBinding `cert-manager-tokenrequest`, and rolls the controller, cainjector and
  webhook Deployments together, each keeping its old pod until the new one is Ready. The wave waits until ArgoCD
  reports the three Deployments rolled and the sync, the chart's `startupapicheck` Job included, Succeeded. On the
  lab (one sample): 3 minutes 8 seconds, `seed-platform` 98 seconds, `workload-platform` 84 seconds, the new pods Ready
  20 to 53 seconds after their ReplicaSet, every Deployment with a Ready pod at every look, none of the 11
  Certificates issued again, no webhook refusal, no failed look of a tenant API or a database. A seed of one VM
  answered slowly for about 30 seconds while it pulled the new images; nothing was refused.
- **From other BOMs** (`PlanFor`): from `0.10.2-rc.1` one change, the release, record only; from `0.10.1-rc.1` five,
  the three above, the tenant chart (record only) and `pc-platform` 0.1.15 to 0.1.16, no machine replaced; from the
  nine BOMs on 1.35.10 refused (21, 22 or 25 rows). A platform that stands in the replacement of its machines toward
  `0.10.1` or one of its candidates goes on to that BOM with a Retry first. Only the move from `0.10.1` (to the
  candidate) was drilled.
- **A site installed with the Ansible installer**: stage 40 moves the management cluster's cert-manager at `make
  upgrade` (its wait can pass on the old Deployment: read the Application `cert-manager` for Synced and Healthy at
  `v1.21.3`), stage 50 the services cluster's at the next `make install`, where plugin-barman-cloud also has two
  Certificates and an Issuer. Back up the cert-manager resources first. Not drilled.
- The app chart is `0.10.2` with appVersion `v0.10.2`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.10.2`. The installer's k3s and Kubernetes pins stay at 1.35.10.

Verified on a lab. One drill on the published images of the candidate `v0.10.2-rc.1` (2026-11-21), on one lab with a
seed of one VM and a platform cluster of four nodes on 1.36.6, checked afterwards by a read-only audit:

- The portal upgrade from `v0.10.1` to `v0.10.2-rc.1` by the script, in 24 seconds, with PostgreSQL, Keycloak and
  Traefik kept.
- The plan from `0.10.1` (three rows, no parts, no seed backup asked) and the Check row `certificates`.
- The run from `0.10.1` to `0.10.2-rc.1` in 3 minutes 8 seconds with no retry (`seed-platform` 98 seconds,
  `workload-platform` 84 seconds).
- cert-manager `v1.21.3` on the seed and on the platform cluster: the CRDs moved in place (`v1` only, storedVersions
  `[v1]`), the tokenrequest Role and RoleBinding removed, the three Deployments rolled with a Ready pod at every look,
  and the `startupapicheck` Job in both syncs.
- No Certificate issued again (11 of 11), no webhook refusal (cert-manager's and Cluster API's), the CA bundles
  unchanged, and no self-heal loop in ArgoCD.
- A tenant cluster at 1.36.6 and a database with no failed look, no tenant pod restarted, no machine replaced and
  nothing written on Proxmox.
- After the move: a new Certificate in about 1 second, a renewal in 1.5 to 1.7 seconds with the new CA on the webhooks
  within 3.4 seconds, and a new tenant cluster and database created and deleted.

Not verified on a lab:

- The installer path: stage 40 and stage 50, new installs, and the services cluster's barman plugin with `v1.21.3`.
- The moves from `0.10.1-rc.1` and `0.10.1-rc.2`: their plans were read, not run.
- The refusal on a real platform on a BOM from `0.8.0` to `0.10.0`: a unit test only.
- A site that already has the `v1.21.3` images: the `startupapicheck` Job may then test the old webhook; the gate on
  the Deployments rolled still holds.
- A failed `startupapicheck`, the path to `component_unhealthy` and the Retry after it.
- A seed of more than one node, and a production site of one node: the figures are one lab's.
- Any way back.

Where an entry below says the BOM on `main` is `0.11.0-dev`, that was the state at that merge: this release's BOM
is `0.10.2`, and the change after its tag sets `main` back to a `-dev` one.

### Added

- **An upgrade may move cert-manager, the first third-party chart it moves.** On the seed and on the platform
  cluster the cert-manager entry moves as a chart of the product does: only to a version whose BOM entry lists the
  installed one in `upgradeFrom` (the release read upstream's upgrade notes for that pair and compared the render,
  `TestCertManagerMoveRenders`), as class `chart` in its wave, and only in an upgrade that changes no k3s, Kubernetes
  version or node image. Next to such a change each cert-manager row is refused with a sentence that says to finish
  an upgrade to a release with this one's Kubernetes version, k3s and node image first, with that release's build.
  The seed's charts sync after its k3s and the platform cluster's after the roll, so in one run the old chart would
  meet the new Kubernetes first. The run's gate is the wave's: the Application Synced and Healthy with its sync, and
  the chart's `startupapicheck` hook in it, Succeeded. No machine is replaced, nothing is written on Proxmox and no
  seed backup is asked for; there is no way back, and the plan names upstream's advice to back up the cert-manager
  resources first. Every other third-party chart stays refused, also when its entry lists `upgradeFrom`. This BOM
  pins no new cert-manager yet: the pin follows once the chart mirror serves v1.21.3. Tested against fakes and
  renders only; not run on a lab.
- **Preflight row `certificates`**: `ok` when cert-manager's controller, cainjector and webhook Deployments are
  available on the seed and on the platform cluster and every Certificate there is Ready, a warning that names each
  one that is not. It never fails and no run waits on it, so a Certificate broken before an upgrade does not hold
  the run ([api.md](docs/api.md)).

### Changed

- **The platform BOM pins cert-manager v1.21.3 from the chart mirror, on the seed and on the platform cluster**
  (`deploy/platform/platform-bom.yaml`: digest `sha256:0f159f17f9ed152737a96aa113f042c43ce1b28061660d14aef7d5c6c2a88600`,
  the manifest digest the `charts-mirror-v2` run pushed, read again anonymously by `bom_verify.py` and `helm pull`;
  both entries list `upgradeFrom: ["v1.20.2"]`). v1.21.3 lists Kubernetes 1.33 to 1.36, which closes the caution that
  v1.20.2 runs outside its list on 1.36. The upgrade from the BOM of 0.10.1 or 0.10.1-rc.2 plans three rows: the
  release (record only) and cert-manager on the seed and on the platform cluster (chart, wave 1); from 0.10.1-rc.1
  also `pc-platform` 0.1.15 to 0.1.16 and `tenantCharts.kubernetes`; no seed row, no machine replaced, no Proxmox
  write. Every BOM on Kubernetes 1.35.10 (0.8.0 to 0.10.0 and their candidates) is refused by the pair gate: such a
  platform upgrades to 0.10.1 with the `v0.10.1` build first. `TestPreviousBOMsMoveCertManager` states each answer
  by name; `TestCertManagerMoveRenders` now pulls both versions from the mirror by the BOM's digest. **The installer
  moves too** (`versions.yml` `cert_manager_chart: v1.21.3`): stage 40 upgrades the management cluster's cert-manager
  at `make upgrade`, and the services cluster's moves at the next `make install` (stage 50), where plugin-barman-cloud
  also uses it; no drill covers the installer path ([install.md](docs/install.md)). The `cert-manager` file of
  `images.clusterctl` stays at v1.20.2 until the image's next rebuild: clusterctl skips a cert-manager that answers,
  whatever its version, so `test_clusterctl_image.py` now checks only that its url names the version it states. Not
  moved: the tenant cluster chart's cert-manager addon (off by default). Not run on a lab.
- **The chart mirror's manifest names cert-manager v1.21.3** (`deploy/platform/charts-mirror.yaml`, upstream
  https://charts.jetstack.io, index digest `sha256:9ec67938b2981d8b333af61b3fcf53e26b4f6f5deb7ccda3de4ea116bb373a17`,
  which is the sha256 of the downloaded `.tgz`). The BOM still pins v1.20.2: the next `charts-mirror-v<n>` tag
  publishes v1.21.3, and a later change pins its mirror digest. `test_charts_mirror.py` now lets the manifest be
  ahead of the BOM for a chart the BOM takes from the mirror (the BOM's version or a greater MAJOR.MINOR.PATCH; an
  older one, or the same three numbers with another suffix, fails), so a new version can be published before the
  BOM names it. v1.20.2 stays published: the mirror run only pushes, and older BOMs pin it. docs/ci-release.md, "A
  new version of a mirrored chart", gives the order: manifest pull request, tag, pin pull request.

- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.10.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.1.yaml`, sha256
  `aec05ea60e6bfd12d73bab3d45a6a2aae8a3f62f3a41d3f43737aa8ef1d74e3b`). The `v0.10.1` release commit left the BOM at
  `0.10.1`, and the release gate refuses only `-dev`. A platform installed by `v0.10.1`, moved to its BOM, or standing
  in the replacement of its machines with that BOM as its target (what a Retry on the release's build records) is not
  refused (`bom_unsupported`) under a later build while the file is carried. Its pins were those of `main`, so the
  plan from it had one change, the release, record only, and `TestUpgradeRollsNoMachine` compared twelve previous
  BOMs with the BOM on `main`, nine of them at step 0 of a roll (until the cert-manager pin above: since then three,
  none of them rolling). `TestRollTowardAKeptCandidateGoesOn` now also takes an installation on `0.10.0` in a
  replacement toward `0.10.1` on to `0.10.1` with a Retry, nothing to confirm. No apply on such a platform was run on
  a lab by this change.
- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.10.2-rc.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.2-rc.1.yaml`, sha256
  `76140de70df83ee767c07b7b2e63893feaf2ef4bed95b4cbdf58ba8973e38a6a`, the file of the tag byte for byte). The
  candidate's commit left the BOM at `0.10.2-rc.1`, and the release gate refuses only `-dev`. A platform moved to
  the candidate's BOM (its drill) is not refused (`bom_unsupported`) under a later build while the file is carried.
  Its pins are those of `main`, so the plan from it has one change, the release, record only.
  `TestPreviousBOMsMoveCertManager` states that answer, and `TestUpgradeRollsNoMachine` compares four of the thirteen
  previous BOMs with the BOM on `main`, none of them rolling. No apply on such a platform was run on a lab by this
  change.
- **The plan's impact text of a cert-manager row gives the drill's figures** (`internal/platformlife/plan.go`, the
  goldens): in place of "not measured on a lab yet", one lab's sample (a run of 3 minutes 8 seconds, about a minute
  and a half per cluster, no Certificate issued again, no webhook refusal, a seed of one VM slow for about half a
  minute), and what holds the wave: ArgoCD reports the three Deployments rolled (Healthy against the new chart) and
  the sync, the `startupapicheck` Job included, Succeeded. On the lab ArgoCD created that Job before the new webhook
  pod was Ready on both clusters, so the old text ("the Job checks the webhook, and the wave waits for it") said more
  than the Job proves. The guide's "A cert-manager move" gives the drill's figures and what it did not show.
- **The guide gives what a production site showed on `v0.10.1`** (2026-11-20, one Proxmox node, the move from
  `0.10.0` to `0.10.1`; docs/managed-services.md, docs/managed-dbaas.md). A database made before database chart 0.5.2
  (one instance, with a PodDisruptionBudget) blocks the part "platform" with `drain_blocked` until it is moved to 0.5.2
  with the operator route `bump-chart`; on CloudNativePG 1.30.1 the bump removed the budget without restarting the
  database. The one-node figures of the roll: no image import when the node image is already a published template,
  clones of 6 to 8 seconds on NVMe, the platform API away 67 seconds at the hand-over, the tenant API 104 seconds in
  the `kamaji` unit, the database 75 seconds in the `dbaas` unit. Cluster API's "DeletingNode: Error deleting Node"
  shows for about 50 seconds during the control plane's hand-over and clears by itself. A seed backup to an NFS store
  came out mode 777: the guide says to `chmod 600` the archive and its log after each one.
- **The platform BOM's release is `0.10.2`** (`deploy/platform/platform-bom.yaml`), with the pins of `0.10.2-rc.1`:
  the two BOMs differ in the `release` line alone, and no chart of the product changed since its tag
  (`proxmox-kamaji-cluster-v0.1.16`, `database-cluster-v0.5.2`, `proxcloud-platform-v0.1.3`), so no chart tag is
  needed. The release gate passes on it and on the thirteen previous BOMs. The plan from each previous BOM
  (`PlanFor`, one control plane machine and one machine per group): from 0.10.2-rc.1 one change, the release, record
  only; from 0.10.1 and 0.10.1-rc.2 three, allowed, no part, phrase `upgrade <name> to 0.10.2`; from 0.10.1-rc.1 five;
  from the nine BOMs on Kubernetes 1.35.10 (21, 22 or 25 rows) refused by the pair gate on both cert-manager rows.
  An installation on 0.10.0 that stands in the replacement of its machines toward 0.10.1-rc.1, 0.10.1-rc.2, 0.10.1 or
  0.10.2-rc.1 goes on to that BOM with a Retry, because the move from 0.10.0 to 0.10.2 is refused by the same gate;
  `TestRollTowardAKeptCandidateGoesOn` now takes the pair gate from 0.10.0 as the reason, not a cert-manager row
  between the kept BOM and this one (0.10.2-rc.1 and 0.10.2 differ in the release alone). The app chart,
  `pc_versions.proxcloud` and `DEFAULT_VERSION` of `proxcloud-up.sh` move to `0.10.2` / `v0.10.2`; the installer's
  `cert_manager_chart` stays at `v1.21.3`, its k3s and Kubernetes pins at 1.35.10.
  `deploy/platform/previous/0.10.2.yaml` is added by the change after the tag.

### Known issues

- **There is no way back.** cert-manager documents no downgrade, and a platform does not go back to a BOM. Back up
  the cert-manager resources before the run.
- **Platforms on a BOM before `0.10.1` cannot take this move.** They upgrade to `0.10.1` with the `v0.10.1` build
  first, and a database made before database chart 0.5.2 blocks that move's part "platform" until it is moved to 0.5.2.
- **The installer path is not drilled.** Stage 40's wait can pass before ArgoCD has synced the new version: read the
  Application yourself.
- **A seed of one VM is slow for about half a minute** while it pulls the new images (one lab: answers up to 3.1
  seconds, one client timeout of 4 seconds, nothing refused).
- **A seed backup by hand on an NFS store is readable by everyone** (mode 777 on one site): `chmod 600` the archive
  and its log after each backup.
- **Cilium is still outside its own support matrix on 1.36.** Cilium `1.18.16` lists Kubernetes 1.30 to 1.33. This
  release does not move it.
- **The tenant cluster chart's cert-manager addon stays at `v1.20.2`.** It is off by default; it moves with a later
  tenant chart version.
- **cert-manager 1.20 reaches end of life when 1.22 is released.** A platform that has not made this move stays on
  `v1.20.2`. 1.22 comes in a later release, from `v1.21.3`.
- The known issues of 0.10.1 still hold, except the one about cert-manager outside its support matrix, which this
  move closes for a platform that makes it.

## [0.10.2-rc.1] - 2026-11-21

The first release candidate of 0.10.2, published as a prerelease and never as `latest`. It is not the release:
`v0.10.1` stays the current stable release, and the chart, the installer's ProxCloud pin and the Compose default
still name it. It holds what is under 0.10.2 above (#407, #408, #409, #410), and it exists so that a lab can run
the first upgrade that moves a third-party chart, cert-manager `v1.20.2` to `v1.21.3`: nothing of it had run on a
lab when it was tagged. The entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.10.2-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had:
  those of `0.10.1` but cert-manager, `v1.21.3` on the seed and on the platform cluster
  (`sha256:0f159f17f9ed152737a96aa113f042c43ce1b28061660d14aef7d5c6c2a88600`, `upgradeFrom: ["v1.20.2"]`). No chart
  of the product changed since its tag (`git diff proxmox-kamaji-cluster-v0.1.16 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing), so no chart tag is
  needed. It is a release BOM, and the release gate passes on it and on the twelve previous BOMs.
- **The plan from each previous BOM** (`PlanFor`, one control plane machine and one machine per group). From 0.10.1
  and 0.10.1-rc.2, three changes, allowed, with no part, no seed row and no machine replaced: the release, record
  only, and `cert-manager` on the seed (class chart, stage `seed-platform`, wave 1) and on the platform cluster
  (class chart, stage `workload-platform`, wave 1). Phrase `upgrade <name> to 0.10.2-rc.1`. From 0.10.1-rc.1, five:
  those, `tenantCharts.kubernetes` 0.1.15 to 0.1.16, record only, and `pc-platform` 0.1.15 to 0.1.16 (class chart,
  stage `cluster`, wave 3). From the nine BOMs on Kubernetes 1.35.10 (21 changes from 0.10.0 and 0.10.0-rc.1, 22 from
  0.9.0, 0.9.1 and the candidates of 0.9.0, 25 from the three 0.8.x BOMs) the upgrade is refused: both cert-manager
  rows carry the pair gate's sentence. Such a platform upgrades to 0.10.1 with the `v0.10.1` build first.
- **This BOM carries no unfinished replacement.** An installation on 0.10.0 that stands in the replacement of its
  machines toward 0.10.1-rc.1, 0.10.1-rc.2 or 0.10.1 goes on to that BOM with a Retry (this build carries the three
  files); the move to 0.10.2-rc.1 is offered as `replacement` and refused by the pair gate until the replacement is
  finished. `TestRollTowardAKeptCandidateGoesOn` now states it: under a release BOM that moves cert-manager, as under
  the development BOM, the Retry goes on to the kept BOM itself.
- **The run.** ArgoCD moves cert-manager in the component's wave on each cluster: CRDs in place (`v1` stays the only
  version), the tokenrequest Role and RoleBinding removed, the controller, cainjector and webhook Deployments rolling
  together, each keeping its old pod until the new one is Ready; the wave waits for the sync, the chart's
  `startupapicheck` hook included, to have Succeeded. No machine is replaced, nothing is written on Proxmox and no
  seed backup is asked for. There is no way back; the notes give upstream's backup command
  (`kubectl get certificates,issuers,clusterissuers -A -o yaml`, on the seed and on the platform cluster).
- **Security.** `v1.20.2` is affected by GHSA-8rvj-mm4h-c258 (high: a direct ACME Challenge could bypass an Issuer's
  DNS01 solver policy and use a ClusterIssuer's DNS credentials), fixed upstream since `v1.20.3`; `v1.21.3` has the
  fix. cert-manager 1.21 lists Kubernetes 1.33 to 1.36, so the platform's 1.36.6 and the installer's 1.35.10 are
  inside its table.
- **The installer path moves too** (`cert_manager_chart: v1.21.3`): stage 40 at `make upgrade`, stage 50 at the next
  `make install`, where plugin-barman-cloud also uses cert-manager. No drill covers it.
- **A candidate moves nothing a final moves.** The app chart, the installer's ProxCloud pin and `DEFAULT_VERSION` stay
  on `v0.10.1`; the installer's k3s and Kubernetes pins stay at 1.35.10.

### Verified on a lab

Nothing. The candidate exists for the drill.

### Not verified

- The cert-manager move, entirely, on a lab and on a site with tenants: ArgoCD's sync of the new chart, the
  server-side apply of the CRDs, the rolling Deployments, the `startupapicheck` Job, whether any Certificate is issued
  again, the webhooks of cert-manager, Cluster API and Kamaji through the move, its duration.
- A new or renewed Certificate on Kubernetes 1.36.
- The Check row `certificates` on a real cluster.
- The installer path (stage 40 and stage 50 with `v1.21.3`).
- The moves from 0.10.1-rc.1 and 0.10.1-rc.2 (plans read, not run).
- The candidate's images and its tag's workflows: they do not exist before the tag.

## [0.10.1] - 2026-11-21

The release after `v0.10.0`, and the first whose BOM moves Kubernetes: the seed's k3s from `v1.35.10+k3s1` to
`v1.36.6+k3s1` and the platform cluster from 1.35.10 to 1.36.6, by two operator actions. Tenant clusters and
databases keep their version. It was prepared by two release candidates, `v0.10.1-rc.1` and `v0.10.1-rc.2`, whose
sections below are kept; everything they held is in this section. The entries under Fixed and Changed were written
when each change merged, most of them before a drill: where one says "Not run on a lab", the two lists below are the
current state.

- **The platform moves to Kubernetes 1.36.6** in two actions: "Seed and node image" (the node image import and
  the seed's k3s), then "Platform cluster" (every machine replaced, one unit at a time). There is no way back once
  the new control plane machine has joined (Changed).
- **A replacement that stands after the control plane's unit gets out with one Retry**: cluster chart 0.1.16
  gives Cluster API 30 minutes to delete the old machine's Node, and the run deletes a Node no machine owns
  (Fixed).
- **The wait between two units holds for tenants that are really away**: it reads the Deployments and pods the
  kubelets report, not only the status written by the controllers the same drain evicts (Fixed).
- **The seed's k3s runs three components without leader election**, and the way back across a minor names the
  etcd snapshot restore (Changed).
- **A network with host bits is refused** where it ends in a Kubernetes field, since 1.36 refuses it (Changed).
- **1.36.6 can be offered to tenants** once the operator has smoke-tested and published its node image in Template
  updates; the run does not publish it.

The platform BOM of this release is `0.10.1`. It pins what the candidate `0.10.1-rc.2` pins, and the two BOMs
differ in the `release` line alone: k3s `v1.36.6+k3s1`, Kubernetes 1.36.6 with the node image
`ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4`, and cluster chart 0.1.16, published from its tag
(`proxmox-kamaji-cluster-v0.1.16`), for the platform cluster and for new tenant clusters. Against `0.10.0` nothing
else moves: no Cluster API provider, no clusterctl image, no third-party chart. The release carries the BOMs of
`0.8.0`, `0.8.1`, `0.8.2`, `0.9.0-rc.1`, `0.9.0-rc.2`, `0.9.0`, `0.9.1`, `0.10.0-rc.1`, `0.10.0`, `0.10.1-rc.1` and
`0.10.1-rc.2` for the platforms those builds installed.

Upgrading from `v0.10.0`. From an older release, read the sections of `v0.10.0`, `v0.9.1` and `v0.9.0` as well:
their steps still apply.

- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.10.1`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. `compose.yaml` did not change since
  `v0.10.0`: PostgreSQL and Keycloak keep their containers (seen on the lab at the upgrades to the candidates). The
  platform stays on its BOM and keeps working until an operator starts the move.
- **Before the move:** take a backup of the seed VM in a quiet hour, some minutes before the run (`vzdump <seed
  VMID>`), and a backup of each database you keep. Read the allow-lists of databases, tenant clusters, the provider
  default and the ArgoCD sources for a network with host bits and correct it through the portal (a stored value
  keeps working; a new one with host bits is refused). Look for a backup job, a firewall rule or a note of yours
  that names a platform machine by its VM id: the roll gives the ids of old machines to new ones, possibly on
  another node.
- **"Seed and node image"**, phrase `upgrade <name> to 0.10.1`. It imports the 1.36.6 node image as a template on
  every node the cluster uses, one node after another, saves an etcd snapshot on the seed, writes the k3s config
  drop-in without leader election, replaces the k3s binary and restarts k3s. It is refused with
  `409 seed_backup_missing` until the Check row `seed-backup` sees a backup of the seed VM younger than 24 hours;
  on most stores the row cannot see one, so check in Proxmox that the archive exists and tick the box
  (`"seedBackupTaken": true`). On the lab: the import 11 to 19 minutes per node (41 minutes for three), the whole
  part 43 minutes 20 seconds, the seed's API away 43 seconds; the platform cluster, tenant API servers and databases
  kept serving, with single failed looks during the import on a shared disk. It replaces no machine.
- **"Platform cluster"**, phrase `upgrade <name> to 0.10.1 and replace its machines`, blocked with
  `seed_not_upgraded` until the first part is done. **THERE IS NO WAY BACK once the new control plane machine has
  joined.** What stops, by the lab's sample (one control plane machine, one machine per group, every unit with one
  more machine first): the control plane's unit, the platform cluster's API for up to 88 seconds during the clone
  on its own node and 65 seconds at the hand-over, about 11 minutes; `system`, the platform's operators for 3 to 5
  minutes, no tenant gap, 8 minutes 50 seconds; `kamaji`, every tenant API server for 6 minutes 54 seconds (tenant
  workloads keep running), 10 minutes 21 seconds; `dbaas`, every database of one instance for 4 minutes 8 seconds
  (53 failed inserts of a writer every 2 seconds, no row lost), 11 minutes 20 seconds. Choose a quiet hour: a clone
  on the control plane's node is felt by the platform and tenant APIs.
- **Pause and Retry.** Pause stops the run between two units (15 seconds on the lab); the installation then reads
  `failed` with its step kept, and the Application `pc-platform` can stay without automated sync until the next
  run.
  Resume and Retry are an apply without a target: they go on at the stored step, keep the units already replaced,
  and delete a Node no machine owns. A platform that stands in the replacement toward `0.10.1-rc.1` or
  `0.10.1-rc.2` goes on to `0.10.1` with a Retry, nothing to type (`carried`, `TestRollTowardAKeptCandidateGoesOn`).
- **A site with one Proxmox node** was never drilled: every clone shares the disk with the control plane,
  kamaji-etcd and the databases, so expect the gaps above to be longer. Your own VMs on that node feel every clone too.
- **After the move:** the provider offers 1.36.6 to tenants only once the 1.36.6 node image is smoke-tested and
  published on every node (Template updates, or the Ready card's button); until then stage 10 ends with the item
  `kubernetes-version` in state `warn`. Existing tenant clusters and databases keep their version and chart.
- **From other BOMs** (`PlanFor`, one control plane machine and one machine per group): from `0.10.1-rc.2` one
  change, the release, record only; from `0.10.1-rc.1` three, the release and the tenant chart record only and
  `pc-platform` 0.1.15 to 0.1.16, no machine replaced; from `0.10.0` and `0.10.0-rc.1` 19 changes in the two parts
  above; from `0.9.0`, `0.9.1` and the candidates of 0.9.0, 20; from the three 0.8.x BOMs, 23. Each is allowed with
  no refusal; only the move from `0.10.0` was drilled.
- **A site installed with the Ansible installer** stays on k3s `v1.35.10+k3s1` and Kubernetes 1.35.10
  (`versions.yml`, `services.k8s_version`).
- The app chart is `0.10.1` with appVersion `v0.10.1`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.10.1`.

Verified on a lab. Two drills on one lab Proxmox VE cluster of three nodes on one shared disk, one control plane
machine, one tenant cluster and one database under a write loop, each checked afterwards by a read-only audit.
Nothing was run on the published images of `v0.10.1` itself. The code of this release is the code of `v0.10.1-rc.2`
plus one change, the fixes for what the second drill found (under Fixed: the wait for the tenants between two
units, the item and audit entry of a deleted Node, the ACL rows of a released token, the reason of a database that
waits for room). **That change has not run on a lab: the corrected wait for the tenants was tested against test
doubles only.**

- **On `v0.10.1-rc.1` (2026-11-20): "Seed and node image", the whole part, with the tenants live.** The seed's k3s
  from `v1.35.10+k3s1` to `v1.36.6+k3s1` with the same node and boot, containerd 2.3.7; the etcd snapshot as an
  object; the config drop-in by its effect (the three components' leases not renewed); the seed's API away 43
  seconds in the k3s stage and 39 seconds once during the import; the database port with no failed look.
- **On `v0.10.1-rc.1`: the control plane's unit** on cluster chart 0.1.15: one clone, the new etcd member added
  and the old one removed, the old VM destroyed, the platform API away 88 and 54 seconds during the clone and 65
  seconds at the hand-over, the database with no failed look. The old machine's Node stayed (corrected since).
- **On `v0.10.1-rc.2` (2026-11-20 and 21): the way out by one Retry.** The stuck roll was carried to the new BOM,
  the Node left behind was deleted by the run, and `nodeDeletionTimeoutSeconds` 1800 was put on the machines in
  place, with no machine replaced.
- **On `v0.10.1-rc.2`: units `system`, `kamaji` and `dbaas`**, each with one more machine first, with no Node left
  behind.
- **On `v0.10.1-rc.2`: Pause and Resume.** Pause honoured in 15 seconds, Resume at the stored step.
- **On `v0.10.1-rc.2`: data kept.** The marker row unchanged; 53 failed inserts in 4 minutes 8 seconds and no row
  lost.
- **On `v0.10.1-rc.2`: the platform cluster on 1.36.6.** Cilium 1.18.16, MetalLB, CloudNativePG, Kamaji and
  proxmox-csi running.
- **On `v0.10.1-rc.2`: tenants on the moved platform.** A new tenant cluster at 1.35.10 and one at 1.36.6 (after
  the node image was smoke-tested and published), a new database, a change of a database's allow-list accepted by
  the 1.36.6 API server, and an entry with host bits refused.

Not verified on a lab:

- **The fixes of this release after `v0.10.1-rc.2`**: the wait for the tenants between two units, the kept item
  and audit entry of a deleted Node, the removal of a released token's ACL rows on a real Proxmox, and the
  scheduler's reason on a real database.
- **This release itself:** its published images, its tag's workflows, any move to the BOM `0.10.1` (the record-only
  step from `0.10.1-rc.2` included), and one uninterrupted run from `0.10.0` to it.
- **The control plane's unit under cluster chart 0.1.16**: on the lab it ran under 0.1.15, and whether 1800
  seconds keep the Node from being left at all is not shown; the run's own deletion is the proven net.
- **A site with one Proxmox node**, a site with SNAT or a routed tenant path, and a unit replaced delete-first.
- **The seed's way back** by the etcd snapshot restore, and a restore of the seed from an archive.
- **A new certificate issued by cert-manager on 1.36**, and a renewal, on the seed or on the platform cluster.
- **More than one control plane machine**, the etcd member list, and a tenant cluster with two workers during the
  roll.
- **The moves from BOMs older than `0.10.0`**: their plans were read, not run.
- Everything the sections of 0.10.0, 0.9.1 and 0.9.0 say is not verified still holds, except what is listed as
  verified here.

Where an entry below says the BOM on `main` is `0.11.0-dev`, that was the state at that merge: this release's BOM
is `0.10.1`, and the change after its tag sets `main` back to a `-dev` one.

Findings of the second lab drill of `v0.10.1` (candidate `v0.10.1-rc.2`, 2026-11-20 and 21): one Retry got the
platform the first drill had left out of its wait, the units `system`, `kamaji` and `dbaas` were replaced, and the
platform cluster ended on Kubernetes 1.36.6. No chart changes with these corrections.

### Fixed

- **The wait for the tenants between two units of a replacement of machines did not hold after the `kamaji`
  unit.** It read the health ArgoCD shows for a tenant's Application and the status of the TenantControlPlane, both
  written by controllers the unit's drain evicts together with the tenant's control plane (Kamaji runs on the same
  machine). On the drill they still said Healthy and Ready 76 seconds after the drain; the run stored the next step
  and cloned the `dbaas` unit's machine while the only tenant API server was away for another 5 minutes 38 seconds.
  The wait (`tenants_unhealthy`, stage 7 between two units and stage 9 after every wave: one function) now also
  reads what the platform cluster's controller manager and kubelets write: per tenant control plane the Deployment
  Kamaji runs it as (available replicas against replicas, its last change observed; a missing Deployment counts as
  away), per database its instance pods that are Ready against `spec.instances` (not the Cluster's own status, which
  the CloudNativePG operator writes), and every Deployment in the namespaces of the Kamaji and CloudNativePG
  operators. The message names each object that is away, the stage's deadline ends the wait under the same code,
  and a Retry waits again. What was away before the upgrade is still noted once (`status.tenantsDownBefore`) and
  not waited for: a Deployment and a database by name, since their counts move, and also when a note taken by an
  earlier release names only the tenant's TenantControlPlane or Application. Tests hold a tenant control plane and
  a database whose status and Application stay Ready and Healthy while the Deployment has no available replica and
  the pod is gone, as on the lab. Not run on a lab.
- **A run keeps the item of a Node it deleted, and the deletion is in the audit log.** The item `node/<name>`
  (state `deleted`) was dropped by the next write of the stage's items, in the roll and in stage 9, so the stored
  run of the drill no longer said that it had deleted a Node; one log line was the only trace. The item now stays
  until the run ends, and each deletion writes an audit entry: `DELETE` on the installation, by the run, with the
  Node's name and the reason.
- **Releasing a tenant cluster's Proxmox token removes the token's ACL rows first.** Proxmox keeps the rows of a
  deleted token as dead `acl:` lines in `user.cfg` until the file is next written and warns at every `pveum`, `qm`
  and `pvesh` call ("ignore invalid acl token"): six lines stayed on the lab. Every token delete
  (`DeleteAPIToken`: the cluster token and an older cluster's CSI token) now lists the ACL and removes the rows
  whose principal is exactly that token, then deletes the token. No new privilege: the rows are on the paths the
  portal token wrote them on at the mint. A row that cannot be read or removed does not hold the revoke. Lines an
  earlier release left go at the next `pveum` change. Tested against a model of Proxmox that keeps a deleted token's
  rows; not run on a cluster.
- **A database that waits for room says so.** A create is accepted when no database node has the CPU or memory
  for it, and the database then read PROGRESSING without a reason (12 minutes on the lab, until another database
  was deleted). While a pod of the database cannot be scheduled, its row carries the scheduler's sentence in
  `database.status_message` ("Waiting for room on the database nodes: pod ... cannot be scheduled (0/4 nodes are
  available: 1 Insufficient cpu, ...)"), and the tenant's database page shows it under the status. The create is
  still not refused: nothing counts the room beforehand.
- Refresh `libxml2` in both console images (CVE-2026-86144, fixed in Alpine's 2.13.9-r3).

### Changed

- **The guide gives the figures of both drills of `v0.10.1`**, as one lab's sample: the platform API away for 88
  seconds during the control plane's clone and 65 seconds at the hand-over; the operators away for 3 to 5 minutes
  in the `system` unit, with the service addresses moving in seconds; every tenant API server away for 6 minutes
  54 seconds in the `kamaji` unit; the database away for 4 minutes 8 seconds in the `dbaas` unit. It says why the
  two stateful gaps are minutes (the CSI node plugin of a new node registers after the node is Ready and the old
  machine is drained: a known issue, not shortened by this release), that a clone on the control plane's node is
  felt, that stage 10 offers the new Kubernetes version to tenants only after the operator has smoke-tested and
  published the node image in Template updates, that the old `ProxmoxMachineTemplate` objects stay on the seed
  (harmless), that the VM ids of replaced machines are used again, possibly on another node, what a Pause leaves
  (the Application `pc-platform` without automated sync until Resume) and that one Retry gets a platform out that
  stood after the control plane's unit on `v0.10.1-rc.1`.
- **The platform BOM's release is `0.10.1`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.10.1-rc.2`: the two BOMs differ in the `release` line alone, and no chart of the product changed since its
  tag (`proxmox-kamaji-cluster-v0.1.16`, `database-cluster-v0.5.2`, `proxcloud-platform-v0.1.3`), so no chart tag
  is needed. The release gate passes on it and on the eleven previous BOMs. The plan from each previous BOM is
  allowed with no refusal (`PlanFor`, one control plane machine and one machine per group): from 0.10.1-rc.2 one
  change, the release, record only; from 0.10.1-rc.1 three, with no machine replaced; from 0.10.0 and 0.10.0-rc.1
  19, from 0.9.0, 0.9.1 and the candidates of 0.9.0 20, from the three 0.8.x BOMs 23, each in the parts "seed" and
  "platform". An installation that stands in the replacement toward 0.10.1-rc.1 or 0.10.1-rc.2 after a step goes on
  to 0.10.1 with a Retry, `carried` the candidate, nothing to confirm. The app chart, `pc_versions.proxcloud` and
  `DEFAULT_VERSION` of `proxcloud-up.sh` move to `0.10.1` / `v0.10.1`; the installer's k3s and Kubernetes pins stay
  at 1.35.10. `deploy/platform/previous/0.10.1.yaml` is added by the change after the tag.

Findings of the lab drill of `v0.10.1-rc.1` (2026-11-20: the first real move of a seed's k3s across a minor and the
first real replacement of a platform cluster's machine). Part "seed" passed; part "platform" stopped after the
control plane's unit. The corrected chart is published from the tag `proxmox-kamaji-cluster-v0.1.16`, and the BOM
of the candidate `0.10.1-rc.2` pins it (below).

### Fixed

- **A platform with one control plane machine got no further than the control plane's unit of a replacement of its
  machines.** Cluster API deletes a Machine's Node object last and gives up once the Machine has been in deletion for
  longer than `nodeDeletionTimeoutSeconds`, 10 seconds by its default, which the drain alone uses up; with one
  control plane machine the delete also falls into the gap in which the API address moves to the new machine (65
  seconds on the lab; the kube-vip lease is 60). The Node of the old machine stayed, NotReady, with no Machine; the
  run's wait between two units counted five Nodes for four machines and ended in `nodes_timeout` after 30 minutes,
  and so did every Retry. Both sides are corrected:
  - **Cluster chart `proxmox-kamaji-cluster` 0.1.16** reads `nodeDeletionTimeoutSeconds` (default 1800) and renders
    `deletion.nodeDeletionTimeoutSeconds` on the `KubeadmControlPlane`'s machine template and on every
    `MachineDeployment`'s (the v1beta2 paths of Cluster API v1.14.4), so the delete is tried again until the API
    answers. 1800 and not 0 (no limit): a Machine of a cluster whose API never answers again would otherwise never
    be removed. Cluster API copies the value to the machines in place, also to one already in deletion, and leaves it
    out of its rollout decision: no machine is replaced for it, and no machine or bootstrap template changes its
    name. The run's check before a sync (`unexpected_rollout`) compares without the value as long as the live
    Application renders none, because ArgoCD reports only Synced or OutOfSync per object; a render test holds the
    chart to "this value and nothing else" (`TestNodeDeletionTimeoutChangesNoMachine`), and the release check of the
    BOM pairs (`TestUpgradeRollsNoMachine`) passes unchanged. A negative value renders nothing, the render of 0.1.15.
    The kube-vip lease stays at 60, 40 and 5 seconds: a changed lease replaces every control plane machine, and a
    shorter one drops a single control plane's own address whenever its etcd is slow.
  - **The run deletes a Node no Machine owns.** In the wait between two units and in stage 9, a Node of the platform
    cluster that is not Ready, that Cluster API joined for a Machine of this cluster which is gone, and that no
    Machine or ProxmoxMachine names by `status.nodeRef`, by its own name or by its VM's provider id, is deleted
    through the platform cluster's API (the admin kubeconfig Cluster API keeps on the seed; no Proxmox call and no
    new privilege), only while its uid is the one that was read. The stage's item `node/<name>` and its message name
    it. A Node that a Machine owns is never deleted. **A site that stood in this wait gets out with one Retry.**
    `nodes_timeout` now names every node that is not Ready, or all of them when only the count is off.
- **An upgrade whose machines are being replaced continues under a later release's BOM.** After a run toward a
  target has started, an apply without a target went on to that target, and a later release's BOM could take its
  place only when named and confirmed. Now, while the
  replacement has begun and a unit is still to be replaced, a later release whose BOM names the same Kubernetes
  version, node image, kube-vip and k3s as the unfinished target (only the product's own charts and the release
  differ, and the installed BOM may move to it too) takes the target's place by itself: Retry goes on under it with
  nothing to confirm, `status.targetBom` becomes the new BOM, the step is kept, the units already replaced render as
  they are and the rest with the new cluster chart. The plan's `upgrade.carried` names the target it replaced. A
  later BOM that changes the Kubernetes version, node image or k3s again is not taken while the replacement runs:
  Retry goes on to the unfinished target, and naming the new BOM is `409 upgrade_refused` with the reason. Before
  the replacement has begun and after its last unit nothing changes.
- **Stage `seed-k3s` can give its job back and record its failure.** The job store took only `a-z` and `_` in a
  classification code, and the stage's codes carry a digit (`waiting_seed_k3s`, `seed_k3s_failed`,
  `seed_k3s_refused`, `seed_k3s_timeout`): the stage could not defer, so it looked once a minute where it means to
  look every 15 seconds and spent one of the job's attempts per look, and a failed or refused k3s upgrade could not
  be recorded under its own code. The store's rule (`durablejobs.ValidCode`) now allows digits after a first letter;
  nothing read more into a code than equality. One test passes every code a stage can end or wait with through that
  rule, read from the source so a new code cannot be left out, and the test double of the job store refuses what
  the real one refuses.
- **The seed's etcd snapshot is recorded by its real name.** The Job of stage `seed-k3s` saves
  `pre-upgrade-<old version>-<node>-<time>` and was deleted with the only log that named it. The stage now reads
  k3s's own list (`ETCDSnapshotFile`) before it drops the Job and keeps the path in `status.seed.preUpgradeSnapshot`,
  in the finished stage's message and in the item `etcd-snapshot`, with the command that deletes it later.
- **An import that is tried again says so.** The `images` stage's item showed `uploading` for as long as the
  import's job needed, also through tries that Proxmox answered with an error. It now says which try runs and the
  code the one before ended with.
- **The Check has a row `nodes`.** It warns, by name, about a node of the platform cluster that is not Ready, and
  says of a Node no Machine owns that the next apply deletes it. Never a failure, so the run that repairs it can
  start. The Check was `ok` with such a Node in the cluster.
- **Template updates lists no VMID twice.** A template that was deleted leaves its receipt as a `template_missing`
  row; when a new import got the freed VMID, the page showed that VMID for both. The old row now names no VMID once
  another guest holds it, with a warning that says which one it was.

### Changed

- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.10.1-rc.2`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.1-rc.2.yaml`, sha256
  `14acba7b0d7ae05bda488066eb2f1699f73139bc78ae44df824a758e587d17bd`). The commit of the candidate `v0.10.1-rc.2`
  left the BOM at `0.10.1-rc.2`, and the release gate refuses only `-dev`. A platform installed by the candidate,
  moved to its BOM, or standing in the replacement of its machines with that BOM as its target (what a Retry on the
  candidate's build records) is not refused (`bom_unsupported`) under a later build while the file is carried. Its
  pins are those of `main`, so the plan from it has one change, the release, record only. From `0.10.1-rc.1` the plan
  has three (the release, the tenant chart and `pc-platform` 0.1.15 to 0.1.16), and the nine older previous BOMs
  still carry the move to k3s `v1.36.6+k3s1` and Kubernetes 1.36.6 in two parts. `TestUpgradeRollsNoMachine`
  compares eleven previous BOMs with the BOM on `main`, nine of them at step 0 of a roll. A development BOM carries
  no unfinished replacement, so on a build of `main` an installation on `0.10.0` after the first unit of a
  replacement toward `0.10.1-rc.1` or `0.10.1-rc.2` goes on to that candidate with a Retry, nothing to confirm
  (`TestRollTowardAKeptCandidateGoesOn`). No apply on such a platform was run on a lab by this change.
- **The guide gives the figures of the `v0.10.1-rc.1` drill**, each marked as one lab's sample: the node image
  import 11 to 19 minutes per node and able to stall etcd for seconds on a shared disk; the seed's API away for 43
  seconds in stage 5a; for the control plane's unit a clone of 6 minutes 5 seconds, the platform API away for up to
  2 minutes 21 seconds during the clone on the control plane's own node and for 65 seconds at the hand-over, a
  tenant API look failing for 13 seconds at most and the database not at all. `nodes_timeout` is listed for stage 7
  as well, with what Retry does now. Not shown on a lab yet: the units `system`, `kamaji` and `dbaas`, the Retry out
  of the leftover Node on a real cluster, and the continuation under a later BOM.

- **A network with host bits is refused where it ends in a Kubernetes field.** Kubernetes 1.36 refuses a new or
  changed CIDR with host bits (`192.168.0.5/24`), a leading zero or an IPv4-mapped IPv6 address in
  `loadBalancerSourceRanges`, and keeps one the Service already holds (KEP-4858). The product took such values and
  the object then failed on the platform cluster. Now a 400 names the entry and the form to write
  (`192.168.0.5/24 has host bits set: use 192.168.0.0/24`) at each input: a database's `allowedCidrs` on create and on
  `SET_ALLOWED_CIDRS`, a tenant cluster's `allowedCidrs` on create (it was not checked at all: any text passed), a
  Kubernetes provider's `config.cluster.allowedCidrs` on create and on an update that carries the cluster block, and
  an installation's `spec.access.argocd.sources` on create and update. One rule for all four
  (`cloud.StrictCIDRs`). **Stored values are not rewritten and do not lock their object**: an update that sends the
  list a database, a provider or an installation already has, with or without more canonical entries, passes; only an
  entry that is new must be canonical. Where an operator sees them: the Check row `argocd-access` warns about a
  stored source with host bits, and a Kubernetes provider's `configNotes` name a stored default. A new tenant
  cluster that takes a stored provider default gets the entry without its host bits, the same range, since the
  Service is new and 1.36 would refuse the stored spelling. Not changed: `spec.seed.apiSources` (a Proxmox firewall
  rule, no Kubernetes field), and a stored `allowedCidrs` of a database has no warning of its own (the list is shown
  as stored). **Not run against a Kubernetes 1.36 API server**: the tests pin the product's rule, which follows the
  three refusals of `apimachinery/pkg/util/validation/ip.go` at v1.36.0; the drill of this release must show a
  database and a tenant cluster with an allow-list created on the 1.36 platform, and an old stored value still
  syncing.
- **A Withdraw or a Delete of a template that changes nothing writes no audit row.** The repeat of a Withdraw that is
  complete (no tag to change, the digest already on the blocklist) wrote a row `withdrawn false -> true`, and a second
  Delete of a freed receipt wrote a row `DELETE` with `unchanged true`. Both answer as before (`unchanged: true`) and
  leave no row. A call that answers `unchanged` and still wrote something keeps its row: a first Withdraw that finds
  no tag to change (it blocklists the digest), and a Delete that finds the template destroyed by hand (it frees the
  receipt; `deleted` stays false).
- **A server's row keeps `pveUUID` after an action.** The row is read again from Proxmox after every action on a
  server (Stop, Start, a snapshot, a NIC change) and on every look at it, and that read replaced the whole row: the
  VM's smbios uuid and, for a VM without a creation stamp, its `identity`, which only the sync reads, were gone until
  the next sync wrote them back. In between a sampled sync could not use its sample for the VM, and a VMID that
  changed hands could not be told from the old VM by its uuid. Both keys now stay through the refresh. Seen on a site
  on v0.10.0 after Stop then Start; the display write that v0.10.0 added to Start is not the cause (it changes the
  VM's config digest, which the row does not hold).
- **The seed's k3s runs kube-controller-manager, kube-scheduler and its cloud-controller-manager without leader
  election.** A seed is one k3s server with embedded etcd, for which k3s keeps the three under a leader lease; on a
  lab seed a lease missed under disk load (a backup, a heavy apply) ended the component and k3s with it, although no
  other server could take the lease. The k3s config drop-in
  `/etc/rancher/k3s/config.yaml.d/10-proxcloud-leader-elect.yaml` (`leader-elect=false` through
  `kube-controller-manager-arg+`, `kube-scheduler-arg+`, `kube-cloud-controller-manager-arg+`) is written at a new
  seed's first boot, and on an existing seed by the Job of stage 5a, after the checksum and before the binary is
  replaced; it takes effect with that stage's restart. The plan has one more `site` row, `seed-k3s-config`.
  `/etc/rancher/k3s/config.yaml` is not rewritten, and the keys append, so an argument set by hand stays. Limits:
  k3s's own controllers keep their lease (no argument turns it off); an existing seed gets the file only from an
  upgrade that replaces its k3s; **not run on a lab**: the drill of this release must show the file, the three
  components with `--leader-elect=false` and no exit of k3s during the roll.
- **The way back of stage 5a across a k3s minor names the snapshot restore.** k3s does not go back a minor without
  the datastore of the old minor. When the move crosses a minor, the message of `seed_k3s_timeout` and the plan's
  row `seed-k3s-binary` now give k3s's whole rollback for embedded etcd in one command: `k3s-killall.sh`, put
  `k3s.prev` back, `k3s server --cluster-reset --cluster-reset-restore-path=` with the newest
  `/var/lib/rancher/k3s/server/db/snapshots/pre-upgrade-<old version>-<node>-<time>` (the snapshot the Job saved; the
  command stops before it changes anything when there is none), `systemctl start k3s`. A patch step keeps the short
  command. The stage's first message names the snapshot too. Text only: the product still restores nothing by itself,
  and **neither command was tried on a lab**. The plan's row `seed-etcd-snapshot` no longer says that k3s's retention
  applies: k3s prunes no snapshot saved on demand.

### Changed

- **The platform BOM on `main` is `0.11.0-dev` again, and the build carries the BOM of `0.10.1-rc.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.1-rc.1.yaml`). The commit of the candidate
  `v0.10.1-rc.1` left the BOM at `0.10.1-rc.1`, and the release gate refuses only `-dev`. A platform installed by the
  candidate, or moved to its BOM by an upgrade, stays on it under a later build and is not refused
  (`bom_unsupported`) while the file is carried. Its pins are those of `main`, so the plan from it has one change, the
  release, record only: no seed part and no machine replaced. The nine older previous BOMs still carry the move to
  k3s `v1.36.6+k3s1` and Kubernetes 1.36.6 in two parts; `TestUpgradeRollsNoMachine` compares ten previous BOMs with
  the BOM on `main`, nine of them at step 0 of a roll, and `TestPreviousBOMsMoveAsTheirPinsDiffer` holds every pair
  to that split. The kept BOM has cert-manager v1.20.2 and Cilium 1.18.16 on Kubernetes 1.36, outside the versions
  their own documents list: accepted as "to be proven by the lab drill", which has not run. No apply on such a
  platform was run on a lab by this change.
- **The platform BOM names k3s `v1.36.6+k3s1` and Kubernetes 1.36.6** (`deploy/platform/platform-bom.yaml`:
  `seed.k3s`, `kubernetes.version`, `kubernetes.nodeImage`; the node image is
  `ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4`, from the same source commit as the 1.35.10 image). It is the
  first BOM that names another k3s or Kubernetes than the ones before it, so an upgrade to it from any earlier BOM
  replaces the k3s binary on the seed and then every machine of the platform cluster, in two parts with their own
  phrases; the upgrade code of v0.10.0 is unchanged. Nothing else moves: no chart, no Cluster API provider, no
  clusterctl image. **No such upgrade has run on a lab or on a site**: the guide's new section "Upgrade to
  Kubernetes 1.36.6" says what each step does and what stops serving, with every duration marked not measured.
  Known cautions, to be proven by the lab drill of the release:
  - **cert-manager v1.20.2** lists Kubernetes 1.32 to 1.35 (new with this BOM; 1.21 is the first that lists 1.36)
    and **Cilium 1.18.16** lists 1.30 to 1.33 (already outside its list on 1.35.10). No source says either breaks
    on 1.36; neither was run on it. An upgrade cannot move a third-party chart in this release.
  - **Kubernetes 1.36 refuses a new address or CIDR that is not canonical** (leading zeros, host bits as in
    `192.168.0.5/24`) in built-in fields, among them the `loadBalancerSourceRanges` the product writes from
    `allowedCidrs` and the ArgoCD address sources. Stored values keep working (KEP-4858); a new or changed one makes
    its Application fail to sync. The product refuses such a value at its inputs (under Fixed above).
  - **Tenant clusters and databases keep their version.** After the upgrade the Kubernetes provider also offers
    1.36.6 for new tenant clusters; none was created at that version anywhere.
  - **The installer path stays on k3s `v1.35.10+k3s1` and Kubernetes 1.35.10** (`versions.yml`,
    `services.k8s_version`), and with it the kubectl in the clusterctl image, one minor behind the API server.
    `TestBOMMatchesInstallerVersions` now lets these two pins be up to one minor behind the BOM and never ahead.
  - The hand step of `seed_k3s_timeout` (put `k3s.prev` back) is not enough across a minor: k3s's rollback also
    needs the etcd snapshot the stage saved. The message and the plan give the whole command (see Fixed).
  `TestUpgradeRollsNoMachine` compares a pair that replaces machines by design at step 0 of the roll (this build's
  chart with the previous BOM's version and templates must render what the previous release rendered); a pair that
  replaces none is compared as before. All nine previous BOMs pass, and the plan from each is allowed (read with
  `PlanFor`: 17, 20 or 23 rows with the drop-in's row below). Only the move from 0.10.0 will be drilled.
- **The platform BOM on `main` is `0.11.0-dev`, and the build carries the BOM of `0.10.0`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.0.yaml`). The `v0.10.0` release commit left
  the BOM at `0.10.0`, and the release gate refuses only `-dev`. A platform installed by `v0.10.0`, or moved to its
  BOM by an upgrade, stays on it under a later build and is not refused (`bom_unsupported`) while the file is
  carried. Its pins are those of `main`, so the plan from it has one change, the release, and
  `TestUpgradeRollsNoMachine` compares nine previous BOMs with the BOM on `main`: three of 0.8.x, the two
  candidates of 0.9.0, `0.9.0`, `0.9.1`, `0.10.0-rc.1` and `0.10.0`. No apply on such a platform was run on a lab
  by this change.

### Known issues

- **The stateful gaps of a replacement are minutes.** A new machine counts as Ready, and Cluster API drains the
  old one, before the CSI node plugin of the new node has registered; the volume of kamaji-etcd, and of each
  database, then waits to be attached ("CSINode ... does not contain driver"). On the lab every tenant API server
  was away 6 minutes 54 seconds in the `kamaji` unit and a database of one instance 4 minutes 8 seconds in the
  `dbaas` unit. Not shortened by this release: the lever is a change of the cluster chart, which needs a drill of
  its own. Announce a window longer than these figures.
- **cert-manager v1.20.2 and Cilium 1.18.16 run outside their own support matrix on Kubernetes 1.36** (1.32 to
  1.35, and 1.30 to 1.33). No fault was seen in the two drills; no new certificate was issued on 1.36. An upgrade
  cannot move a third-party chart in this release.
- **Sites installed with the Ansible installer stay on k3s `v1.35.10+k3s1` and Kubernetes 1.35.10**, and the kubectl
  of the clusterctl image with them; the installer path takes the minor step in a release of its own.
- **The machine replacement cannot be undone.** After the new control plane machine has joined, the nets are the
  seed VM's backup and the tenants' own backups.
- **With one control plane machine a clone on its Proxmox node is felt.** The platform API was away up to 88
  seconds during the control plane's own clone on the lab, and single looks at the platform and tenant APIs failed
  during the `kamaji` unit's clone and a smoke test on that node (up to 52 seconds on a tenant API). A one-node site
  has every clone there.
- **A snapshot backup of the seed can restart its k3s** before the seed part has run on it: the seed then still
  runs its components under leader election (the 0.10.0 entry). Take the backup in a quiet hour, some minutes
  before the run, and wait until the seed's API answers again. After the seed part, k3s's own controllers still
  keep a lease.
- **The `seed-backup` row cannot see a backup on most stores.** The upgrade is then started with the box that says
  a backup was taken.
- The known issues of 0.10.0 that still hold: seed ISOs of deleted servers stay until they are cleaned; the ArgoCD
  address is reachable by tenants behind SNAT; a Reboot does not change the display; the platform's token ids are
  fixed; a chart version that changes only its hook does not run the hook; workers do not join when the node that
  holds the platform's addresses also translates tenant traffic; the health check's guard; Replace on a node group
  of one; a standalone volume that loses its name; the `pve-api-rule` row behind a translating router; seed ISOs of
  servers created before 0.8.0. No longer an issue: the seed's k3s upgrade and the machine replacement have run on
  a lab with tenants.

## [0.10.1-rc.2] - 2026-11-20

The second release candidate of 0.10.1, published as a prerelease and never as `latest`. It is not the release:
`v0.10.0` stays the current stable release, and the chart, the installer and the Compose default still name it. It
holds what is under 0.10.1 above: the corrections of the lab drill of `v0.10.1-rc.1` (#402) next to what that
candidate held (#397, #398, #399). It exists so that the lab that stands in the middle of that drill can go on, and
so that the whole replacement can be run on the corrected code: nothing of it had run on a lab when it was tagged.
The entries above are not repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.10.1-rc.2`, and it pins cluster chart `proxmox-kamaji-cluster` 0.1.16**
  (`deploy/platform/platform-bom.yaml`), for `pc-platform` and for `tenantCharts.kubernetes`, at
  `sha256:2a58fa81b5a96fd7b23c76b06418af5a477586f151ec8e9eab1bad1536e6f000` (read from the registry manifest, by
  `helm show chart` and by `bom_verify.py`). The chart was published from the tag `proxmox-kamaji-cluster-v0.1.16`;
  `git diff proxmox-kamaji-cluster-v0.1.16 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing, so no chart tag is owed.
  Nothing else of the BOM moves against `0.10.1-rc.1`: the same k3s, Kubernetes version, node image, providers and
  third-party charts. It is a release BOM, and the release gate passes on it and on the ten previous BOMs.
- **`upgradeFrom` of `pc-platform` is `0.1.13`, `0.1.14`, `0.1.15`**, so every previous BOM reaches it: 0.1.13 is
  the chart of the three 0.8.x BOMs, 0.1.14 that of 0.9.0, 0.9.1 and the candidates of 0.9.0, 0.1.15 that of
  0.10.0-rc.1, 0.10.0 and 0.10.1-rc.1. `TestUpgradeRollsNoMachine` passes with the pin on all ten pairs, nine of
  them at step 0 of a roll: chart 0.1.16 with this build's values and the chart each previous BOM pins render the
  same machines.
- **What rc.1's drill showed** (one lab, 2026-11-20; three Proxmox nodes on one shared disk, one control plane
  machine, one tenant cluster and one database under a write loop of one insert every 2 seconds). Part "seed"
  passed with the tenants live: 43 min 20 s, of which 41 min were the node image import on three nodes; the k3s
  stage took 2 min 1 s; the seed's API was away 43 s; the seed came back at `v1.36.6+k3s1` with containerd 2.3.7,
  the same node and boot; the database port and the write loop had no failure, the platform API 12 failed looks of
  1535 and the tenant API 7 of 1526, all in the minutes of the image uploads and imports. Part "platform" stopped
  after the control plane's unit: Cluster API gave up deleting the old machine's Node after its default 10 seconds,
  inside the 65 seconds in which the API address moves to the new machine (the kube-vip lease is 60), and the run
  waited for a node count that could not come true (`nodes_timeout`, and again at Retry).
- **What rc.2 changes** (#402, in detail under 0.10.1): cluster chart 0.1.16 sets
  `nodeDeletionTimeoutSeconds` to 1800 on the control plane's and every node group's machines, with no machine
  replaced for it; the run deletes a Node that is not Ready and that no Machine owns, so a site that stands in that
  wait gets out with one Retry; a replacement of the machines that has begun continues under a later BOM that
  differs in own charts and the release only; stage `seed-k3s` can defer and record its failure (codes with a
  digit); the etcd snapshot's real name is recorded; an import that is tried again says so; the Check has a row
  `nodes`; Template updates lists no VMID twice; the guide carries the drill's figures.
- **An installation in the middle of the replacement toward `0.10.1-rc.1` continues under this BOM.** With
  `status.bom` 0.10.0, `targetBom` 0.10.1-rc.1 and `roll.step` 1 to 4, `PlanFor` without a target gives the move to
  0.10.1-rc.2 as the upgrade in progress: `carried` 0.10.1-rc.1, allowed, nothing to confirm, nothing blocking,
  part `seed` done, part `platform` allowed, 19 changes. A Retry runs with the BOM 0.10.1-rc.2, records it as
  `status.targetBom` and keeps the step. An installation whose seed part is done and whose replacement has not
  begun goes on to 0.10.1-rc.1 with an apply without a target; the plan offers 0.10.1-rc.2 as `replacement`, with
  the phrase `upgrade <name> to 0.10.1-rc.2 and replace its machines`.
- **The plan from each previous BOM is allowed, with no refusal** (`PlanFor`, one control plane machine and one
  machine per group). From 0.10.0 and 0.10.0-rc.1, 19 changes in two parts: the 17 of `0.10.1-rc.1`,
  `tenantCharts.kubernetes` 0.1.15 to 0.1.16, record only, and `pc-platform` 0.1.15 to 0.1.16 (class chart, stage
  `cluster`, wave 3, with the first step of the roll). From 0.9.0, 0.9.1 and the candidates of 0.9.0, 20, with the
  cluster chart from 0.1.14. From the three 0.8.x BOMs, 23, with the cluster chart from 0.1.13. From 0.10.1-rc.1,
  three, with no part and no machine replaced: the release and the tenant chart, record only, and `pc-platform`
  0.1.15 to 0.1.16, phrase `upgrade <name> to 0.10.1-rc.2`.
- **A candidate moves nothing a final moves.** The app chart, the installer pin and `DEFAULT_VERSION` stay on
  `v0.10.0`; the installer's k3s and Kubernetes pins stay at 1.35.10.

### Verified on a lab

Nothing of this candidate. What the audit of the drill of `v0.10.1-rc.1` confirmed, on that candidate's images and
BOM, one run on one lab:

- Part "seed": the run from 06:28:16 to 07:11:40 UTC (43 min 20 s), seven stages, no retry; the seed node at
  `v1.36.6+k3s1` with the same uid and boot id, containerd 2.3.7; the pre-upgrade etcd snapshot as an object
  (11,743,264 bytes, ready to use; the file itself was not seen); the config file without leader election by the
  saved pod spec and by its effect (the three components' leases not renewed after the restart; the file and
  `k3s.prev` were not seen); the seed's API away 43 s in the k3s stage, and 39 s once, unplanned, during the image
  import; failed looks: platform API 12 of 1535, tenant API 7 of 1526, database port 0 of 1569.
- Part "platform", the control plane's unit only: one clone (6 min 5 s), the new member added to etcd and the old
  one removed, the old VM destroyed; platform API not answering for 88 s and 54 s during the clone, 22 s after the
  join and 65 s at the hand-over (29 failed looks between the etcd join and the end of the hand-over); tenant API
  longest 13.4 s; database port 0 failed looks of 1558; the write loop without a failed insert and the marker row
  unchanged; the Cilium agent Ready with no restart on the one 1.36.6 node.
- The state it left: control plane at 1.36.6, the three node groups at 1.35.10, the old control plane machine's
  Node still listed (not Ready, ten stale pods), and it serves.
- The findings, each confirmed real: the Node left behind (blocker; the 65 seconds are the chart's own lease, not
  the lab's disk), the seed stage's codes refused by the job store (major), the clone of the control plane's
  surge machine taking a single control plane away before the join (major for a one-node site), and five minor
  ones (the import's load on a shared disk, the hidden import tries, the snapshot's name, the Check with a
  machineless node, the guide's `nodes_timeout` row).

### Not verified

- Anything of this candidate: its images and its tag's workflows do not exist before the tag, and none of #402
  ran against a real cluster.
- Units 2 to 4 of the replacement (`system`, `kamaji`, `dbaas`) and every gap of theirs.
- The whole replacement on the corrected code, and the Retry that takes a stuck installation on under this BOM.
- Pause between two units and Resume.
- The seed's way back with the snapshot restore.
- A new tenant cluster at 1.36.6.
- cert-manager issuing a certificate on 1.36, on the seed or on the platform cluster.
- A one-node site, and a site with SNAT.
- The moves from the BOMs other than 0.10.0 (plans read, not run).

## [0.10.1-rc.1] - 2026-11-20

The first release candidate of 0.10.1, published as a prerelease and never as `latest`. It is not the release:
`v0.10.0` stays the current stable release, and the chart, the installer and the Compose default still name it. It
holds what is under 0.10.1 above (#397, #398, #399), and it exists so that a lab can run the first upgrade that
moves Kubernetes: nothing of it had run on a lab when it was tagged. The entries above are not repeated here. Not
for a production site.

### Changed

- **The platform BOM's release is `0.10.1-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had.
  No chart of the product changed since its tag
  (`git diff proxmox-kamaji-cluster-v0.1.15 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing), so no chart tag is
  needed. It is a release BOM: no `pending` digest and no `0.0.0+` version, and the release gate passes on it and
  on the nine previous BOMs.
- **It is the first release BOM that moves Kubernetes.** Against `0.10.0`: the seed's k3s `v1.35.10+k3s1` to
  `v1.36.6+k3s1`, the platform cluster 1.35.10 to 1.36.6, and the node image to
  `ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4`. No chart, no Cluster API provider and no other image
  moves. A candidate moves nothing a final moves: the app chart, the installer pin and `DEFAULT_VERSION` stay on
  `v0.10.0`; the installer's k3s and Kubernetes pins stay at 1.35.10.
- **The upgrade is two operator actions.** "Seed and node image" (phrase `upgrade <name> to 0.10.1-rc.1`) imports
  the node image on every node, saves an etcd snapshot on the seed, writes the config file without leader election,
  replaces the k3s binary and restarts k3s: the seed's API is away (expected about a minute), reconciliation
  stops, the platform cluster and every tenant keep serving. It needs a backup of the seed VM:
  `409 seed_backup_missing` until the portal sees one younger than 24 hours or the operator confirms one
  (`seedBackupTaken`; on most stores the portal cannot see it). "Platform cluster" (phrase
  `upgrade <name> to 0.10.1-rc.1 and replace its machines`, blocked with `seed_not_upgraded` until the first is done) replaces the
  control plane machine, then `system`, `kamaji` and `dbaas`: the platform cluster's API is away during the
  control plane's replacement, every tenant API server while a group of one `kamaji` machine is replaced, every
  one-instance database on a `dbaas` machine while it is replaced. No duration was measured.
- **The way back.** The seed's k3s: by hand, the command of `seed_k3s_timeout` (`k3s-killall.sh`, `k3s.prev` back,
  `k3s server --cluster-reset --cluster-reset-restore-path=` with the snapshot the run saved,
  `systemctl start k3s`), then the backup of the seed VM. The machine replacement has none once the new control plane machine has
  joined etcd.
- **The plan from each previous BOM is allowed, with no refusal**, in two parts (`PlanFor`, one control plane
  machine and one machine per group). From 0.10.0 and 0.10.0-rc.1, 17 changes: the row `roll`; the release, record
  only; `kubernetes.version` and `kubernetes.nodeImage`, class machines, stage `cluster`; `seed.k3s`, class seed,
  stage `seed-k3s`, with its four `site` rows (`seed-k3s-job`, `seed-etcd-snapshot`, `seed-k3s-config`,
  `seed-k3s-binary`); four `roll/<unit>` rows, class machines, and four `roll-vm/<unit>` rows, class site, stage
  `cluster`. From 0.9.0, 0.9.1 and the candidates of 0.9.0, 20: those, the two tenant charts, record only, and
  `pc-platform` 0.1.14 to 0.1.15 (class chart, stage `cluster`, wave 3, with the first step of the roll). From
  the three 0.8.x BOMs, 23: those with the cluster chart from 0.1.13, and `capi` (stage `seed-platform`, wave 2),
  `capi-tenant` and `platform` (stage `workload-platform`, waves 2 and 3) from 0.1.2 to 0.1.3. Only the move from
  0.10.0 is drilled.
- **Two components are outside their own support matrix on Kubernetes 1.36**, accepted as "to be proven by the lab
  drill": cert-manager v1.20.2 (lists 1.32 to 1.35) and Cilium 1.18.16 (lists 1.30 to 1.33). An upgrade moves no
  third-party chart in this release.
- **Tenant clusters and databases are not moved.** Each tenant cluster stays on its own version (1.35.10).

### Verified on a lab

Nothing. The candidate exists for the drill.

### Not verified

- Both parts of the upgrade, entirely, on a lab and on a site with tenants: "Seed and node image" (the import,
  the snapshot, the config file, the binary, the seed API gap, the seed's Applications on k3s 1.36) and "Platform
  cluster" (every unit, every gap, Pause and Resume, delete-first, a failed join, three control plane machines).
- The way back of the seed with the snapshot restore.
- cert-manager v1.20.2 and Cilium 1.18.16 on Kubernetes 1.36, and every component that states no version.
- Tenant clusters and databases on the moved platform: existing ones at 1.35.10, a new cluster at 1.35.10 and at
  1.36.6, a new database; the CIDR rule against a 1.36 API server.
- The moves from the eight BOMs other than 0.10.0 (plans read, not run).
- The candidate's images and its tag's workflows: they do not exist before the tag.

## [0.10.0] - 2026-11-20

The release after `v0.9.1`. The Proxmox console of a VM made by ProxCloud opens on the graphical display, a
withdrawn template can be deleted from the admin console, the platform cluster's ArgoCD can be given an address,
and an upgrade run can replace the seed's k3s and the platform cluster's machines. The last two ship dark: this
release's BOM moves neither k3s nor Kubernetes, so no upgrade to it runs them. It was prepared by one release
candidate, `v0.10.0-rc.1`, whose section below is kept; everything it held is in this section. The entries under
Added, Changed and Fixed were written when each change merged, most of them before the lab round: where one says
"Not run on a lab" or "Not verified on a lab", the two lists below are the current state.

- **The graphical console is the default in the Proxmox web UI.** New templates have `vga: std` and keep `serial0:
  socket`. The templates of this portal and its stopped servers change at the next services sync; a running server
  at its next Stop then Start from the portal; a Kubernetes worker only when its node group is rolled. A Reboot
  does not change it (Changed).
- **Delete of a withdrawn template** from the admin console, without `qm destroy` on the node (Added).
- **The platform cluster's ArgoCD can be given an address**, with ArgoCD's self-signed certificate, for the
  operator networks only (Added).
- **The upgrade machinery for the seed's k3s and for replacing the platform cluster's machines** (Added). Both
  ship dark, see the BOM below.
- **Thirteen dependency updates** (the section of 0.10.0-rc.1 lists them).

The platform BOM of this release is `0.10.0`. It pins what the candidate `0.10.0-rc.1` pins: cluster chart 0.1.15
and database chart 0.5.2, both published from their tags (`proxmox-kamaji-cluster-v0.1.15`,
`database-cluster-v0.5.2`), and platform chart 0.1.3; the two BOMs differ in the `release` line alone. Against
`0.9.1` the two tenant charts and the chart of the platform cluster move, and nothing else: the BOM keeps k3s
`v1.35.10+k3s1` (no newer patch of 1.35 exists) and Kubernetes 1.35.10 with its node image. So an upgrade to
`0.10.0` moves charts only: it does not run the stage `seed-k3s`, does not ask for a seed backup, and replaces no
machine. The release carries the BOMs of `0.8.0`, `0.8.1`, `0.8.2`, `0.9.0-rc.1`, `0.9.0-rc.2`, `0.9.0`, `0.9.1`
and `0.10.0-rc.1` for the platforms those builds installed.

Upgrading from `v0.9.1`. From an older release, read the sections of `v0.9.1` and `v0.9.0` as well: their steps
still apply.

- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.10.0`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. `compose.yaml` did not change since
  `v0.9.1`: PostgreSQL and Keycloak keep their containers (seen on the lab at the upgrade to the candidate).
- **Then the platform**, in step 4 of System → Managed services. It stays on its BOM and keeps working until an
  operator starts the move. From `0.9.1`, `0.9.0` or a candidate of 0.9.0 the plan lists four changes and refuses
  nothing: the release, `tenantCharts.kubernetes` (0.1.14 to 0.1.15) and `tenantCharts.database` (0.5.1 to 0.5.2),
  all three record only, and `pc-platform` 0.1.14 to 0.1.15 (class chart, stage `cluster`, wave 3). Nothing is
  rolled, and no seed backup is needed for this BOM. From a 0.8.x BOM, seven changes: the same four, with the
  cluster chart from 0.1.13, and `capi`, `capi-tenant` and `platform` from 0.1.2 to 0.1.3. From `0.10.0-rc.1`, one
  change, the release, record only. `TestUpgradeRollsNoMachine` compares all eight previous BOMs with this one and
  passes. The confirmation is `upgrade <name> to 0.10.0`.
- **Then let one services sync run** (every 15 minutes; without the scheduler `POST
  /api/v1/admin/job/external-service/sync`). It writes `vga: std` on the templates that a Template updates import
  of this portal holds (not on a withdrawn one) and on the stopped servers and imported images in the pools of its
  projects. A second sync writes nothing. A running server changes at its next Stop then Start from the portal, or
  at an action that stops and starts it; a Reboot does not change it. Kubernetes workers, the platform's machines
  and the seed are never written: they change when their node group or the platform is rolled.
- **Tenant clusters and databases made before the upgrade** keep their chart until their own chart update. A new
  database of one instance has no disruption budget and restarts when its node is drained.
- **The ArgoCD address:** leave it unset on a site where tenant traffic leaves under a Proxmox node's address
  (SNAT); the Check row `argocd-access` warns there.
- **A platform installed from a `dev-<sha>` build of `main`** is still refused (`bom_unsupported`).
- The app chart is `0.10.0` with appVersion `v0.10.0`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.10.0`.

Verified on a lab. One round on 2026-11-20, on one lab Proxmox VE cluster of three nodes, on the published images
of the candidate `v0.10.0-rc.1`, checked afterwards by a read-only audit. Nothing was run on the published images
of `v0.10.0` itself. The code of this release is the code of the candidate plus one change, the fixes for what the
round found (under Fixed: the `seed-backup` row that says what it cannot see, the withdraw warning, the audit row
of a Delete that deleted nothing; with them the guides' statements about the round). That change has not run on a
lab.

- **The candidate's images and the portal upgrade.** A Compose upgrade from `v0.9.1`, with the PostgreSQL and
  Keycloak containers kept.
- **The move from the BOM `0.9.1` to `0.10.0-rc.1`**, on a three-node platform in bridge mode: 23 seconds, no
  retry, no machine replaced, no write on Proxmox, the seed's firewall file untouched, the platform API answering
  throughout. One tenant server was present; no tenant cluster and no database.
- **The graphical console.** Six templates of the portal changed at the first sync, and later syncs wrote nothing.
  Withdrawn templates and templates the portal did not import were untouched. No running VM had a pending change.
  A stopped server was changed at a sync and started with `vga: std`. A new server and a new worker had `vga:
  std`. Both tabs of the portal's console page showed a login prompt on Ubuntu 26.04 servers. Volume attach and
  detach worked on an old running server.
- **Delete of a withdrawn template.** The refusals `not_withdrawn` and `template_in_use`; one destroy, with only
  the template's base disk gone; the receipt reading `template_missing`; a second Delete answering `unchanged`.
- **The ArgoCD address.** An address from the pool; `400` for an address outside it and for `/0`; the row
  `argocd-access` as a warning and as ok; HTTPS with the self-signed certificate from the operator address; no
  answer from the Proxmox nodes, from the gateway host and from a tenant VM, on a layout without SNAT; clearing
  back to ClusterIP.
- **The machine replacement stays dark.** No roll was offered, and its phrase and `part` were refused.
- **A tenant cluster on chart 0.1.15**, created and deleted.

Not verified on a lab:

- **This release itself:** its published images, its tag's workflows, and the move of a platform to the BOM
  `0.10.0` from any previous BOM, the record-only step from `0.10.0-rc.1` included.
- **The fixes merged after the candidate:** the corrected `seed-backup` row on a real Proxmox (the rule by which
  Proxmox hides the archives is modelled by a test fake), the withdraw warning that names who still uses a copy,
  and the audit row of a Delete that deleted nothing.
- **The seed's k3s upgrade, entirely:** stage 5a, `seed_backup_missing` and its override, an old archive of
  another VM. The `seed-backup` row turning ok was not seen: the row of the candidate could not.
- **The replacement of the platform cluster's machines**, and any move of k3s or Kubernetes.
- **An upgrade with a tenant cluster or a database on the platform.**
- **A database on chart 0.5.2** and the drain of its node; **three control plane machines.**
- **The graphical console:** the Console button of the Proxmox web UI (the display was read through the portal's
  console page and from the VM's configuration), the graphical console of a worker and of a Debian guest, the
  revert after a start race, and Stop then Start of a server that still has the serial display, without a sync
  between.
- **Delete of a withdrawn template:** `linked_clones`, `template_published`, `template_mismatch`,
  `template_present`, `job_running`, `smoke_running`, a node group pinned to the image, and a template with a
  cloud-init drive (the deleted one had none).
- **The ArgoCD address:** the sign-in, `argocd_address_unavailable`, and a site with SNAT.
- **`routed` mode and a one-node site.**
- Everything the sections of 0.9.1 and 0.9.0 say is not verified still holds, except what is listed as verified
  here.

Where an entry below says the BOM on `main` is `0.10.0-dev`, that was the state at that merge: this release's BOM
is `0.10.0`, and the change after its tag sets `main` back to a `-dev` one.

### Added

- **Managed services: the platform cluster's ArgoCD can have an address with TLS.** Opt-in, off by default: under
  **Access** in step 1 name one address of the load balancer range and the operator networks
  (`spec.access.argocd {address, sources[]}`), save and apply. The ArgoCD that holds every tenant cluster and
  database then answers at `https://<address>` for those networks only, with ArgoCD's own self-signed certificate
  (the browser warns once), and the ready card shows the link (`status.argocdUrl`). The block can be set, changed and
  cleared on an installed platform; the plan lists it (`access.argocd`) with what it restarts. The seed's ArgoCD
  never gets an address. No ArgoCD account is created and no password is stored, shown or returned: sign in as
  `admin` with the password read through the one-hour platform kubeconfig. New Check row `argocd-access`, which
  warns when an operator network contains a Proxmox node's address (where tenant traffic leaves under that address,
  tenants then reach the login page); new failure `argocd_address_unavailable` when MetalLB does not give the
  address (it names the Service that holds it), with the platform still serving. A platform that does not set the
  block is rendered exactly as before. **Not verified on a lab**: whether the platform cluster enforces the operator
  networks is the open point, so do not set it on a production site yet. A domain with a trusted certificate is not
  built ([managed-services.md](docs/managed-services.md#an-address-for-the-platform-clusters-argocd)).

- **Managed services: an upgrade can replace the platform cluster's machines for a newer Kubernetes.** A release
  whose BOM names a newer Kubernetes version (a patch, or one minor), or a rebuilt node image, was refused. It is now
  two actions in step 4: **Seed and node image** (`part: "seed"` on `POST …/runs`: the node image on every node, the
  seed's k3s and charts, no tenant touched) and **Replace the machines**, with its own typed phrase. Cluster API
  replaces every machine, and the run orders it: the control plane first, then one node group and node at a time,
  with the tenants waited for between units. The plan says first that there is no way back once the new control
  plane machine has joined, then per unit what stops serving meanwhile and what is cloned and deleted on Proxmox. A
  node without room for one more machine of a group gets that unit delete-first, and the plan says so; a control
  plane node without room refuses the upgrade. New refusals, each with its fix: `node_image_missing`,
  `seed_not_upgraded`, `capacity_short`, `drain_blocked`, `platform_unhealthy`; in the run `rollout_stalled`; `400
  bad_part`. Cancel and Retry are Pause and Resume while machines are replaced. **This release's BOM changes neither
  Kubernetes nor k3s, so no upgrade between released BOMs does this yet**; it has run in tests against a fake seed
  only, never on a lab, and every duration in the plan is marked "not measured". A minor step has no BOM that could
  carry it while third-party charts are refused
  ([managed-services.md](docs/managed-services.md#the-platform-clusters-machines)).

- **Template updates: delete a withdrawn template.** A withdrawn template stayed in Proxmox until someone ran
  `qm destroy` on the node. The Imported templates table now has **Delete** on withdrawn rows
  (`DELETE /admin/template-updates/templates/{importId}`, `admin:service:manage`, audited). It destroys the
  template with the disks its configuration names, never with "destroy unreferenced disks", and frees the import
  receipt. It is refused, with the reason, while a Kubernetes cluster's node group or a provider's version map
  still names the template, a linked clone uses its disk, or a smoke test or its import is running. The digest
  stays blocked. No new Proxmox privilege. Not run on a lab yet
  ([template-updates.md](docs/template-updates.md#delete-a-withdrawn-template)).

- **Managed services: an upgrade can replace the seed's k3s.** A platform could not move to a release whose BOM
  names a newer k3s. An upgrade run now has a stage `seed-k3s` (5a, "Upgrade the seed's k3s") that replaces the k3s
  binary on the running seed VM and restarts k3s, through a Job on the seed that runs the first boot's own download
  and sha256 check. The plan says what is away (the seed's API, about a minute: no machine is repaired or replaced
  and no run goes on; the platform cluster, tenant clusters and databases keep serving) and lists what the run
  writes on the seed: the Job, an etcd snapshot `pre-upgrade-<version>` and the previous binary as
  `/usr/local/bin/k3s.prev`. k3s moves by a patch, a k3s build or one minor, never back. New failure codes
  `seed_k3s_failed`, `seed_k3s_refused`, `seed_k3s_timeout`. The Kubernetes version, the node image and kube-vip
  stay refused. **No released BOM names a newer k3s yet**, so no upgrade between releases does this today; it has
  run in tests against a fake seed only, not on a lab
  ([managed-services.md](docs/managed-services.md#the-seeds-k3s)).
- **Managed services: a seed backup check, and an upgrade of the seed's k3s asks for one.** The check has a row
  `seed-backup`: ok when a store of the seed's node holds a `vzdump` archive of the seed VM younger than 24 hours,
  a warning otherwise (never a failure). An upgrade that replaces the seed's k3s is refused with
  `409 seed_backup_missing` until the row is ok or the operator ticks "I took a backup of seed VM … that the portal
  cannot see" (`seedBackupTaken` on `POST …/runs`, recorded on the run and in the audit log). The row reads with the
  portal token, which may not see every backup store.
- **Charts `proxmox-kamaji-cluster` 0.1.15 and `database-cluster` 0.5.2**, published from their tags and pinned by
  the platform BOM (the entry under Changed). 0.1.15
  reads two optional keys of a node group, `kubernetesVersions` (`<node>: <version>`) and `deleteFirstOn` (a list of
  nodes: `maxSurge 0`, `maxUnavailable 1`), on the MachineDeployment only; without them it renders what 0.1.14
  rendered, so no machine is replaced. 0.5.2 sets `enablePDB: false` on a PostgreSQL or FerretDB database of one
  instance, whose budget blocked every drain of its node; the database then restarts when its node is drained.
  Existing databases keep their chart until `bump-chart` ([managed-dbaas.md](docs/managed-dbaas.md#chart-pin--platform-update)).

### Changed

- **Guides: what the lab round of v0.10.0-rc.1 showed, and what it did not.** The graphical console, Delete of a
  withdrawn template, the ArgoCD address and the move from the BOM of 0.9.1 were run on a lab on 2026-11-20; the
  guides name what was seen and what is still not run (among them: every part of the seed's k3s upgrade and of the
  replacement of the platform cluster's machines, the ArgoCD address on a site with SNAT). New note: take the seed
  backup some minutes before an upgrade, as on a busy node or a slow disk a backup of the seed can make its k3s
  restart (a run that meets the restart retries)
  ([managed-services.md](docs/managed-services.md#the-seeds-k3s), [template-updates.md](docs/template-updates.md#7-open-items)).

- **The platform BOM pins cluster chart `proxmox-kamaji-cluster` 0.1.15 and database chart `database-cluster`
  0.5.2**, both published from their tags (`proxmox-kamaji-cluster-v0.1.15`, `database-cluster-v0.5.2`), by the
  digest ghcr.io serves: 0.1.15 for the platform cluster (`pc-platform`) and for `tenantCharts.kubernetes`, 0.5.2
  for `tenantCharts.database`.
  - A new install on this BOM registers its Kubernetes provider with chart 0.1.15 and its database provider with
    chart 0.5.2: a new database of one instance has no disruption budget. An upgrade run writes both on the
    providers (`record only` in the plan); an existing tenant cluster or database keeps its chart until its own
    bump.
  - `pc-platform` lists 0.1.13 and 0.1.14 in `upgradeFrom`, so a platform on any BOM from 0.8.0 to 0.9.1 may move
    to this one in one run. 0.1.13 stays in the list because a build accepts only its own BOM as the target: without
    it a 0.8.x platform would have to install a 0.9 release first. The release check (`TestUpgradeRollsNoMachine`)
    renders the chart each of the seven previous BOMs pins with the values that release rendered, and chart 0.1.15
    with this build's values, and finds the same control plane, machine templates and bootstrap templates: the
    move replaces no machine. No run writes the two new keys of 0.1.15 yet.
  - A platform is not changed by the upgrade of ProxCloud: it stays on its BOM until an operator starts the
    upgrade. A build of `main` (`0.10.0-dev`) stays refused as a target. Not run on a lab.

- **The Proxmox console of a VM made by ProxCloud opens on the graphical display.** Templates were made with
  `vga: serial0`, so the Proxmox web UI opened every server, worker and platform machine on the serial terminal
  ("starting serial terminal on interface serial0", an empty page until a key is pressed). A new template now has
  `vga: std` and keeps `serial0: socket`: the graphical console is the default and the serial terminal is still
  there (Console → xterm.js, `qm terminal`). The portal's own console page works as before, on both tabs.
- **Existing templates and stopped servers are moved by the portal at the next services sync; a running server at
  its next start from the portal.** The write is what `qm set <vmid> --vga std` writes. No new Proxmox privilege.
  - At once: the templates a Template updates import of this portal holds (not a withdrawn one), and the stopped
    servers and imported images in the pools of its projects.
  - At the next start from the portal: a running server. Stop then Start, or an action that stops and starts it
    (resize, rebuild, restore, rollback), sets `vga: std` while the server is stopped. A Reboot does not.
  - Not written by the portal: Kubernetes workers and the platform's machines and seed. They change when their node
    group or the platform is rolled to a template that has `vga: std`.
  - **The portal never writes `vga` on a running VM and never leaves a pending change.** Proxmox cannot apply `vga`
    to a running VM, and the portal's inventory and volume and server operations refuse a VM with a pending change.
  - By hand: `qm set <vmid> --vga std`, then stop and start the VM, in one go: between the two the VM has a pending
    change and its project's sync and operations on it are refused.
  - What a guest shows on its graphical console and on its serial terminal depends on the guest's own kernel
    arguments; this was not measured.
  - A template built by `images/build.py`, or any template this portal did not import, is not written: run the
    command on it. See [docs/template-updates.md](docs/template-updates.md#console-display).
- **The platform BOM on `main` is `0.10.0-dev` again, and the build carries the BOM of `0.9.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.9.1.yaml`). The `v0.9.1` release commit left
  the BOM at `0.9.1`, and the release gate refuses only `-dev`. A platform installed by `v0.9.1`, or moved to its
  BOM by an upgrade, stays on it under a later build and is not refused (`bom_unsupported`) while the file is
  carried. Its pins are those of `0.9.0`, so `TestUpgradeRollsNoMachine` compares seven previous BOMs with the
  BOM on `main`: three of 0.8.x, the two candidates, `0.9.0` and `0.9.1`. No apply on such a platform was run on
  a lab by this change.

- **The platform BOM on `main` is `0.10.0-dev` again, and the build carries the BOM of `0.10.0-rc.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.10.0-rc.1.yaml`). The commit of the candidate
  `v0.10.0-rc.1` left the BOM at `0.10.0-rc.1`, and the release gate refuses only `-dev`. A platform installed by
  the candidate, or moved to its BOM by an upgrade, stays on it under a later build and is not refused
  (`bom_unsupported`) while the file is carried. Its pins are those of `main`, so the plan from it has one change,
  the release, and `TestUpgradeRollsNoMachine` compares eight previous BOMs with the BOM on `main`: three of 0.8.x,
  the two candidates of 0.9.0, `0.9.0`, `0.9.1` and `0.10.0-rc.1`. Four tests took the first file under `previous/`
  for the oldest BOM; `0.10.0-rc.1` sorts before `0.8.0`, so they name `0.8.0` now. No apply on such a platform was
  run on a lab by this change.

- **The platform BOM's release is `0.10.0`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.10.0-rc.1`: the two BOMs differ in the `release` line alone, and no chart of the product changed since its
  tag (`proxmox-kamaji-cluster-v0.1.15`, `database-cluster-v0.5.2`, `proxcloud-platform-v0.1.3`), so no chart tag
  is needed. It names k3s `v1.35.10+k3s1`, Kubernetes 1.35.10 and the node image that `0.9.1` names. The release
  gate passes on it and on the eight previous BOMs. The app chart, `pc_versions.proxcloud` and `DEFAULT_VERSION`
  of `proxcloud-up.sh` move to `0.10.0` / `v0.10.0`. `deploy/platform/previous/0.10.0.yaml` is added by the change
  after the tag.

### Fixed

- **Managed services: the `seed-backup` row no longer says that no backup exists when it cannot see one.** Found on
  the lab round of v0.10.0-rc.1: with today's archive of the seed VM on the site's backup store the row read "the
  stores read (…) hold none". Proxmox lists a VM's backups only to a token with `VM.Backup` on that VM or
  `Datastore.Allocate` on the store, and answers `200` with a list without them otherwise; the portal token is fenced off
  the seed VM by design, so on a site set up by the product's own command the row could never be ok, and an upgrade
  that replaces the seed's k3s could only start with the override. No privilege is added and the fence is as it was.
  The row now reads with the portal token and with `capmox@pve!capi`, counts a store as read only where one of them
  holds what the listing needs (the import store, the platform's ISO store), and for every other backup store says
  that the portal cannot see backups of the seed VM there and what to check in Proxmox. It is then `ok` (for
  information) and a warning only while such an upgrade waits to be started. `409 seed_backup_missing` and the box
  in step 4 now say what the operator confirms: an archive of this seed VM, made after the seed VM was created,
  younger than 24 hours (`seedBackupTaken` is the usual answer). The corrected row has run in tests against a fake
  Proxmox that filters the list as Proxmox does, not on a lab
  ([managed-services.md](docs/managed-services.md#the-seeds-k3s),
  [proxmox-permissions.md](docs/proxmox-permissions.md#what-the-fence-hides-from-the-portal-the-seeds-backups)).
- **Template updates: a withdraw of a Kubernetes node image warns only when something still names a withdrawn
  copy.** It always said that the provider's version map names the withdrawn VMIDs and to "replace that entry",
  also when the map already was the line it printed. The warning now comes only for copies a provider's version, a
  cluster's node group or a platform still names, and says who.
- **Template updates: a Delete that deleted nothing no longer writes `deleted` false → true in the audit log.** The
  repeat of a Delete, and a Delete of a template already gone from Proxmox, answer `unchanged`; their audit row now
  keeps `deleted` false.
- **Managed services: an old archive of another VM no longer counts as the seed's backup.** Proxmox gives a freed VMID
  to the next VM and a backup store keeps the old archives, so the `seed-backup` row could be satisfied by a year-old
  archive of an earlier VM that had the seed's VMID. An archive now counts only when it was made after the seed became
  ready and, when it has notes, the notes name the seed VM; the row says how many archives it left out.
- **Managed services: an apply no longer rewrites the seed VM's firewall options when they are as wanted.** Stage 3
  sent the same options at every run, which changed the VM's firewall file while the plan listed nothing for the
  site. It now reads them first and writes only a difference.
- **Managed services: the node wait counts every control plane machine.** With a control plane of three the wait for
  the nodes (stage 9) expected one control plane node and could not end.

### Known issues

- **A snapshot backup of the seed can restart its k3s.** On a busy node or a slow disk the backup stalls the
  seed's disk for seconds; the seed's single k3s server then loses its leader election, exits and is started again
  by systemd, and the seed's API is away for about a minute. Seen on the lab twice: during the seed's own
  `vzdump`, and during an apply with no backup running (that run retried its stage and passed). The same `vzdump`
  also left the etcd of another control plane VM on that node unhealthy until the backup ended. Take the backup in
  a quiet hour, some minutes before a run, and wait until the seed's API answers again ("The seed's k3s",
  `docs/managed-services.md`). Not fixed in this release.
- **Seed ISOs of deleted servers stay on the seed store until they are cleaned.** Without the optional complete
  inventory reader the portal keeps the first-boot CD of a deleted server, which holds the server's user data, and
  warns "seed cleanup pending" in the API log at every sync. It affects every site without that reader ("Optional
  complete inventory reader", `docs/server-seed-safety.md`).
- **The `seed-backup` row cannot see a backup on most stores.** The portal token is kept off the seed VM on
  purpose, and Proxmox leaves the seed's archives out of its list. The row now says so; in the candidate it said
  that the stores hold none. An upgrade that replaces the seed's k3s will then be started with the box "I took a
  backup". No upgrade to this release's BOM asks for it.
- **The upgrade of the seed's k3s and the replacement of the platform cluster's machines have never run on a real
  seed or cluster:** tests against a fake seed only, and no released BOM starts them. Run the first release whose
  BOM does on a lab first.
- **The ArgoCD address is reachable by tenants** on a site where tenant traffic leaves under a Proxmox node's
  address that an operator network contains; the Check row `argocd-access` warns. Leave the address unset there.
- **A Reboot from the portal does not change the display of a running server:** Proxmox cannot change `vga` on a
  running VM. Stop then Start does.
- **The root command stops when the platform's token ids are taken.** The ids `capmox@pve!capi` and `csi@pve!csi`
  are fixed. It affects a new install on a Proxmox cluster where one of them exists for something else ("The root
  command of an installation", `docs/managed-services.md`).
- The known issues of 0.9.1 that still hold: a chart version that changes only its hook does not run the hook;
  workers do not join when the node that holds the platform's addresses also translates tenant traffic; the
  platform upgrade is not shown on a lab with tenants; the health check's guard; Replace on a node group of one; a
  standalone volume that loses its name; the `pve-api-rule` row behind a translating router; seed ISOs of servers
  created before 0.8.0.

## [0.10.0-rc.1] - 2026-11-20

The first release candidate of 0.10.0, published as a prerelease and never as `latest`. It is not the release:
`v0.9.1` stays the current stable release, and the chart, the installer and the Compose default still name it. It
holds everything 0.10.0 brings since 0.9.1 except the fixes for what its lab round found, and it exists so that a
lab can run it: nothing of it had run on a lab when it was tagged. Its content is the section of 0.10.0 above;
nothing is repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.10.0-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had:
  cluster chart 0.1.15, database chart 0.5.2, platform chart 0.1.3. No chart of the product changed since its tag
  (`git diff proxmox-kamaji-cluster-v0.1.15 -- deploy/charts/proxmox-kamaji-cluster`,
  `git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
  `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing), so no chart tag is
  needed. It is a release BOM: no `pending` digest and no `0.0.0+` version, and the release gate passes on it and
  on the seven previous BOMs.
- **The BOM moves neither k3s nor Kubernetes.** It names k3s `v1.35.10+k3s1`, Kubernetes 1.35.10 and the node image
  that 0.9.1 names. So no upgrade to it runs the stage `seed-k3s` or asks for a seed backup, and no upgrade to it
  replaces a machine of the platform cluster: both features ship without a BOM that starts them. The first real
  replacement of machines comes with a later BOM.
- **The plan from each previous BOM to it is allowed, with no refusal.** From 0.9.1, 0.9.0 and the two candidates
  of 0.9.0, four changes: the release, `tenantCharts.kubernetes` (0.1.14 to 0.1.15) and `tenantCharts.database`
  (0.5.1 to 0.5.2), all three record only, and `pc-platform` 0.1.14 to 0.1.15 (class chart, stage `cluster`, wave
  3). From the three 0.8.x BOMs, seven changes: the same four, with the cluster chart from 0.1.13, and `capi`
  (stage `seed-platform`, wave 2), `capi-tenant` and `platform` (stage `workload-platform`, waves 2 and 3) from
  0.1.2 to 0.1.3, class chart. `TestUpgradeRollsNoMachine` compares all seven pairs and passes. The confirmation
  is `upgrade <name> to 0.10.0-rc.1`.
- **A platform moved to the candidate's BOM stays on it** until an operator upgrades it again. Later builds accept
  that platform only while they carry `deploy/platform/previous/0.10.0-rc.1.yaml`, which the change after the tag
  adds: keep such a platform on a lab.
- The app chart (`0.9.1`), `pc_versions.proxcloud` and `DEFAULT_VERSION` of `proxcloud-up.sh` do not move. The
  tag's workflows publish the five images as `v0.10.0-rc.1` and the app chart as `0.10.0-rc.1` with that
  appVersion. Run the candidate with `./proxcloud-up.sh upgrade v0.10.0-rc.1`, or `--version v0.10.0-rc.1` on a
  new install.
- Thirteen dependency updates since 0.9.1: in the Go
  module the MCP Go SDK, the AWS S3 client, `koanf` 2.4.1 and `mustache` 1.4.2; in the consoles
  `@stripe/stripe-js` 10.3.0, `@base-ui/react` 1.9.0, `radix-ui` 1.8.1, `lucide-react` 1.73.0, `react-router-dom`
  7.18.5, `vite` 8.3.8 and `oxlint` 1.94.0.

### Verified on a lab

Nothing, when the candidate was tagged: nothing of 0.10.0 had run on a lab, and the candidate's images do not exist
before the tag. The lab round ran on them afterwards; what it showed is in the section of 0.10.0. The list below
is the state at the tag.

### Not verified

- This candidate itself: its images, the tag's workflows, and the move of a platform to the BOM `0.10.0-rc.1` from
  any previous BOM.
- The graphical console: a server from a newly imported template on noVNC, with xterm.js and `qm terminal` still
  working, and the portal's console page on it, both tabs; an upgrade with old running servers and workers, two
  syncs, no pending change on a running VM and volume and server operations still working; an old template and an
  old stopped server after one sync and a second sync that writes nothing; an old running server after Stop then
  Start from the portal; a withdrawn template, a pipeline-built template and a VM outside the portal's pools left
  as they are; no `vga` write on a platform machine at an apply; what each console shows on a worker and on new
  Debian and Ubuntu servers.
- Delete of a withdrawn template: the destroy with the portal token, base disk and cloud-init drive gone and no
  other volume removed; `linked_clones` on the site's storage type; `template_in_use` for a cluster whose node
  group still carries the image.
- The seed's k3s, on any seed: on a build whose BOM differs in k3s alone, with a tenant cluster and a database
  under load, the run and the seed API gap, no gap for tenants, the node at the target version, the etcd snapshot,
  the seed's Applications and the clusterctl inventory unchanged, no Machine replaced; a wrong sha256 giving
  `seed_k3s_failed` with k3s untouched, then Retry; `409 seed_backup_missing`, and whether the `seed-backup` row
  sees the site's own kind of backup store.
- Replacing the platform cluster's machines, on any cluster: on a BOM with the next Kubernetes patch and its node
  image, the one-node shape from the template import to the finished roll with the gap per unit; Pause after the
  control plane and Resume; a PodDisruptionBudget that blocks; memory too short for the control plane's surge and
  for a group; a surge machine that fails to join, then Retry; `unexpected_rollout` when a machine size is edited
  with the upgrade; three control plane machines; a minor step.
- The ArgoCD address: the link over HTTPS from an operator address; the refusal from a tenant VM and from an
  address outside the operator networks (the gate of the feature); the sign-in; tenant status in the consoles
  afterwards; `argocd_address_unavailable` for an address a tenant load balancer holds; clearing the block; no
  change of any Application on a platform that never sets the block; the `argocd-access` warning on a single node
  that translates tenant traffic.
- The three entries under Fixed that the candidate held (the old archive of another VM, the seed's firewall
  options, the node wait), a tenant cluster on chart 0.1.15, and a drain of a
  node that holds a one-instance database on chart 0.5.2.
- Everything the sections of 0.9.1 and 0.9.0 say is not verified.

### Known issues

- A Reboot from the portal does not change the display of a running server: Proxmox cannot change `vga` on a
  running VM. Stop then Start does.
- The replacement of the platform cluster's machines and the upgrade of the seed's k3s have never run on a real
  cluster or seed: tests against a fake seed only.
- The ArgoCD address is reachable by tenants on a site where tenant traffic leaves under a Proxmox node's address
  that an operator network contains; the Check row `argocd-access` warns.
- The known issues of 0.9.1 that still hold: the root command stops when the platform's token ids are taken; a
  chart version that changes only its hook does not run the hook; workers do not join when the node that holds the
  platform's addresses also translates tenant traffic; the platform upgrade is not shown on a lab with tenants;
  the health check's guard; Replace on a node group of one; a standalone volume that loses its name; the
  `pve-api-rule` row behind a translating router; seed ISOs of servers created before 0.8.0.

## [0.9.1] - 2026-11-19

A patch release on top of `v0.9.0`. It adds no feature: it fixes what the first production upgrade to `v0.9.0`
and the last lab round of `v0.9.0` found.

- **A provider sync no longer writes on withdrawn node templates.** The `onboot` pass of `v0.9.0` also wrote on
  Kubernetes node templates whose import was withdrawn. It now leaves them alone (Fixed).
- **The plan lists the Proxmox API firewall rule a run writes.** On a platform installed by 0.8.x the run wrote
  the rule while the plan did not name it (Fixed).
- **The stored result of the tenant path test ends with Remove**, and the Check row `boot-store` no longer calls
  a leftover volume a VM (Fixed).
- **Deleting a tenant cluster no longer sends a token delete that must fail** (Fixed).
- **PostgreSQL is pinned to a minor release**, in the Docker Compose install and in the app chart, so that a
  portal upgrade restarts the database only when a release says so (Changed).

The platform BOM of this release is `0.9.1`. It pins what `0.9.0` pins, cluster chart 0.1.14 and platform chart
0.1.3: the `release` line alone differs, and neither chart changed since its tag
(`proxmox-kamaji-cluster-v0.1.14`, `proxcloud-platform-v0.1.3`). The release carries the BOMs of `0.8.0`,
`0.8.1`, `0.8.2`, `0.9.0-rc.1`, `0.9.0-rc.2` and `0.9.0` for the platforms those builds installed.

Upgrading from `v0.9.0`. From an older release, read the section of `v0.9.0` as well: its steps still apply.

- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.9.1`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first. The PostgreSQL line of
  `compose.yaml` changes in this release (`17-alpine` to `17.12-alpine`), so expect this upgrade to recreate
  the PostgreSQL container, and Keycloak with it, one more time; the volumes stay. Later upgrades restart the
  database only when that line changes. Not run on a lab.
- **A Helm install** restarts the portal's database once at the `helm upgrade`: the CloudNativePG image name
  changes from `18` to `18.6`. Not run on a lab.
- **An installed platform stays on its BOM** and keeps working. From `0.9.0` the move to `0.9.1` is one change
  in the plan, the release, record only: nothing is synced for it, and it can wait. From a 0.8.x BOM the plan is
  that of `v0.9.0`, six changes and no refusal, and it now also lists the entry `pve-api-rule` while the cluster
  firewall rule is missing. `TestUpgradeRollsNoMachine` compares all six previous BOMs with this one and passes.
  No move to `0.9.1` was run on a lab.
- **Then let one provider sync run** (every 15 minutes; without the scheduler `POST
  /api/v1/admin/job/external-service/sync`). It writes `onboot: 1` on the Kubernetes node templates that an
  import of this portal still holds, staged or published, and on the running workers of its tenant clusters,
  where the option is missing. It skips a withdrawn import (tag `pc-tu-withdrawn`), which it neither reads nor
  writes, a template the portal did not import, a worker that is not running, and everything that has the
  option already. An `onboot` that a sync of `v0.9.0` wrote on a withdrawn template stays: nothing takes it
  back (`docs/managed-services.md`, "After a Proxmox node reboots").
- The app chart is `0.9.1` with appVersion `v0.9.1`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.9.1`.

Verified on a lab: nothing. No fix of this release has run on a lab yet, and nothing was run on its published
images. What `v0.9.0` verified is in its section and in `docs/releases/v0.9.0.md`. One site run stands behind two
of the entries: on 2026-11-19 a production site with one tenant cluster and one database moved its platform from
the BOM `0.8.0` to `0.9.0`, on the published images of `v0.9.0` (one run of 68.6 seconds, no retry, nothing
rolled, no tenant request failed in 149 looks). That run wrote the firewall rule the plan had not listed, and the
portal upgrade before it recreated PostgreSQL and Keycloak. It is a site run of `v0.9.0`, not a lab run of this
release.

Not verified on a lab:

- **This release itself:** its published images, its tag's workflows, and the move of a platform to the BOM
  `0.9.1` from any previous BOM, the record-only step from `0.9.0` included.
- **The plan entry `pve-api-rule`** on a platform that has no rule, and the run after it.
- **The `boot-store` row** on a store that holds a leftover volume.
- **`404 no_probe`** from the tenant path test after a Remove and after a new install.
- **A tenant cluster delete** without the failing `csi@pve` token delete in the Proxmox log.
- **The provider sync on a site with a withdrawn node template**, and a worker cloned from one.
- **A Compose upgrade** from `v0.9.0` to this release, and a later one that keeps the container ids of
  PostgreSQL and Keycloak; the `helm upgrade` with the new CloudNativePG image name.
- **The operator console:** the message for `409 bom_installed` and the count on the card "Managed clusters".
- **The button on the ready card that tests and publishes the node image and offers the version.** The notes of
  `v0.9.0` dropped it from their list without a run: it was never pressed on a lab.
- Everything `v0.9.0` and the releases before it say is not verified still holds
  (`docs/releases/v0.9.0.md`, "Not verified").

The entries below were written when each change merged. Where one says the BOM on `main` is `0.10.0-dev`, that
was the state at that merge: this release's BOM is `0.9.1`, and the release after it sets `main` back to a `-dev`
one.

### Changed

- **The operator's provider page counts the clusters behind the provider's chart.** The card "Managed clusters"
  already listed each cluster's chart version and offered Bump; it now says how many are behind. The tenant
  console already shows "Platform update available" on such a cluster. `docs/managed-k8s.md` says where both
  read it, and that the cluster's row in the tenant API carries its own version only.
- **`docs/proxmox-permissions.md` says who gets `403` on `GET /cluster/status` and why no privilege is added.** It
  is CAPMOX's client library reading the cluster's name with the cluster's token; the answer is ignored by
  design and nothing degrades.
- **The Docker Compose install pins PostgreSQL to a minor release** (`postgres:17.12-alpine` in
  `deploy/compose/compose.yaml`, up to 0.9.0 the moving tag `17-alpine`). `proxcloud-up.sh upgrade` pulls every
  image of the file, so a newer `17-alpine` recreated PostgreSQL, and Keycloak with it, during a portal upgrade
  (seen on a production site at the upgrade to 0.9.0). The database now restarts only when a release changes that
  line; Dependabot proposes the next minor (`.github/dependabot.yml`, `docker-compose`). The tag is a version
  without a digest, as Traefik and Keycloak are pinned: a rebuild of `17.12-alpine` by its publisher would still
  be pulled. The app chart's CloudNativePG image is pinned the same way (`cnpg.imageName`
  `ghcr.io/cloudnative-pg/postgresql:18.6`, before `18`; both named the same image on 2026-11-19). On an
  installed chart the changed image name makes the operator restart the portal's database once at the next
  `helm upgrade`. Not run on a lab: an upgrade of a Compose install from 0.9.0, and that `helm upgrade`. Left as
  they are: the bundled Bitnami PostgreSQL and RabbitMQ subcharts (off by default; `values.yaml` says why),
  `haproxy:3.0-alpine` of the tenant chart `database-cluster`, and the `postgres:17-alpine` of the development
  `docker-compose.yml` and of the integration tests.
- **CI: `rebrand-gate` installs nothing.** It searched with ripgrep installed by `apt-get`, and failed twice on
  an Ubuntu mirror error. It now searches the tracked files with `git grep` (`-P` for the previous-backend
  terms), which the runner has. The `go-test` job runs `gofmt -l .` and fails on any file it lists: nothing
  checked the format. With the Go of CI and of the images (1.27.4) `main` is formatted; gofmt of Go 1.26 lists
  `internal/platform/project/quota_usage.go`, because the two format a return of two composite literals
  differently, so format with the Go that CI uses.

- **The platform BOM on `main` is `0.10.0-dev`, and the build carries the BOM of `0.9.0`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.9.0.yaml`). The `v0.9.0` release commit left
  the BOM at `0.9.0`, and the release gate refuses only `-dev`. A platform installed by `v0.9.0`, or moved to its
  BOM by an upgrade, stays on it under a later build and is not refused (`bom_unsupported`) while the file is
  carried. Its pins are those of the two candidates, so `TestUpgradeRollsNoMachine` compares six previous BOMs
  with the BOM on `main`: three of 0.8.x, the two candidates and `0.9.0`. No apply on such a platform was run on
  a lab by this change.
- **The platform BOM's release is `0.9.1`** (`deploy/platform/platform-bom.yaml`), with the pins of `0.9.0`: the
  two BOMs differ in the `release` line alone, and neither chart of the product changed since its tag. The
  release gate passes on it and on the six previous BOMs. The app chart, `pc_versions.proxcloud` and
  `DEFAULT_VERSION` of `proxcloud-up.sh` move to `0.9.1` / `v0.9.1`. `deploy/platform/previous/0.9.1.yaml` is
  added by the change after the tag.

### Fixed

- **The provider sync no longer writes `onboot` on withdrawn Kubernetes node templates.** The pass of v0.9.0 set
  it on every node template an import receipt of the portal holds, withdrawn imports included. A withdrawn
  template is kept as it was for forensics and no new node group gets it, so the pass now skips a template tagged
  `pc-tu-withdrawn` without reading it (`templateupdates.StartNodeTemplatesAtBoot`). A cluster created on such a
  template before the withdraw still clones from it, and Proxmox copies `onboot` at the clone, so such a worker
  has none until the sync's worker pass sets it, at the next sync (`docs/managed-services.md`, "After a Proxmox
  node reboots"). Not verified on a lab.
- **Deleting a tenant cluster no longer asks Proxmox to delete a `csi@pve` token that was never made.** No cluster
  made since the CSI gateway has one, and Proxmox answered the delete with a `500` in its log. The release now
  lists the tokens of `csi@pve` and sends the delete only when the cluster's id is among them. A token that exists
  and cannot be deleted still fails the release, and when the list cannot be read the delete is sent as before
  (`pve.RevokeClusterCSIToken`). Not verified on a lab.
- **Admin console: the refusal `409 bom_installed` reads as a plain message.** When an upgrade had finished in
  another session and Upgrade was pressed on a page that still showed the plan, the page showed the API's own
  text about `targetBom`. It now says that the platform is already on this release and that no run was started.
  The console's mock API answers `bom_installed` as the API does and carries the plan's current impact text.
- **`docs/managed-services.md` names the one upgrade that ran on a production site** (2026-11-19, a single-node
  site with one tenant cluster and one database, 0.8.0 to 0.9.0) where it said that none had run with tenants or
  on a production site.
- **The plan of an installed platform names the cluster firewall rule a run writes.** On a platform installed by
  0.8.x the rule `proxcloud:pve-api:<installation>` does not exist, and an apply or an upgrade run writes it in
  stage 3 (`seed-vm`) while the plan listed no such change (production upgrade of 2026-11-19: six changes in the
  plan, the rule not among them). `GET …/plan` now reads the cluster firewall and, while the rule is missing or
  differs from the settings, lists the entry `pve-api-rule` (kind `site`, stage `seed-vm`) in the apply's changes
  and in the upgrade's, with the rule spelled out and what the run does: one `ACCEPT` rule for the platform's
  addresses to the Proxmox API port, nothing else in the firewall touched. It never blocks; with the rule in place,
  or when the firewall cannot be read, the plan is as before. Not seen on a lab.
- **The Check row `boot-store` no longer calls a leftover volume a VM.** It listed the ids in the volumes' names
  as "other VMs", so an orphan `vm-129-cloudinit` read as "VM 129" where no guest 129 exists (lab round 3 of
  v0.9.0, R3-1). The row now says "disks of N other VM ids" and marks an id that no guest of the cluster holds
  (`/cluster/nextid?vmid=`, which answers whatever the token may read) as "no VM has this id: a leftover volume".
  Not run on a lab.
- **The stored tenant path test ends with the platform it tested.** The result (`GET …/tenant-path-probe`) was one
  document per installation that nothing deleted, so after a Remove and a new install the fresh platform answered
  the old verdict, taken with other machines holding the addresses (lab round 3 of v0.9.0, R3-2). The Remove run
  now deletes it when it wipes the recorded status, and the start of an install deletes one that was saved after
  that. Not run on a lab.

### Known issues

Three known issues of `v0.9.0` are fixed here: the `boot-store` row that named a VM that does not exist, the
stored result of the tenant path test that outlived Remove, and the failed token delete at a cluster delete. The
`403` on `GET /cluster/status` is by design and now documented (`docs/proxmox-permissions.md`). The others
remain as the section of `v0.9.0` states them:

- **The root command stops when the platform's token ids are taken.** The ids `capmox@pve!capi` and
  `csi@pve!csi` are fixed. It affects a new install on a Proxmox cluster where one of them exists for something
  else ("The root command of an installation", `docs/managed-services.md`).
- **A chart version that changes only its hook does not run the hook.** No chart of this release is such a
  version ("What a release checks about its BOM pairs", `docs/ci-release.md`).
- **Workers do not join when the node that holds the platform's addresses also translates tenant traffic**, on a
  `simple` tenant zone. Read the Check row `tenant-path` and use one of the two layouts that work
  (`docs/managed-services.md`).
- **The platform upgrade has not run on a lab that has tenants.** It ran once on a production site with one
  tenant cluster and one database (above). Take the seed backup first.
- A health check replaces nothing when every worker of a group fails; Replace on a node group of one leaves the
  group without a node; a standalone volume can lose its name at create; the `pve-api-rule` row can read `ok`
  while the rule matches nothing behind a translating router; seed ISOs of servers created before `v0.8.0` are
  never swept.

## [0.9.0] - 2026-11-19

The release after `v0.8.2`. An installed managed-services platform can be upgraded in place, a new install is
easier to size and to place on a network and is usable by a tenant as soon as it is green, and tenant Kubernetes
workers can be repaired and put behind a firewall. It was prepared by two release candidates, `v0.9.0-rc.1` and
`v0.9.0-rc.2`, whose sections below record the lab rounds; everything they held is in this section. The entries
under Added, Changed and Fixed were written before or between those rounds: where one says "Not verified on a
lab", the two lists below are the current state.

The platform BOM of this release is `0.9.0`. It pins what `0.9.0-rc.1` and `0.9.0-rc.2` pin, cluster chart
0.1.14 and platform chart 0.1.3, both published from their tags (`proxmox-kamaji-cluster-v0.1.14`,
`proxcloud-platform-v0.1.3`): the three BOMs differ in the `release` line alone. A new install on this BOM has
both charts. A platform installed by 0.8.x stays on its BOM, with cluster chart 0.1.13 and platform chart 0.1.2,
until an operator upgrades it. The release carries the BOMs of `0.8.0`, `0.8.1`, `0.8.2`, `0.9.0-rc.1` and
`0.9.0-rc.2` for the platforms those builds installed.

Upgrading from `v0.8.2`. From an older release, read the sections in between as well: their steps still apply.

- **Portal first.** On the Docker Compose host check out the tag and run `./proxcloud-up.sh upgrade v0.9.0`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first.
- **Let one provider sync run** (every 15 minutes; without the scheduler `POST
  /api/v1/admin/job/external-service/sync`) before a planned Proxmox node reboot. It sets `onboot` on the
  Kubernetes node templates this portal imported and on the running workers of its tenant clusters, where it is
  missing. It does not cover a template the portal did not import or a stopped worker
  (`docs/managed-services.md`, "After a Proxmox node reboots").
- **An installed platform stays on its BOM** and keeps working. Moving it to `0.9.0` is a separate run the
  operator starts in step 4 of Managed services, and it can wait. Until then three control plane machines and a
  VLAN tag are refused on that platform and Replace answers 502.
- **The platform upgrade has run on a lab without tenants only**: once, from the BOM `0.8.0` to `0.9.0-rc.1`, on
  a platform with no tenant cluster and no database. It has not run on a platform with tenants and not to the
  BOM `0.9.0` itself. **Take the seed backup first** (`vzdump` of the seed VM and a dump of the portal database;
  `docs/managed-services.md`, "Credentials and the seed backup"), check that tenant clusters and databases are
  healthy, and change no machine size or count in the same step. A restore after a failed upgrade has not been
  run on a lab.
- **The plan from a 0.8.x BOM** lists six changes and refuses nothing: the release and `tenantCharts.kubernetes`
  as record only, and the four charts of the product (`pc-platform` 0.1.13 to 0.1.14; `capi`, `capi-tenant` and
  `platform` 0.1.2 to 0.1.3). No machine is replaced (`TestUpgradeRollsNoMachine` compares all five previous
  BOMs with this one and passes). From `0.9.0-rc.1` or `0.9.0-rc.2` the plan lists one change, the release,
  record only; not run on a lab.
- **Tenant clusters made before the upgrade** stay on cluster chart 0.1.13 and get no health check until their
  own chart bump.
- **Kubernetes provider settings:** `nodeStartupTimeoutSeconds` must be `0` or 900 to 86400; a stored value
  outside that is read as the default, and the provider's page says so.
- **Scripts:** a security group name that starts with `k8s-` is refused at create (400); the installed BOM as
  `targetBom` answers `409 bom_installed` (it was `400 invalid_confirmation`); a preset other than `custom` on
  an installed platform answers `409 fixed_fields`; `spec.sandbox` is refused.
- **A platform installed from a `dev-<sha>` build of `main`** is still refused (`bom_unsupported`).
- The app chart is `0.9.0` with appVersion `v0.9.0`, and `pc_versions.proxcloud` and `DEFAULT_VERSION` of
  `proxcloud-up.sh` name `v0.9.0`.


Verified on a lab. Three rounds on 2026-11-19, on one lab Proxmox VE cluster. Nothing was run on the published
images of `v0.9.0` itself. The code of `v0.9.0-rc.2` is the code of this release without one fix to the sandbox
template build.

Round 3, on the published images of `v0.9.0-rc.2`:

- **Fresh install in bridge mode.** One run of 40 minutes 5 seconds, no Retry and no failed stage. The platform
  was on the BOM `0.9.0-rc.2` with charts `0.1.14` and `0.1.3`, all five VMs had `onboot`, the plan showed no
  change and the Check passed with 25 rows.
- **Remove.** 54.2 seconds, no retry, and nothing left on Proxmox. The Proxmox API firewall rule was written by
  the install and deleted by Remove.
- **`409 bom_installed`.** Three requests that named the installed BOM as the target got this answer.
- **Control plane growth.** The plan listed 1 to 3 as a change and blocked it with the node's numbers, and the
  Check row `capacity` failed. After the count was put back the plan was empty.
- **`onboot` on old templates and workers.** The first provider sync wrote it on six node templates of this
  portal, withdrawn imports among them, and touched no other template. A second sync wrote nothing. A worker
  cloned from such a template had `onboot`, and the sync wrote nothing on workers that had it.
- **A tenant cluster of two workers.** Both nodes were Ready on chart `0.1.14`. The node group of two had its
  health check object, with a guard of 1 and a timeout of 1800 seconds.
- **LoadBalancer.** A Service of type LoadBalancer answered from both workers.
- **Database.** A managed database was ready and was reached from a tenant pod, with the tenant network as the
  allowed source, in a VLAN zone.
- **Tenant path test `passed`.** On a `simple` zone, from a node that does not hold the platform's addresses.
- **Deletes.** The cluster, the database and the network were deleted. The cluster's Proxmox token was released
  by the job and the quota went back to 0.

Round 2, on the published images of `v0.9.0-rc.1`, over a platform that `v0.8.0` installed:

- **The upgrade from `0.8.0` to `0.9.0-rc.1`, on a lab without tenants.** One run, no retry, 72.8 seconds. The
  plan showed six changes and nothing blocking. The lab had no tenant cluster and no database.
- **Nothing rolled.** The same four machines, nodes and boot ids, the same QEMU processes, and no Proxmox task.
  No pod restarted: 47 on the platform cluster, 16 on the seed. The platform API answered at each of 238 looks,
  one every 2 seconds.
- **What ArgoCD did.** It synced one Application of fourteen, `platform`. The three other chart moves were
  compared and found in sync, and no clusterctl hook ran. A second apply took 5 seconds and changed nothing.
- **Refusals.** Seven requests with a wrong confirmation or a wrong target were refused, and none started a run.
- **Chart `0.1.14` on a new tenant cluster.** A worker was Ready 9 minutes after the create. A node group of one
  gets no health check, by design; a group of two got one, with its guard.
- **Replace.** On the last Ready worker it asked for a confirmation, then made a new machine whose node was
  Ready 7 minutes 21 seconds later. The group had no node for about 6.7 minutes.
- **The health check, all workers failed.** On a cluster whose two workers could not join, it replaced nothing,
  as its guard says.
- **Worker firewall, one worker.** From a second tenant machine, the kubelet, SSH and port 4240 were closed
  afterwards, and NodePort and LoadBalancer stayed open. The node stayed Ready. Two of about 6000 one-second
  requests failed in 52 minutes, none at the change.
- **Provider settings check and join error.** The refusals and their texts, and `kubeadm`'s own error in the
  join log, with no token in it.
- **Tenant path test `failed`.** A full run on a `simple` zone gave the predicted verdict for the node that
  holds the platform's addresses, and cleaned up.

Round 1, on a build of `main`, over the same platform:

- **Volumes.** A volume keeps its row through attach, detach and rebuild, and the quota is right.
- **Worker firewall, one worker.** The Proxmox rules were written in the right order, the node stayed Ready, and
  Remove and the cluster delete cleaned up.
- **Replace.** Its refusals on a `0.8.0` platform, and "needs attention" after its time limit.
- **One root command.** The Check rows, the firewall rule and the refusals. The command was not run on a node.
- **Presets, network modes.** Only the refusals, the `capacity` row and the cleanup.
- **Usable after install, invitations, rule edit.** At the API only, and in part.

Not verified on a lab:

- **This release itself.** Not run: its published images, its tag's workflows, and the upgrade of a platform to
  the BOM `0.9.0`, from a `0.8.x` BOM or from a candidate's. The step from a candidate's BOM was planned as one
  record-only change and never run.
- **The upgrade with tenants.** No upgrade has run on a platform that has a tenant cluster or a database. Not
  seen: the wait for tenants in stage 9, a tenant that is unhealthy (`tenants_unhealthy`), the `platform`
  chart's sync beside live tenant control planes, and an existing cluster staying on chart `0.1.13`.
- **A failed upgrade or install.** Not run: a failed stage and Retry, a restore, and the refusal of a chart that
  would replace a machine (`unexpected_rollout`). The seed's backup was not taken before the lab upgrade.
- **The upgrade from other starts.** Not run: the portal's own step from `v0.8.2` (the lab's portal came from a
  build of `main` and the candidates), and a platform on the BOM of `0.8.1` or `0.8.2`.
- **The one root command on a fresh token set.** Not run: the command to its end and the `PC-ENROLL` line. On
  the lab the two token ids already existed, and the command stopped as designed. The lab install stored
  existing token secrets through the credentials step.
- **The Homelab preset as an install.** Only its save was seen. The lab's nodes have no room for it.
- **`routed` mode and a VLAN tag.** Not run: an install in `routed` mode, its Remove, and a VLAN tag on the
  platform network.
- **A replacement by the health check.** No worker was broken. The lab showed only the guard refusing when both
  workers of a group were unhealthy. Also not seen: Replace of a machine that has no node.
- **`onboot` beyond what is listed.** Not run: the sync on a running worker that has no `onboot`, a node
  template freshly imported by this release and its clone, and a Proxmox node reboot.
- **The tenant path test, both verdicts on one install.** `failed` and `passed` were seen on two different
  installs.
- **Three control plane machines.** Not built: on a new install or by an apply.
- **Worker firewall.** Not run: two workers, a new or replaced worker, and `workerFirewall: true` on the
  provider.
- **The consoles.** Every round ran at the API. No page of either console was opened, step 4 of Managed services
  included.
- **Earlier releases.** Everything `v0.8.0`, `v0.8.1` and `v0.8.2` say is not verified still holds, except what
  is listed as verified here.

### Added

- **Managed services: an installed platform can be upgraded to the platform release of the running ProxCloud**,
  when the two releases differ in ProxCloud's own charts alone. Step 4 of Managed services shows the plan and
  asks for a typed confirmation (`POST …/runs` with `targetBom` and `confirm`). A release that changes the
  Kubernetes version, an image, k3s, a Cluster API provider or a third-party chart is refused with a reason per
  entry (`upgrade_refused`), and so is a `-dev` target. `GET …/plan` answers `upgrade`: every difference of the
  two BOMs with its class, stage, wave and effect; and `replacement`, the move to this release in place of an
  unfinished upgrade toward an earlier one. New failure codes `upgrade_refused`, `unexpected_rollout` and
  `tenants_unhealthy`. `PROXCLOUD_PLATFORM_SKIP_ROLLOUT_GATE` (`PLATFORM_SKIP_ROLLOUT_GATE` in Compose) is off
  by default. Not verified on a lab: the tests run on fakes, and the lab tests owed are in
  [docs/managed-services.md](docs/managed-services.md).
- **Managed Kubernetes: a cluster's workers can be put behind the Proxmox VM firewall.** Inbound traffic to a
  worker is dropped except what the cluster's security group `k8s-<clusterId>` allows (pod network, health
  checks, kubelet and MetalLB between the cluster's workers; LoadBalancer addresses; NodePorts from the
  project's networks), and a worker can only send from the cluster's own addresses. Off for existing clusters:
  the operator protects one cluster at a time (Managed clusters, Firewall;
  `/admin/service/{id}/k8s-clusters/{clusterId}/worker-firewall`), and the action takes the firewall off again
  when a node stops reporting. New clusters get it with `config.cluster.workerFirewall: true` on the Kubernetes
  provider, off by default. A new or replaced worker is covered at the next provider sync, up to 15 minutes
  later. Needs no chart. Not verified on a lab: nothing was run against Proxmox or a cluster
  ([docs/managed-k8s.md](docs/managed-k8s.md), "Worker firewall").
- **Managed Kubernetes: Replace for one worker node**, in the customer console (cluster page: next to a node
  group's status for each machine that needs attention, and on the instance list; action `REPLACE_MACHINE`) and
  the operator console (Machines dialog;
  `POST /admin/service/{id}/k8s-clusters/{clusterId}/machines/{machine}/replace`). The node count, placement and
  quota do not change; the last Ready node of a group asks for a confirmation. On a managed-services platform it
  needs chart `proxcloud-platform` 0.1.3, which this BOM pins: a platform on a 0.8.x BOM answers 502 until it
  is upgraded to this release's BOM. Not verified on a lab.
- **Managed Kubernetes: a worker that registers no node is replaced by Cluster API** after
  `config.cluster.nodeStartupTimeoutSeconds` (1800 by default, `0` = off), in node groups of two or more
  machines, while at most `config.cluster.repairMaxUnhealthy` (1) machines of the group are not healthy. Needs
  cluster chart 0.1.14, which this BOM pins for new tenant clusters: a platform installed from it registers its
  Kubernetes provider with 0.1.14. A provider that names an older chart version gives no health check until the
  operator sets 0.1.14 there, and an existing cluster gets it with its bump. The
  provider's two keys apply to every cluster on that chart: they are written at each change of a cluster, and
  **Apply settings** on the provider's page writes them to one cluster at once. Not verified on a lab.
- **Operator console: the Machines dialog shows the join error a worker reports** (cloud-init result and log
  tail, credentials masked, nothing stored) and each node group's automatic repair state. Not verified on a lab.
- **A Kubernetes node image imported by Template updates starts its clones with their Proxmox node** (`onboot`
  on the template). Not verified on a lab.
- **Managed services: the platform network has a mode, and a VLAN tag.** `spec.network.mode` is `bridge` (an
  existing bridge, the default; an installation saved before the field means `bridge`) or `routed`: on a site
  with one Proxmox node ProxCloud makes the network at the install, as an SDN vnet whose gateway the node holds,
  with SNAT, so that tenant traffic to the platform never leaves the node. Everything outside the node then
  needs a route to the platform network through the node's address. With mode `bridge`, `spec.network.vlan`
  (1 to 4094) tags every machine's NIC, so a VLAN needs no bridge or vnet of its own; it needs a platform BOM
  whose cluster chart is 0.1.14 or newer, which this BOM is, and is refused on an installation whose BOM names
  an older chart. The
  network stays fixed after the first install. New Check row `bridge`; new failure code
  `platform_network_refused`. Not verified on a lab: every test uses a fake Proxmox API.
- **Managed services: "Test the tenant path".** A button in step 3 of an installed platform
  (`POST …/installations/{id}/tenant-path-probe`) that opens a TCP connection from a throwaway VM on a throwaway
  tenant network to what listens in the load balancer range, on the node you choose, and removes both again. It
  costs one VM clone and two SDN applies and never runs with Check. It needs a tenant cluster or database to
  connect to and, published on that node, an Ubuntu or Debian image that Template updates imported for the
  provider (see Fixed). Not verified on a lab.
- **Cluster chart `proxmox-kamaji-cluster` 0.1.14:** a `MachineHealthCheck` per node group of two or more
  machines (`machineHealthCheck.nodeStartupTimeoutSeconds`, `0` by default: none is rendered) and
  `proxmox.network.vlan`. Without the values the render is what 0.1.13 gave. Published (tag
  `proxmox-kamaji-cluster-v0.1.14`) and pinned by the platform BOM. Not
  verified on a lab: the chart tests render with Helm only.

- **Managed services: one root command per installation.** Step 2 shows a command for the installation
  (`GET /api/v1/admin/setup/proxmox-script?installation=<id>`) that prints no secret and ends with one
  `PC-ENROLL:` line carrying the secrets of the tokens it made; paste that line into the page
  (`PUT …/credentials` with `enroll`; the two by-hand fields stay under a fold). The command stops, before it
  changes anything, on a `capmox@pve!capi` or `csi@pve!csi` that was not made for the installation and whose
  secret ProxCloud does not hold, and prints the command that removes it; it never removes a token itself. A
  stored secret counts only while it still opens the token. A replayed or older line is refused
  (`409 enrollment_replayed`), and so is a line from a node whose clock is more than five minutes ahead
  (`400 enrollment_clock_ahead`). Not verified on a lab: the command was not run on a Proxmox node.
- **Managed services: the install writes the cluster firewall rule that lets the seed and the platform machines
  reach the Proxmox API** (comment `proxcloud:pve-api:<installation id>`), at every apply too, and Remove
  deletes it. Not verified on a lab.
- **Managed services: Check rows `boot-store` and `pve-api-rule`.** Not verified on a lab.
- **Managed services: a green install is usable by a tenant.** Up to v0.8.2 a platform that read ready refused
  every tenant cluster create, because the install registered the Kubernetes provider with no version and
  attached neither provider to a project. Not verified on a lab.
  - Stage 10 of every apply offers the BOM's Kubernetes version on the Kubernetes provider once a node image of
    it is published on every node the platform cluster uses. A version the provider already names is never
    overwritten or removed, and a version is offered once: one you delete from the provider afterwards is not
    added again by an apply. On a site that stays on a 0.8.x BOM, the first apply after the upgrade adds the
    BOM's version when its image is published on the platform's nodes and the provider does not name it, also
    when that image was published only to test it.
    `POST /api/v1/admin/platform/installations/{id}/kubernetes-version` does the same on demand
    (`409 node_image_unpublished` names the nodes).
  - The install still does not publish the node image it imports: a copy is published only after its smoke test
    passed. The ready card lists each node's copy and has one button, **Test, publish and offer Kubernetes
    <version>**, that runs "Publish on all nodes" for the image and then offers the version. It needs the
    provider's smoke-test network (`config.templateSmoke`).
  - New setting `spec.tenants.attach` (`all`, `new`, `none`; step 1 of the page): who gets managed Kubernetes
    and databases. Preselected `new` on a site that has projects and `all` on a site with none. A run applies a
    changed choice once, with the attach policy and a bulk attach of every project, both audited; saving `all`
    needs `admin:project:update` besides `admin:service:manage`. An installation saved before v0.9.0 has no
    setting, which means `none`: nothing changes for it.
  - The Check (row `scheduler`, a warning) and the ready card say what tenants miss while the job scheduler is
    off (project delete, refresh of resource state, charges) with the command that turns it on. Nothing turns it
    on.
- **Customer console: a first Kubernetes cluster without leaving the form.** The form selects the project's
  network when exactly one can hold the nodes, offers **Create a network for this cluster** when the project has
  none, and marks networks listed with DHCP; the API refuses a network in a zone with DHCP. Not verified on a
  lab.
- **Managed services: presets.** Step 1 starts from **Homelab** (one node or a small site: one control plane
  machine, one small machine per group), **Standard** (three control plane machines on one node where the
  platform's BOM builds three, else one; two machines per group) or **Custom**. A preset fills the form of a new
  installation and every field stays editable; it is switched off on an installed platform. The API takes
  `preset` on a create or update and stores the filled spec, not the preset. Not verified on a lab.
- **Managed services: one or three control plane machines** for the platform cluster
  (`spec.clusters[].controlPlane.replicas`), all on one Proxmox node. Three keep the platform's API up when one
  control plane VM is lost or replaced; they do not protect against the loss of the node or its disk. From 1 to
  3 is an apply on an installed platform; once a run has built three, 3 to 1 is refused. Three need cluster
  chart 0.1.14 in the platform's BOM, which this BOM pins: a new install can choose 3. The save refuses 3 on an
  installation whose BOM names an older chart, so on a platform installed by 0.8.x until it is upgraded to this
  release's BOM.
  Not verified on a lab.
- **Managed services: the Check row `capacity`.** It compares, per node, the memory of the machines and of the
  spare machines a rolling update makes with what can be reserved on the node, counted as the machines are
  counted when they are cloned. Before the first install it fails without room for the machines and the control
  plane's spare, and warns without room for the groups' spares; on an installed platform it warns. Not verified
  on a lab.
- **Cluster chart `proxmox-kamaji-cluster` 0.1.14:** `controlPlane.kubeadm.replicas` (1 or 3) and
  `controlPlane.kubeadm.vipLease`, in the same 0.1.14 as the health check and the VLAN tag. Without
  the values the control plane renders as 0.1.13 gave it. Published and pinned by the platform BOM. Not verified
  on a lab.
- **Customer console, Members: a list of pending project invitations with Revoke.** Each pending invitation has
  Copy link. On a site that sends no email the page says so and the invite dialog shows the link instead of
  "sent"; the invitee can also sign in with the invited address and find the invitation on the start page. API:
  `GET /project-invites/project/{projectId}`, `DELETE /project-invites/project/{projectId}/{inviteId}`;
  `POST /project-invites/invite` answers with the invitation. Not verified on a lab.
- **Security group rules can be edited** (`UPDATE_RULE`: the changed rule is added, then the old one removed). A
  rule made in Proxmox with settings the form does not have is not edited (409). The rule form has a protocol
  list and a description, and states that rules are ingress only. Not verified on a lab.
- **Customer console, servers: Hard reboot under More actions.** A "How to reach this server" card, and a notice
  on the create form, when the project has no public network. Not verified on a lab.
- **Customer console: the load balancers page lists what the operator must enable** (public network, public
  address quota, appliance image) instead of failing at create (`LIST_LOAD_BALANCER_WARNINGS`). The file shares
  page shows the reach warning on the page. Not verified on a lab.

### Changed

- **CI: a pull request runs only the jobs its changed files can affect.** `test.yml`, `docker.yml` and
  `security-scan.yml` start with a job `changes` (`.github/scripts/ci_changes.py`) that maps the changed files to
  jobs: a documentation-only pull request runs the naming gate and nothing else, a Go change skips the consoles, a
  console change skips the Go tests and images, and the fifteen-minute scan of the eleven sandbox templates runs
  only for `deploy/templates/**`, `cmd/mcp-gateway/**`, `go.mod`/`go.sum` and `.github/**`. A file in no known
  area (under `docs/`, anything that is not Markdown), a change under `.github/`, and every push to `main`, tag,
  schedule or manual run still run everything; a new path in no area also fails one test of the `installer` job
  until it is given a row or listed. The required check `security-gate` reports a skipped scan as not applicable
  only when the change detection succeeded and said, in a readable output, that it did not ask for it; a failed
  or cancelled scan, a failed detection, or a job without such an output blocks. Because pull requests no longer
  rescan everything, `security-scan.yml` now also runs in full every day on `main`, a full scan of `main` is no
  longer cancelled by the next merge, and one that fails opens or updates an issue. It still blocks no pull
  request and no release tag.
  [docs/ci-release.md](docs/ci-release.md), "What runs when".
- **The platform BOM pins cluster chart `proxmox-kamaji-cluster` 0.1.14 and platform chart `proxcloud-platform`
  0.1.3**, both published from their tags, by the digest ghcr.io serves: 0.1.14 for the platform cluster
  (`pc-platform`) and for `tenantCharts.kubernetes`, 0.1.3 for `capi`, `capi-tenant` and `platform`.
  - A new install on this BOM can have three control plane machines and a VLAN tag, registers its Kubernetes
    provider with chart 0.1.14 (the health check for new clusters) and gives the provider's service account the
    `delete` on Machines that Replace needs.
  - Each of the four components lists the version it replaces in `upgradeFrom` (0.1.13, 0.1.2), so the BOMs of
    0.8.0, 0.8.1 and 0.8.2 may move to this one. For `pc-platform` the release check
    (`TestUpgradeRollsNoMachine`) renders chart 0.1.13 with the values each of those releases rendered and chart
    0.1.14 with this build's values, and finds the same control plane, machine templates and bootstrap templates:
    the move replaces no machine. Platform chart 0.1.3 differs from 0.1.2 in the provider's ClusterRole alone
    (the `delete` on Machines, the read of MachineHealthChecks).
  - A platform installed by 0.8.x is not changed by the upgrade of ProxCloud: it stays on its BOM, where the
    save refuses 3 and a VLAN tag and Replace answers 502, until an operator starts the upgrade in step 4 of
    Managed services. The plan then lists the four charts as `chart` and the release and
    `tenantCharts.kubernetes` as `record only`, and refuses nothing once this BOM is a release's; a build of
    `main` (`0.9.0-dev`) stays refused as a target. The upgrade was not run on a lab.
- **Security groups: a name starting with `k8s-` is reserved for Kubernetes clusters.** Creating one answers
  400, attaching one to a server 409, and deleting the group of a cluster the project has 409. A group with such
  a name from an earlier version stays the tenant's unless the rest of its name is the id of one of the
  project's clusters, and can no longer be attached to another server. A security group can no longer be
  attached to a worker node of a Kubernetes cluster, and the cluster's own group cannot be detached from it
  (409). Not verified on a lab.
- **Managed services: a Retry after a failed upgrade continues to the upgrade's target.** `status.bom` names the
  BOM the platform left until a run has succeeded; `status.targetBom` names the target, and
  `status.tenantsDownBefore` the tenants that were unhealthy when the upgrade reached stage 9. `POST …/runs`
  refuses `confirm` on an apply unless it starts an upgrade; the `invalid_confirmation` and `bom_unsupported`
  messages changed. Not verified on a lab.
- **Managed Kubernetes: "Needs attention" of a node group is measured per machine**: a healthy or starting
  worker no longer clears it for machines that are stuck. `node_groups[].progress` carries `machineNames`: at
  most five machines of the group that need attention. The tenant's text for a worker that did not join points
  to Replace instead of lowering and raising the count. Not verified on a lab.
- **The Kubernetes provider's service account may delete Machines** (cluster-wide; the portal deletes worker
  Machines of its own clusters only) **and read MachineHealthChecks** (`deploy/mgmt-cluster/rbac.yaml`, chart
  `proxcloud-platform` 0.1.3, published and pinned by the platform BOM). Not verified on a lab.
- **Managed services: the Check row `tenant-path` is `ok` for a platform network of mode `routed`**, and
  `privileges:capi` reads `SDN.Use` on the path of the machines' NIC for a VLAN tag or a routed network. The
  guide gives the node leg for an existing VLAN site in a persistent form, as commands, marked as not
  lab-verified ([docs/managed-services.md](docs/managed-services.md)). Not verified on a lab.
- **The platform BOM on `main` is `0.9.0-dev` again, and the build carries the BOM of `0.8.2`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.8.2.yaml`). The `v0.8.2` release commit left
  the BOM at `0.8.2`, and the release gate refuses only `-dev`. A platform installed by `v0.8.2` stays on its BOM
  after an upgrade, as one installed by `v0.8.0` or `v0.8.1` does; a platform installed by a `dev-<sha>` build of
  `main` is still refused by a later build (`bom_unsupported`).
- **The platform BOM on `main` is `0.9.0-dev` again after the candidate, and the build carries the BOM of
  `0.9.0-rc.1`** (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.9.0-rc.1.yaml`). The commit of
  `v0.9.0-rc.1` left the BOM at `0.9.0-rc.1`, and the release gate refuses only `-dev`. A platform that is on the
  candidate's BOM, installed by `v0.9.0-rc.1` or moved to it by an upgrade, stays on that BOM under a later build
  and is not refused (`bom_unsupported`) while the file is carried. It is the first previous BOM with cluster
  chart 0.1.14 and platform chart 0.1.3: `TestUpgradeRollsNoMachine` now compares four previous BOMs. No apply
  on such a platform was run on a lab.
- **The platform BOM on `main` is `0.9.0-dev` again after the second candidate, and the build carries the BOM of
  `0.9.0-rc.2`** (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.9.0-rc.2.yaml`). The commit of
  `v0.9.0-rc.2` left the BOM at `0.9.0-rc.2`, and the release gate refuses only `-dev`. A platform that is on that
  BOM, installed by `v0.9.0-rc.2` or moved to it by an upgrade, stays on it under a later build and is not refused
  (`bom_unsupported`) while the file is carried. Its pins are those of `0.9.0-rc.1`, so
  `TestUpgradeRollsNoMachine` compares five previous BOMs with the BOM on `main`: three of 0.8.x and the two
  candidates. No apply on such a platform was run on a lab.
- **The platform BOM's release is `0.9.0`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.9.0-rc.2`: the two BOMs differ in the `release` line alone, and neither chart of the product changed since
  its tag. The release gate passes on it and on the five previous BOMs. The app chart, `pc_versions.proxcloud`
  and `DEFAULT_VERSION` of `proxcloud-up.sh` move to `0.9.0` / `v0.9.0`. `deploy/platform/previous/0.9.0.yaml`
  is added by the change after the tag.
- **The body of a GitHub Release is `docs/releases/vX.Y.Z.md`, written for a reader, when the tag has that file.**
  The Release pages of `v0.8.0` to `v0.8.2` showed their changelog section, whose wrapped lines a Release body
  renders as line breaks and which has no headings. The release job (`docker.yml`) now takes the file, refuses an
  empty one or one over 120000 characters, and falls back to the changelog section, with a warning, when there is
  none; `--generate-notes` is unchanged. The file has the sections Highlights, Upgrade, What changed, Verified on a lab,
  Not verified and Known issues, one paragraph or list item per line, and links at the release's tag; the rules and
  the command that renders it are in [docs/ci-release.md](docs/ci-release.md#release-notes). This changelog stays
  the complete record. `docs/releases/` has the files of `v0.8.0`, `v0.8.1` and `v0.8.2`; their published Release
  pages are not changed by this. A script test (`test_release_notes.py`, in `test.yml`) checks every file, and fails
  when a changelog section from `0.8.0` on has no file or a link is relative. Not verified: the release job has not run on a tag with such a file; its step was run by hand on a copy of the tree.

- **Managed services: saving the settings adds the seed's address, the API address and the node range to the
  provider's blocked networks** and says what it added; the next provider sync writes the firewall rules. A
  later save takes off the entries an earlier save added and the settings no longer need (a corrected address or
  range), never an entry you wrote; a Remove takes none off. Not verified on a lab.
- **Managed services: saving the settings sets `"dhcp": false` on the provider's default tenant zone** when it
  is a `simple` zone whose config says nothing about DHCP and Proxmox does not have it yet, so the zone is
  created without a DHCP server, which tenant Kubernetes clusters need. A zone that exists is never changed; a
  config that says `"dhcp": true` stays. Not verified on a lab.
- **Managed services: the `credentials`, `isolation`, `tenant-network` and `flags` checks name the one action
  that fixes them.** Not verified on a lab.
- **Managed services: the review step** shows each node's free memory next to what the settings place on it. Not
  verified on a lab.
- **Managed services: "Use the lab profile" is replaced by the Homelab preset.** Not verified on a lab.
- **Managed services: the API refuses `spec.sandbox`** (the place for the sandbox service's placement, built in
  a later release) and `controlPlane.nodes` other than the control plane's one node. Not verified on a lab.
- **Customer console: Create network says it is waiting for the network to be applied on the nodes.** Not
  verified on a lab.
- **Customer console: the create server form explains which security group applies when none is ticked.** Not
  verified on a lab.

### Fixed

- **A sandbox template build no longer fails on a package mirror that does not answer.** The `browser` template
  is built on Microsoft's Playwright image, which keeps its apt sources on `azure.archive.ubuntu.com`. That name
  resolved to one address, the address stopped answering, and apt does not connect again to an address that timed
  out, so its three retries ended within seven seconds: `apt-get upgrade` failed, the template builder stopped at
  `browser`, and the required `template-images` job of `security-scan` failed on `main` on a network fault. Two
  changes. The `browser` image now takes the same Ubuntu archive from `archive.ubuntu.com`, which has many
  addresses that apt tries in turn; the suites, the packages and `apt-get upgrade` are the same.
  `deploy/templates/build.sh` now tries a failed image build again, twice at most, 60 seconds apart
  (`RETRY_PAUSE`), for every template, in the scan and in the publishing workflow; a build that fails three times
  ends the run with the same status and the same `FAILED: ...; NOT BUILT: ...` line as before. Covered by the
  builder tests; the `browser` image itself was not rebuilt on a workstation, the pull request's own
  `template-images` run builds it.

- **Managed Kubernetes: tenant workers of a site upgraded from 0.8.x start with their Proxmox node.** `onboot` was
  written on a Kubernetes node image only at its import, so a node template imported before v0.9.0 had none, and
  neither had the workers cloned from it, before or after the upgrade: after a node reboot they stayed stopped (lab
  round 2 of v0.9.0-rc.1, finding G2F-1). The provider sync now sets `onboot` on the Kubernetes node templates that
  a Template updates import of this portal holds and on the running worker VMs of this portal's tenant clusters,
  where it is missing, and writes nothing else. No new Proxmox privilege (`VM.Config.Options`, which the portal's
  role holds on `pc-templates` and the tenant pools). **Upgrade note**: the first provider sync after the upgrade
  (every 15 minutes, or `POST /api/v1/admin/job/external-service/sync`) writes it; wait for one before a planned
  node reboot. Not covered: a node template built by `images/build.py` or not imported by this portal
  (`qm set <vmid> --onboot 1` on the node), the workers of a `sharedWorkers` provider, and a worker that is
  stopped. See
  [docs/managed-services.md](docs/managed-services.md#after-a-proxmox-node-reboots). Not verified on a lab.

- **Managed services, four small findings of the second lab round of 0.9.0-rc.1.** The upgrade plan's `impact`
  texts and the guide no longer say that ArgoCD syncs every chart that moves and runs the clusterctl hook again:
  the Application moves to the new chart version, and ArgoCD syncs objects, and runs a hook, only where the new
  chart renders something differently (on the lab one Application of four was synced and no hook ran). A stage
  that applies waves shows `step 3 of 4, wave 2` where it showed `wave 3 of 4`: the wave is now the BOM's number,
  the one the plan and the guide name. An apply whose `targetBom` is the BOM the platform is already on answers
  `409 bom_installed` in place of `400 invalid_confirmation` with a text about `confirm`. The join log of a
  worker that did not join no longer ends with SSH host key art: the filter looked for it at the start of a
  line, and every line of a real log begins with a time stamp, so it never matched (cloud-init's `ci-info`
  tables are dropped the same way). Not verified on a lab.

- **Managed services: a larger control plane that does not fit is no longer accepted in silence.** On an
  installed platform, 3 control plane machines saved over 1 on a node without room for them left the Check ok
  with a warning and a plan with no change and nothing blocking (lab round 2 of v0.9.0-rc.1). The plan now lists
  the count as a change (`controlPlane.replicas`, 1 to 3), the Check row `capacity` fails for the machines that
  are not built yet, with the node's numbers, `GET …/plan` carries that message in `blocking` (step 4 switches
  **Apply this release** off), and an apply stops at its first stage. What a run already placed still only
  warns: a healthy platform on a full node keeps passing the Check. Not verified on a lab.

- **The two consoles' images run on NGINX 1.30.7.** The base `nginx:1.30.6-alpine` carried five fixable HIGH findings
  in the nginx package (CVE-2026-113425, CVE-2026-121810, CVE-2026-123007, CVE-2026-123633, CVE-2026-123634; fixed in
  `1.30.7-r0`), which failed the image scans of `main` and of every pull request that scans a console. `web/Dockerfile`
  now builds on `nginx:1.30.7-alpine`. A patch release of the same stable line: no configuration change. Not verified on
  a lab.

A lab round of 2026-11-19 ran this build over a platform that v0.8.0 installed. The five entries that follow fix
what it found in managed services. None of the fixes was run on a lab: each is covered by tests against a fake
Proxmox API.

- **Managed services: the API no longer takes a preset on an installed platform.** A `PUT` of an installation
  with `preset: standard` and a spec without `controlPlane` and `nodeGroups` answered `200` and rewrote the
  stored machines: the control plane's node, and every group's size and count. The next **Apply this release**
  would have replaced every machine. The page already switched the presets off there; the API did not look. Now
  a preset other than `custom` on an installed platform is a `409 fixed_fields`, and a spec that leaves out the
  clusters, a cluster's `controlPlane` or its `nodeGroups` keeps the stored ones: missing means keep, never a
  default. `controlPlane.node` is fixed after the first install as well (`409 fixed_fields`; the page disables
  the field): every control plane machine is on that node, so another node is each of them made anew, which no
  lab has run. A group's nodes stay changeable. `GET` of an installation saved before v0.9.0 now names
  `controlPlane.replicas` (1), as the API reference said it did.
- **Managed services: "Test the tenant path" clones only an image of this portal's own, and no longer waits for
  DHCP.** The test took the first published Ubuntu template in slug order and cloned it onto that template's
  own store. On a Proxmox cluster that several portals share that can be another party's template; on the lab it
  was an older template on an NFS store, the VM got no address from the zone's DHCP, and the test ended `error`
  without a verdict. The image is now resolved in the provider's catalog as a tenant's server create resolves
  one, from a template that Template updates imported for the provider or that `config.templates` names with its
  VM id (Ubuntu before Debian, the newest build); the clone goes to the store of the provider's default
  storage profile; and the VM gets a fixed address over cloud-init in every zone. An import counts only when it
  finished and the template at its node and VM id still has the import's name and source digest, because
  Proxmox reuses the VM id of a failed import or of a deleted template. A site whose images were all built
  outside Template updates gets `409 no_os_template` until it imports one (a `config.templates` pin works too,
  but it also changes the tenant catalog: see the guide). The `202` answer and the result
  name the image, its template, the node and the store (`image`, `template`, `node`, `storage`), and
  `409 no_os_template` names the published templates it did not take. Why the lab VM got no address was not
  determined (the VM is gone): the test no longer depends on it. New in the result: `notes`, what Proxmox may
  keep by itself, the DHCP mapping of the test VM's NIC in a zone with a DHCP server, with the files to clean.
- **Managed services: the Check row `boot-store` lists VMs of the pool `pc-templates` that are no template.** Every member of
  that pool was skipped as the product's own, so VMs an operator had in the pool were not named among the disks
  the CSI token reaches. Only the pool's templates are left out now.
- **Managed services: the Check row `tenant-path` of a VLAN tenant zone no longer points at a test that the zone
  refuses.** It ended with "Test the tenant path", which answers `409 vlan_zone` there; it now names the check
  from a tenant's server.
- **Managed services: the Check row `pve-api-rule` and the guide say what the rule matches.** The rule's sources
  are the addresses of the seed and the platform machines as the node sees them. On a site whose router
  translates the platform network on the way to the node the row read `ok` while the rule matched nothing; the
  row still reads the rule only, and now says that such a site needs a rule for the translated address
  ([guide](docs/managed-services.md#when-a-router-translates-the-platform-network)). Nothing else changed.

- **Managed Kubernetes: the provider save checks the worker repair and worker firewall settings.** A lab round on
  the build of main (2026-11-19, finding Q2) saved `nodeStartupTimeoutSeconds: 30`, `-5`,
  `repairMaxUnhealthy: "40%"` and `workerFirewall: "yes"` with 200. Read as sent, 30 s deletes every machine that
  is still cloning, in a loop, on cluster chart 0.1.14; `-5` turned the repair off; `"40%"` became 1; `"yes"`
  meant off. A create (the install's register stage included) and an update that carries the `cluster` block now
  answer 400 with the field and the reason: `config.cluster.nodeStartupTimeoutSeconds` is `0` or a whole number
  from 900 to 86400, `repairMaxUnhealthy` a whole number of 1 or more, `workerFirewall` a boolean. 900 s is the
  15 minutes the status sync gives a running machine to join before it asks for attention; the slowest worker of
  that lab was provisioned 5 min 23 s after its create. A value stored before the check is read as the default
  (1800 s, 1, off), and the provider's answer and page say so in `configNotes`; they also say when the two
  repair keys are set and the provider's chart is older than 0.1.14, where they have no effect. Not verified on
  a lab ([docs/managed-k8s.md](docs/managed-k8s.md), section 2).
- **Operator console: the join error of a worker shows kubeadm's error.** On the node image of the platform BOM
  cloud-init writes the SSH host keys after the failed join, so the last 40 lines of its log were key art and
  the cause was not on the screen (the same lab round, finding Q3). `joinError.logTail` is now an excerpt: from
  the start of the last `kubeadm join` to the end of the log, without the host key output and cloud-init's
  `ci-info` tables; when that is over 40 lines or 6000 bytes, the first line, the lines that name an error and
  the last eight, with a line for each gap. Credentials are masked as before. Tested on a log built to the
  shape the lab reported, not on a recorded one. Not verified on a lab.
- **The `mcp-gateway` sandbox template installs its npm MCP servers from a committed lockfile.** The image named
  the eleven servers at exact versions but had no lock, so every build took the newest release of each transitive
  dependency. In `security-scan` run 44327069354 the registry listed `@supabase/storage-js` 2.119.1 (a dependency
  of `desktop-commander`) and answered 404 for its tarball, which npm does not retry: the build failed.
  `deploy/templates/mcp-gateway/package.json` and `package-lock.json` are now in the repository and the image
  runs `npm ci`. The lock is the one inside the published image `20261116-c8b3364` (the digest in
  `images.yaml`), so the image gets the packages it had (`storage-js` 2.119.0): a build on a workstation installed the
  same 1726 packages at the same versions as the published image, and every package server answered the
  build's MCP check. npm retries network errors and 5xx answers twice by default; the build now asks for five.
  To change a server, edit `package.json` and regenerate the lock (the command is in the Dockerfile). Not run
  in CI or on a lab yet.
- **A failed sandbox template build says which template failed.** `deploy/templates/build.sh` stops at the first
  template that fails, and the `security-scan` report listed that one and every template after it together as
  `missing`. The script now ends with `build.sh: FAILED: <name> (exit <n>); NOT BUILT: <names>` in the build
  log, and `template-status.json` and the job log name `failed` and `not_built` apart (`missing` is unchanged).
  The exit status and the failing gate are the same as before.
- **The `desktop` sandbox template build no longer fails when Launchpad is briefly unavailable.** The image takes
  Firefox ESR from `ppa:mozillateam/ppa`, and one HTTP 503 from Launchpad (its API during `add-apt-repository`,
  the PPA index, or the package download) failed the build; in the `security-scan` workflow that stopped the
  template builder, so `desktop` and every template after it were reported missing and the security gate failed.
  The PPA step is now retried up to five times with a growing pause, and `apt-get update` fails on an index it
  could not fetch instead of leaving a later "no installation candidate". A build that succeeds installs the
  same packages as before.

### Known issues

- **The upgrade is not shown for a site with tenants.** See Upgrade and Not verified. It affects a production
  platform on a `0.8.x` BOM. Take the seed backup first, or leave the platform on its BOM, where it keeps
  working.
- **The root command stops when the platform's token ids are taken.** The platform's two Proxmox tokens have
  fixed ids, `capmox@pve!capi` and `csi@pve!csi`. On a Proxmox cluster where one of them already exists for
  something else, the command stops before it changes anything and prints the command that removes the token. It
  affects a new install on such a site. The only way forward it offers is to remove that token, which breaks
  whatever else uses it. See "The root command of an installation" (`docs/managed-services.md`).
- **Workers do not join when the node that holds the platform's addresses also translates tenant traffic.** On a
  `simple` tenant zone, tenant connections to the platform network are dropped on the Proxmox node that both
  translates tenant traffic and runs the platform's machines. The path test reads `failed` from that node and
  `passed` from another. The network of an installed platform is fixed, so `routed` mode helps a new one-node
  install only. Read the Check row `tenant-path`, run the test, and use one of the two layouts that work
  (`docs/managed-services.md`).
- **A chart version that changes only its hook does not run the hook.** ArgoCD syncs an Application only when a
  rendered object differs. No chart of this release is such a version. It affects a later release, which must
  also change a rendered object. See "What a release checks about its BOM pairs" (`docs/ci-release.md`).
- **The Check row `boot-store` can name a VM that does not exist.** The row counts the owners of volumes on the
  store, so a leftover volume is listed under the id of a VM that is gone. It affects the row's text only.
- **The stored result of the tenant path test outlives Remove.** After a Remove and a new install, the page
  shows the verdict of the last test of the old platform until a new test runs. Run the test again after an
  install.
- **Deleting a tenant cluster logs one failed request on Proxmox.** The portal asks Proxmox to delete a
  `csi@pve` token that was never made, and Proxmox answers `500`. The delete still succeeds and nothing is left
  behind. It is noise in the Proxmox log; the cause is not found yet.
- **A tenant cluster's token gets `403` on a few status reads.** Seen on `GET /cluster/status` and on a VM's
  current status, at clone start and at delete. The machines came up and no effect was seen.
- **A health check replaces nothing when every worker of a group fails.** That is its guard. The machines stay,
  show "not joined" with "needs attention", and are counted in the tenant's quota. It affects a tenant whose
  workers cannot reach the platform. Repair the cause, then use Replace or delete the cluster.
- **Replace on a node group of one leaves the group without a node.** The old machine goes first: on the lab the
  group had no node for about 6.7 minutes. It affects a tenant with one worker. Raise the group's count by one
  first when the workload cannot wait.
- **A standalone volume can lose its name at create.** A volume created without a server can answer `200` with
  no `id` when Proxmox is slow, and is then named after its disk at the next provider sync. It affects a tenant
  who creates such a volume. Nothing is lost but the name; not fixed.
- **The `pve-api-rule` row can read `ok` while the rule matches nothing.** This happens on a site whose router
  translates the platform network on the way to the Proxmox node. It affects installs on such a site. Add a rule
  for the translated address: see the guide (`docs/managed-services.md`).
- **Seed ISOs of servers created before `v0.8.0` are never swept.** When such a server is deleted, its seed ISO
  stays on the seed store and holds the server's user data. It affects a site with such servers. Clean it up by
  hand with the commands in the release notes of v0.8.2.

## [0.9.0-rc.2] - 2026-11-19

The second release candidate of 0.9.0, published as a prerelease and never as `latest`. It is not the release:
`v0.8.2` stays the current stable release, and the chart, the installer and the Compose default still name it. It
is `v0.9.0-rc.1` plus the three fixes for what the second lab round found, and it exists so that a lab can run a
fresh install, and the upgrade again, on a build that holds them. Its content is the section of 0.9.0 above;
nothing is repeated here. Not for a production site.

### Changed

- **The platform BOM's release is `0.9.0-rc.2`** (`deploy/platform/platform-bom.yaml`), with the pins of
  `0.9.0-rc.1` (cluster chart 0.1.14, platform chart 0.1.3): the two BOMs differ in the `release` line alone, and
  neither chart of the product changed since its tag. The release gate passes on it and on the previous BOMs
  0.8.0, 0.8.1, 0.8.2 and 0.9.0-rc.1. The plan from each of the three 0.8.x BOMs to it is allowed, with six
  changes and no refusal, as it was to `0.9.0-rc.1`; the plan from `0.9.0-rc.1` is allowed with one change, the
  release, record only. `TestUpgradeRollsNoMachine` compares all four pairs and passes.
- **A platform on the BOM `0.9.0-rc.1` is applied by this build as it is** (it carries
  `deploy/platform/previous/0.9.0-rc.1.yaml`) and can be moved to `0.9.0-rc.2` by one upgrade run. A platform
  moved to `0.9.0-rc.2` is accepted by later builds only while they carry `deploy/platform/previous/0.9.0-rc.2.yaml`,
  which the change after the tag adds: keep such a platform on a lab.
- The app chart (`0.8.2`), `pc_versions.proxcloud` and `DEFAULT_VERSION` of `proxcloud-up.sh` do not move. The
  tag's workflows publish the five images as `v0.9.0-rc.2` and the app chart as `0.9.0-rc.2` with that appVersion.
  Run the candidate with `./proxcloud-up.sh upgrade v0.9.0-rc.2`, or `--version v0.9.0-rc.2` on a new install.

### Fixed

The three entries are in the section of 0.9.0, under Fixed; each is covered by tests, and none had been run on
a lab when this candidate was cut.

- Tenant workers of a site upgraded from 0.8.x start with their Proxmox node: the provider sync sets `onboot` on
  the portal's Kubernetes node templates and on its running tenant workers where it is missing.
- A larger control plane that does not fit is listed in the plan and blocked (the Check row `capacity` fails for
  the machines not built yet), in place of a warning and a plan with no change.
- The upgrade plan's texts no longer say that ArgoCD syncs every chart that moves; a stage's progress reads
  `step N of M, wave W`; the installed BOM as `targetBom` answers `409 bom_installed`; the join log of a worker
  that did not join drops SSH host key art.

### Verified on a lab

The second lab round, on 2026-11-19, with the published images of `v0.9.0-rc.1` on one lab Proxmox VE cluster,
over a platform that `v0.8.0` installed. The lab had no tenant cluster and no database during the upgrade. It
ran before the three fixes above. Nothing was run on this candidate's own images.

- The upgrade between BOMs, 0.8.0 to 0.9.0-rc.1: one run, no retry, 72.8 seconds. The plan showed six changes,
  allowed, nothing blocking. Nothing rolled: the same four Machines, nodes and boot ids, the same QEMU
  processes, no Proxmox task, no pod restarted (47 on the platform cluster, 16 on the seed), and the platform
  API answered 200 at each of 238 looks, one every 2 seconds. ArgoCD synced one Application of fourteen
  (`platform`); the three other chart moves were compared and found in sync, and no clusterctl hook ran. A
  second apply took 5 seconds and changed nothing. The seven wrong confirmations and targets were refused.
- Chart 0.1.14 on a new tenant cluster: a worker Ready 9 minutes after the create; no health check for a node
  group of one, by design; one for a group of two, with its guard.
- Replace on the last Ready worker: `409 last_ready_node` without confirm, then a new machine with its node
  Ready 7 minutes 21 seconds after the confirm. The group had no node for about 6.7 minutes.
- The health check on a cluster whose two workers could not join: it replaced nothing, as its guard says for
  "all failed". A replacement by the health check was not seen.
- The worker firewall on one worker, with saved measurements: from a second tenant machine the kubelet, SSH and
  port 4240 were closed afterwards, NodePort and LoadBalancer stayed open, and the node stayed Ready. Two of
  about 6000 one-second requests failed in 52 minutes, none at the change.
- The provider settings check (the refusals and their texts) and the join error excerpt on real `kubeadm`
  output, with no token in it.
- The tenant path test on a `simple` zone: a full run with a verdict, `failed`, the predicted one for the node
  that holds the platform's addresses, and its cleanup. `passed` was not seen.
- Three control plane machines: the save was accepted on a node without room and the plan showed no change.
  That is the second fix above; nothing was applied.

### Not verified

- This candidate itself: its images, its three fixes, the plan and the upgrade from `0.9.0-rc.1` or from a 0.8.x
  BOM to `0.9.0-rc.2`.
- The upgrade with a tenant cluster or a database on the platform: the wait for tenants in stage 9,
  `tenants_unhealthy`, the `platform` chart's sync beside live tenant control planes, and an existing cluster
  staying on chart 0.1.13.
- A failed stage of an upgrade, Retry, a restore, and the refusing half of the rollout check
  (`unexpected_rollout`). The seed's `vzdump` was not taken before the lab upgrade.
- The portal's own step from `v0.8.2`: the lab's portal came from a build of `main`. An upgrade from the BOM of
  0.8.1 or 0.8.2. The console's step 4: the upgrade was run at the API.
- A replacement by the health check, Replace of a machine that has no node, and `onboot` on a freshly imported
  node template and its clone.
- The worker firewall with two workers, on a new or replaced worker, and with `workerFirewall: true` on the
  provider.
- The tenant path test's verdict `passed`, `routed` mode and a VLAN tag.
- A fresh install from a candidate, with a preset, the one root command run on a node, or three control plane
  machines; an apply of three control plane machines.
- Everything the section of 0.9.0 said then and the sections of 0.8.0 to 0.8.2 say is not verified, except what is
  listed as verified here.

## [0.9.0-rc.1] - 2026-11-19

A release candidate of 0.9.0, published as a prerelease and never as `latest`. It is not the release: `v0.8.2`
stays the current stable release, and the chart, the installer and the Compose default still name it. The
candidate exists for one reason. The platform upgrade between BOMs takes only a release BOM as its target,
never a `-dev` one, so a lab cannot drill the upgrade from 0.8.0 on a build of `main`. Its content is the
section of 0.9.0 above; nothing is repeated here.
Not for a production site.

### Changed

- **The platform BOM's release is `0.9.0-rc.1`** (`deploy/platform/platform-bom.yaml`), with the pins `main` had
  (cluster chart 0.1.14, platform chart 0.1.3). It is a release BOM: no `pending` digest and no `0.0.0+` version,
  and the release gate passes on it and on the previous BOMs 0.8.0, 0.8.1 and 0.8.2. The plan from each of
  those three to it is allowed, with six changes (the release and `tenantCharts.kubernetes` as record only, the
  four charts of the product) and no refusal, and `TestUpgradeRollsNoMachine` compares all three pairs and
  passes.
- **A platform moved to the candidate's BOM stays on it** until an operator upgrades it again. The builds after
  the candidate (`main`, a later candidate, 0.9.0) accept that platform only while they carry
  `deploy/platform/previous/0.9.0-rc.1.yaml`, which the change after the tag adds. From the candidate's BOM to
  that of 0.9.0 is one more upgrade run, allowed when the two differ in the release line and the product's own
  charts alone. A release that drops the file refuses the platform (`bom_unsupported`): keep such a platform on
  a lab.
- **`TestEveryReleaseHasItsPreviousBOM` counts a release candidate** (`## [X.Y.Z-rc.N]`), so the build after
  this one fails its tests without the candidate's BOM.
- The app chart (`0.8.2`), `pc_versions.proxcloud` and `DEFAULT_VERSION` of `proxcloud-up.sh` do not move: the
  release contract test keeps them on a stable release. The tag's workflows publish the five images as
  `v0.9.0-rc.1` and the app chart as `0.9.0-rc.1` with that appVersion. Run the candidate with
  `./proxcloud-up.sh upgrade v0.9.0-rc.1`, or `--version v0.9.0-rc.1` on a new install.

### Verified on a lab

One lab round, on 2026-11-19, on the build `dev-68a15e8` of `main` over a platform that v0.8.0 installed. It ran
before the five managed-services fixes, the provider settings check and the join error excerpt listed under
Fixed above: none of those was run on a lab.

- Volume rows through attach, detach and rebuild, and the quota: verified.
- The upgrade between BOMs: not verified. Only the refusal of a `-dev` target and the plan's block were seen.
  The candidate is for this.
- The worker firewall, on one worker: the Proxmox writes and their order, the node Ready throughout, Remove and
  the release. Which ports were closed and open afterwards was read but not saved.
- Replace: its refusals on a 0.8.0 platform (`409 last_ready_node`, 502 with the chart reason), "needs
  attention" after its bound, and the read of the guest. No working Replace and no health check: both need
  chart 0.1.14.
- The one root command: the Check rows, the cluster firewall rule written and marked, the refusals and the
  command's text. The command was not run on a node, and the rule matched nothing on that lab (its router
  translates the platform network).
- Presets and the control plane count: the refusals and the `capacity` row. No preset install, no three control
  planes.
- Network modes and the tenant path test: the refusals and the cleanup. The test gave no verdict; no routed
  mode and no VLAN tag.
- "Usable after install", invitations, rule edit and the other console items: at the API only, and in part.
  No console page was opened.

### Not verified

- The upgrade from 0.8.0, 0.8.1 or 0.8.2 to this BOM, a second upgrade from this BOM, and a failed upgrade's
  Retry and restore ([docs/managed-services.md](docs/managed-services.md), "What the upgrade still owes a lab").
- A fresh install from the candidate's images, with a preset, three control plane machines, routed mode or a
  VLAN tag.
- Chart 0.1.14 on a cluster: the health check, Replace, and the worker firewall on two workers.
- The tag's workflows: no candidate carried a `docs/releases/` file or a previous BOM before.
- Everything the section of 0.9.0 said then and the sections of 0.8.0 to 0.8.2 say is not verified.

## [0.8.2] - 2026-11-18

A patch release on top of `v0.8.1`. It adds no feature of its own size: it tells an operator about the
site layout the first production site ran into, closes a gap that left a credential behind, and fixes
what the production acceptance of `v0.8.0` found in the two consoles.

- **A warning for the site layout that drops tenant traffic to the platform.** On a site where one
  Proxmox node both translates tenant traffic and hosts the platform's machines, tenant Kubernetes
  workers never join and tenants cannot reach managed databases. The Check has a row `tenant-path`
  that warns, and the guide has the rule, the two supported layouts and what to do with workers that
  did not join. The product does not repair the layout (Added, Changed, Known issues).
- **A deleted tenant cluster's Proxmox token is released without waiting for a provider sync.** The
  token and its ACL rows stayed until a sync, and on an install without the scheduler until an
  operator started one. A job now revokes them minutes after the delete, on every install (Fixed).
- **The operator's Ready card** shows the platform's addresses, says what a tenant cannot use yet,
  downloads a kubeconfig of the platform cluster that works for one hour, and prints the commands
  that open the two ArgoCD instances (Added, Fixed).
- **Customer console fixes from the first production acceptance**: a network's Servers and Subnets
  tabs, a snapshot delete after a rollback, backups, the role form, the image list of Rebuild, pages
  the menu hides, a warning in the Create share dialog, the serial console, and the Kubernetes create
  form (Fixed, Changed).
- **Sandbox template image pins** and **dependency updates** of the operator console's build
  (Changed).

The platform BOM of this release is `0.8.2`. It pins what `0.8.0` and `0.8.1` pin: only the `release`
line differs, and no chart, image or download changes. The release carries the BOMs of `0.8.0` and
`0.8.1` for the platforms those releases installed.

Upgrading from `v0.8.1`. From `v0.8.0`, read the section of `v0.8.1` as well: its steps still apply.

- **Docker Compose host:** check out the tag and run `./proxcloud-up.sh upgrade v0.8.2`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first; nothing else is run by hand.
- **An installed platform stays on its BOM** (0.8.0 or 0.8.1). Step 4 of the Managed services page
  says that the platform stays on its release and shows the 0.8.2 BOM as a newer release that is not
  available to an installed platform yet. That is the intended state: nothing has to be installed
  again, and this release needs no **Apply this release**. A platform installed from a `dev-<sha>`
  build of `main` is still refused (`bom_unsupported`).
- **Sites with managed services: run the Check after the upgrade** (System → Managed services) and
  read the new row `tenant-path`. A warning does not fail the Check and changes nothing by itself.
  Where it warns, read "The path from tenants to the platform" in
  `docs/managed-services.md` before tenants create clusters or databases (Added).
- **Tenant clusters deleted before the upgrade** have no release job: their Proxmox token goes with
  the provider sync, as before. On an install without the scheduler start one sync after the upgrade
  (`POST /api/v1/admin/job/external-service/sync`) if clusters were deleted on `v0.8.1` (Fixed).
- **A platform kubeconfig is a new download**, valid for one hour; the seed kubeconfig is unchanged.
  Each download writes the break-glass account and its roles on the platform cluster again (Added).
- **Scripts:** no answer was taken away. `CREATE_BACKUP` on a server now answers the new archive's
  id, and `GET_SERVERS` on a network answers a list where it was refused (Fixed).

Verified on a lab, on 2026-11-18, on one lab Proxmox VE cluster, over the platform that the published
images of `v0.8.0` had installed for the round of `v0.8.1`. The portal was upgraded with
`./proxcloud-up.sh upgrade` to the build `dev-03bc9f3` of `main`. That build is this release without
the sandbox template image pins (they are in no image), without the fix of the port-forward rule
(below, H1), and without its changelog and its release commit. On that build:

- **The platform kept working.** `status.bom` stayed the 0.8.0 one, the 14 Applications were Synced
  and Healthy, the plan had 0 changes and no blocker and named the build's own BOM as newer, both
  previous BOMs (0.8.0, 0.8.1) were answered by their hash, and the Check passed with 20 rows, the
  19 of `v0.8.1` and `tenant-path`.
- **The `tenant-path` row**, in two cases: `ok` for a VLAN default zone, and a warning for a `simple`
  default zone, with the nodes, the platform network and the two layouts in its text. The warning did
  not fail the Check. The row reads the configuration: no path was measured.
- **The token release job**, for a plain delete of a tenant cluster whose one worker had joined: with
  the scheduler off and no provider sync, the job revoked the cluster's token and its seven ACL rows
  2 minutes 53 seconds after the delete, 23 to 38 seconds after the cascade had ended, at its first
  attempt. The ACL list was then equal to the one before the create; the cluster's namespace, its
  address and its disk were gone as well.
- **The Ready card's data and the platform kubeconfig**, at the API and with `kubectl` 1.35.10: the
  four addresses and the two provider rows against the installation; a kubeconfig with a token of
  `kube-system/proxcloud-break-glass` whose claims give it one hour; the audit row `BREAK_GLASS` with
  `operation: platform-kubeconfig` and no token in it; `403` for a tenant and `401` without a token;
  the reads the guide's table names, and a refusal for Secrets, `pods/exec`, a delete, a patch of an
  Application and a port-forward outside `argocd`. Both ArgoCD commands worked on the platform
  cluster and on the seed: the password was read and the forwarded port answered `200`.
- **Customer console fixes, two of them, at the API:** `GET_SERVERS` on a network answered `200` with
  an empty list (D1), and a new network's row held `subnetDetails` in the create answer and in the
  read that followed, without a sync (D2).

Outside the lab: one production site was upgraded to the released `v0.8.1` on 2026-11-18. Its
installed platform stayed on its BOM: the plan had 0 changes, the apply succeeded and set `onboot` on
the platform's machines.

**Not** verified on a lab:

- **Anything on the published artifacts of this release.** The lab ran a build of `main` whose BOM was
  `0.9.0-dev`, on a platform installed by `v0.8.0`. A platform installed by `v0.8.1` or by this
  release, an apply on this release, and an installer or Helm install of it were not run.
- **The fix of the port-forward rule (H1).** The lab build still had the old rule: `kubectl` was
  refused on its first request and fell back without a word, which is how the defect was found. That
  a port-forward opens on its first request with the new rule rests on a test against a fake cluster.
- **Every customer console fix that needs a tenant server.** The lab node that holds the OS image had
  no capacity for one (the create answered `409`), so none of these ran: a snapshot delete after a
  rollback (D8), the id that `CREATE_BACKUP` answers (D12), the serial console that ends its session
  and the console that opens on the Serial tab (D13, `GET_CONSOLE_STATE`), the image list of Rebuild
  (D14), and a Servers tab with a server in it. Not run either: the share warning (D9, the lab has no
  share provider), the role form (D10), a hidden page opened by its URL (D15), the Subnets tab for a
  network made before this release, the backup confirm dialog and the Kubernetes create form's
  defaults (the lab provider offers one version, so "lowest" and "newest" are the same). No page of
  the customer console was opened on the lab.
- **The `tenant-path` row beyond the two cases above:** `ok` because the node routes the platform
  network, `ok` because the platform's machines are on other nodes, an EVPN zone, the warning for a
  Proxmox that could not be read, and the row against a path that is in fact closed. The node leg of
  the guide was run by hand on one production site only, and the layout with a platform network
  routed by the node was run nowhere.
- **The release job beyond a plain delete:** a cascade that hangs (the job `dead` after 24 hours),
  `release_failed` with the management cluster or Proxmox down, a project teardown, an API restart
  while the job is pending, a VM `<clusterId>-…` left in the pool, and a cluster with a
  `csi@pve!<clusterId>` token or an RWX dataset.
- **The Ready card in a browser**, a user with the support role, the `409`, `502` and `503` answers of
  the kubeconfig route, the line "Offers no Kubernetes version yet" (the lab provider has a version),
  the token after its hour, and a login to either ArgoCD.
- **The new text for a worker that has not joined**, the operator console after its dependency
  updates (it serves; no page was opened), and the sandbox template images of the new pins (nothing
  was built or started from them).
- Everything `v0.8.0` and `v0.8.1` say is not verified still holds, except what is listed as verified
  here.

The entries below were written when each change merged, before the lab round. Where one ends with "Not
verified on a lab", the lists above say what has been run since and what has not. Where one says the
BOM on `main` is `0.9.0-dev`, that was the state at that merge: this release's BOM is `0.8.2`.

### Added

- **A warning when tenants cannot reach the platform network.** On a site where a Proxmox node translates
  tenant traffic (a `simple` tenant zone, or an exit node of an EVPN zone) and also runs the platform machine
  that owns the destination address, a tenant's TCP connection to the platform network is dropped on that node
  unless the node itself routes the platform network: tenant Kubernetes workers run and never join, and tenants
  cannot reach managed databases (production acceptance of v0.8.0, a one-node site with the platform network
  on a VLAN). The managed-services Check has a row `tenant-path` that reads the layout and warns; it never
  fails and opens no connection, so `ok` is not proof. The guide has the rule, the two supported layouts and
  what to do with workers that did not join
  ([docs/managed-services.md](docs/managed-services.md#the-path-from-tenants-to-the-platform)). The node leg
  for an existing site was run by hand on one production site and is lost at a reboot; the layout with a
  platform network routed by the node was run nowhere. Not verified on a lab: the row is tested against a fake
  Proxmox API only.
- **The ready card of Managed services shows where the platform is and how to open ArgoCD.** For an installed
  platform the page lists the seed API address, the platform cluster API address, the load balancer range and the
  node range, and prints the `kubectl` commands that open the two ArgoCD instances (a port-forward and the read of
  `argocd-initial-admin-secret`); no ArgoCD gets an address in this release. **Download platform kubeconfig**
  (`GET …/platform-kubeconfig`, `admin:service:manage`, audited as `BREAK_GLASS` with `operation:
  platform-kubeconfig`) saves a kubeconfig of the platform cluster that works for one hour: a token of the service
  account `kube-system/proxcloud-break-glass`, which the route writes on the cluster. It reads the cluster without
  its Secrets, forwards a port in `argocd` and reads ArgoCD's initial admin password; it is never the admin
  certificate Cluster API keeps on the seed. Through ArgoCD its holder can still change the cluster
  ([docs/managed-services.md](docs/managed-services.md#after-the-install)). Not verified on a lab: the tests use a
  fake seed and a fake cluster, and the ArgoCD Service and Secret names are the chart's defaults.
- **The ready card says what a tenant cannot use yet.** A green install registers the Kubernetes provider with no
  version and both providers attached to no project, so every tenant cluster create was refused while the page
  looked finished. The card now says "Offers no Kubernetes version yet" and "New projects do not get this
  provider", each with a link to where it is put right. Nothing is published or attached for you. Not verified on
  a lab.

### Changed

- **The platform BOM on `main` is `0.9.0-dev` again, and the build carries the BOM of `0.8.1`**
  (`deploy/platform/platform-bom.yaml`, `deploy/platform/previous/0.8.1.yaml`). The `v0.8.1` release commit left
  the BOM at `0.8.1`, and the release gate refuses only `-dev`. A platform installed by `v0.8.1` stays on its BOM
  after an upgrade, as one installed by `v0.8.0` does; a platform installed by a `dev-<sha>` build of `main` is
  still refused by a later build (`bom_unsupported`). Not verified on a lab.
- **A worker that has not joined sends the tenant to the operator.** Past its 15 minutes the node group's line
  in the customer console read "Check that the cluster's network has a router or gateway and reaches the
  cluster endpoint", which a tenant cannot check. It now says that the machine most likely could not reach the
  cluster's endpoint (the product does not read the join error), to contact the operator, and that the machine
  is replaced by reducing the count by one and raising it again once the cause is repaired (a group of one is
  raised first: a count below one is refused); inside the bound it reads "has not joined the cluster yet"
  (`web/client/src/pages/platform/KubernetesPage.tsx`, `docs/managed-k8s.md`). Not verified on a lab.
- **The server console opens on the Serial tab** when the server's display is its serial port (cloud images), and
  says that Enter brings the login prompt. **The Kubernetes create form starts on the lowest version offered, with
  high availability off**; it preselected the newest version and three control planes. A provider marks no default
  version. Not verified on a lab.
- **The sandbox template images are pinned to their builds of 2026-11-16** (`deploy/templates/images.yaml`,
  commit `c8b3364` of the image sources): the twelve `sandbox-*` images, each by tag and digest. The file is in no
  image of the portal. Not verified on a lab.
- **Dependency updates of the operator console's build**: `@tailwindcss/vite` 4.3.5 and `@vitejs/plugin-react`
  6.3.0 (`web/admin`). Not verified on a lab: the console was built and served, no page was opened.

### Fixed

- **A deleted tenant Kubernetes cluster's Proxmox token is revoked without a provider sync** (lab finding F57).
  The token `capmox@pve!<clusterId>` and its ACL rows stayed until a provider sync ran after the cluster's
  30-minute marker grace, and on an install without the job scheduler (the Compose default) until an operator
  started a sync. The delete, and a project teardown, now queue a durable job (`kamaji.release/v1`) that the API
  runs with or without the scheduler. Its first look at the cluster comes within seconds of the delete, the next
  ones 15 seconds apart, growing to 5 minutes, and at the first look that finds the cascade over it revokes the
  token and releases the cluster's addresses, dataset and Secrets. After 24 hours it gives up (`dead`,
  `cascade_not_finished`) and the sync's sweep, which stays as the backstop, does the release. The job and the
  sweep now also wait for the cluster's Machines, and neither revokes while a VM named `<clusterId>-…` is left in
  the project's pool or while the management cluster or Proxmox does not answer: CAPMOX deletes the workers with
  that token ([docs/managed-k8s.md](docs/managed-k8s.md#deletion-and-orphan-finalization)). Not verified on a
  lab: the tests use a fake management cluster and a fake Proxmox on PostgreSQL.
- **Customer console, the defects of the production acceptance (D1, D2, D8 to D10, D12 to D15).** A network's
  **Servers** tab lists the servers with a NIC on it (`GET_SERVERS` was refused on Proxmox), and its **Subnets** tab
  shows a network's subnet on a site without the scheduled sync (the create writes `subnetDetails`; for a network
  made before, the tab reads the row's own `cidr`, `gateway` and `snat`). A snapshot
  delete right after a rollback asks Proxmox up to three times for the VM's config lock before it answers 409; what
  holds that lock after a rollback was not found. `CREATE_BACKUP` answers the new archive's id, and the console asks
  before it starts a backup. The role form shows a name the server accepts and states the rule. Rebuild offers the
  project's own images, like Create. A page the menu hides (volume snapshots, S3 access keys) no longer opens by its
  URL. The Create share dialog warns when the file server is inside a network the site keeps tenant networks out of
  (`LIST_SHARE_WARNINGS`). A serial console session ends on the page when its server was stopped or restarted
  (`GET_CONSOLE_STATE`, asked every 10 s). Not verified on a lab: the API tests use a fake Proxmox, the console was
  run against its mock app ([docs/cloud-integration.md](docs/cloud-integration.md)).
- **The platform kubeconfig's port-forward is no longer refused on its first request** (lab finding H1). The
  break-glass Role in `argocd` allowed only `create` on `pods/portforward`. The API server authorizes a WebSocket
  port-forward, which `kubectl` tries first since 1.31, as `get`: `kubectl` 1.35 was refused (one 403 in the
  cluster's audit trail per port-forward), fell back to SPDY without a word and worked; a client without that
  fallback would have been refused. The Role now allows `get` and `create`. The route writes the Role at every
  call, so an installed platform cluster gets the rule with the next download of the kubeconfig. The seed
  kubeconfig is cluster-admin and was not affected. The refusal and the fallback were seen on the lab
  (2026-11-18, `kubectl` 1.35.10); the fix is not verified on a lab: the test pins the Role's rules on a fake
  cluster ([docs/managed-services.md](docs/managed-services.md#after-the-install)). The guide of the cluster
  release job now says that only a failed look (`release_failed`) writes the line `kamaji: release of a deleted
  cluster, will retry`; a look that finds the cascade still running (`cascade_running`) logs nothing
  ([docs/managed-k8s.md](docs/managed-k8s.md#deletion-and-orphan-finalization)).

### Known issues

- **A standalone volume can lose its name at create (G1).** A volume created without a server gets a
  small holder VM. When Proxmox lists that VM later than about 12 seconds after its create, the create
  answers `200` with a row that has no `id`, the volume is missing from the tenant's list until the next
  provider sync, and the sync then names it after its disk (`vm-<vmid>-disk-0`): the name the tenant
  gave is lost. Seen once on a lab under load; a second create kept its name. The disk and its size are
  right, and on the lab the volume was deleted through the API once it was listed. The same code is in
  `v0.8.0` and `v0.8.1` (`internal/platform/project/cloud_writes_pve.go`, the read-back after the
  create gives up and answers a row that is never stored). Not fixed in this release.
- **Managed Kubernetes: workers do not join where one node both translates tenant traffic and hosts the
  platform machines.** A tenant cluster's workers and a tenant's database clients open TCP connections
  to the platform network. On a Proxmox node that translates the tenant's traffic (a `simple` tenant
  zone, or an exit node of an EVPN zone) and also runs the platform machine that owns the address,
  while the platform network is reached over the site's router, those connections are dropped: the
  control plane becomes ready and no worker joins. A one-node site is this case; a VLAN tenant zone is
  not. The product still does not repair it. This release warns (the Check row `tenant-path`, which
  reads the layout and opens no connection, so `ok` is not proof) and documents the two layouts that
  work, one of them a change on the node itself, by hand and lost at a reboot
  ([docs/managed-services.md](docs/managed-services.md#the-path-from-tenants-to-the-platform)). A
  platform network that the node routes, set up by the product, is planned for `v0.9.0`. Found on a
  production site on `v0.8.0`; not reproduced on a lab.
- **A node group cannot be scaled to 0, so a worker that never joined is replaced by scaling up, then
  down.** `kubeadm join` runs once, at a worker's first boot: a worker that timed out stays "not
  joined" after its cause is repaired, and a count below one is refused. The tenant raises the group's
  count by one and reduces it again once the new worker is ready, once per dead worker; this needs
  quota for one more worker. On the production site the scale-down removed the machine without a node
  both times; which machine goes is Cluster API's choice, and only that order was run.
- **"Needs attention" stays on such a node group until a new machine is healthy.** Repairing the cause
  does not clear it, because the dead worker does not join again. It is computed at a provider sync:
  on an install without the scheduler it shows, and goes away, only when an operator starts a sync.
- **Seed ISOs of servers created before `v0.8.0` are never swept (O2).** A server's seed ISO
  (`pcseed-v1-<...>.iso` on the provider's seed store) is deleted with its server only when its record carries the
  cluster and storage identity, which is recorded for servers created on v0.8.0 or later
  (`internal/cloud/pve/seed.go`). For a server created on an earlier release the record has none: when that server
  is deleted, the API logs `seed cleanup pending ... original cluster/storage identity was not available; operator
  inspection required`, and the ISO stays on the store for good (seen on a lab after an upgrade, and again in the
  lab round of `v0.8.1`). It holds the
  server's user data. Clean it up by hand once its server is deleted, as root on a node: list the seed ISOs
  (`pvesm list <seed store> --content iso | grep pcseed-v1-`); one that is older than a day and that no VM's config
  names any more (`grep -l <file name> /etc/pve/nodes/*/qemu-server/*.conf` prints nothing) is such a leftover:
  `pvesm free <volume>`. Not verified on a lab: the hand cleanup, that a server created on v0.8.0 or later gets the
  identity, and the sweep that follows.

## [0.8.1] - 2026-11-18

A patch release on top of `v0.8.0`. It adds no feature of its own size: it makes a site that runs
`v0.8.0` safe to upgrade, now and later, and fixes what the first production site and the acceptance
of `v0.8.0` found.

- **A platform installed by `v0.8.0` keeps working after this and every later upgrade.** `v0.8.0`
  carried one bill of materials (BOM), so any later release, a patch included, would have refused every
  apply on an installed platform (`409 bom_unsupported`). From this release on a release carries the BOM
  of each earlier release back to `0.8.0`, and an installed platform stays on the BOM that installed it
  (Fixed).
- **Tenant Kubernetes create on a site whose portal user or token has ACL rows of its own on single
  stores.** Every create answered `502` there while the Check was green. The managed-services script
  writes the missing rows, the Check names the path that fails, and a refused create answers `409` with
  the reason, leaves an audit event and a JSON log line (Fixed, Added).
- **The DHCP flag of a tenant network** says what Proxmox does: `dhcp: false` in a zone that runs no
  DHCP server, and `enableDhcp: true` there is a `400` (Fixed, Changed).
- **The platform's machines start with their Proxmox node**: `onboot` on every VM of the platform, and
  a Check row `machines` (Fixed).
- **Security group rules**: a delete could remove another rule, `ADD_RULE` stored a deny rule as an
  allow rule, an egress rule kept its CIDR in the wrong field, and a rule's remote could name a cluster
  IP set (Fixed, Changed, Security).
- **Volume rows**: a rebuilt server no longer keeps the row of its old boot disk, a volume keeps its
  name, id and bill item when it moves to another server, and a server with VM id 9999 can be deleted
  without destroying the CSI volumes (Fixed, Added).

The platform BOM of this release is `0.8.1`. It pins what `0.8.0` pins: only the `release` line
differs, and no chart, image or download changes.

Upgrading from `v0.8.0`. Read the entries named here before the upgrade.

- **Docker Compose host:** check out the tag and run `./proxcloud-up.sh upgrade v0.8.1`
  (`deploy/compose/README.md`, "Upgrade"). The script takes its backup first; nothing else is run by hand.
- **Sites with managed services: run the Check after the upgrade** (System → Managed services). Where
  the row `privileges:kubemint` fails, run the managed-services script again as root on a Proxmox node
  (step 2 of the Managed services page), once per portal token, or `make upgrade` on an installer site
  in split mode. The row names the path that lacks the right. It fails on a site where the portal's
  user or token holds an ACL row of its own on a store, a zone or a vnet, and it passed there on
  `v0.8.0`; until the script has run, tenant cluster creates answer `409` and an install or apply of
  the managed services stops at its first stage (`preflight_failed`) (Fixed).
- **An installed platform stays on the 0.8.0 BOM.** Step 4 of the Managed services page says that the
  platform stays on release 0.8.0 and shows the 0.8.1 BOM as a newer release that is not available to
  an installed platform yet. That is the intended state, not an error: nothing has to be installed
  again, and **Apply this release** runs with the 0.8.0 BOM. A platform installed from a `dev-<sha>`
  build of `main` is still refused (`bom_unsupported`).
- **Press Apply this release once** on an installed platform, at a time of your choice: it sets
  `onboot` on the platform's machines, which `v0.8.0` set on the seed alone. Until then the Check row
  `machines` warns and names each VM with the command (`qm set <vmid> --onboot 1`). A stopped machine
  is not started by it (Fixed).
- **Scripts that call the security group actions:** `DELETE_RULE` takes `ruleId` only, as
  `LIST_RULES` returns it; `pos` and an id of the shape `<group>-<position>` answer `400` or `409`.
  `ADD_RULE` answers `400` for a field it cannot store (`remoteGroupId`, `action`, `log`, `priority`).
  An egress rule written before this release is listed with remote "any": delete it and add it again
  (Fixed, Changed).
- **Scripts that create networks:** `enableDhcp: true` is a `400` in an EVPN zone and in a `simple`
  zone without a DHCP server. Existing rows are corrected by the next provider sync (Fixed, Changed).
- **Log collectors:** every line of the API log is JSON now; a parser that expected plain text lines
  for `backend request failed` has to change (Fixed).
- **A guest with VM id 9999** on the Proxmox cluster gets a warning in the Check and in Test
  connection (`csi-vmid`): read it before destroying that guest (Added).

Verified on a lab, on 2026-11-18, on one lab Proxmox VE cluster. The portal ran the published images
of `v0.8.0` and installed a platform from the 0.8.0 BOM in one run without a Retry (37 minutes 45
seconds). The portal was then upgraded with `./proxcloud-up.sh upgrade` to the build `dev-751a190` of
`main`, which is this release without its release commit and its changelog. On that build:

- **The platform kept working.** The plan had 0 changes and no blocker and named the build's own BOM
  as newer; the Check was green with the two new rows (`csi-vmid` ok, `machines` a warning); **Apply
  this release** succeeded in 6.6 seconds without a retry; `status.bom` was unchanged (0.8.0, the
  sha256 of the tag's file); the seed's 14 Applications were not synced again. The 0.8.0 BOM was
  answered by its hash, and the BOM of an earlier `dev` build was refused.
- **Tenant Kubernetes create on per-store ACL rows.** With a stand-in row on the boot store the Check
  failed on `privileges:kubemint` and named the store and the script; the tenant's create answered
  `409` with the new text; the audit had a `FAILURE` event with the Proxmox reason for the operator and
  without it for the tenant; the API log had one JSON line with the request id; nothing was left on
  Proxmox.
- **The DHCP flag**, on a network of a VLAN zone and of a `simple` zone made without DHCP: both
  answered `dhcp: false`, no DHCP range was written, and `enableDhcp: true` was a `400` in both.
- **`onboot`** on the four running machines of the platform that `v0.8.0` had installed: the apply set
  it with the CAPI token, it was still there 23 minutes later, and the `machines` row then read ok.
- **Security group rules**, at the API: the egress warning and the CIDR as destination in Proxmox,
  `400` for `remoteGroupId` and `action`, `409` for a stale id and for the id shape of `v0.8.0`, `400`
  without `ruleId`, and a delete by the current id that removed that rule only. Proxmox took the digest
  of the rule list on the delete.
- **Volumes**, in part: the `csi-vmid` row read ok, and a standalone volume was created and deleted
  (see Known issues for what its first create showed).

**Not** verified on a lab:

- **Anything on the published artifacts of this release.** The lab ran a build of `main` whose BOM was
  `0.9.0-dev`. That BOM and the 0.8.1 one pin the same versions as 0.8.0, so the lab does not show an
  apply that keeps older pins while the release carries other ones.
- **Volume attach, detach and rebuild.** The lab node that holds the image had no capacity for a tenant
  server, so none of these ran: that a volume keeps its name, id and bill item on an attach to another
  server, that a rebuild archives the row of the old boot disk, and the delete of a server with VM id
  9999. The warning case of `csi-vmid` was not provoked. These fixes rest on their tests alone.
- **A stopped platform machine**, a machine the token cannot read, a node reboot with `onboot` in
  place, and an install on this release from the start.
- **The production shape of the ACL fix**: an older `ProxCloudPlatform` row on a store, a row held
  through a group, and the script's new lines that repair them (they ran against a fake `pveum`).
- An EVPN zone's `enableDhcp: true`, a rule-list digest that is stale at Proxmox, the console pages of
  every change above, and an installer or Helm install of this release.
- Everything `v0.8.0` says is not verified still holds, except what is listed as verified here.

On the lab, the install's stage 7 (three node clones at the same time) restarted containers of another
Kubernetes cluster whose VMs share the nodes' disks; all recovered. This is the behaviour of `v0.8.0`
and is unchanged.

The entries below were written when each change merged, before the lab round. Where one ends with "Not
verified on a lab", the lists above say what has been run since and what has not. Where one says the
BOM on `main` is `0.9.0-dev`, that was the state at that merge: this release's BOM is `0.8.1`.

### Added

- **A `simple` tenant zone without DHCP from the provider's config**: `"dhcp": false` on a zone of
  `config.network.zones` (`internal/platform/externalservice/proxmox.go`, `docs/managed-services.md`,
  "Prerequisites"). ProxCloud then creates the zone without Proxmox's DHCP server, so tenant Kubernetes clusters
  and load balancers can use its networks on a site without EVPN or VLANs. It applies to a zone that does not
  exist yet; an existing zone is never changed, and the default (a `simple` zone with DHCP) is the same. Not
  verified on a lab.
- **A warning when VM id 9999 is in use on the Proxmox cluster.** proxmox-csi names every volume
  `vm-9999-pvc-<uuid>`, the platform's own and the tenant clusters', and Proxmox counts a disk with that
  name as VM 9999's. On a site where a guest has that id, destroying it with "Destroy unreferenced disks
  owned by guest" would remove these volumes (acceptance audit, A2). The managed-services Check and the
  provider's Test connection have a row `csi-vmid` that says so; it never fails, and the id the plugin
  uses is unchanged ([docs/managed-services.md](docs/managed-services.md#vm-id-9999-and-the-csi-volumes)).
  Not verified on a lab.
- **A cloud resource create that is refused or fails leaves an audit event** (`CLOUD_RESOURCE_CREATE`, outcome
  `FAILURE`), for every resource type. `errorMessage` is what the caller was answered. `internalReason` is the
  backend's own error and is returned by the operator's audit routes alone; the operator's Audit page shows both.
  The type is recorded only when it has the form of a type name, and the name, the answer and the reason are cut.
  Not verified on a lab: the tests use a fake Proxmox, and the two new lines of the Audit page were built and
  smoke-tested, not looked at in a browser.

### Changed

- **The platform BOM on `main` is `0.9.0-dev`** (`deploy/platform/platform-bom.yaml`). The `v0.8.0` release commit
  left it at `0.8.0`, and the release gate refuses only `-dev`, so any `v*` tag cut from `main` would have passed
  that part of the gate.
- **`ADD_RULE` on a security group refuses what it cannot store.** `remoteGroupId`, `action`, `log`, `priority` and
  any other key the rule does not have were accepted and dropped, so a deny rule was stored as an allow rule and a
  rule for a remote group as a rule for every source, each with a 200. They are a `400` now that names the key. The
  protocol (`tcp`, `udp`, `sctp`, `icmp`, `gre`, `esp`, `ah`, or none), the ports (1 to 65535, with `tcp`, `udp` or
  `sctp` only) and the remote (a CIDR or one address) are checked before Proxmox is called, with a message per
  field; a port sent as text was stored as "every port". Not verified on a lab: the tests use a fake Proxmox, and
  that Proxmox accepts each protocol name of the list on a group rule was not tried.
- **Security groups no longer promise outbound rules.** A server's outbound traffic is always allowed, so an egress
  rule restricts nothing. The API still stores one and now says so in the answer (`result.warning`); the console
  and the networks guide say the same. Egress filtering is not in this release. Not verified on a lab.
- **`enableDhcp: true` on a network of an EVPN zone is now a `400`** (`POST /project/{id}/cloud`, type
  `NETWORK`; `internal/platform/project/cloud_network_pve.go`, `docs/api.md`). Up to `v0.8.0` the request was
  accepted and the network answered `dhcp: true`, though Proxmox runs no DHCP server in an EVPN zone and the
  platform assigns the addresses. It is refused like the same request in a VLAN zone or a `simple` zone
  without DHCP; leave the field out or send `false`. Neither console sends it. Not verified on a lab.
- **CI compares the previous BOMs with the registries too.** `bom-verify.yml` (pull requests on `deploy/platform/**`,
  weekly) and the release gate of `docker.yml` run `bom_verify.py --release` on each file under
  `deploy/platform/previous/`, so a release is not built with a previous BOM whose pin is no longer served.
  `docs/ci-release.md`, "Previous BOMs", has the step that follows each release tag. Not verified: that procedure
  has not been run at a release.

### Fixed

- **A platform installed by v0.8.0 keeps working after a ProxCloud upgrade.** A platform records the bill of
  materials that installed it by the sha256 of the BOM file, and a release carried one BOM only. The file holds the
  release number, so every later release, a patch included, was another BOM: the plan was blocked and **Apply this
  release** answered `409 bom_unsupported`. A release now also carries the BOM of each earlier release back to
  `0.8.0` (`deploy/platform/previous/`, byte for byte the file of the tag), and an installed platform stays on the
  BOM that installed it: the Check, an apply (repair, changed sizes and counts, credential renewal) and Remove run
  with that BOM's pins. Step 4 of System → Managed services says that the platform stays on its release, lists that
  release's contents and names the newer one; `GET …/plan` has `newer` for it, and `target` is now the BOM the apply
  runs with. `GET /admin/platform/bom?sha256=` answers a carried BOM by hash. Moving an installed platform to a
  newer BOM is not in this release, so the message for a platform image on the template update blocklist no longer
  sends an installed platform to a newer release: it says that the platform stays on its release's images. A
  platform installed by a build of `main` (`dev-<sha>`) is still refused: only the BOMs of releases are kept. Not
  verified on a lab: tests show that the embedded file has the hash a v0.8.0 platform records and that this release
  renders the 0.8.0 BOM as v0.8.0 did; no apply was run against a platform that v0.8.0 installed.
- **A network in a zone without a DHCP server no longer answers `dhcp: true` (F2)**
  (`internal/platform/project/cloud_network_pve.go`, `internal/cloud/pve/network.go`, `docs/api.md`,
  `docs/cloud-integration.md`). The network create took the flag from the zone's configured kind and wrote a
  DHCP range on the subnet, and the sync took it from the subnet alone. On a `simple` zone made without DHCP,
  and on every EVPN zone, a tenant read "DHCP on" where Proxmox hands out no address. The create now reads the
  zone in Proxmox: where it runs no DHCP server the network answers `dhcp: false`, no range is written, and
  `enableDhcp: true` is refused with a `400` that says why. The sync and the subnet actions answer from the
  subnet and the zone together, so the next sync corrects existing rows. The DHCP range on subnets written
  before stays: it does nothing without a DHCP server in the zone, and removing it would need an SDN apply.
  Load balancers and tenant Kubernetes clusters already decided from the zone and work on such a network. Not
  verified on a lab.
- **The platform's machines start again with their Proxmox node**
  (`internal/platformlife/cluster.go`, `internal/platformlife/preflight.go`, `docs/managed-services.md`,
  "After a Proxmox node reboots"). Only the seed had `onboot`, so after a reboot of a node the platform
  cluster's machines stayed stopped, and with them every tenant control plane and database. Stage 7 of every
  install and apply now sets `onboot` on each VM of the platform that lacks it, with the CAPI token, and the
  Check has a row `machines` that warns for a VM that is stopped or has no `onboot`, with the command, and names
  a VM it could not read without hiding the others. A platform installed by `v0.8.0` gets it at its first apply;
  until then `qm set <vmid> --onboot 1`. No start order is set (Proxmox takes `Sys.Modify` on `/` for it, which
  the token does not hold; the guide has the command), tenant clusters' machines are not covered, and a stopped
  VM is still started by hand. No chart, BOM or role change. Not verified on a lab.
- **Deleting a security group rule could delete another rule.** A rule was deleted by its position, and a new rule
  takes position 0 and moves the others down: a delete from a page loaded before someone added a rule removed a
  different rule, and `DELETE_RULE` with neither `pos` nor `ruleId` removed the first rule. A listed rule's `id` now
  holds a fingerprint of the rule next to its position, `DELETE_RULE` takes only `ruleId`, and the delete carries the
  digest of the rule list to Proxmox. When the id no longer names a rule the answer is `409`, nothing is deleted, and
  the console reloads the list; without `ruleId` the answer is `400`. A script that sends `pos`, or an id of the old
  shape `<group>-<position>`, has to list the rules and send the `id` (`docs/api.md`, security group rules). Not
  verified on a lab: the tests use a fake Proxmox; that the rule list of a group carries a digest and that Proxmox
  checks it on the delete was not tried, and the console's `409` path has no automated test.
- **An egress security group rule stored its remote CIDR as the rule's source.** It is the destination now, and the
  console's rule table has a Direction column. A rule written before keeps the CIDR in the source and is listed with
  remote "any": delete it and add it again. Such a rule had and has no effect, see Changed. Not verified on a lab.
- **The rule table of a security group showed an empty cell** for "any protocol" and "any remote". Not verified on
  a lab: the page was built and opened by the smoke test only.
- **A rebuilt server no longer keeps the row of its old boot volume.** A rebuild gives the server a new
  boot disk and destroys the old one, but the old volume's row stayed until the scheduled sync ran, and
  for good on a site that runs no scheduler: the volume list showed the disk twice and the quota counted
  it twice (production acceptance of v0.8.0, D3). Rebuild, restore from backup and
  snapshot rollback now reconcile the project's volume rows in the same request, so a row whose disk is
  gone is archived, and a resize refreshes the boot volume's size at once. The reconcile does not run
  while another server of the project is locked, offline or unreadable; a rebuild then still archives
  the row of the disk it destroyed. A rollback to a snapshot older than an attached volume now archives
  that volume's row at once (the disk stays on the store). Not verified on a lab.
- **A volume keeps its name, id and bill item when it is attached to another server.** Proxmox gives the
  disk a new id on such an attach, and the portal archived the volume's row and made a new one: a volume
  named "t-vol1" showed as "t-srv2-scsi1", created "just now", and its charge was split over two bill
  items (D4). The row now follows the disk, as it does for a change of volume type, also when the attach
  answered an error after the disk had moved. An attach while a change of type is still copying answers
  `409`. Not verified on a lab.
- **Deleting a server that has VM id 9999 no longer destroys the CSI volumes.** The portal deletes a VM
  with "destroy unreferenced disks", and Proxmox counts every `vm-9999-pvc-<uuid>` volume as an
  unreferenced disk of VM 9999. A server can get that id (Proxmox hands out the lowest free one), so
  the flag is now left out for it. Not verified on a lab.
- **Managed Kubernetes: a cluster create no longer fails on a site with ACL rows of its own on single stores.**
  In Proxmox a row on a path replaces what its parents pass down. On a site where the portal's user or token
  held a row on a store (an older setup wrote `ProxCloudPlatform` on single stores), the mint role on `/storage`
  did not reach that store, Proxmox refused the cluster token's row there, and every tenant cluster create
  answered `502`, while the Check was green. The managed-services script and installer stage 10 now write
  `ProxCloudKubeMint` next to each such row on a store, a zone or a vnet, for the user or token that holds
  the row; the Check row `privileges:kubemint` reads the paths a mint writes (the template pool, `/nodes`,
  `/sdn/zones/localnetwork`, each zone of the provider, the ISO store, the import store and the boot stores of
  the storage profiles) and names the path that fails. **To take it: run the script again as root on a node
  (step 2 of the Managed services page), or `make upgrade` on an installer site in split mode. Until then, on a
  site with such rows, `privileges:kubemint` fails where it passed on v0.8.0, and an install or apply run of the
  managed services stops at its first stage (`preflight_failed`).** Each added row
  is `Permissions.Modify` on that store or zone for the portal token: what `/storage` gives on every other
  store ([docs/proxmox-permissions.md](docs/proxmox-permissions.md#a-row-of-the-portals-own-on-a-store-or-a-zone)).
  Not verified on a lab: nothing of it ran on a Proxmox cluster; the script's new lines ran with a fake `pveum`.
- **Managed Kubernetes: a create that Proxmox refuses for a permission says so.** It answers `409 Could not mint
  the cluster's Proxmox tokens: the operator must fix a permission of the portal on Proxmox first…` where it
  answered `502 The compute backend is unavailable. Try again later.` The guide named a text that did not exist
  ([docs/managed-k8s.md](docs/managed-k8s.md#5-troubleshooting)). Not verified on a lab.
- **The API log is JSON on every line.** Lines written through the default logger, the `backend request failed`
  errors among them, were plain text, so a filter on `level` did not see them. That line now carries the request
  id of the answer (`X-Request-Id`). Not verified on a lab or on a site.

### Security

- **A security group rule's remote must be a CIDR or an address.** The value was passed to Proxmox as written, so a
  tenant could name a cluster IP set or alias as the source of a rule. Not verified on a lab.

### Known issues

- **A standalone volume can lose its name at create (G1).** A volume created without a server gets a
  small holder VM. When Proxmox lists that VM later than about 12 seconds after its create, the create
  answers `200` with a row that has no `id`, the volume is missing from the tenant's list until the next
  provider sync, and the sync then names it after its disk (`vm-<vmid>-disk-0`): the name the tenant
  gave is lost. Seen once on a lab under load; a second create kept its name. The disk and its size are
  right, and on the lab the volume was deleted through the API once it was listed. The same code is in
  `v0.8.0` (`internal/platform/project/cloud_writes_pve.go`, the read-back after the create gives up
  and answers a row that is never stored). Not fixed in this release.
- **Managed Kubernetes: workers do not join where one node both translates tenant traffic and hosts the
  platform machines.** A tenant cluster's workers and a tenant's database clients open TCP connections
  to the platform network. On a Proxmox node that translates the tenant's traffic (a `simple` tenant
  zone, or an exit node of an EVPN zone) and also runs the platform machine that owns the address,
  while the platform network is reached over the site's router, those connections are dropped: the
  control plane becomes ready and no worker joins. A one-node site is this case; a VLAN tenant zone is
  not. It was found on a production site on `v0.8.0` and is the same in this release, where managed
  Kubernetes works on such a site only after a change on the node itself, outside the product. This
  release neither detects nor repairs it: `v0.8.2` adds a Check warning and the description with the
  node-side steps to [docs/managed-services.md](docs/managed-services.md). Not reproduced on a lab.
- **Seed ISOs of servers created before `v0.8.0` are never swept (O2).** A server's seed ISO
  (`pcseed-v1-<...>.iso` on the provider's seed store) is deleted with its server only when its record carries the
  cluster and storage identity, which is recorded for servers created on v0.8.0 or later
  (`internal/cloud/pve/seed.go`). For a server created on an earlier release the record has none: when that server
  is deleted, the API logs `seed cleanup pending ... original cluster/storage identity was not available; operator
  inspection required`, and the ISO stays on the store for good (seen on a lab after an upgrade, and again in the
  lab round of this release). It holds the
  server's user data. Clean it up by hand once its server is deleted, as root on a node: list the seed ISOs
  (`pvesm list <seed store> --content iso | grep pcseed-v1-`); one that is older than a day and that no VM's config
  names any more (`grep -l <file name> /etc/pve/nodes/*/qemu-server/*.conf` prints nothing) is such a leftover:
  `pvesm free <volume>`. Not verified on a lab: the hand cleanup, that a server created on v0.8.0 or later gets the
  identity, and the sweep that follows.

## [0.8.0] - 2026-11-17

A feature release on top of `v0.7.0`: the managed-services platform (P3, ADR 0009). The platform is off
by default.

- **Managed services from the operator console** (System → Managed services, `/platform`). The page and
  its API (`/api/v1/admin/platform/installations`) install, repair and remove a platform on your Proxmox VE
  cluster as a durable job: a seed VM, a platform Kubernetes cluster, Kamaji, CloudNativePG and the two
  providers. What a run applies comes from a bill of materials that ships in the release
  (`deploy/platform/platform-bom.yaml`, `0.8.0`): every chart, image and download with an exact version and
  a digest, compared with the registries in CI and again on the release tag. On the Docker Compose install
  the switch is `./proxcloud-up.sh install --managed-services` (it implies `--template-updates`). There is
  no upgrade from one BOM to another yet (`bom_unsupported`). Operator guide:
  [docs/managed-services.md](docs/managed-services.md).
- **Managed Kubernetes on Kamaji**: node group replicas are placed by free capacity on sites without
  shared storage (cluster chart `proxmox-kamaji-cluster` 0.1.13), and a cluster that is not there yet says
  why, per node group, with a "needs attention" state (`docs/managed-k8s.md`).
- **Managed databases** on the platform cluster: placement on the `dbaas` node group and an egress fence
  (`docs/managed-dbaas.md`). No tenant database is recorded healthy on a lab; see what was verified, below.
- **Chart mirror**: the nine third-party charts of the BOM are served from
  `ghcr.io/mutex-solutions/charts-mirror` by a CI flow of their own (`docs/ci-release.md`, "Chart mirror").
- **Template updates: publish a version on all nodes**, for sites that keep a copy of a Kubernetes node
  image on every node (`docs/template-updates.md`).
- **Fixed Ubuntu 26.04 VM images.** Every `proxcloud-vm-ubuntu-2604` and `proxcloud-vm-ubuntu-2604-k8s`
  image exported before the fix can come up with a read-only root filesystem after a reboot (a stale
  ext4 orphan-file field). The four images were exported again with the fix; the Kubernetes node images
  also set the AppArmor sysctl a PostgreSQL container needs on kernel 7.0.0-42 and newer. The affected tags
  stay in the registry (Fixed).
- **Security:** the portal token can create Proxmox API tokens for `capmox@pve` and `csi@pve` only (it
  could for every user of the cluster, `root@pam` included), a node group's `imageId` is no longer taken
  from a request, and ArgoCD's `default` project is locked on installer-built services clusters
  (Security).

Upgrading from `v0.7.0`. Read the entries named here before the upgrade; the exact steps are in the
guides they link.

- **Docker Compose host:** check out the tag and run `./proxcloud-up.sh upgrade v0.8.0`
  (`deploy/compose/README.md`, "Upgrade"). Managed services stay off unless you install with
  `--managed-services`.
- **Ubuntu 26.04 templates imported before the fix** (the **Imported templates** tab shows the digest;
  the affected tags and digests are listed in `docs/public-vm-sanitization.md`): import the fixed tag,
  publish it, then withdraw the affected one. Servers and Kubernetes nodes already cloned from an affected
  template keep the defect: test and repair them as `docs/template-updates.md` says ("Known issue: Ubuntu
  26.04 images exported before the orphan-file fix"), and do not reboot a VM that has logged the error.
  Ubuntu 24.04 images are not affected.
- **Sites with managed Kubernetes** (the mint grant, on a `v0.7.0` site only where it was set up: an
  installer site in split mode, or one that ran the managed-services script of a build of `main`): run
  `make upgrade` on the installer site, or the managed-services script again as root on a node, once per
  portal token. Until then the preflight row `mint-scope` fails and Test connection warns. Managed
  Kubernetes now needs `libpve-access-control` 9.2.1 or later on every node, which the product does not
  check (Security; `docs/proxmox-permissions.md`, "An existing site").
- **Installer-built services clusters:** the next run of stage 50 (`make install`) locks ArgoCD's
  `default` project and first stops on any Application still in it (Security). Stage 10 adds narrower rows
  for the CAPMOX and CSI tokens and leaves the broad ones for the operator to remove (Changed).
- **Behaviour changes:** an `imageId` sent for a node group is ignored (Security); a tenant network in a VLAN zone is written and answered without DHCP and SNAT,
  and asking for either is a `400`; a project's "Delete now" answers `409` while the job scheduler is off;
  an `UPGRADE` of a tenant cluster on a provider that offers no version is refused (Fixed).
- **A platform installed from a `dev-<sha>` build of `main`** is refused by this release
  (`bom_unsupported`): keep the build that installed it, or Remove and install again
  (`docs/managed-services.md`, "Not implemented yet").

Verified on a lab: the managed path ran on builds of `main` on one lab Proxmox VE cluster, the last
round on 2026-11-17 (`cb2f052`). The install finished twice, the second time in one run without a Retry
on cluster chart 0.1.13 and platform chart 0.1.2. Remove of a ready platform and Remove from a failed
stage 7 passed. A tenant cluster on a network of a VLAN zone had three workers Ready 12 minutes after the
create, and a scale-up and scale-down placed and removed one replica. Publish on all nodes published two
nodes. On a seed built from the fixed image two clean reboots passed, and one restore of the seed over
itself passed. A seed credential rotation was measured. An API restart in stages 7 and 8 of an apply with
nothing new to write resumed, and a cancel followed by Retry passed. The record, row by row, is in the
operator guide, section [Not verified on a lab yet](docs/managed-services.md#not-verified-on-a-lab-yet).

**Not** verified: anything on the published artifacts of this release (the lab used builds of `main`;
`--managed-services` on a Compose host against a published release was not run). The lab checks L0 to
L11 of the design are not recorded as passed. No tenant database became healthy on the lab's first
end-to-end run, and none is recorded healthy since, on the fixed node image either. Also not run: an API
restart in stages 2, 3 and 9 of a real install, the failure drills, the scheduled seed backup job and its
pruning, a restore from an archive older than a credential rotation, the mint grant on the group and its
migration on a Proxmox cluster, an upgrade of a tenant cluster, the console dialogs of publish on all
nodes, an installer or Helm install of this release, and an install on a single-node Proxmox cluster.
Everything `v0.5.0` to `v0.7.0` say is not verified still holds.

The entries below were written when each change merged, and a later entry corrects an earlier one. Where an entry says the BOM stays `0.8.0-dev`, holds a `pending` digest or
a `0.0.0+` version, or names chart 0.1.11, 0.1.12 or `proxcloud-platform` 0.1.0 or 0.1.1, that was the
state at that merge: this release's BOM is `0.8.0`, has no `pending` digest, and pins
`proxmox-kamaji-cluster` 0.1.13 and `proxcloud-platform` 0.1.2. Where one says a switch or a function is
inert because nothing reads or calls it yet (`PROXCLOUD_PLATFORM_ENABLED`, `MarkTemplateGone`, the
renderers), the later entries add the caller. Where one says nothing was installed from the fixed images
on a lab yet, lab round C (2026-11-17) did so.

### Added

- **Template updates: publish a version on all nodes** (`docs/template-updates.md`, admin console System > Template
  updates). A site without shared storage needs a copy of a Kubernetes node image on every node, and each copy had to
  be imported, smoke-tested and published by hand. **Publish on all nodes…** on a version's row does it for every node
  of one Proxmox provider: `GET /admin/template-updates/publish-all` (`admin:service:read`) reports the state per
  node, `POST` (`admin:service:manage`, audited) advances one step. A step publishes every node whose smoke test
  passed, through the existing publish with all its gates, then starts at most one import or one smoke test, and
  only when no import and no smoke test of that provider is queued or running. The open dialog drives it (it reads
  every 15 s); closing it pauses. A failed import or smoke test is not repeated, nothing is withdrawn, a node without
  the provider's import storage or the default profile's storage is skipped with the reason, and a failed node keeps
  its older published copy. The report carries the `config.cluster.versions` line, which the operator still copies
  by hand, and warns while it covers fewer nodes than publish another Kubernetes version or while a node is down
  (its copy cannot be read). An advance that published a node and then cannot start the next import still answers
  the report, with the error in it, so the publish is shown and audited. Lab round C (2026-11-17, main cb2f052):
  two advances published two nodes, one at a time, each once its copy's smoke test had passed; the report and the
  versions line were right; nothing was imported; the smoke clone upgraded no package. Not verified on a lab: an
  import started by it, a smoke test started by it and so its one-job-at-a-time rule under contention (both copies
  carried a failed smoke test, which it does not repeat, and were smoke-tested from their rows by hand, one after
  the other), and the console dialog.

- **Managed Kubernetes: node group replicas are placed by free capacity** (cluster chart `proxmox-kamaji-cluster`
  0.1.13, `docs/managed-k8s.md`). On a site without shared storage a node group is one MachineDeployment per node
  that holds the image, and the chart split the count evenly, so a full node still got its share and its machines
  hung. ProxCloud now decides the split and stores it (`machinePlacement`): a new replica goes to a node that holds
  the image (its template is still on that node), has the cluster's storage with room for the disk (storage
  headroom), stays under the CPU overcommit and
  has the memory free; among those the node with the fewest replicas of the group, then the most free memory. It is
  decided at create, scale-up and upgrade, and is stable: running workers never move, only the difference is placed
  or removed. Workers that are decided and not cloned yet count (memory, CPU and root disk), and decisions on one Proxmox provider run one at a
  time. Scale-down takes a stuck replica first and empties a node last. A request that does not fit answers 409
  with how many fit, before anything is reserved. The console warns when a group is scaled below the number of
  hosts it runs on (volumes on a host's local storage stay there). A cluster on chart 0.1.12 or older is unchanged;
  one moved to 0.1.13 renders the same objects until its node groups change. Existing sites get it only after the
  operator sets the provider's `chartVersion` to 0.1.13 and bumps each cluster. Chart 0.1.13 is published (tag
  `proxmox-kamaji-cluster-v0.1.13`) and the platform BOM pins it by digest, for the platform cluster (whose render
  is the same as with 0.1.12) and for new tenant clusters: a managed-services platform installed from this BOM
  registers its Kubernetes provider with `chartVersion` 0.1.13. Lab round C (2026-11-17, main cb2f052, a fresh
  install from this BOM): the placement was stored and rendered (one MachineDeployment per node with the decided
  replicas), three workers were Ready 12 minutes after the create, a scale-up to four and back to three placed and
  removed one replica and left the other nodes' workers alone, and a node group and a second cluster that did not
  fit were refused with 409 and the count that fits, with nothing minted or written. The round also found that the
  free memory was read without the platform's own VMs (F58, under Fixed). Not verified on a lab: an upgrade, a
  cluster moved from chart 0.1.12, and the console pages.

- **Managed Kubernetes: why a cluster is not there yet, and "needs attention"** (`internal/cloud/kamaji/progress.go`,
  `docs/managed-k8s.md`). A cluster whose provisioning broke half-way used to read PROGRESSING for ever with no
  reason. The status sync now reads what Cluster API and CAPMOX report per worker (Machine, ProxmoxMachine) and on
  the provider access (ProxmoxCluster) and stores, per unsettled node group, `progress = {code, since, machines,
  attention}`, and for the cluster `attention = {code, since}`. Eleven reason codes (a machine that never joined, a
  clone that is retried, one CAPMOX gave up on, no capacity on the host, a refused token, a control plane that never
  came up, and the plain steps); the tenant gets a fixed text per code and what to do, never the upstream message.
  A group or control plane without progress for a bounded time (15 to 90 min by state) needs attention, shown as an
  amber chip on the list and the cluster page; it is recomputed at every sync and clears by itself, and
  `cluster.status` is unchanged. The node group table has a Status column. Operators get the full detail in the
  admin console (**Machines** on a cluster's row of the Kubernetes provider page;
  `GET /admin/service/{id}/k8s-clusters/{clusterId}/machines`, `admin:service:read`), with credentials cut out of
  the upstream messages. The sync needs read access to `machines`, `proxmoxmachines` and `proxmoxclusters`:
  `deploy/mgmt-cluster/rbac.yaml` and chart `proxcloud-platform` 0.1.2 grant it. An installer-built site must re-apply
  `rbac.yaml`; until then the new fields are absent and nothing else changes.
  Chart 0.1.2 is published (tag `proxcloud-platform-v0.1.2`) and the platform BOM pins it by digest for `capi`,
  `capi-tenant` and `platform`, so a new managed-services install gets the access. No entry has `upgradeFrom`: no
  release upgrades between BOMs, so a platform installed from the previous BOM is refused (`bom_unsupported`) and
  keeps charts 0.1.12 and 0.1.1 until a Remove and a new install. On it the operator can set the Kubernetes
  provider's `chartVersion` to 0.1.13 by hand; the ClusterRole is kept by the platform's ArgoCD Application
  (self-heal), so re-applying `rbac.yaml` is no way to the access there. Lab round C (2026-11-17, main cb2f052):
  the codes `provisioning`, `creating_vm`, `starting_vm`, `node_not_ready` and `not_joined` showed in the
  documented order; a worker on a network without a router read `not_joined` and its node group's
  `progress.attention` turned true at the first sync after the 15-minute bound, with `cluster.attention` absent as
  defined; the admin machines route gave the conditions with no secret and nothing of another cluster. Not verified
  on a lab: attention clearing by recovery (that cluster was deleted), the other codes, `cluster.attention`, and
  the console pages (chip, Status column, Machines dialog).

- **Operator documentation for managed services from the console** (`docs/managed-services.md`, ADR 0009): when to
  use this path and when the installer's split mode, the prerequisites, the Proxmox identities script (what it
  creates, the fixed token ids, what happens when a token exists), the four setup steps with every check, the eleven
  stages with their time limits and how a waiting stage looks, every failure code with cause and action, Retry,
  Cancel, Remove and Remove with the seed gone, credentials, the seed backup, the limits on the Compose install and
  the security boundaries. It lists what is not implemented (no upgrade between BOMs, `seed.mode: existing`, the
  seed's other Applications at Remove) and states that
  no run of this path has finished on a lab (M0 and checks L0 to L11 are pending). `docs/install.md` and the Compose
  README point to it from `--managed-services`; `docs/proxmox-permissions.md` drops two sentences that predate the
  job and the page and gains a table for the L1 drill results, marked not yet measured; `docs/quickstart.md` no
  longer says that the Compose install cannot have managed Kubernetes and databases; the console's in-app
  documentation page for Managed services no longer says that **Apply this release** upgrades the components after a
  ProxCloud upgrade, which this release refuses with `bom_unsupported`, and no longer advises an upgrade path for
  that code.

- **Admin console: the Managed services page** (System → Managed services, `/platform`, ADR 0009). It walks an
  operator through the platform installation: the settings of a draft (node groups, platform network, seed, control
  plane, storage, checked in the page before the API checks them), the Proxmox identities script with the two
  write-only token secrets, the checks with what to do for each failed one, the review, and the run with its stages,
  failure code, Retry and Cancel. A ready installation shows the live state of its components, its two providers,
  the seed kubeconfig download, the seed credential replacement and Remove behind a typed confirmation. With the
  switch off the page names `./proxcloud-up.sh install --managed-services`. Platform setup and the Dashboard gain an
  optional `managed` step after Go live (`GET /api/v1/admin/setup/status`), which Go live never waits for. The mock
  mode (`npm run dev:mock`, `?mock-platform=<state>`) walks every state without a cluster.

- **Cluster chart `proxmox-kamaji-cluster` 0.1.11 can render the platform's own cluster** (`controlPlane.kind:
  kubeadm`, ADR 0009): the managed-services job of a later release creates its workload cluster (node groups
  `system`, `kamaji`, `dbaas`) from the same chart the tenants use, through ArgoCD, instead of a template of its own.
  That mode renders a `KubeadmControlPlane` with one replica, pinned to one PVE node, with the kube-vip static pod,
  its prepare script, `preKubeadmCommands`, provider-id kubelet argument and certSANs of installer stage 50's
  services cluster (`services-cluster.yml.j2`), the endpoint on the `ProxmoxCluster`, and the admission plugins
  `NodeRestriction,PodNodeSelector,PodTolerationRestriction`, so namespace annotations can place pods on a node
  group. It renders no CAAPH add-ons, no ClusterResourceSet, no services-side CSI controller and no Gateway API
  carriers; the node groups are unchanged. The control plane's endpoint, `machineImageId` and `machineTags` have no
  default, so values that leave one out fail the render instead of cloning another template or leaving the VM without
  its `pc-platform-<id>` tag. The default stays `kamaji`: for tenant values the output is
  byte-identical to 0.1.10 (`TestChartGolden` pins the defaults alone, with the CSI gateway and with Gateway Fabric,
  as 0.1.10 renders them), and existing clusters and providers keep their chart pin. Inert:
  nothing renders kubeadm mode yet. The tag `proxmox-kamaji-cluster-v0.1.11` is not cut, so the chart is not
  published, and kubeadm mode has not run against a cluster (the first kubeadm join of the platform cluster is a
  lab check of the managed-services design).
- **The Proxmox identities of managed services in the setup script**
  (`GET /api/v1/admin/setup/proxmox-script?managedServices=true&bootStorage=<store>&isoStorage=<store>`): managed
  Kubernetes and databases from the console (ADR 0009) run on a seed VM and a workload cluster of their own, driven by
  two tokens the portal cannot mint for itself. The answer is the portal token script plus a block that creates the
  roles `ProxCloudCAPI`, `ProxCloudCSI` and `ProxCloudKubeMint`, the new pool `proxcloud-seed` for the seed VM, the
  users `capmox@pve` and `csi@pve` with the tokens `capi` and `csi`, their ACLs and the portal's mint grant, and ends
  with what both tokens hold on both platform pools. `capmox@pve!capi` gets `ProxCloudCAPI` on the two platform pools
  instead of `/pool`, `csi@pve!csi` gets `ProxCloudCSI` on `proxcloud-system` and the boot store instead of `/pool`
  and `/storage`, both users get rows of their own on the platform pools (so a `/pool` grant written later for them
  does not reach the platform VMs), and the portal token and every `csi@pve` token get `NoAccess` on `proxcloud-seed`.
  That fences the seed VM, not its disk: the disk is on the boot store, where `csi@pve!csi` holds
  `Datastore.Allocate`, which in PVE opens every volume on the store, so the CSI token can attach the seed's disk to a
  workload VM (accepted for M1 in ADR 0009, documented, measured by lab check L1; a store of the seed's own is a later
  change). With `importStorage` the portal also gets `ProxCloudKubeMint` on that store, as stage 10's `nas_mint_acls`:
  its `PVEDatastoreAdmin` row there replaces what `/storage` passes down, and on a site without a NAS the import store
  is `local`, the ISO store of tenant clusters, so without it every cluster create would fail on the ACL its token
  gets there. The rows (`pve.ManagedACLs`), the privilege tables the portal will check each token against
  (`pve.CAPIPrivileges`, `pve.CSIPrivileges`, `pve.KubeMintPrivileges`) and a check that the CAPI token sees exactly
  one ISO store (`(*pve.Client).ISOVisibilityProblem`, asked of PVE per store as stage 10 does) live next to
  `pve.PlatformACLs`. Tests keep the generated block equal to `docs/proxmox-permissions.md`, the rows and roles equal
  to installer stage 10 (the `proxcloud-seed` rows the only difference, the CSI gateway's rows left out), work out
  each token's effective rights from the rows, and refuse a storage or token id that is not a plain id. The route is
  live, behind no flag: running its answer creates the identities and gives the portal token `ProxCloudKubeMint`
  (`Permissions.Modify`, `User.Modify`) at once. Only the privilege tables and the ISO check are inert: nothing calls
  them until the managed-services runner (later PRs). `docs/proxmox-permissions.md` documents the mint-grant risk the
  owner accepted for M1 (on `/pool`), its reach through `/storage` (a stolen portal token can grant itself
  `Datastore.Allocate` on the boot store and read every disk there, the seed's included; accepted for M1 in ADR 0009)
  and what lab check L1 measures. The script has not been run on a Proxmox node, and L1 has not run.
- **The managed-services platform's bill of materials** (`deploy/platform/platform-bom.yaml`,
  `GET /api/v1/admin/platform/bom`): ADR 0009 says every release ships one list of the exact version and digest of
  everything the platform runs, so that the installer and the console's "Enable managed services" build the same
  thing and an upgrade has one source. The file names release `0.8.0-dev`, Kubernetes 1.35.10 with its published
  node image, the seed image and k3s v1.35.10+k3s1 (binary and install script by sha256), the Cluster API
  providers, the tenant charts and the 15 seed and workload components with wave, namespace, node group and chart.
  The API embeds it (`deploy/embed.go`; the image copies `deploy/embed.go` and `deploy/platform`, and the docker
  workflow rebuilds on a change to either) and validates it in `internal/platformlife`: exact versions only, a
  digest on every chart and image, and `pending` digests (our own four packages, not public yet, and the `kamaji` and
  `kamaji-crds` charts at `0.0.0+latest`, which have no stable digest) and `0.0.0+` versions only in a `-dev`
  release. The route needs `admin:service:read`. Tests keep `versions.yml`,
  `pc_defaults.services.k8s_version` and stage 40's ArgoCD and cert-manager chart and values equal to the BOM; the
  installer itself is unchanged and does not read the file. Inert: nothing installs from the BOM, there is no run,
  job or flag yet. Not verified: the third-party chart and image digests and the k3s sha256 values were read once
  from the public registries, chart indexes and k3s release on 2026-11-15, with no CI job checking them yet; whether
  `proxcloud-platform` 0.1.0, `proxmox-kamaji-cluster` 0.1.11 and `proxcloud-clusterctl` v1.14.4-1 get those
  versions depends on PRs that do not exist yet.
- **Chart `proxcloud-platform` 0.1.0 for the managed-services platform** (`deploy/charts/proxcloud-platform`, ADR
  0009): the platform's own objects as one pinned chart, which ADR 0009 installs through ArgoCD on both provisioning
  paths. Installer stage 50 is unchanged and still applies `deploy/{mgmt,dbaas}-cluster/*.yaml` with `kubectl`. One
  part per component of the platform BOM, which is not in the repository yet. `clusterctl.enabled` (components
  `capi` on the seed and `capi-tenant` on the workload cluster) renders `clusterctl init` as an ArgoCD Sync hook Job
  (`BeforeHookCreation`, cluster-admin service account, `PROXMOX_*` empty, no version check against GitHub) whose
  arguments are the BOM's providers, two per provider (`--core cluster-api:v1.14.4`, …), and whose image,
  `proxcloud-clusterctl`, must be
  pinned by `@sha256:` digest: the chart refuses the BOM's `pending` placeholder and a tag. `platform.enabled`
  (component `platform`) renders on the workload cluster the provider service account, token Secret and RBAC of
  `deploy/mgmt-cluster/rbac.yaml` and `deploy/dbaas-cluster/rbac.yaml` (the Namespace, ServiceAccount, Secret, Role
  and RoleBinding both define, once), a ClusterRole for `ciliumnetworkpolicies` (get, create, patch) that the
  database egress fence needs, both tenant AppProjects, a locked `default` AppProject (no source repository, no
  destination, no cluster-scoped kind), the MetalLB pool `services` with its L2 advertisement, and the tenant chart
  repository entry, OCI and without credentials. Bare defaults do not render. `internal/platformlife/chart_test.go`
  pins the render of each component's example and fails when the RBAC or the tenant AppProjects stop equalling those
  four files, or when the chart renders anything else. `helm.yml` lints and renders the examples on chart PRs and
  publishes the chart on a `proxcloud-platform-v*` tag that matches `Chart.yaml`. Inert: nothing applies the chart,
  the tag is not cut, and the `proxcloud-clusterctl` image and its entrypoint (the argument contract above, run as
  uid 65532) do not exist yet. The hook's outcome counts in neither the Application's sync status nor its health: a
  failed or timed-out Job leaves the `capi` or `capi-tenant` Application Synced and Healthy with only
  `status.operationState.phase` Failed, automated sync does not retry it, and a change to the image or providers
  alone does not make the Application OutOfSync. Whatever applies those Applications must set `syncPolicy.retry`,
  gate on `operationState.phase` Succeeded and on the clusterctl inventory, and request a sync itself when the
  inventory differs; nothing does yet. Not verified on a cluster: the hook running after its service account (sync
  wave 1), and whether the seed's ArgoCD takes over the `default` project the workload's ArgoCD created and shows it
  Synced with empty lists.
- **Docker Compose install: opt-in managed services** (`deploy/compose/`): `./proxcloud-up.sh install
  --managed-services` writes the `.env` key `PLATFORM_ENABLED=true`, which `compose.yaml` passes to the API as
  `PROXCLOUD_PLATFORM_ENABLED` (`false` by default), and `--no-managed-services` writes `false`. The in-platform path
  of ADR 0009 runs its platform job only with this variable and VM template updates both on, and the API reads both
  only when it starts, so a one-host install needs a switch for it. It behaves like `--template-updates` and
  `--scheduler`: a later `install` without the flag and every `upgrade` keep the recorded value, the last lines of
  `install` (also with `--dry-run`) say whether it is on, and the script removes `PLATFORM_ENABLED` from the
  environment it hands to Compose, so a variable exported in the calling shell cannot turn it on. It implies
  `--template-updates`, because the job imports the seed and node images through the template update runner:
  `--managed-services` turns template updates on, `--no-managed-services` leaves them on, and `--no-template-updates`
  is refused while managed services are on. `install` and `upgrade` refuse the switch with a release older than
  v0.8.0, the first with the platform API: an older API ignores the variable, and the switch would read as on while
  nothing can run. `deploy/compose/README.md` (Optional features) says what it needs from the host, a route from the
  API container to the platform network, and what a platform built from Compose lacks: tenant block volumes (no CSI
  gateway listener) and database backups. **Inert today**: no API reads `PROXCLOUD_PLATFORM_ENABLED` yet, so the
  switch only records the value until the release with the platform API. **Not verified**: the switch on a running
  Compose install (the tests run the script with `--dry-run` and against a fake `docker`, and check the model
  `docker compose config` builds), and a platform built from this stack.
- **Node-group placement and an egress fence for the Kubernetes and database providers** (ADR 0009, shared layout). On
  one cluster with node groups, tenant control planes and databases must stay on their own groups whatever their
  charts render, and a tenant with PostgreSQL superuser can run a shell in its database pod and reach the rest of that
  cluster. Two optional provider keys, API only: `config.namespacePlacement` (`nodeSelector`, `tolerations`; kamaji
  and dbaas) stamps the `PodNodeSelector` and `PodTolerationRestriction` namespace annotations
  (`scheduler.alpha.kubernetes.io/node-selector`, `…/defaultTolerations`) and the label `proxcloud.io/platform-role:
  kamaji` or `dbaas` on a `pc-*` or `pc-db-*` namespace, and `config.database.egressFence: true` adds the
  CiliumNetworkPolicy `proxcloud-default-egress` to a `pc-db-*` namespace, so its pods reach only that namespace,
  kube-dns on 53 and the API server on 6443. Both are stamped on a namespace when it is next ensured: a project's
  first attach to the provider, or a new cluster or database create. A namespace ensured before the key was set keeps
  what it has until then (nothing in the sync ensures it again), and pods already running are not moved. A selector
  that is not label keys and values, or a toleration the API server would refuse (it would then refuse every pod in
  the namespace), is refused when the provider is built, and so is the fence together with `config.database.backup`:
  it has no object-store rule yet, and WAL archiving would fail behind it until `pg_wal` filled the volume. A provider
  without the keys applies exactly the namespace and policies it applied before, and nothing writes the keys yet: the
  managed-services run that will set them when it registers the providers comes later and stays off behind its flag
  until the lab run, so this is inert on every existing site, installer sites included. The keys need both admission
  plugins on the cluster's API server, and the fence needs Cilium and `ciliumnetworkpolicies` rights for the provider
  ServiceAccount, which `deploy/dbaas-cluster/rbac.yaml` does not grant (attaching a project and database creates then
  fail with 403). Setting the fence back to false leaves the policies in place. Tested with table tests, a golden of
  the policy and a fake API server. Not run on a cluster: that the API server places the pods, that a database works
  behind the fence while kamaji-etcd, the kubelets, the PVE API and argocd-repo-server stay unreachable, and that
  Cilium matches the API server on its port 6443 after the `kubernetes` Service is translated are all unverified until
  the lab run.
- **Managed-services installations, their credentials, preflight and plan** (`/api/v1/admin/platform/installations`,
  behind the new `PROXCLOUD_PLATFORM_ENABLED`, default off): the console's "Enable managed services" needs a record
  of what it builds before any job can build it (ADR 0009, design §3). An installation (`internal/platformlife`,
  collection `platformInstallation`, one per Proxmox provider) holds the spec of the shared layout (PVE API address,
  boot, ISO and import stores, the untagged platform network, the seed, one cluster with the node groups `system`,
  `kamaji` and `dbaas`, the node image) and the status a run will write. Create and update check every rule of the
  design that needs no cluster: addresses inside the prefix and apart from each other, a node range that holds every
  machine plus one surge per MachineDeployment, SSH keys that parse, no overlap with the provider's tenant pool,
  sandbox or public networks, and a MetalLB range tenants can reach. An update names the `rev` it read and is refused
  during a run, for a change of provider, and for the fields that cannot change once a run succeeded (path, layout,
  name, network, the seed's mode, pool, node and address, cluster name, endpoint and node groups); a draft stays
  correctable. The two platform tokens (`capmox@pve!capi`, `csi@pve!csi`) are pasted write-only
  into collection `platformSecret`, sealed with the data key like the Ceph credentials, refused without one, and
  never returned or put into an audit diff; `kubeconfig`, `seedKubeconfig`, `k3sToken`, `bootstrapToken` and `caKey`
  join the audit's secret keys. The preflight reads only: flags, both tokens' privileges and the portal token's mint
  grant (`pve.CAPIPrivileges` and the other tables of the managed-services script), the fence of both platform pools,
  the ISO store the CAPI token sees, the stores and image placement, the blocklist, and layer 0 through the new
  `(*pve.Service).CheckIsolation` (the sandbox gate's check without its sandbox requirement): the seed, VIP and node
  range must be blocked networks, and it names the CIDRs to add; before the first tenant network, when no sync has
  written the cluster firewall rules yet, the row passes, also on a provider with `sandboxCidrs`. The plan diffs the installed BOM against this
  release's. The model of a run (`platformRun`, its states and the failure codes) is in place for the job. Inert: no
  job kind is registered, so nothing provisions; there are no run, seed-kubeconfig or rotation routes and no page yet,
  and with the flag off the routes answer `404 platform_disabled`. Not verified: the preflight against a real Proxmox
  (tests use a fake PVE API), and whether every privilege row reads the same on PVE 9.2 as the tables assume.
- **`proxcloud-clusterctl` image and its tag flow** (`deploy/clusterctl/`, `.github/workflows/clusterctl.yml`): the
  image the clusterctl Job of the managed-services platform will run (ADR 0009; the `proxcloud-platform` chart that
  runs it comes separately). It holds clusterctl v1.14.4, kubectl v1.35.10 and, as a clusterctl local repository, the
  provider manifests of Cluster API core, kubeadm bootstrap and kubeadm control plane v1.14.4, CAPMOX v0.9.2, the
  in-cluster IPAM provider v1.1.1, the Kamaji control plane v0.22.0 and CAAPH v0.6.4: the versions of
  `installer/playbooks/group_vars/all/versions.yml` (the platform BOM is not on `main` yet), which a test keeps equal.
  Every download is checked against a sha256 in the Dockerfile, and the build resolves every provider with no network.
  clusterctl and kubectl are built in the image build from the upstream source of those two tags (cluster-api: the
  GitHub archive of the tag, sha256 computed from the download; Kubernetes: the release's `kubernetes-src.tar.gz`
  at the sha256 upstream publishes beside it) with Go 1.26.11, as static binaries with upstream's build tags and
  version fields; they are not the upstream release binaries. Upstream releases both with Go 1.26.10; 1.26.11 is the
  patch release that fixes CVE-2026-97054 in the standard library (unrated on 2026-11-15, so the gate did not
  count it). `deploy/clusterctl/patches/` changes only `go.mod`
  and `go.sum`: `golang.org/x/net` v0.62.0 and `golang.org/x/text` v0.43.0 (the version x/net v0.62.0 requires), and
  what module resolution then forces (`x/crypto`, `x/mod`, `x/sync`, `x/sys`, `x/term`, `x/tools`, and the `go`
  directive 1.26.0 in Kubernetes). No Go source is changed. The build runs with `-mod=readonly`, so it fails rather
  than edit `go.mod` or `go.sum`, and every module download is verified against upstream's `go.sum` plus the patch's
  lines; it reads the two module versions back from each binary. The binaries report the upstream versions, the
  tag's commit and commit date, and `gitTreeState: dirty` because `go.mod` and `go.sum` differ from that commit.
  Why: the Trivy gate refuses the upstream release binaries (clusterctl v1.14.4: CVE-2026-78661 in
  `golang.org/x/net` v0.60.0; kubectl v1.35.10: CVE-2026-78661 and CVE-2026-78669 in `golang.org/x/net` v0.56.0 and
  CVE-2026-56851 in `golang.org/x/text` v0.39.0; kubectl v1.36.6 and v1.37.2 have them too),
  and the owner decided to rebuild at the same tags instead of waiting for upstream.
  The entrypoint reads the clusterctl inventory and installs only the missing providers, does nothing when every
  provider is at its wanted version, and refuses, installing nothing, when one is at another version (upgrades are a
  later release) or when it is asked for a provider, type or version it does not carry (clusterctl would otherwise
  take a built-in one such as docker or metal3 from GitHub). It empties `PROXMOX_*` as stage 50 does and turns
  clusterctl's version check off; cert-manager is not baked, so a cluster without one fails on a local path instead of
  downloading it from GitHub. Why: the Job makes no GitHub call at runtime, and the platform pins one reviewed image
  by digest. A pull request builds and scans the image; a `proxcloud-clusterctl-v<version>` tag, which must equal the
  image's version label and point at a commit of `main`, builds it, scans it with the existing Trivy gate and pushes
  it once to `ghcr.io/mutex-solutions/proxcloud-clusterctl`. Nothing uses the image yet. Verified locally on
  2026-11-15: the image builds, two builds without cache on one machine give byte-identical binaries, both
  binaries print their upstream version, and a scan with the gate's Trivy release and that day's database, read by
  the gate's script, finds no HIGH or CRITICAL finding in the image. That is one scan on one day, not a statement about later
  databases. Not verified: the workflow has not run on GitHub, so the build on a hosted runner and the gate there
  are not confirmed; the rebuilt binaries have not been run against any cluster and upstream's test suites were not
  run against the changed modules or the newer Go patch release; GitHub generates the cluster-api source archive on
  request and does not promise stable bytes, so a changed archive fails the build until the sha256 is recomputed.
  The entrypoint ran against k3s v1.35.10 in Docker (empty, partial, equal and different-version inventories, cert-manager down, egress through a dead
  proxy) with the upstream release binaries, before this rebuild, not on a lab seed or workload cluster; the refusal
  of a provider it does not carry ran only against a fake kubectl and clusterctl, under busybox.

- **What the managed-services platform job will apply, rendered from the BOM** (`internal/platformlife`, P3 design
  §4.2 to §5): pure functions, no PVE or Kubernetes call, so each stage of the coming job has exact objects to apply
  and tests to hold them. The seed VM gets a server CA the portal generates (ECDSA P-256, key never stored) and a
  NoCloud CD whose user-data writes that CA before k3s first starts, installs the BOM's k3s after `sha256sum -c` of
  binary and install script, applies a one-shot manifest from tmpfs (the portal's cluster-admin service account and a
  bootstrap token that expires after 3 hours and may `get` only that account's token Secret), then turns cloud-init
  off; meta-data, vendor-data (break-glass SSH keys) and the static network config come from the tenant seed's
  `pve.RenderSeed` and `pve.RenderNetworkConfig`. The workload cluster gets `proxmox-kamaji-cluster` values in kubeadm
  mode with the node groups (group label on every machine, a `NoSchedule` taint on every group but `system`, the
  machine tags a seed-lost Remove finds the VMs by). The seed ArgoCD gets its HelmChart, an AppProject that may source
  only the BOM's charts and deploy only to the seed and the workload cluster (any namespace there: ArgoCD checks every
  object a chart renders, and cert-manager writes leader-election Roles into `kube-system`, cilium into
  `cilium-secrets`), a locked `default` project, OCI repository entries without
  credentials and one Application per BOM component: values inline, secrets named only (`pc-platform-pve`,
  `proxmox-csi-plugin`, rendered separately), automated sync with self-heal, no prune for stateful components, no
  resources finalizer, a retry policy on every Application (the clusterctl hooks can fail), `CreateNamespace=true`
  (capi-tenant syncs into `proxcloud-system` before the platform component creates it), and the namespace placement
  annotations on the one Application per namespace whose pods all go to one group, as a backstop: `kamaji-etcd` (its
  StatefulSet and hook Jobs) and `kamaji` set the `kamaji` group's `nodeSelector` and toleration in their own values,
  since `kamaji-crds`, which owns `kamaji-system`'s metadata, may sync after `kamaji-etcd` in the same wave and
  `PodNodeSelector` acts only on new pods. The workload ArgoCD's values carry the tenant health checks of
  `deploy/{mgmt,dbaas}-cluster/argocd-health.yaml`, which `deploy/embed.go` now embeds (the API image copies both files;
  the docker workflow rebuilds on a change to either). The Kubernetes and Databases provider bodies are stage 60's, with
  the BOM's tenant charts, no node image versions yet, node-group placement, the database egress fence, no backup store,
  PostgreSQL 3 replicas only with three `dbaas` nodes, `attachToNewProjects: false` and the `platformInstallationId`
  marker. Tests: goldens of every renderer, a scan that planted credentials appear only where design §5 puts them, a
  key-set test against stage 60's loop, the providers parsing the bodies, the Applications against the
  `proxcloud-platform` chart's examples and contract, the cluster values rendered through the chart, and every
  Application's chart rendered with its values to check each object's namespace against the project and the placement of
  every pod on the `kamaji` group (upstream charts only from the local Helm cache, so not in CI). Inert: nothing calls
  the renderers; the runner (stages 1 to 11) and the flag are later changes. Not verified: no seed has booted this
  user-data (whether k3s keeps a portal-made `server-ca` and generates the other CAs is lab check L2, as is
  bootstrap-token auth with `anonymous-auth=false`), and no ArgoCD has synced these Applications; the upstream charts
  (cilium, cert-manager, metallb, argo-cd, cloudnative-pg, proxmox-csi-plugin, kamaji-crds, kamaji-etcd, kamaji)
  rendered the workload values once by hand with `helm template` on 2026-11-15, not in CI.
- **The managed-services run, stages 1 to 5, and the seed part of Remove** (`POST /api/v1/admin/platform/installations/{id}/runs`,
  durable job kind `platform.run/v1`, `internal/platformlife/{job,runner,stages}.go`; with
  `PROXCLOUD_PLATFORM_ENABLED` off the route answers `404 platform_disabled` like the other installation routes; with
  only `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` off, `409 platform_disabled`): the installation record needs a job that builds the seed before the platform stages can
  run on it (ADR 0009, design §4). One run per installation at a time; the runner holds a one-minute lease renewed
  every 20 seconds, writes intent and result through `Checkpoint`, gives the job back with `Defer` for waits and
  transient errors (40 per stage, no attempt spent), fails it only on a permanent error, a spent budget or a passed
  deadline, honours Cancel at every claim and write, and ends `portal_changed` when the release no longer embeds the
  run's BOM. Stage 1 runs the preflight; stage 2 imports the seed and node images through Template updates (a finished
  import is reused only while its template is still in `pc-templates`, else `MarkTemplateGone` frees it); stage 3
  clones the seed with `capmox@pve!capi` into `proxcloud-seed` (ours = the receipt's VMID, the pool and the exact name
  `pc-seed-<id>-g<n>`, never the tags), replaces its tags, puts net0 on the platform bridge behind the new VM firewall
  (`pve.AddVMFirewallRule`: 6443, and 22 with SSH keys, from `apiSources` only), uploads and attaches the
  `pcplat-<id>-g<n>.iso` CD and starts it, with a new generation when the CD was lost with its CA key, when the VM of a generation
  that has its CA is gone or no longer ours (a CD fits one clone only), or when a Retry finds a seed that never became
  ready; stage 4 waits for `/readyz` over TLS pinned to the seed's CA (`seed_tls_mismatch`
  otherwise) and tells `seed_not_booted` from `seed_api_unreachable` with the guest agent; stage 5 stores the seed's
  service-account kubeconfig, deletes the bootstrap token and RBAC, deletes the CD and wipes the bootstrap token, done
  only when neither the spec's ISO store nor the one the seed status recorded at upload lists the CD. An apply then ends `stage_unavailable` at stage 6; Remove deletes
  the seed and every `pcplat-<id>-*` CD and wipes `platformSecret`, and refuses while a workload cluster status
  exists. Messages carry fixed text and clipped Proxmox or Kubernetes errors with every secret the run holds
  replaced; a run's job is never replayed through `/admin/durable-jobs`. Not verified: none of it has run against a
  real Proxmox or k3s (tests use a fake PVE API, a fake seed API and fake stores; the run routes, the start lock and
  the replay refusal are tested on PostgreSQL); lab checks L2, L8 and L9 are owed, including whether k3s keeps the
  server CA the CD writes whether PVE adds a clone to its pool only when the clone ends, which stage 3 assumes, and whether detaching the CD
  from the running seed and deleting the ISO it had attached (stage 5) takes effect on a real PVE: an IDE CD removed
  from a running VM may stay a pending change, and only the fake shows the store no longer listing the volume.
- **The managed-services run, stages 6 to 11, the cluster part of Remove, the live read, the break-glass kubeconfig
  and the seed credential rotation** (`internal/platformlife/{cluster,remove,seedaccess}.go`,
  `internal/platform/admin/platform.go`): with the seed ready, a run now builds the platform on it, so an apply no
  longer ends `stage_unavailable` at stage 6 (ADR 0009, design §4.2, §4.6, §5). Stage 6 creates the seed's ArgoCD
  (a k3s HelmChart through the API), its project and repositories and the seed's components wave by wave; stage 7
  writes the CAPI token's Secret and the workload cluster's Application, deletes the MachineDeployments of nodes no
  longer in a group and waits for the control plane, the admin kubeconfig and every Machine Running (`machine_failed`
  names the machine, its VMID and Cluster API's message); stage 8 gives the seed's ArgoCD a `TokenRequest` token of
  `kube-system/argocd-manager` on the workload, written only to the seed's cluster Secret and renewed only under 90
  days or when the account was recreated, and writes the CSI token's Secret; stage 9 applies the workload's
  components, each wave only when the one before is Synced and Healthy against the source the Application has now,
  version and values (20 minutes per wave, counted from the first acquisition that reaches the wave; a wave that stays
  stuck fails `component_unhealthy` at that deadline), waits for every node Ready with its group label and records the proxmox-csi volumes of the platform's own
  claims in `status.volumes`; stage 10 creates or adopts the Kubernetes and Databases providers by `config.provider`
  plus `config.platformInstallationId` (`provider_conflict` otherwise; on an adopted row only `argocd`,
  `namespacePlacement`, `cluster.pveApiUrl`, `database.{egressFence,lbAddressPool,storageClasses}`,
  `database.engines.postgresql.replicas` and the kubeconfig are rewritten, so name, status, versions and the attach
  policy stay the operator's; the list and the insert run under the installation's run lock, and a body the provider
  validation refuses also fails `provider_conflict`); stage 11 writes the components
  and the BOM. The clusterctl components are gated on the clusterctl inventory equal to the BOM, and the run starts
  the Application's sync again when the hook ended with providers missing; a provider at another version fails
  `capi_init_failed`. Stage 7 fails `capmox_auth_failed` when a condition of the ProxmoxCluster or of a
  ProxmoxMachine carries Proxmox's refusal of the token. Stage 6 deletes every `proxcloud-portal-token-g*` Secret but
  the one whose token is stored. Remove now refuses while tenant Applications exist on the workload (`tenants_exist`), sets the
  providers `DISABLED`, reads the platform's volumes again, deletes the Application and then the Cluster, waits up to
  45 minutes for the machines (`machine_delete_stuck`) and deletes exactly the recorded volumes with the CSI token
  before the seed; that part then takes the cluster and the volumes out of the status, so a Remove started again
  after the seed VM was deleted needs no confirmation. `POST …/runs` takes `confirm: "remove <name>"`: with it a remove whose seed VM is gone deletes the
  VMs of `proxcloud-system` tagged `pc-platform-<id>` and the recorded volumes, and a remove whose workload does not
  answer goes on without the tenant check; without it the first fails `seed_lost` and the second is retried.
  `GET …/installations/{id}?live=1` adds the components' sync and health from the seed and a derived `degraded`;
  `GET …/seed-kubeconfig` answers a one-hour cluster-admin kubeconfig with `Cache-Control: no-store`, audits actor
  and expiry only and answers `503` when the API has no audit service;
  `POST …/seed-credential/rotate` creates the next token Secret, checks and stores its token, then deletes the older
  ones, deletes the Secret it created when it stops before the store took the token, and is refused during a run. Not verified: none of it has run against a real k3s, ArgoCD, Cluster API or
  Proxmox (the tests use a fake Kubernetes API for the seed and the workload and a fake PVE API; the PostgreSQL tests ran against a local container). Owed on the lab: that ArgoCD
  reports `status.sync.comparedTo.source` equal to `spec.source` once it compared the applied spec, and takes an `operation` patched onto an Application the
  way the gate assumes, the clusterctl inventory's `type`/`providerName`/`version` values, the Cluster and Machine
  status fields of the Cluster API release in the BOM, the name `argocd-repo-server` under the k3s HelmChart,
  the condition messages CAPMOX sets when Proxmox refuses its token, the
  proxmox-csi volume handle form `region/node/storage/volume`, and that the CSI token may list and delete on the
  boot store. Not in this change: deleting the seed's other Applications before the seed (the seed VM is deleted
  with them) and the `seed.mode: existing` path.

- **CI: a chart mirror flow** (`charts-mirror.yml`, `.github/scripts/charts_mirror.py`,
  `deploy/platform/charts-mirror.yaml`). The manifest lists the nine third-party charts of the platform BOM; a test
  keeps it equal to the BOM. A pull request fetches every chart as pinned (sha256 of the archive against the index
  digest, or a pull by OCI digest), packages `kamaji` and `kamaji-crds` from the upstream tag `26.11.2-edge` at that
  exact chart version with upstream's `LICENSE` and `NOTICE` and a statement of the change added (in `NOTICE` and as
  an annotation in `Chart.yaml`), and pushes nothing.
  A `charts-mirror-v*` tag on a commit of `main` publishes them under `ghcr.io/mutex-solutions/charts-mirror`. The
  script pushes only a version the mirror definitely does not serve: `denied` or any answer other than Helm's "not
  found" for exactly that chart and version fails the run before the first push, and a run that fails or is killed
  later leaves a record of the digests it got. That is a rule
  of the script; GHCR itself moves a tag on push, and who can publish is decided by repository settings, listed as
  owner actions. Nothing is published yet and the BOM still takes the charts from upstream. Next steps: the owner
  settings, the tag, making the packages public, and a BOM change that moves the installer templates and the tests
  tied to the BOM in the same pull request (the list is in `docs/ci-release.md`, "Chart mirror", "Order of use"). Not verified: any push, which has only run against a fake `helm`, and how GHCR answers a logged-in
  token for a package that does not exist yet.

### Changed

- **The platform BOM takes every third-party chart from the chart mirror, and nothing in it is pending**
  (`deploy/platform/platform-bom.yaml`, `docs/ci-release.md` "Chart mirror"). The nine third-party charts (argo-cd,
  cert-manager, cilium, metallb, cloudnative-pg, proxmox-csi-plugin, kamaji-etcd, kamaji, kamaji-crds) now come from
  `ghcr.io/mutex-solutions/charts-mirror` with the manifest digests of the `charts-mirror-v1` run, and
  `proxcloud-platform` 0.1.1 has its digest. `kamaji` and `kamaji-crds` are version `26.11.2-edge` instead of
  `0.0.0+latest`, and the `kamaji` component no longer sets `image.tag` (the chart's `appVersion` is that tag; same
  render). The BOM validation never accepts `pending` for a chart of the mirror, which is third-party content in our
  registry. The installer's stage 40 takes ArgoCD and cert-manager from the mirror as well (cert-manager as an OCI
  source). The BOM stays `0.8.0-dev`; dropping `-dev` is now the only change the release gate needs. A platform
  installed from the previous `-dev` BOM is refused with `bom_unsupported`, as for any other BOM change.

- **The platform BOM pins our four published packages by digest, and CI checks every pin**
  (`deploy/platform/platform-bom.yaml`): the charts `proxmox-kamaji-cluster` 0.1.11, `database-cluster` 0.5.1 and
  `proxcloud-platform` 0.1.0 and the image `proxcloud-clusterctl:v1.14.4-1` held `pending`; they now hold the digests
  ghcr.io serves. New `.github/scripts/bom_verify.py` (standard library, anonymous pulls) compares each pin with its
  registry: the manifest digest of an OCI chart, image or VM image, the `digest` of the exact chart version in an
  `https://` repository's `index.yaml`, and the sha256 of the downloaded k3s binary and install script. It runs on
  pull requests that touch the BOM or the script and weekly (`bom-verify.yml`), and with `--release` on every `v*`
  tag as a job the image builds and the GitHub Release wait for (`docker.yml`) and as a step before the app chart is
  pushed (`helm.yml`): a tag whose BOM is still `-dev`, holds a `pending` digest or a `0.0.0+` version, or pins a
  digest that has moved, publishes no image and no `proxcloud` chart. The BOM stays
  `0.8.0-dev`: the `kamaji` and `kamaji-crds` charts are still `0.0.0+latest` with a `pending` digest, so `v0.8.0`
  cannot be tagged until the chart mirror pins them. Not checked: the bytes of OCI layers and of chart archives from
  `https://` repositories (only the digest the registry or index states) and signatures. The two workflows run
  the gate separately, so a registry error can fail one and not the other.

- **Installer stage 10 narrows the services cluster's CAPMOX and CSI tokens** (`split_acls` in
  `installer/playbooks/10-pve-identity.yml`): `capmox@pve!capi` held `ProxCloudCAPI` on all of `/pool` and
  `csi@pve!csi` `ProxCloudCSI` on all of `/pool` and `/storage`, although each only works on the services cluster.
  They now hold `/pool/<services.pool>` (and the CSI token `/storage/<services.storage>`), and both users get a row of
  their own on the services pool, the same rows as the console's managed-services script. The users keep `/pool` and
  `/storage` for the tenant clusters' tokens and the CSI gateway. Stage 10 only adds rows, so a site installed before
  keeps its broad token rows until an operator removes them (`docs/proxmox-permissions.md`, "An existing site"); the
  added rows grant nothing new there. One loop item of the stage is now quoted so that strict YAML parsers read the
  file, as a Go test now does. Checked with the tests above and `ansible-playbook --syntax-check`; not run against a
  cluster.
- **Template updates: the import placement check and a way to re-import a deleted template live in
  `internal/templateupdates`** (P3 groundwork, design §11 PR 11). The managed-services job will import its
  two images without the admin handler, so the node/storage check it needs (`PlacementOptions`,
  `PlacementAllowed`) moved out of `POST /admin/template-updates/vm` as pure functions; the handler calls
  them and answers as before. A finished import blocked every re-import of that artifact on that node,
  even after its template VM was deleted, which would stop the job's image stage for good. NEW
  `Service.MarkTemplateGone` reads the receipt's provider and marks the receipt `templateGone` only on proof
  that the template was deleted: `GET /cluster/nextid?vmid=`, which sees every guest, says its VMID is
  free; or the token sees the VM now holding it, and that VM has a name, is on a node that is up and has
  neither the template's name nor its source tag; or it is a later import of the same artifact recorded at
  that node and VMID (every import takes the lowest free VMID, so a copy on another node can take a deleted
  template's VMID). A read error, a down node, a held VMID the token cannot see (a template moved out of
  `pc-templates`, or lost pool rights) or a template that was renamed or migrated changes nothing.
  `EnqueueVM` then treats that receipt as failed, and the page shows it as `template_missing`. Inert:
  nothing calls `MarkTemplateGone` yet (no route; the managed-services runner, still to come and off behind
  its flag, will). Tested with unit tests and the opt-in PostgreSQL tests (`TEMPLATEUPDATES_TEST_POSTGRES`)
  against a throwaway database; not run against a Proxmox cluster, so nextid's answer for a held VMID is
  taken from the PVE source and `images/vm_export/coldboot.py`, not measured.

### Fixed

- **The provider sync retires the rows of servers and volumes that are gone, on the documented Proxmox grants
  (F60)** (`internal/cloud/pve/vm_inventory.go`, `docs/vm-inventory-safety.md`, `docs/proxmox-permissions.md`).
  Before a cached SERVER or VOLUME row was retired, the sync asked the provider's token to show that it sees every
  guest of the cluster: `Sys.Audit` on `/access`, propagated `VM.Audit` on `/vms`, `/pool` and every ACL path under
  them. The documented grants have none of that by design (no ACL on `/vms` or `/access`; `NoAccess` on the fenced
  pools of managed services), so the proof failed at every sync on every such install: the log read `provider
  visibility unverified; cached inventory preserved`, no row was ever retired, and the project's server list was
  not updated at all. On the lab the worker of a deleted cluster stayed ACTIVE in the tenant's list and counted in
  the project while its VMID already belonged to another machine. The proof is now what it was after: the VM of
  the row is gone. A row whose VM is no longer in its project pool is retired when `GET /cluster/nextid?vmid=`
  says no guest in the cluster holds the VMID (Proxmox answers that from its whole guest list, whatever the token
  may audit), or when the token reads the VM that holds it in another pool and its SMBIOS UUID is not the row's
  (Proxmox hands out the lowest free VMID, so a VM of another project takes the number). A row whose VMID now
  belongs to another VM of the same pool is replaced from the VM the token read. The guard against inventory loss
  is kept and no longer depends on an ACL: a VMID held by a guest the token cannot see (a VM moved out of the
  project pool, hidden by an ACL, in a pool the token has `NoAccess` on) keeps the row, and so does one the token
  sees elsewhere with the row's UUID. The warning now carries the cause and names the path (`/pool/<pool>` when
  the token lost `VM.Audit` on the project pool, else `/vms/<id>`). No grant changes; do not add a `/vms` ACL.
  Known limit: when a guest the token cannot see takes the VMID of a deleted VM before the next sync (on a
  managed-services install, a machine of the platform), the token cannot tell it from its own VM, the row stays
  and that project's sync stops at it until the guest is gone or the server is deleted through the API (traced
  in the code for a plain server and for a cluster worker's row, which takes the same route; not run). For that
  case the warning no longer ends with "repair token permissions": the error carries the cause and the guide
  says there is no grant to repair. `Client.VMIDFree` now counts as free only a 2xx whose `data` is the id that
  was asked; null, an empty body or another value is an error and keeps the row (it was "any 2xx", and it is the
  only evidence of absence here; the template updates' check before an import is removed uses it too). The
  automatic release of a retired server's public address still asks for the view of every guest and so still never
  happens on these grants (unchanged; `docs/vm-inventory-safety.md`). Integration tests against a fake Proxmox API
  that answers as the documented token is answered; not run on a lab.

- **Publish on all nodes says when it is blocked (O3)** (`internal/templateupdates/publishall.go`,
  `docs/template-updates.md`, `docs/api.md`). A copy whose smoke test had failed, or whose import or publish had
  failed, ended the action with `next: done` and `complete: false`: an advance changed nothing and the dialog read
  "Nothing more to do". On the lab two copies with an eight-hour-old failed smoke test, whose cause was long fixed,
  stopped it that way. `next` is now `blocked` in that state, the `smoke_failed` node's `message` says to run the
  copy's smoke test again from its row under Imported templates, and the dialog says "Blocked" and goes on by
  itself once the test has passed. Nothing is retried automatically; a refused publish (`publish_failed`) is
  tried again by the next post, a smoke test or an import never is. Clients that only know `done` must treat
  `blocked` as "stop posting". Tests on PostgreSQL with a fake Proxmox; not run on a lab, the dialog not opened.

- **Guides corrected from lab round C (2026-11-17, main cb2f052).**
  `docs/managed-k8s.md` and `docs/proxmox-permissions.md` now say how long a deleted cluster's Proxmox token lives
  (F57): the sync's sweep revokes it, not the delete, because CAPMOX deletes the workers with it; with the
  scheduler that is up to one 15-minute sync after the cascade, and for a cluster deleted in its first 30 minutes
  the first sync after its marker is 30 minutes old (the guard for a create that has not applied its Application
  yet counts from the creation); without the scheduler the token and its ACL rows stay until an operator starts a
  provider sync. The code is unchanged: the delete answers before the cascade runs and nothing but the sweep sees
  it finish. They also say that Proxmox prints `ignore invalid acl token` on every CLI call after a revoke until
  `user.cfg` is next written, which is harmless (O1). `docs/api.md` and `docs/managed-k8s.md` say that a stalled
  worker is reported on `node_groups[].progress.attention` and never on `cluster.attention`, that a client reads
  both, and that attention is evaluated at a provider sync, so up to one interval late (O5). `docs/managed-k8s.md`
  says that a create or a scale-up clones all new workers at the same time and what that did to other VMs on slow
  storage. `docs/managed-services.md`: the seed restore sequence and the clean-stop release were both run on the
  lab and passed, so the guide no longer says they were not; it states what was run (a healthy seed restored over
  itself from a three-minute-old zstd archive; a restart 33 ms into stage 8 of a re-check apply) and what was not
  (an archive older than a credential rotation or a machine change, `qm stop` on a dead guest, `--unique`, a boot
  store other than ZFS, the scheduled backup job and its pruning, a stop with a slow call in flight). The restore
  steps gain a pool membership check, the `vmgenid` line that changes, the times to expect, what keeps working,
  the saved firewall copy to delete, and the audit-log check's blind spot for a rotation made before the
  `ROTATE_CREDENTIAL` entry existed. The backup section cites the measured zstd backup (2.39 GiB in 1 minute 45
  seconds) and warns, where it tells the operator to back the seed up, that the archive holds the seed's secrets
  and that `vzdump` wrote it with mode 644 on the lab's shared export.

- **Placement counts the memory of VMs the provider's token cannot see (F58)** (`internal/cloud/pve/scheduler.go`
  `CommittedMem`, `docs/managed-k8s.md` section 9). The node choice for a server and for Kubernetes workers summed
  the configured memory of the VMs `GET /cluster/resources` returns, and Proxmox leaves out every VM the token may
  not audit. On a managed-services install those are the platform's own machines (the token has `NoAccess` on
  `proxcloud-seed` and `proxcloud-system` by design), so their nodes read as free: on the lab a worker went to the
  fullest node and a refusal said "2 of 8" 8 GiB workers fit where none did. Memory taken on a node is now the
  larger of the configured memory of its running visible VMs and the node's own used memory (`mem` of `GET /nodes`,
  already read, `PVEAuditor` on `/nodes`, no new permission), plus the configured memory of every visible VM that
  does not run (stopped, or cloned and not booted), the workers decided and not built yet and the picks of the
  request. The scale-down order reads the same figure. Running workers do not move.
  **This changes the free memory of every site, not only managed-services installs:** wherever a node's used
  memory is above the configured memory of its running visible VMs (the host itself, the ZFS ARC, LXC containers,
  VMs of another owner), the node now reads less free than before. A 64 GiB ZFS node with a 32 GiB ARC and 16 GiB
  of running VMs read 48 GiB free and now reads about 30, so a server create, a failover target choice or a worker
  placement that passed can answer `409`. The figure is never below what was computed before. Operator's remedy
  for the ARC: cap it (`zfs_arc_max`). Known limits, in the guide: a hidden VM counts with what it uses now, not
  its configured size, and its vCPUs are not counted (the installation record is not read by the placement); and
  on a node with hidden VMs a running visible VM that uses less than its configured memory counts with what it
  uses, so several creates in a row can still overfill such a node once the earlier VMs have booted. The guide no
  longer says every VM's configured size is counted. Unit tests only, one of them against a fake Proxmox API; not
  run on a lab.

- **`SET_NODE_GROUPS` takes a node group without `rootVolumeGiB` when the provider has a default (F59)**
  (`internal/platform/project/cloud_kamaji.go`). The shape check ran with empty provider defaults, so it answered
  `400 rootVolumeGiB is required (no provider default configured)` for a group the create accepts, before the
  default was ever filled in. It now gets the provider's default root disk size. Without a default it still
  answers 400. Because that request now passes, a group that already runs and is sent without `rootVolumeGiB` keeps
  the root disk size it has (`kamajiRootGiB`, from the cached cluster row) instead of falling back to the default:
  another size would change the machine spec and replace the group's workers. Unit tests; not run on a lab, and
  the worker replacement itself was not observed.

- **Managed Kubernetes guide: which worker a scale-down removes (O4)** (`docs/managed-k8s.md` section 9).
  ProxCloud decides how many replicas each node loses; which machine of that node goes is Cluster API's choice,
  and on the lab it was the older one. The guide said so nowhere. The chart is unchanged.

- **Seed backup and restore, findings of the lab run (F52 to F55)** (`docs/managed-services.md`, the Managed services
  page, ADR 0009 A7, `internal/platformlife/runner.go`). The guide said only "restore the seed VM from your backup",
  and Proxmox refuses a restore over the protected seed: it now gives the sequence (shut down, protection off,
  `qmrestore` over the same VMID without `--unique`, check protection, MAC and firewall file, start, apply). The
  restore itself has not been run on a lab, only the refusal was seen. The printed backup job gets
  `--compress zstd --prune-backups keep-daily=7` (it wrote one uncompressed archive a day and removed none). The
  advice on the backup store is now "no API token other than the operator's may read the archive", with the checks
  for `Datastore.Allocate` and for `Datastore.AllocateSpace` with `VM.Backup`, and says that on a file store the
  archive is also a readable file. ADR 0009 A7 no longer says the console shows attention until a backup job exists.
  An API that stops cleanly during a platform run gives the job back at once, so the next start resumes without
  waiting out the one-minute lease and without spending an attempt (covered by a test, not by a lab run).

- **Seed restore and clean stop, review findings** (`docs/managed-services.md`, the Managed services page,
  `cmd/api/main.go`). The guide now says that a seed backup taken before **Replace the seed credential** cannot be
  restored from (the restored seed refuses the stored credential and none can be made; read from the code, not tried
  on a lab): back up by hand after every rotation and take only an archive newer than the last `ROTATE_CREDENTIAL`
  entry. The restore sequence keeps a copy of the seed's firewall file and puts it back when the restore lost it. The
  check of who can read the archive goes through every token of every user, since ACL rows on pools, users and
  groups also give rights. The API cancels a platform run at the stop signal, before its servers drain, so the job
  is given back within the container's stop grace period more often; the guide names that condition. Not run on a
  lab: the restore, and the clean stop.

- **Findings of the managed-services lab run (F42 to F51).** A project's "Delete now" is refused with a `409` while
  the job scheduler is off, instead of leaving the project `DELETE_IN_PROGRESS` for good with an organization that can
  not be deleted (the teardown is a scheduler job; `deploy/compose/README.md`). A tenant network in a VLAN zone is
  written and answered without DHCP and SNAT, which Proxmox does not do there (also one made before this release), and asking for either is a `400`; the
  guides say that the site's router is its gateway. The template smoke test sets `ciupgrade=0` on its clone, so it no
  longer needs a package mirror on the smoke network; keeps cloud-init's own error in the record when it fails; and
  names a disk a destroy task left behind in a warning (it is freed only where the portal token holds
  `Datastore.Allocate` on the store, which the documented rights do not include; also when the attempt that started
  the destroy did not see it end). The images a platform run imports
  are no longer smoke-tested automatically beside the build (start the test by hand before a Publish). The seed ISO of
  a live server no longer logs `seed artifact cleanup pending` at every sync: the store's identity is read from the
  storage list the portal token may read, and an attached seed is not a cleanup candidate. A finished Remove keeps the
  VMID of each machine it deleted, and the live read no longer says "the record still says ready" of a failed
  installation. Guides: the seed's read-only root is repaired by the documented orphan-file sequence, not a plain
  `e2fsck -f` (`docs/managed-services.md`, run on a lab), and `docs/managed-dbaas.md` says what `allowedCidrs` can
  isolate when tenant networks use SNAT (a node's address admits every tenant behind it: the VM's node in a `simple`
  zone, the exit node in an EVPN zone).

- **Defect in the published Ubuntu 26.04 VM images: the root filesystem can go read-only at a reboot.** Every
  `proxcloud-vm-ubuntu-2604` and `proxcloud-vm-ubuntu-2604-k8s` image exported before this fix has an ext4
  superblock that names inode 12 as the orphan file while the `orphan_file` feature is off; once a VM stores a file
  in inode 12, Linux 6.16 and newer answer its lookup with `EFSCORRUPTED` after the next reboot (measured on a lab
  seed VM). The export sanitizer removed only the journal and left e2fsck to drop the feature, which keeps the
  field; it now removes `orphan_file` with the journal (`images/vm_sanitize.py`). The sanitize stage and the
  read-only postscan of the final export refuse any ext4 partition with such a superblock or with an unclean
  `e2fsck -fn`. The ten tags published before the fix (listed in `docs/public-vm-sanitization.md`) stay in the
  registry and stay affected. The four images were exported again on 2026-11-16 from commit `72f0eaa`:
  `ubuntu-2604-os-20261116170652-72f0eaa3d0d4`,
  `ubuntu-2604-k8s-1.35.10-20261116181221-72f0eaa3d0d4`,
  `ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4` and
  `ubuntu-2604-k8s-1.37.2-20261116210716-72f0eaa3d0d4`; on each published disk the field is 0 and
  `e2fsck -fn` is clean. `deploy/platform/platform-bom.yaml` now pins the first two as seed and Kubernetes node
  image. On an existing portal: import the fixed tag, publish it, then withdraw the affected digests; VMs already
  cloned from an affected template still need the test and repair. A managed-services platform installed from
  the earlier BOM runs on affected images and is not upgraded by this one (`bom_unsupported`): repair its VMs,
  or remove it and install again. Ubuntu 24.04 images are not affected. A VM that has logged the
  error must not be rebooted: its boot-time `e2fsck -p` can clear the file in inode 12 and stop in the initramfs
  (measured on a scratch image only); shut it down and repair it offline. Test and repair:
  `docs/template-updates.md`.

- **Managed services: what the review of the L7, L11 and seed-reboot fixes found.** A Kubernetes node template that
  still has its Proxmox cloud-init drive on another slot than `ide0` (imported before the drive moved) booted and
  passed its smoke test, so it could be published to tenant clusters, whose machines then never join: the smoke test
  now fails it `template_mismatch` before it clones, with the `qm set <vmid> --delete <slot>` command; a version
  already published from such a template stays published and the template must be corrected by hand. The check row
  `tenant-network` reads the provider's default zone in Proxmox instead of its configured kind (a `simple` zone made
  without DHCP warned, a live zone with DHCP under another kind read `ok`), no longer says every create is refused,
  and names the provider with shared workers, which is not affected. An `UPGRADE` of a tenant cluster on a provider
  with no version is refused (`version is not offered by this provider`); it passed any next-minor version and kept
  the old node image. A seed credential rotation that answered `502` after the new credential was stored writes its
  `ROTATE_CREDENTIAL` entry with outcome `FAILURE`. `live.error` names the guide's section by its title, "When the
  seed does not answer". A negative autoscale storage ceiling answers `maxStorageGiB must be a whole number, at least
  0` where it said `0..0`. Docs: the AppArmor sysctl of Ubuntu 26.04 node images is cited to containerd issue 14485
  and pull request 14487 (merged to main on 2026-11-16, in no release yet, 2.3 backport 14590), Launchpad 2172640
  stays the reference for the symptom only, and the kernel change is said to be intentional; the recovery section
  says that a failed apply sets the installation to `failed` and hides the seed's live read until a Retry passes;
  the link to the smoke test section resolves. Unit-tested; nothing of it was run on a lab.

- **Managed services: findings of the first end-to-end, credential and seed-reboot lab checks** (L7, L11, L10).
  A node template without a Proxmox cloud-init drive, which is what stage 7 tells an operator to make of a template
  with the drive on another slot than `ide0`, could never pass its smoke test (`cloudinit_failed`, cloud-init
  `disabled`), so its Kubernetes version could never be published: the smoke test now gives its own clone the
  drive when it has none, in the slot an import of that variant uses, and stage 7's message says the template
  needs none. A cluster create on a Kubernetes provider that offers no version is refused before anything is
  reserved or minted (it passed the version check with any version). The platform check has a row
  `tenant-network` that warns, without failing, when the provider's default zone is a `simple` zone, on which
  every tenant cluster create answers 400; the page no longer says "Every check passed" over a warning. The
  database size refusals say `cpu must be a whole number, at least 1` where they said `cpu must be 1..1` for a
  provider without a maximum. A seed credential rotation writes its own audit entry (`ROTATE_CREDENTIAL`, the two
  Secret names, never a token); it read `CREATE`. A ready installation whose seed cannot be read says which of
  the two it is (the API does not answer, or it refuses the credential) with the seed's address, VMID and node
  and `live.code: seed_api_unreachable`, and the guide has a section for it: what keeps running, what Apply,
  Retry and Remove do meanwhile, what to check, the filesystem repair (not run on a lab) and the path without a
  backup. The guide also says that the old seed token works for some seconds after a rotation and that a
  break-glass kubeconfig outlives one. Kubernetes node images of Ubuntu 26.04 are built with
  `kernel.apparmor_restrict_unprivileged_unconfined = 0`: with Ubuntu's default a container's processes cannot
  signal each other on kernels from 7.0.0-42 (Launchpad 2172640) and no PostgreSQL database comes up; the
  export's cold-boot check refuses a Kubernetes image without it. The node images with the setting are published
  (`ubuntu-2604-k8s-1.35.10-20261116181221-72f0eaa3d0d4`, and the 1.36.6 and 1.37.2 images of the same day), and the
  BOM names the 1.35.10 one; the file was read from each published disk, and no platform was installed from
  them on a lab yet. The seed image keeps Ubuntu's default for this setting. The seed's filesystem error after
  a reboot is the orphan-file defect of the entry above.

- **Managed services: a seed that restarts no longer ends the run, and what the first finished lab run found**
  (M0, attempt 5). The seed's k3s restarted under the disk load of stage 7's clones; for some seconds its API
  answered `403 … RBAC: [clusterrole … "cluster-admin" not found …]`, which the run took for a refusal and ended
  `seed_api_unreachable` with nothing wrong. A starting API server also answers a bare `401`, a `403` without that
  list and an admission plugin's `403 … not yet ready to handle request`, and no text (nor `/readyz`, which answers
  an unknown token `401` too) tells them from a real refusal. So every Kubernetes `401` and `403` from the seed or
  the platform cluster is now a passing error at first: counted, tried again, and final at the fifth look on end
  that is refused (3 min 45 s or more; the stage's new field `refused` counts them), with the same code and the
  server's text. A revoked token, a deleted role or binding and an expired bootstrap token in stage 5 still fail the
  run, after those looks instead of at once, and never only at a stage's time limit. Stage 5 waits for a service
  account not yet reported as cluster-admin until its time limit. Reporting: a
  run ended before its stages (`portal_changed`) sets the stage it stood at `failed` and logs that stage, not
  `validate`; a Remove lists what it deleted, left and did not check (`tenants`, each machine, `cluster`, volumes,
  the seed VM, the secrets), and the list stays on the finished run; a machine without a VM reads `no VM`, not
  `VM 0`; a node template that passed stage 7's check has an item `ok`; a finished Application's item no longer
  carries the text of an earlier failed attempt. Cluster chart `proxmox-kamaji-cluster` 0.1.12 (kubeadm mode only;
  tenant output unchanged; the installer's `services-cluster.yml.j2` moves with it): the control plane's kubelet
  gets the machine's own address as `node-ip`, written by the prepare script, where the node registered the kube-vip
  address as its InternalIP; kube-vip's lease is 60 s with a renew deadline of 40 s and a retry period of 5 s
  (`vip_leaseduration`, `vip_renewdeadline`, `vip_retryperiod`; were 15, 10, 2), where an etcd write of 19.5 s made
  it give up the API address five times. kube-vip's client still cuts each request at 10 s, so the 40 s buy more
  tries, not one write of 40 s: a stall of about half a minute is survived. The chart is published
  (tag `proxmox-kamaji-cluster-v0.1.12`) and the BOM pins 0.1.12 by digest, for the platform cluster and for new
  tenant clusters (`tenantCharts.kubernetes`; the installer already registered providers with the source version),
  so a new managed-services install gets the chart change. A platform installed from the previous BOM does not: the
  BOM is recorded by content, an apply on it is refused with `bom_unsupported`, and the way to 0.1.12 is Remove and
  a new install. On an installer-built site **a re-run of stage 50 on an existing split-mode site replaces the
  services cluster's control plane machine** (Cluster API rolls a changed kubeadm configuration; one spare address
  and room for a second control plane VM are needed, and the API address can be away for up to a minute, see
  `docs/install.md`). Neither has run on a cluster. A Remove whose seed VM is gone and that has no cluster recorded
  no longer says that an earlier Remove deleted one; the console's advice for `seed_api_unreachable` and
  `workload_api_unreachable` covers a refused credential. The guide says that a
  plain Remove's API body is `{"kind":"remove"}` and `confirm` is the option's field, and names the disk load of
  stage 7's parallel clones.

- **Managed services: stage 7 could not see a machine that failed, a sync hook that failed passed for done, and a
  Remove could wait 45 minutes or leave things behind** (static audit and the third lab attempt, before the first run
  reaches stage 7). Stage 7 read a Machine's `Failed` phase and failure message, which Cluster API v1.14 no longer
  sets and CAPMOX v0.9.2 never fills: every provisioning error waited out the 60 minutes as `control_plane_timeout`.
  It now reads the ProxmoxMachine's condition `VirtualMachineProvisioned`: the items and the waiting message carry the
  step and CAPMOX's error, `VMProvisionFailed` and `TaskFailed` fail the stage `machine_failed` with the command that
  makes Cluster API build the machine again, and a ProxmoxCluster that CAPMOX has set no condition on for 10 minutes
  fails `capmox_auth_failed` naming the Proxmox API URL and CAPMOX's log. Stage 7 also refuses a node template whose
  Proxmox cloud-init drive is not on `ide0`, and template updates now puts that drive on `ide0` for a Kubernetes node
  image: CAPMOX writes its own ISO to `ide0` only, and a machine with two `cidata` media starts from the wrong one
  (templates imported earlier need `qm set <vmid> --delete ide2`, [template-updates.md](docs/template-updates.md)).
  An Application now counts as done only when its last sync operation ended `Succeeded`: ArgoCD shows Synced and
  Healthy while a sync hook still fails, which let `capi` look done on the lab while its hook failed. The item and the
  stage's message name the failed hook with its message, the providers clusterctl has not installed are named with
  their type, and a sync that fails again after the run started it once fails the stage instead of being started a
  third time. Remove no longer retries the tenant check on a cluster that never could hold a tenant (no control
  plane), looks at the seed once more after deleting the Application `pc-platform` before it
  calls the cluster gone, deletes a tagged VM's bootstrap ISO with it when the seed is gone, lists the untagged
  `pc-platform-…` VMs it left, and says when it could not look for volumes. The providers a Remove disabled are
  enabled again by the next install; one the operator disabled stays disabled. Stage 9 records the platform's volumes
  at every look, not only after `kamaji-etcd`'s wave. Texts: stage 4 no longer says the seed "does not answer" for an
  API that answers 401 while it starts; the guide says that the seed kubeconfig route answers JSON, lists the site
  prerequisites no check reads (the seed's route to the Proxmox API, memory for CAPMOX's scheduler, one layer-2
  segment, clone space, disk latency, MTU, the region), and states what a BOM recorded by the file's bytes means for
  an install on a `-dev` build. Not verified on a lab.

- **Managed services: review of the stage 7 and Remove fixes above.** A machine whose VM runs but never joins showed
  nothing for the 60 minutes, because CAPMOX calls it provisioned: its item and the waiting message now carry the
  Machine's `Ready` condition, and the message the KubeadmControlPlane's `Available`; the Application `pc-platform`
  is named last and only while its sync is not done. A Retry after `capmox_auth_failed` could fail the same way with
  the cause fixed, since CAPMOX tries a ProxmoxCluster it could not open only every up to 17 minutes: the run now
  updates an annotation on it once, which makes CAPMOX look at once. A sync that keeps failing no longer waits out
  stage 6 or a wave: after four failed attempts of one operation, the last in this run, the stage fails with the hook
  or resource ArgoCD names. The `machine_failed` message no longer tells the operator to delete the Machine when
  CAPMOX's text is the VM name mismatch, where that deletes another VM. A node template the portal token may not
  read is listed as not checked instead of failing the stage. Remove takes only a control plane that never came up
  for a cluster without tenants, not a missing provider record: stage 10 can leave a public provider row without
  recording it. Not verified on a lab.

- **Managed services: the seed VM could not be built from a template made by template updates (second lab attempt,
  M0).** Such a template has no network device, so Proxmox gives the seed's `net0` its MAC address when stage 3 sets
  it; the stage rendered the seed's CD from the value it had sent, without the address, and failed
  `seed_config_refused` ("net0 has no MAC address yet") on every Retry. It now reads the VM's config back and the CD
  names the address the VM has; the CD is rendered and built before the generation's CA and tokens are stored, so a
  CD that cannot be made stores nothing. Found by reading on from there, not yet seen on a lab: Proxmox removes no
  drive from a protected VM, so the stage now protects the seed in the call that attaches the CD (after the
  template's cloud-init drive is dropped), and stage 5 ejects the CD from the running seed instead of removing its
  drive; a disk size Proxmox writes in bytes is no longer read as GiB. Stage 2 no longer fails
  `image_import_failed` when an import has just finished and Proxmox does not list its template yet (template
  updates answers `template_present`): it looks again and shows `checking` with the VM it waits for, for 5 minutes
  after the import's last step; after that a VM that is still no template in `pc-templates` fails the stage at once
  with the refusal that names it. Stage 5 reads the seed's config in effect after the eject: an eject Proxmox took
  but left pending keeps the CD's file and the stage waiting (`seed_cd_not_deleted`), where the CD used to be
  deleted and called gone. A failed run is logged at ERROR (`platform run failed`, with the stage,
  the code and the redacted message), no longer at INFO. The guide, the in-app page and the console say what
  `seed_config_refused` can still mean.
- **Managed services: four defects of stages 6 to 9, found by reading the code against the pinned charts and
  ArgoCD before the next lab attempt (none of them has run on a lab).** The `proxcloud-platform` chart rendered each
  Cluster API provider of the clusterctl Sync hook as one argument (`--core=cluster-api:v1.14.4`); the image's
  entrypoint reads two (`--core cluster-api:v1.14.4`) and left with its usage and exit code 2, so no provider was
  ever installed and stage `seed-platform` ended in `capi_init_failed` at its time limit. Chart 0.1.1 renders two
  arguments; the BOM names it for `capi`, `capi-tenant` and `platform` with a `pending` digest until tag
  `proxcloud-platform-v0.1.1` publishes it, and a test feeds the rendered arguments to the entrypoint. The `kamaji`
  Application sets `helm.skipCrds`: the chart ships the three CRDs that `kamaji-crds` renders, and ArgoCD, unlike
  `helm install`, rendered them in both Applications, which then took the CRDs from each other at every self-heal.
  A run now starts the sync of an Application whose last sync ended `Failed` or `Error` (ArgoCD does not sync the
  same revision again once its ten retries are spent, so a **Retry** after the cause was fixed waited out another
  time limit), with the Application's own retry policy, for every component and for `pc-platform` in stage
  `cluster`. Both ArgoCDs, and the installer's, set `applicationSet.replicas: 0`: the argo-cd chart has no
  `applicationSet.enabled`, the key was ignored and the ApplicationSet controller ran.
  On a site the installer already built, the next run of stages 40 and 50 therefore scales
  `argocd-applicationset-controller` to 0 on the management and the services cluster, where it runs today: the
  product uses no ApplicationSet, but a site that added its own there must set the replicas back.

- **Managed services: the defects of the first lab attempt (M0, a Proxmox cluster shared by two portals).** The
  provider sync now writes the isolation rules of a provider that has a managed-services installation (in any
  phase, a saved draft too) whether or not it has a project (`(*pve.Service).ReconcileProviderIsolation`, once per
  such provider and cycle, under the SDN lock, and only with `PROXCLOUD_PLATFORM_ENABLED=true`). For every other
  provider the sync is unchanged: no lock, no new Proxmox request and no new log line, so an install that sets up no
  managed services gets no new firewall write from this release. The order is therefore: save the installation, run
  a provider sync, check; saving reads the provider's settings only and does not wait for the check. That call, which
  needs no project, only ever adds `DROP`s. It writes nothing while a node does not report its networks, nothing
  for a `cidrPool` that overlaps a management network, and nothing when the rules in place are for another pool or
  drop a network its own set does not (an equal or a wider network covers one) or accept other sandbox endpoints
  than its own, and nothing, not even a wider set, while a project's `ACCEPT` rules for the pool are in place and
  the set is not its own: the rules carry the pool prefix
  only, so a second portal or provider with the same prefix (`pc-` is the default) would otherwise delete the first
  one's `DROP`s at every sync, or the two would replace each other's set on every cycle. These refusals are logged as warnings (`pve.ErrIsolationRefused`), a busy or failed
  SDN lock is logged too, and the `isolation` check gives the same sentences instead of "a provider sync writes
  them", also when rules are in place. A project's sync and a network create write the rules in all these cases, as
  before. A changed `cidrPool`, a narrowed `blockedCidrs` or changed sandbox endpoints on a provider with no project is applied by its first
  tenant network, or by deleting its rules under Datacenter > Firewall and running a sync; the messages say so.
  A provider sync skips a disabled provider, and the `isolation` check now says so instead of only sending the
  operator to a sync.
  Before, only a project's sync or its first tenant network wrote
  them, while the preflight's "no rule yet" pass ends as soon as any tenant network exists on the cluster: on a
  cluster where another portal had tenant networks, a portal with no project failed `isolation` with
  `0 in place, N wanted; the next sync rewrites them` and no sync ever did. The pass itself is unchanged (a vnet does
  not say which portal made it), and the messages and the guide now say what writes the rules and that one covering
  network in `blockedCidrs` works as well as the listed ranges. New preflight row `token-scope`: it fails when
  `capmox@pve!capi` or `csi@pve!csi` holds any privilege on `/pool`, `/storage` or `/vms` as a whole (rows of an older
  setup, which the script never removes; the guide's section 10 claimed they could not exist and nothing read them).
  It and `iso-store-visibility` now name the `pveum acl delete` command for each granting row the portal token can
  read, and the `pveum acl list` recipe otherwise: it says which listed row is the one to remove (on the path or on
  a parent with propagate, not `NoAccess`; the script's own rows are listed too and stay), with the
  `pveum user token modify … --privsep 1` command for a token that has no such row because it was made without
  privilege separation. `iso-store-visibility` names
  rows only for stores the token sees, and none when the extra store is the boot store (that row is the script's;
  the fix is the `boot-iso` row's). `proxcloud-up.sh` accepts a build of main as its version
  (`--version dev-<sha>`, `upgrade dev-<sha>`, 7 to 40 hex digits) with a warning that it is not for production, and
  allows `--managed-services` on it; before, such a build could neither be installed nor upgraded to, and an `.env`
  naming one stopped every later command. `--scheduler` stays refused on a dev build, and every other tag stays
  invalid. The script now runs under `LC_ALL=C`: in a UTF-8 locale its bracket ranges also matched letters and
  digits outside ASCII (`dev-ééééééé`, `v0.٨.0`), for the release pattern since before this change. The guide no
  longer says that existing token secrets are always in the installer's `generated/` directory: stage 10 writes
  `token-capi` and `token-csi` there in any mode, but only for a token it created itself or was given in the vault.
  Not verified on a lab.

- **Managed services (ADR 0009): what the API accepts and what the page advises now match what a run can do.** The
  API refuses an installation whose import store is its boot store (the portal token's `PVEDatastoreAdmin` on the
  import store would reach the platform's disks behind its `NoAccess` on the platform pools), and refuses
  `seed.mode: existing` and `provisionedBy: installer` when the installation is saved, where before a run failed at
  stage 3 with `stage_unavailable`; the page checks the store rule before it saves when both stores are typed, and
  the refusal names the provider's `importStorage`, which an empty import store takes (the guide lists the steps for
  a provider whose import store is the boot store). An installed record stored with `provisionedBy: installer` can
  be corrected to `portal`. `bom_unsupported` no longer
  names an upgrade path that does not exist, in the page, the plan and the `409`: it says to go back to the release
  that installed the platform, or to delete every tenant cluster and database, remove and install again. The in-app
  failure table gains `credentials_missing` and `stage_unavailable`. The `pool-fence` check says to run the identities
  script again first, and not to remove the token's `/pool` permissions. The Cancel hint gives the real bound (up to
  5 minutes while a stage pauses after repeated errors), and the ready card says which stores not to back the seed up
  to. The failure codes `privileges_missing`, `network_problem` and `iso_store_visibility`, which no stage emitted,
  are gone from the code and the page. A draft saved with the boot store as its import store fails the `spec` check
  until the store is changed; where the provider's `importStorage` is that store, the provider must change too.

### Security

- **Managed Kubernetes: a node group's `imageId` is no longer taken from a request.** It went into the cluster's
  values as it came, so a caller of the API could pin a group's workers to Proxmox nodes and template VMIDs of its
  choice, published or not. The image now always comes from the provider's version matrix and the group's
  `imageVariant`. The console never sent the field; an API client that did gets the provider's image instead.

- **The portal token can create API tokens for `capmox@pve` and `csi@pve` only, no longer for every Proxmox user,
  and holds no `Permissions.Modify` on `/pool`**
  (`internal/cloud/pve/access.go`, the managed-services setup script, installer stage 10). The mint grant of managed
  Kubernetes put `User.Modify` on `/access/groups`, where Proxmox VE takes it for every user of the cluster: on the
  lab (2026-11-16, `pve-manager/9.2.26`) the portal token created a privilege-separated API token for `root@pam`. A
  token without privilege separation (full root) was not tried; by the PVE source the same grant allowed it before
  `libpve-access-control` 9.2.1. `User.Modify` now sits in a role of its own, `ProxCloudKubeToken`, on
  `/access/groups/proxcloud-kube`, a new group that holds the two service users. `ProxCloudKubeMint` keeps
  `Permissions.Modify` on `/storage`, `/sdn/zones` and `/nodes` and has no row under `/access` or on `/pool`: the
  portal writes a cluster token's pool rows with its own `ProxCloudPlatform` rights, so a stolen token can no longer
  write any role on every pool. **Managed Kubernetes now needs `libpve-access-control` 9.2.1 or later on every
  node** (not checked by the product).
  **Existing sites must act:** run the managed-services script again as root on a node, or `make upgrade` on an
  installer site in split mode. Both write the new grant first, then remove exactly the old rows, then the old
  privilege, so the portal token creates and deletes cluster tokens throughout. **A cluster with more than one
  portal token** (a second provider or portal): run it for each; until the last one has run, the role keeps
  `User.Modify` so the others keep working, and the script and stage 10 end with `NOT FINISHED` and the tokens
  left. Until then the new preflight row `mint-scope` fails and Test connection warns (`mint-scope`), with what to
  run; the row also fails when one of the two users is not in the group. Stage 10 asserts the result. Afterwards
  list the tokens of `root@pam` and of the other administrator accounts for ones nobody on your side created. Still
  open and accepted for M1 (ADR 0009 A8, corrected): `Permissions.Modify` on those three paths, and the portal
  token's power to delete the tokens of the two service users. Neither the grant on the group nor a cluster create
  without the `/pool` row has run on a Proxmox cluster yet; the tests are the generated script against the docs and
  against a fake `pveum`, the rows against stage 10, stage 10 against a fake Proxmox API, the effective rights worked
  out from the rows, and the checks against a fake API (`docs/proxmox-permissions.md`, "The mint grant in M1" and
  "The re-drill, still to run").

- **ArgoCD's `default` project is locked on installer-built services clusters**
  (`installer/playbooks/files/argocd-default-project.yaml`, applied by stage 50's guardrails step in
  `services-platform.sh`): empty `sourceRepos`, `destinations` and `clusterResourceWhitelist`. ArgoCD creates
  `default` open (every repository, every cluster and namespace, every kind) and never resets it, so any
  Application in `default` could deploy anything anywhere on the cluster that holds every tenant's control plane
  and database, around the `proxcloud-k8s` and `proxcloud-dbaas` guardrails. Neither the installer nor the
  console picks `default` there: tenant clusters and `pc-sandbox` are in `proxcloud-k8s`, databases in
  `proxcloud-dbaas`, and a new test (`installer/tests/test_services_argocd.py`, in `make test`) keeps it so. The
  console's Kubernetes provider form used to fall back to `default` when its ArgoCD AppProject field was blank,
  and when it loaded a provider stored without `argocd.project`; it now falls back to `proxcloud-k8s`, as
  `kamaji.BuildApplication` and the database form already did (`web/admin/e2e/provider-connection.spec.ts`). The
  management cluster stays open, because stage 40's `cnpg`, `cert-manager` and `proxcloud` Applications use
  `default`. Nothing here is behind a flag: the lock applies at the next run of stage 50 (`make install`; `make
  upgrade` does not run it), on existing sites too. Locked, an Application in `default` (or with no project) on
  the services cluster stops syncing, and deleting it deletes nothing it created, because ArgoCD only deletes what
  the project permits: its workloads stay, and for a tenant cluster its control plane, Cluster API objects and PVE
  VMs. Such an Application is one made by hand, or a tenant cluster of a provider whose `config.argocd.project` is
  `default` (typed in, set through the API, or saved from the console with a blank AppProject before this change).
  So stage 50 first lists those Applications and stops, naming them, while any is left; move each, and its
  provider, to a named project, then run it again. Checked with `ansible-playbook --syntax-check`, the offline test
  (which runs the listing against a stub `kubectl`) and the console e2e spec only; not applied to a running
  cluster.

### Known issues

- **Seed ISOs of servers created before this release are never swept (O2).** A server's seed ISO
  (`pcseed-v1-<...>.iso` on the provider's seed store) is deleted with its server only when its record carries the
  cluster and storage identity, which is recorded for servers created on v0.8.0 or later
  (`internal/cloud/pve/seed.go`). For a server created on an earlier release the record has none: when that server
  is deleted, the API logs `seed cleanup pending ... original cluster/storage identity was not available; operator
  inspection required`, and the ISO stays on the store for good (seen on a lab after an upgrade). It holds the
  server's user data. Clean it up by hand once its server is deleted, as root on a node: list the seed ISOs
  (`pvesm list <seed store> --content iso | grep pcseed-v1-`); one that is older than a day and that no VM's config
  names any more (`grep -l <file name> /etc/pve/nodes/*/qemu-server/*.conf` prints nothing) is such a leftover:
  `pvesm free <volume>`. Not verified on a lab: the hand cleanup, that a server created on this release gets the
  identity, and the sweep that follows.

## [0.7.0] - 2026-11-15

A feature release on top of `v0.6.0`.

- **Platform setup** in the operator console (`/setup`) takes a new install from the Proxmox token to the
  first tenant in one page, on new API routes: every connection check in one answer (`?checks=all`), a
  generated script for the portal token, the setup defaults and the setup status. Its Network step asks
  how servers get their seed (by URL or on an ISO). The Docker Compose install gains `--template-updates`
  and `--scheduler`, both off by default.
- **Security fix:** turning the job scheduler on no longer mounts the unauthenticated `POST /debug/*` job
  triggers on the management port. Every tag from `v0.4.0-rc.1` to `v0.6.0` did; see the management port
  entry under Security.
- HIGH/CRITICAL findings without a fixed version stay tracked, not blocking, until 2027-03-12 instead of
  2026-12-12 (Security).
- The smaller template disks of `v0.6.0` have one lab build each for `ubuntu-2604` (5 GiB, then
  6 GiB), `ubuntu-2404` (5 GiB) and a Kubernetes 1.37.2 node on `ubuntu-2604` (8 GiB); native Ubuntu
  26.04 templates now build at 6 GiB. The Debian slugs at 5 GiB and a Kubernetes node on `ubuntu-2404`
  have not been built (Changed).
- Groundwork for managed services, inert in this release: the Kubernetes API client
  (`internal/cloud/kamajik8s`) gained cluster-scoped apply, TokenRequest, an access review and a
  kubeconfig builder, with no caller yet (Added), and ADR 0009 records the managed-services decisions
  of 2026-11-15 (Changed). Nothing provisions a platform yet.

No breaking API change, but two behaviour changes can break an existing setup:

- A script that calls a `/debug/*` trigger on an install with only the scheduler on now gets `404`. Use
  the authenticated `/api/v1/admin/job/*` endpoints, or set `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` on a
  development or lab install only (Security).
- Saving a Proxmox provider whose `config.network` has a new problem (for example a `cidrPool` smaller
  than `/24`, a `defaultZone` that names no zone, or a VLAN zone without a bridge or with a tag outside
  1 to 4094) is now a `400`; a problem the stored block already has does not stop a save. Installer
  stage 00 now refuses such an inventory before it changes anything. It checks the inventory alone, so
  on an existing installer site `make upgrade` (and `make templates`, `make mgmt-scale`,
  `make storage-rebench`) stops on such a value even when the stored block already had it. For a VLAN
  zone made by hand in Proxmox, set `network.bridge` to that zone's bridge (Changed).

Verified on a lab: Platform setup end to end on `v0.7.0-rc.1`, on a nested three-node Proxmox VE 9.2
cluster, from a fresh Docker Compose install with TLS mode `internal`, through go live to a tenant server
running on a tenant network. That run found that URL seeds cannot work on such an install; the seed
fixes (under Fixed, and the server seed choice under Added) came from it. On the same install with
images built from `main` (`9c09945`), the setup status `seeds` item and seed URL, and the Network step's
Server seeds form showing the stored choice, were re-checked; the `proxcloud-up.sh` next steps and
saving the seed choice from the form were not. The steps, and what the run did not check, are in the
release readiness page, section [Platform setup lab acceptance](docs/release-readiness.md#platform-setup-lab-acceptance).

The not-verified notes in the entries below were written when each change merged. The lab run covers
the connection checks (`?checks=all`), the generated script run on a node, the defaults apply, the live
setup status, the setup page steps 1 to 7 except a template import, and a Compose install started with
`--template-updates` (nothing was imported). The other not-verified notes still hold, among them anything
that applies only to an EVPN or VLAN zone (the run used a simple zone) and `--scheduler`.

Everything `v0.5.0` to `v0.6.0` say is **not** verified still holds, except one lab build each of
`ubuntu-2604` (5 and 6 GiB), `ubuntu-2404` (5 GiB) and a Kubernetes 1.37.2 node on `ubuntu-2604`
(8 GiB); the Debian slugs at 5 GiB are still not built. Also not verified: a template import through the setup page, the job scheduler on a running
install, a login into a tenant server, an installer or Helm install of this release, and the network
check in a full installer run.

### Added

- **Every check of the Proxmox connection test in one answer**
  (`POST /api/v1/admin/service/proxmox/auth?checks=all`): the test stopped at the first failure, so an
  operator fixed the datacenter firewall only to learn about the node firewall, and then about missing
  privileges. With `?checks=all` the route answers `200` with `checks`, one `{id, status, message}` row per
  check (reachable, token, both firewalls, public networks, privileges per ACL path, the stored provider's
  store checks), and runs every check it still can. The call without the parameter is unchanged: the first
  failure is a `400` with the same message, which installer stage 60 reads. The body also takes `apiUrls`
  (failover addresses were tested only from a stored provider). With a stored provider and a blank secret,
  the fields the body carries now replace the stored ones for the test, so an edited form can be tested
  before Save; this needs `admin:service:manage`, because it sends the stored secret to the edited address.
  Such a test, when it sends the stored secret with an address, CA, fingerprint or `insecureTls` other than
  the stored ones, writes a `CONNECTION_TEST` audit event on the provider (actor, the fields that differ, the
  addresses tested, success or failure): the route is otherwise not audited and the provider does not change,
  so nothing else would say where the secret went. The event is built by a tested function; its write to the
  audit store was not run in a test. A read-only caller still tests the stored connection. `docs/api.md` lists the check ids. Tested against a
  fake PVE API only; not run against a lab cluster.
- **Setup checks in the Proxmox connection test** (`?checks=all` only): Proxmox VE 9.2 or later, the pool
  `pc-templates`, a storage with `import` content, and for a stored provider its default network zone,
  `cidrPool` (valid, and a warning when it overlaps a management network of the nodes or when a node's
  networks could not be read, so a node left out is not reported clear), default storage
  profile and, with an EVPN zone, `frr-pythontools` on every node. A new provider used to pass the test
  and then answer 503 on the first tenant network, or show no images, with nothing saying why. These rows
  never fail the default call and never add to `warnings`. A caller with `admin:service:manage` also gets
  the bridges of every node with a management flag, to choose a public network from, and, when the
  connection is refused over the certificate, the SHA-256 fingerprint the server presented, so it can be
  compared on the node and pinned. A read-only caller gets neither. A refused pin now reads
  `tls: failed to verify certificate: pve: certificate fingerprint mismatch` in the error text. Not
  lab-verified: reading a node's package list with the least-privilege token (the check warns when it is
  refused), and the type Proxmox reports for an SDN vnet in the bridge list.
- **A generated script for the portal token** (`GET /api/v1/admin/setup/proxmox-script`): a portal that was
  not set up by the installer needed the role, two pools, a user, a token and seven ACLs typed from
  `docs/proxmox-permissions.md`, and the usual mistake (an ACL on the user but not on the token) only showed
  as missing privileges later. The route answers one script as text, for a token ID, with or without the
  GPU mapping ACL and the import store ACL. It can be pasted into a root shell and run again: it runs in a
  subshell, single-quotes every value (the `!` of a token ID is history expansion otherwise), creates the
  role empty and appends its privileges, and ends with the token's effective permissions and
  `pvenode cert info`. The ACLs now live in one table next to the privilege list
  (`pve.PlatformACLs`); tests keep the script in `docs/proxmox-permissions.md`, the installer's role and
  ACL rows (stage 10) and that table equal, and refuse a token ID or storage that is not a plain id. The
  portal still holds no root credential. The script has not been run on a Proxmox node.
- **Defaults applied from the API** (`GET` and `POST /api/v1/admin/setup/defaults`). A new install needs a flavor
  catalog, a public price plan, a default project quota and a region row before a tenant can create a server. Until
  now that took a checkout, python3 and `deploy/seed/seed-operator.py` with four overridden variables on a Compose
  install. The API now carries the same `deploy/seed` documents (`deploy/seed/seed.go` embeds them; the API image
  copies `deploy/seed`) and applies them on request. The preview shows what would be written. The apply only creates:
  it never changes a document that exists, runs in one transaction under one advisory lock, and answers one result
  per item. The price plan is created enabled, so the apply refuses it without `"confirmRates": true`, and it is
  skipped when any enabled PUBLIC plan exists, because every enabled public plan is billed. When every such plan is
  scoped to other providers than `serviceId` the item is blocked and says so, instead of reading done while that
  provider is billed nothing; the setup status agrees. The plan gets the rules a
  Proxmox install bills; GPU, share and bucket rules are added only when such a provider exists. The default quota
  is merged under the stored one, so per-model GPU limits and limits the operator set are kept. Each item needs the
  permission of the endpoint that writes the same document by hand. `seed-operator.py` is unchanged.
  `docs/api.md` describes both routes. Not verified: a run against a Compose install, and the published image build
  (the build stage was built locally; CI builds the image).
- **Setup status from the API** (`GET /api/v1/admin/setup/status?serviceId=&live=1`). The Dashboard and the setup
  page need to know which step of a new install is done, and the row counts of `/admin/stats` call billing and
  branding configured on a fresh Compose install, because the seed writes those rows. One pure function computes six
  steps (connect, network, storage, templates, catalog, go live) with one item per check, from the database alone,
  and with `live=1` also from the cluster: every row of the connection test (`?checks=all`, the same function, not a
  copy of its checks), each in the step it is about (the zone, cidrPool, frr, public network and store reach rows
  in network, so a network problem does not send the operator back to the connection form; network and storage
  rows replace the database item that asks the same, keeping its id), the nodes with an active import store, the
  published OS images and the imported templates. A table test pins a fresh Compose install to no step done. Go live waits for connect, network, storage and templates, and is `unknown`
  while one of them could not be read (templates, without `live=1`), so the database-only answer never offers
  go live on a cluster nobody looked at; an open catalog and
  a job scheduler that is off are warnings, by the owner's decision, so they block nothing. Only enabled public
  plans that apply to the provider count as pricing it, and the quota counts as set only when it sets every limit
  the defaults would add, the same rule as the defaults preview. The route needs the four reads of the defaults
  preview.
  Not verified: a live run against a Proxmox cluster (the live checks run against the test fake).
- **Proposed storage profiles** (`proposedProfiles` in `POST /api/v1/admin/service/{id}/storage/detect`). The
  installer proposes profiles from the storages it finds (`classify_storages`); a console install had to write them
  by hand. The detect answer now carries the same proposal (`pve.ProposeProfiles`, tested on the installer's own
  cases), nothing of it stored. It skips a storage active on no node, claims a flag only when the storage has it, and
  claims no snapshots on a storage the operator set to capacity class (`storage-classes`), so the proposal passes the
  profile save unchanged. The rest of the answer is as before.
- **Template imports create pool `pc-templates` when it is missing** (`docs/template-updates.md`). A template import
  creates its VM in that pool, and on a cluster where nobody ran `pveum pool add pc-templates` the create failed. The
  job now reads the pool before the upload and creates it once, fenced like its other Proxmox writes; an existing
  pool is only read, so a token without `Pool.Allocate` keeps importing where the pool was made by hand. A token that
  may not create it fails the job as `pve_forbidden` before anything is downloaded. The connection test's
  `template-pool` row, which the setup status shows in the connect step, says so instead of sending the operator to a
  root shell for `pveum pool add`, which it keeps as the fallback for such a token. Not verified: that the
  least-privilege portal token can create `pc-templates` on a lab cluster (it holds `Pool.Allocate` on `/pool`, which
  creates the project pools).
- **Docker Compose install: opt-in VM template updates and job scheduler** (`deploy/compose/`): the API
  reads both switches only when it starts, and the Compose install had no way to set them, so on one host
  nothing was synchronised or charged on a timer and no template could be imported. `compose.yaml` now
  passes `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` and `PROXCLOUD_JOBS_SCHEDULER_ENABLED` to the API from the
  `.env` keys `TEMPLATE_UPDATES_VM_ENABLED` and `JOBS_SCHEDULER_ENABLED`, both `false` by default.
  `./proxcloud-up.sh install --template-updates` and `--scheduler` write `true`, `--no-template-updates`
  and `--no-scheduler` write `false`. A later `install` without the flag and every `upgrade` keep the
  recorded value, so a re-run cannot turn billing off or on by omission; the last lines of `install`,
  also with `--dry-run`, say what is on. The script removes both names from the environment it hands to
  Compose: an install whose `.env` predates the keys cannot have its billing timers started by a
  variable exported in the calling shell. The scheduler needs nothing the stack does not have: its job
  locks (ADR 0007) and the queued charge work live in the stack's PostgreSQL. `deploy/compose/README.md`
  (Optional features) lists what each switch starts and what the scheduler means on one host. Turning
  the scheduler on does not mount the unauthenticated `POST /debug/*` job triggers on the management
  port: only `PROXCLOUD_JOBS_DEBUG_TRIGGERS` does (see the management port entry under Security), and
  this stack never sets it. That is true from the first release after v0.6.0: up to v0.6.0 the API
  mounted them with the scheduler too, so `install` and `upgrade` refuse to run such a release with the
  scheduler on (`install --scheduler` on a checkout still pinned to v0.6.0 included) and say why. The
  port stays unpublished and unrouted. The README points on-demand runs at the authenticated
  `/api/v1/admin/job/*` endpoints and names the jobs they cover; the bill, dunning and project deletion
  jobs run only on the timer, and `send-test-mail` is not available here. The README also says that `--no-scheduler`
  only stops new timed work: the API's job worker always runs and still processes the billing work
  already queued in the database, which matters when a backup is restored. **Not verified**: either
  switch on a running Compose install (the tests run the script with `--dry-run` and against a fake
  `docker`, and check the model `docker compose config` builds), a billing month with the scheduler on,
  the bill and reminder jobs without an SMTP integration, and how charging catches up after the host
  was down.
- **Platform setup page in the operator console** (`/setup`, also reached as `/onboarding`, where the API sends an
  install without a platform configuration; linked from System and from the Dashboard for an admin who holds the four
  reads the status needs: service, price plan, platform configuration and billing configuration). A new install was
  set up from four pages and a doc block, and the Dashboard checklist counted rows that the Compose seed writes, so it
  called a fresh install configured. The page shows one tab per step with the state `GET /api/v1/admin/setup/status`
  reports (the database answer at once, the live one when the cluster has answered; a state the API could not read
  shows Unknown, never done, and when a later read fails the page drops the answer it had instead of showing it as
  current), and the Dashboard card shows the same steps. Step 1 asks for the token ID, GPUs and an import store and
  shows the generated root script with a copy button and the two firewall switches. Step 2 takes the connection, runs
  every check of the connection test (`?checks=all`) with one row each, offers to pin the fingerprint the server
  presented only after the operator compared it with `pvenode cert info`, and saves the provider with
  `attachToNewProjects: false`, so no project is attached before Go live. Steps 3 to 8 are described below. Playwright
  specs against the mock API cover the script, the check rows, the pin, the create body and the axe checks; not run
  against a live API or cluster.
- **Network and storage steps of the Platform setup page.** A provider's network and storage were raw JSON on the
  provider page, and a wrong block showed up only as a failed tenant network. Step 3 is a form: zone kind (EVPN, VLAN,
  simple), zone ID, the VLAN tag range (1 to 4094, first not above last, which the API does not check), the project
  network range (`cidrPool`, default `10.100.0.0/16`, locked once saved because every tenant network is cut from it),
  EVPN peers and exit nodes from the cluster's nodes, an optional public network that only a bridge without a node
  address or default gateway on any node can carry, and the networks tenants must not reach. It saves with
  `PUT /api/v1/admin/service/{id}`, which replaces each top-level config key, so it reads the stored block first and
  sends it whole: public networks, isolation and other zones are kept. Step 4 detects the storage, shows the proposed
  profiles to confirm (a saved profile wins over a proposal of the same name), and saves the default profile, the
  import store and the backup store. Without a store that takes `import` content it shows the one `pvesm set` command;
  it also lists the three `pvesm add nfs` commands of a NAS (the stores installer stage 05 creates) and the
  prerequisites of a TrueNAS provider, with a link that opens Add provider on TrueNAS
  (`/system/cloud-providers?add=truenas`). Playwright specs check the saved bodies against the mock API; not run
  against a live API or cluster.
- **OS templates step of the Platform setup page.** Getting a first template onto a console install meant the
  Template updates page, the Integrations page and raw JSON for the smoke network, one image and one node at a time.
  The step is done once the provider publishes an OS image, however it was built, and then shows nothing more.
  Otherwise it shows whether template updates are on (if not, the installer command
  `./proxcloud-up.sh install --template-updates`, because the API reads the flag only at start), takes the GitHub
  token (`read:packages`; version listing has no anonymous access), saves the smoke-test network
  (`config.templateSmoke`), imports the picked OS images on the picked nodes in one go (one request per image and node,
  each staged and smoke-tested as before) with their progress, and has one **Publish all that passed** button that
  sends one publish request per template whose smoke test passed. Nothing is published without that click.
  Kubernetes node images are left to the Template updates page. Playwright specs check the flag-off state, the import
  requests and that publish is called once per passed template, against the mock API; not run against a live API.
- **Catalog, Go live and Next steps of the Platform setup page.** The defaults (flavors, public price plan, default
  quota, region row) needed a checkout and `seed-operator.py`, and going live meant the provider's Projects tab plus a
  sync nobody ran on a Compose install. Step 6 shows what `GET /api/v1/admin/setup/defaults` would write, the rate
  table included, before anything is applied; the price plan is applied only after the operator ticks that the rates
  were reviewed, and the apply sends `confirmRates` only then. It warns about flavors that no rule prices (billed as
  zero) and links the billing details and mail as recommended. Step 7 is enabled when Connect, Network, Storage and OS
  templates are done (a warning there does not hold it back, a state the cluster did not report does) in a live
  answer, and the catalog is done or the operator ticks going live without it. A database-only answer never enables
  it: there Connect is done once the provider is saved, without its connection checks. When the job scheduler is off
  it says that nothing then syncs or charges on a timer and shows `./proxcloud-up.sh install --scheduler`, without
  blocking. Go live turns the attach policy on, runs one provider sync
  (`POST /api/v1/admin/job/external-service/sync`) and starts a bulk attach of the projects created meanwhile when
  there are any, then lists the first tenant's path. Step 8 is a static card that links ADR 0009. `docs/quickstart.md`
  (path A2 and After any install) and `deploy/compose/README.md` now point at the page; the install flags
  `--template-updates` and `--scheduler` the page names are those of the Docker Compose entry above (opt-in VM
  template updates and job scheduler). Playwright specs check the preview before Apply, the confirmation in the apply
  body and Go live staying disabled until its prerequisites hold, against the mock API; not run against a live API,
  and Go live not run against a cluster.
- **Server seed choice in the Network step of the Platform setup page.** Whether a server on a DHCP network
  fetches its NoCloud seed from the API URL or gets it on an ISO was a raw JSON key on the provider page
  (`config.metadata.forceIso`), and a server that missed its URL seed booted without its hostname, password and
  guest agent with nothing on the page saying why. Step 3 now has a **Server seeds** choice: "By URL from
  `<seedUrl>`" (the URL the setup status reports: `config.metadata.url`, else the API's own base URL) for tenant
  servers that reach it with a trusted certificate, or "On an ISO" for LAN installs and self-signed certificates;
  without a seed URL the URL choice is disabled, and a saved one is shown as going on an ISO. It shows what is
  saved, and saves only `config.metadata` with `PUT /api/v1/admin/service/{id}`, read fresh and sent whole because
  the route replaces top-level config keys: `url`, `isoStorage`, `vendorData` and the other keys are kept, and the
  network block is not sent. Each of the step's two forms is reset only when the block it edits
  changes, so saving the seeds keeps an unsaved network edit and the other way round. Playwright specs check both
  saves' bodies, the kept edits, the disabled URL choice and the axe checks against the mock API; not run against
  a live API or cluster.
- **Kubernetes API client calls for managed services** (`internal/cloud/kamajik8s`; groundwork, nothing calls
  them in this release): `ApplyClusterScoped` (server-side apply of a cluster-scoped kind such as a
  ClusterRoleBinding; an object with a namespace is refused), `CreateServiceAccountToken` (TokenRequest; returns
  the expiry the API server granted), `CanI` (SelfSubjectAccessReview; an answer without a verdict counts as
  denied) and `BuildKubeconfig` (server, CA and bearer token, no client certificate; a server that is not
  `https` is refused). `BuildKubeconfig` writes only the CERTIFICATE blocks of the CA it is given and refuses a
  CA input with any other block or with no certificate, so a CA private key passed with its certificate never
  lands in the file. Unit-tested against a fake API server; not run against a cluster.

### Changed

- **The smaller template disks are confirmed by one lab build each** (`docs/template-build-disk.md`): a
  native `ubuntu-2604` template at 5 GiB and a Kubernetes 1.37.2 node template at 8 GiB were built on
  the lab and exported through the export workflow; both images are listed in
  `docs/vm-artifact-publishing.md`. The native 5 GiB size is close to its limit on Ubuntu 26.04: the
  root filesystem is 3.7 GiB and the build left less than 1 GiB free before its cleanup. Not done: the
  other native OS slugs at 5 GiB, a portal import of either image, a node group joining a cluster.
- **Native Ubuntu 26.04 templates build at 6 GiB** (`BUILD_DISK_GIB_BY_OS` in `images/build.py`; 6 GiB
  added to the sanitizer's allowlist). Its vendor image has a separate `/boot`, and the 5 GiB build kept
  less than 1 GiB free on `/` after the upgrade, too close to the 1 GiB admission of the cleanup step.
  Ubuntu 24.04 at 5 GiB (built and exported on 2026-11-15, 1.7 GiB free) and the other OS keep 5 GiB.
  Templates and images already built at 5 GiB stay valid. The first 6 GiB lab build (2026-11-15) kept
  2.0 GiB free after the upgrade and was exported through CI; the image is listed in
  `docs/vm-artifact-publishing.md`.
- **A Proxmox provider's `config.network` is checked when it is saved** (`POST /api/v1/admin/service`,
  `PUT /api/v1/admin/service/{id}` and `PUT /api/v1/admin/service/{id}/update`): the block was stored as
  sent, so a `cidrPool` of `/25`, a zone id of nine characters or a VLAN zone without a bridge showed up
  only as a failed or 503 network create in a tenant's project. A save whose body carries `network` is now a
  `400` naming the field when `cidrPool` is not an IPv4 range of `/24` or larger, `defaultZone` names no
  zone of `zones` (or there are several zones and no `defaultZone`), a zone's `kind` is not `evpn`, `vlan`
  or `simple`, a zone id is longer than 8 characters, a `vlan` zone has no `bridge`, a `vlan` zone's
  `vlanMin` or `vlanMax` is missing or not a whole number from 1 to 4094 (a VLAN tag), or `vlanMin` is
  above `vlanMax`, or `isolation` is `evpn-per-project` with a default zone that is not EVPN. What is only missing is still accepted (no
  `cidrPool`, no zone yet, as the console's Add provider form sends it). A save is refused only for a
  problem the block it replaces does not have already: the console's Connection tab sends the stored block
  back with every save, and a block saved before this check may work (a zone that exists in Proxmox is used
  as it is, and the portal reads only the default zone), so it must not lock the provider. A new problem, or
  a wrong value changed to another wrong value, is still refused. The dev
  seed and the network block installer stage 60 builds from the inventory defaults pass. Installer stage 00
  now refuses, before anything is changed, an inventory the check would refuse at stage 60 after stages 10 to
  55 had run: `network.kind` not `evpn`, `simple` or `vlan`, `kind: vlan` with the shipped empty `bridge`
  or with a `vlan_min` or `vlan_max` that is not a whole number from 1 to 4094 or a `vlan_min` above
  `vlan_max`, `isolation: evpn` with `kind: simple` or `vlan`, a `cidr_pool` that is not IPv4 `/24` or larger (or is
  empty), and a `default_zone` that is empty or longer than 8 characters. A site whose VLAN zone was made by
  hand in Proxmox with no bridge in the inventory sets `network.bridge` to that zone's bridge (the portal
  uses an existing zone as it is). Tested with unit tests and the preflight task run on its own against the
  inventory defaults; not run in a full install.
- **ADR 0009 is amended with the managed-services decisions of 2026-11-15**
  (`docs/adr/0009-managed-services-platform-lifecycle.md`, section "Amendments (2026-11-15)"; the original
  decisions stay readable): ArgoCD Helm values go inline, the seed VM is cloned from a published native OS
  template into its own pool, Cluster API providers are installed by a clusterctl Job from a CI-built image,
  how the seed's ArgoCD and cert-manager are installed, the workload cluster keeping its own ArgoCD and Cluster
  API for tenant objects, chart digests verified by CI until a mirror exists, seed backup by a PVE
  backup job, the risks accepted for the first milestone, existing installer sites staying observe-only, and
  the BOM's wave order as the upgrade order. Documentation only: the ADR says "Not implemented yet".

### Fixed

- **The setup status asks how servers get their seed** (`GET /api/v1/admin/setup/status`, network step, item
  `seeds`). On a DHCP (simple) zone a server fetches its NoCloud seed from the API URL; a tenant server that cannot
  reach that URL, or does not trust its certificate (Compose TLS mode `internal` is self-signed), boots with the
  template's hostname, no password and no guest agent, and nothing told the operator (lab acceptance of
  v0.7.0-rc.1). The item is `done` once `config.metadata.forceIso` is `true`, or is `false` with a seed URL, or
  `config.metadata.url` is set, and `attention` otherwise, naming the URL seeds are fetched from and that tenant
  servers must reach it and trust its certificate; without any seed URL it stays `attention` even with
  `forceIso: false`, saying every seed goes on an ISO. It never holds go live back, and nothing is probed. The
  answer also carries `seedUrl`, the base a URL seed is fetched from (`config.metadata.url`, else
  `PROXCLOUD_SELF_API_BASE_URL`; `""` when neither is set). Unit-tested; not run against a live API.
- **`proxcloud-up.sh` sends the operator to Platform setup** (Docker Compose). Its "Next steps" named System →
  Cloud providers, a raw form that skips the setup steps; it now names System → Platform setup. With TLS mode
  `internal` it adds that tenant servers cannot fetch seeds over HTTPS with the self-signed certificate, so choose
  "On an ISO" in Platform setup → Network; `deploy/compose/README.md` says the same under the TLS modes.
  `deploy/tests/test_compose_production.py` checks both lines per TLS mode in the dry run; not run in a real install.
- **The seed choice is documented** (`docs/cloud-integration.md` "Server seeds", and step 3 of the console's
  Platform setup docs page): when to choose a URL seed and when an ISO, what a missed URL seed looks like, and
  that the setup status asks until the operator chose.
- **The Proxmox provider page saves the failover URLs and the cluster CA** (operator console, Cloud providers,
  Connection tab). The form showed both fields, but Save sent neither, so an edit of `apiUrls` or `caCert` was
  dropped while the page reported it saved. Save now takes the connection keys from the same mapping as Add
  provider. A Playwright spec checks the request body against the mock API; not run against a live API.
- **The admin console's Proxmox resources editors have a label a screen reader announces**
  (`CloudProviderDetailPage.tsx`): the five JSON text areas (network, storage profiles, OS templates, GPU
  mappings, server seeds) were not tied to their visible headings, so assistive technology read them as
  unlabelled edit fields. Each heading is now the text area's label. Checked with the axe matrix of
  `e2e/a11y.spec.ts`, light and dark.

### Security

- **HIGH/CRITICAL findings without a fixed version stay tracked until 2027-03-12** instead of
  2026-12-12 (`UNFIXED_REVIEW_BY` in `.github/scripts/check_scan_report.py`; owner decision of
  2026-11-15). The reason is the review in `docs/security-unfixed-review.md`: per-target counts by
  Debian status for the eleven non-GPU templates, the controlled-base prototype and the VM export tool
  image (the product images and lockfiles have none), which findings a third party's data can reach,
  and what was not checked. The gate still blocks every finding with a fixed version in those targets,
  and from 2027-03-12 the unfixed ones again. The default `python` template is outside that set: it is
  a public image run as is and scanned report-only, so its 2 HIGH zlib findings with a fixed version
  and its 47 without one do not block, before or after 2027-03-12. The single VEX (CVE-2025-47273) did not move: it stops applying
  on 2026-12-31 (UTC), and from then the setuptools finding it covers blocks in nine templates and the
  prototype unless the owner decides otherwise.
- **The job triggers on the management port are no longer exposed when the scheduler is on**
  (`cmd/api/main.go`, `debugTriggers`). The API mounted every unauthenticated `POST /debug/*` trigger
  on the management port (`:8081`, every interface) when `PROXCLOUD_JOBS_SCHEDULER_ENABLED` **or**
  `PROXCLOUD_JOBS_DEBUG_TRIGGERS` was true. That includes `run-charge`, `run-collect`,
  `run-send-bills`, `run-dunning`, `run-project-deletion` (deletes cloud resources),
  `run-registry-prune`, `send-test-mail` (mail to any address), `sse-emit` and `jobs-status`; only
  `gen-hmac-key` was already limited to debug triggers. The chart never routes the port through its
  gateway or ingress, but any pod that could reach the API pod, or any process on a Docker host
  through the container's bridge address, could call them. This was in the code since the first
  ProxCloud release and is in every tag from `v0.4.0-rc.1` to `v0.6.0`. Affected: every install with
  the scheduler on (Helm `api.jobs.schedulerEnabled: true`, installer `jobs.scheduler_enabled: true`,
  or the environment variable), which is every production install that bills on a timer. Now only
  `PROXCLOUD_JOBS_DEBUG_TRIGGERS` mounts them; health and readiness on the port are unchanged.
  **Behaviour change:** a script that called a `/debug/*` trigger on an install with only the
  scheduler on now gets `404`. Migration: on a production install, use the authenticated
  `/api/v1/admin/job/*` endpoints (admin credential), which cover charge, metrics, sync, collect,
  savings expiry, reminders and the transaction scan, and `GET /api/v1/admin/durable-jobs` for the
  `jobs-status` history; the mapping is in `docs/jobs-scheduling.md` ("Manual triggers"). The other
  triggers have no admin endpoint: set `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` (Helm
  `api.jobs.debugTriggers`, installer `jobs.debug_triggers`) only on a development or lab install to
  keep them. Docs, the chart's values and README, the installer defaults and the development compose
  file say so. Verified by unit tests only (the management router with the scheduler on and triggers off answers `404` on
  `/debug/*`, the admin job endpoints still run their jobs); not checked on a running install.

## [0.6.0] - 2026-11-14

A feature release on top of `v0.5.2`, with no breaking API change.

- The portal can be installed on one host with one command (`deploy/compose`).
- VM images are now exported, published, attested and verified by one GitHub Actions run; nothing is
  prepared by hand. The images published this way are listed in
  [docs/vm-artifact-publishing.md](docs/vm-artifact-publishing.md).
- Template disks shrink to 5 GiB (native OS) and 8 GiB (Kubernetes node). **Upgrade portals before using
  images built with the new sizes**: portals up to `v0.5.2` refuse an image below 20 GiB.

Everything `v0.5.0` to `v0.5.2` say is **not** verified still holds. Also not verified: a lab build with
the new disk sizes, Let's Encrypt issuance in the Compose install, and an import of a CI-published image
into a portal.

### Added

- **One workflow run from a template to a published VM image** (`vm-artifact-export.yml`): choose the
  variant, the OS, the Kubernetes version and the template VMID, and one manual run on `main` does the rest.
  The `export` job (self-hosted, inside the lab, no registry credential) guards and copies the template
  read-only, then runs inventory, review, sanitize, postscan and the two-clone cold boot in a fixed order
  (`images/vm_export/pipeline.py`: a stage refuses unless the earlier ones completed and their records
  match), and builds the three-file bundle. Hosted jobs then publish it to GHCR (only for the variant,
  OS and Kubernetes version that were dispatched), create a GitHub build-provenance attestation for the
  digest of that push, check it with the command the run prints, and pull the digest with no credential
  to verify the files. The run also uploads one public evidence record (hashes, check results, versions,
  the tool image digest, the commit and the run), built from an allowlist so it holds no path, address,
  VMID or guest data; its sha256 is attested too and the final summary prints it. A cold boot that could
  not clean up keeps its state on the runner, and the next export finishes that cleanup before it
  starts. `docs/vm-artifact-publishing.md` has the flow, the trust model with the settings the owner
  must make outside the repository (runner group, fork approval, branch protection, the environment
  `vm-artifact-export`), the secrets and variables to set (`EXPORT_TEST_ADDRESSES` and `EXPORT_VLAN` are
  new) and the commands anyone can use to verify an image. The workflow has run on the lab for
  `ubuntu-2404` and for the three Kubernetes versions; the document lists those images.

- **VM export tool image built by CI** (`images/vm_export/tools/`, `vm-export-tools.yml`): the image the
  export containers run from (`qemu-img`, Python with libguestfs, the appliance kernel) was built on a
  workstation and loaded onto the runner by hand. It now has a recipe in the repository: Debian 12 pinned
  by digest, every package from one `snapshot.debian.org` timestamp and checked against a committed
  package list, no `sudo`, no account and no setuid file, usable as any non-root uid. The workflow builds it on a hosted runner, runs a smoke test without network as an
  arbitrary uid, scans it with the existing Trivy gate and, on `main`, pushes it to
  `ghcr.io/mutex-solutions/proxcloud-vm-export-tools` under one tag that the workflow never replaces. The
  digest is recorded in the job summary and as an artifact. `capsule.py` now pins that published image by
  registry digest: the export job pulls it and containers start only from its local ID.

- **VM image export steps as repository code** (`images/vm_export/`): the copy, inventory, review,
  sanitize, postscan and bundle steps that prepared the published VM images were private operator
  scripts. They are now reviewed modules with one evidence chain: a guarded read-only copy receipt, an
  inventory bound to that copy, a review derived from a committed baseline
  (`baselines/ubuntu-2604-k8s.json`, only the Kubernetes package versions may differ), a sealed
  sanitize run and a postscan bound to it. Each step runs in a container with no network, no
  capabilities and read-only inputs, and the parent checks the container's settings before start and
  proves it stopped. The container runs as the invoking host user (non-root, uid and gid 1000 or
  higher), no longer only uid 1000, so a CI runner account can run it. The cold-boot acceptance is
  code too (`vm_export.coldboot`): through the PVE API with the build token it uploads the sealed
  disk, boots two clones with fresh cloud-init, checks both guests over SSH (a failed snapd change
  fails the check), requires snapd on both and different machine-id, host key and snapd identity, and
  removes only what its own state file names. Names, the ownership tag and the upload carry a random
  per-run token, so two runs of one disk never touch each other's VMs or files. Cleanup ignores a
  cancel, retries, and can be repeated from the state file. The export workflow above runs them.

- **Production install on one host with Docker Compose** (`deploy/compose/`): `./proxcloud-up.sh` asks for the
  domain (or four hostnames), the TLS mode and two email addresses, generates every secret once into `.env`
  (mode 600), starts Traefik, Keycloak in production mode, PostgreSQL, the API and both consoles at the pinned
  release, creates the first operator with a temporary password and prints the URLs. TLS modes: `acme-http`
  (Let's Encrypt HTTP-01), `acme-dns-cloudflare` (DNS-01, the token as a Docker secret file) and `internal`
  (Traefik's self-signed certificate, for a LAN evaluation). Also `upgrade <version>` (backup first, pull before
  anything changes), `status`, `backup` and `uninstall` (volumes are deleted only with `--purge`). The Compose
  project is `proxcloud-prod` by default, and the install refuses a project name that the development stack,
  another directory's containers or a database volume without its `.env` already use. `--dry-run` writes its
  preview to `.env.dry-run` and never changes `.env`; variables of the calling shell do not reach Compose. No
  password is a command argument, inside the containers either. The Keycloak
  realm settings and the first-run seed are rendered from the Helm chart's own templates and a test keeps them
  equal; the default image tag is part of the release contract test. It installs the portal only: no Proxmox
  provider, no managed services. Guide: `deploy/compose/README.md`, quick start path F.
  **Verified** on one Docker host with `internal` TLS: install, re-run, hostname change, backup and restore into
  an empty database, upgrade to the same release, uninstall with and without `--purge` (and the install after a
  purge, which creates the operator again), a switch of the TLS mode and back, a realm import that fails during
  that switch (the running portal, its Traefik included, is left untouched), and the refusal of a project name
  in use. In the two Let's Encrypt modes Traefik was only started against an unreachable CA: the configuration
  loads and requests a certificate for the four hostnames. **Not verified**: certificate issuance and renewal
  (HTTP-01, and DNS-01 through Cloudflare), and an upgrade between two different releases.

### Changed

- **The hand-fed VM publishing path is removed**: `vm-artifact-stage.yml` read a directory an operator had
  filled on the runner, and `vm-artifacts.yml` took a run id and two hashes typed by hand. Both are gone.
  `vm-artifacts.yml` is now a reusable workflow that only `vm-artifact-export.yml` calls, and it accepts
  one producer: the successful `export` job of the same run, at the same commit on `main`
  (`images/vm_publish_stage.py`). `images/vm_stage_local.py` is deleted. Images published earlier stay
  where they are; an export through the workflow gets a new tag. The runner label
  `proxcloud-vm-artifact-stage` is no longer used.

- **Smaller template disks: 5 GiB for native OS templates, 8 GiB for Kubernetes node templates** (both were
  20 GiB). The built images hold 0.4 to 1.3 GB (native OS) and about 4 GB (Kubernetes node), and every clone,
  copy and sanitize run paid for the whole 20 GiB. A server or node made from a template still gets the root
  disk size its user asks for: cloud-init grows the filesystem at first boot. One constant per builder
  (`DEFAULT_BUILD_DISK_GIB` in `images/build.py`, `K8S_BUILD_DISK_GIB` in `images/k8s-build-config.py`).
  - **A server with a root disk below 20 GiB now gets that size.** With a 20 GiB template a smaller request
    (the seeded `s1-small` flavor has 10 GiB) still produced a 20 GiB disk and counted 20 GiB against the
    quota; with a 5 GiB template it produces and counts 10 GiB. The create page's minimum root size drops
    from 20 to 5 GiB.
  - The minimum disk of an image is the largest of its per-node template copies, not the newest copy's. A
    cluster where one node has the 5 GiB copy and another still has the 20 GiB copy keeps a 20 GiB minimum:
    before, a 10 GiB server placed on the second node passed the check, counted 10 GiB and got 20 GiB.
  - The sanitizer and the export tools accept an explicit allowlist per variant instead of one fixed size:
    native OS 5 GiB or 20 GiB, Kubernetes node 8 GiB or 20 GiB. Any other size is refused, the export must
    keep exactly its source's size, and the facts and the manifest carry the actual size. The cold-boot
    acceptance requires the import to have the artifact's own size (reason `import_size_mismatch`, formerly
    `import_not_20g`), then still grows it to 30 GiB and checks the growth in the guest. The Kubernetes
    review checks the source size against the allowlist, not against the 20 GiB recorded in the committed
    baseline.
  - The artifact contract's minimum virtual size is 5 GiB (was 20 GiB) in `images/vm_artifact.py` and in the
    portal's Template updates import.
  - **Compatibility:** portals up to v0.5.2 refuse an image whose virtual disk is below 20 GiB (invalid
    artifact contract, nothing is downloaded), so an image built with the new sizes needs a portal with this
    change. Existing 20 GiB templates and the already published 20 GiB images stay valid; nothing is rebuilt
    or resized. **Upgrade the portals before the third small image of a line is published:** a portal up to
    v0.5.2 checks only the newest three tags of a line, so after the third 5 or 8 GiB image it offers nothing
    on that line, and the older 20 GiB image is importable only by an API call that names its digest.
  - A build that runs out of disk space still fails: the provisioning scripts stop at the first failing
    command, and the last cleanup step of both builders now refuses a root filesystem with less than 1 GiB
    free (in both network modes of the Kubernetes build). The new sizes were not yet confirmed by a lab build; what the first build of each image type has
    to confirm is listed in [docs/template-build-disk.md](docs/template-build-disk.md).

- **Helm chart: the realms derived by `keycloakConfigCli` have Keycloak's brute force detection on**
  (`bruteForceProtected`, Keycloak's default thresholds: a temporary lockout after repeated failed logins). Their
  token endpoints are public and the console clients allow the password grant. Applied at the next
  `helm upgrade`, and by the Compose install, which renders the same realm files.

### Fixed

- **Native OS images kept the build VM's snapd identity**: the offline sanitizer reset the snapd device
  key and serial only on the Kubernetes path. The native Ubuntu path now does the same reset and its
  final check refuses a remaining key, serial, session macaroon or store user.

- **Kubernetes 1.37 node templates did not build**: the pinned image-builder `v0.1.55` renders its kubeadm
  config with `kubeadm.k8s.io/v1beta3`, which kubeadm 1.37 refuses ("old API spec"), so
  `kubeadm config images pull` failed the build. The build now applies upstream's own fix (image-builder
  `db2e0f41`, after `v0.1.55`) to the exported pinned tree: `v1beta4`, read by kubeadm since 1.31, for every
  selectable version. The pinned file's hash is checked first; any other content refuses the build.
  Found on the first 1.37.2 lab build.

## [0.5.2] - 2026-11-14

Patch release on top of `v0.5.1`, with no breaking change. It fixes VM template smoke-test and import
cleanup on PVE 9, where the least-privilege token gets 403 instead of 404 for a VM outside its pool
(found on the first real import + smoke run, a homelab running PVE 9.2.21). Everything `v0.5.0` and
`v0.5.1` say is **not** verified still holds.

### Fixed

- **A VM template smoke test that passed ended `dead` in `cleaning` on PVE 9**: the provider token reaches a VM
  only through its pool, so after the smoke job destroyed its test VM, PVE answered the follow-up config read with
  `403 Permission check failed (/vms/<id>, VM.Audit)` instead of "does not exist", and the job retried the
  already-done cleanup until its attempts ran out (the result, PASS, was kept; the dead-job pass would have
  retried every 10 minutes). The import and smoke jobs now treat a 403 for a VMID that is not (or no longer) a
  member of `pc-templates` (on any node) as out of view: gone, or never theirs, and never touched. A 403 for a
  member, or with a pool or task list that cannot be read, is still an error. PVE adds a created or cloned VM to
  its pool only when the task ends, so the import job also keeps the error while its VMID's create task runs, and
  the smoke job while a clone of the template runs; the task is checked before the pool, so a task that ends in
  between has already joined it. A clone not yet started when the check runs (a superseded attempt) is left to the
  orphan sweep.
  Found on the first real import + smoke run (PVE 9.2.21).

## [0.5.1] - 2026-11-14

Patch release on top of `v0.5.0`, with no breaking change. It fixes the VM template import, which
failed on PVE 9 with the documented least-privilege token (found on the first real import, a homelab
running PVE 9.2.21), and lets the image publication bridge accept Kubernetes node templates 1.36.6 and
1.37.2 next to 1.35.10. Everything `v0.5.0` says is **not** verified still holds: both template-update
flags default **off**; the VM import and its smoke test are still not accepted on PVE 9.2 (the fix was
reproduced and verified against one real PVE 9.2.21 cluster, the full import + smoke run follows the
release); the sandbox mirror is not lab-verified; and live installer (P6) and split/managed-service
(P7) acceptance were not performed. The security gate policy is unchanged.

### Changed

- **Public VM artifacts: Kubernetes node templates 1.36.6 and 1.37.2** are accepted for publication next to
  1.35.10 (Ubuntu 26.04 only), so clusters can be tested on an upgrade path 1.35 → 1.36 → 1.37. The publisher,
  the offline sanitizer and the copy policy share one explicit allowlist (`K8S_VERSIONS`), kept equal by a test;
  the copy policy now requires `kubeadm`, `kubelet` and `kubectl` at exactly the reviewed version. Any other
  version is still refused. Each new version still needs its own build, inventory and runtime acceptance.

### Fixed

- **VM template import failed on PVE 9 with the least-privilege token**: the import created its VM with
  its ownership tag in the create call, and PVE 9 checks a create's tags with `VM.Config.Options` on
  `/vms/<id>` without the pool, which the provider token never holds (its rights reach a VM only through
  `pc-templates`). The create task failed `403 Permission check failed (/vms/<id>, VM.Config.Options)` and
  the job retried until it gave up. The VM is now created untagged and tagged by the import write; until
  then it is the job's only by its exact build name, membership of `pc-templates` and the absence of every
  tag, and the orphan sweep recognises such a VM too. Found on the first real import (PVE 9.2.21).

## [0.5.0] - 2026-11-13

Second stable ProxCloud release, after `v0.4.0` (owner decision 2026-11-13: this is
`v0.5.0`, not `0.4.1`). It adds portal template updates from GitHub Packages with a
GitHub classic PAT integration and a staged → smoke test → publish/withdraw
lifecycle, sandbox template image updates mirrored from GHCR into the sandbox
registry (off by default), a per-provider attach policy and bulk attach of a provider
to existing projects, a real month-end cost forecast, and Kubernetes node-image
sanitizer fixes for the real template. It has **breaking API and CLI changes**, listed first below;
review them before upgrading scripts, clients or installer inventories.

What is **not** verified: `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` defaults **off**,
and the VM import and its smoke test have **not been lab-verified on PVE 9.2**; the
GitHub classic PAT integration has **not been verified against the real GitHub
packages**. Sandbox image mirroring from the portal is included but
`PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED` defaults **off**, and it has **not been
lab-verified** on a real sandbox cluster with Zot and GHCR (copy, digest check, prepull
rollout, adoption, the stage-55 admission-policy probe). It pulls only the **public**
`sandbox-*` packages, with an anonymous pull token; the GitHub PAT never reaches the
sandbox cluster. With the flag off, sandbox template updates stay on the manual
`deploy/templates/images.yaml` pin path. **Upgrade note**: the first `make upgrade`
with this release restarts the sandbox registry (Zot) pod once (its users Secret gains
the `mirror` account); pulls fail for the restart's few seconds.
As for `v0.4.0`, "stable" does not mean full acceptance: live installer (P6) and
split/managed-service (P7) acceptance were **not** performed, the job scheduler and
debug triggers stay off by default, and production mail delivery and the
managed-sandbox saved lifecycle remain held. The security gate policy is unchanged
from `v0.4.0`: HIGH/CRITICAL findings without an upstream fix are tracked, not
blocking, until 2026-12-12, and the single VEX expires on 2026-12-31. Upgrading a broker-era installation still needs the `v0.4.0`
drain/reconcile and retention runbook. The `v0.5.0` tag publishes the images and app
chart `0.5.0`; verify that they can be pulled before installing with
`images.build: false`. The managed charts are unchanged (`database-cluster` `0.5.1`,
`proxmox-kamaji-cluster` `0.1.10`).

### Breaking changes

- **Attaching a provider to a project is `POST` (#214).** The admin attach is
  `POST /api/v1/admin/project/{id}/external-service/{externalServiceId}`. `GET` and
  `HEAD` on that path answer `405` and change nothing; the old `GET` provisioned the
  project on the provider. The attach stays idempotent: re-attaching a service the
  project already carries makes no provider call and adds no second binding. Scripts
  calling the old `GET` must switch to `POST`.
- **A named service the project is not attached to is refused (#215).** A project
  cloud call that names a service (`x-service-id` or `?serviceId=`) the project is not
  attached to now answers `400 Project is not attached to service <id>` (`…to the
  requested service` when the id is not a plain service id) before any provider is
  called, instead of silently running on the project's Proxmox service (a BUCKET
  create for a newly registered S3 store failed on Proxmox). This covers creates,
  actions, deletes and the reads `quota-usage`, `gpu-capacity`, `public-networks` and
  the S3 key routes. Requests naming no service keep the Proxmox default (else the
  project's first attached service). Existing projects still need an operator to
  attach a provider registered after they were created (per project, or with the bulk
  attach below).
- **`POST /api/v1/project/{id}/init` can answer `409` (#216).** It no longer answers
  `200 {}` when it leaves the project down. It answers `409` with `errors.details`
  `{status, reason[, billingProfileStatus]}`, reason one of
  `billing_profile_inactive`, `billing_profile_suspended`, `billing_profile_missing`,
  `disabled_by_operator`, `scheduled_for_deletion`, `deletion_in_progress`, `deleted`,
  `not_enabled`; `billingProfileStatus` is present when a paying billing profile
  exists. Success is unchanged. The customer API error envelope may now carry an
  optional `details` object; clients must ignore details they do not know. The client
  console shows the reason above the project's pages, with a link to Billing where
  that is the fix.
- **VOLUME create answers the created volume (#218, shipped in #220).**
  `POST /api/v1/project/{id}/cloud` with `{"type":"VOLUME"}` answers the volume's cache
  row: `id`, `externalId` (the PVE volid) and `data.volume` (`name`, `size`,
  `status`, …), so the caller can use the volume at once. It used to answer an empty
  `result` (create on a server) or `{"result":{"node":…}}` (standalone create); the
  standalone create **no longer returns `result.node`** (the node is in
  `data.volume.availability_zone`). If the new row cannot be read back yet, the answer
  is the volume as the create knows it, without an `id`.
- **Publishing at import is removed (#223).** `POST /api/v1/admin/template-updates/vm`
  with `"publish": true` is refused with `400` (`errors.details.code =
  publish_at_import_removed`) before any provider or GitHub call; an absent or `false`
  `publish` is ignored. Every import lands **staged**, tagged
  `pc-tu-import;pc-tu-staged` without any catalog tag, until it passes its smoke test
  and is published.
- **`images/build.py --keep` must be 1–999 (#223).** `--keep 0`, which used to prune
  nothing, is now refused with a clear message; the installer's preflight checks
  `proxcloud.templates.keep` the same way (default 2, unchanged).
- **The per-project admin attach refuses projects being deleted (#222).** A project
  scheduled for deletion, being torn down or deleted gets
  `400 Project is being deleted. Cannot attach a provider` instead of being
  provisioned.

### Added

- Add **System → Template updates** to the admin portal: a GitHub integration holding a
  classic personal access token (`read:packages`, validated with GitHub before it is saved,
  encrypted and write-only) that every GitHub Packages call uses — no anonymous listing; the
  newest published VM artifact per OS/variant/Kubernetes line and the newest sandbox image per
  template, content-checked against the OCI and `manifest.json`/`SHA256SUMS` contract before
  being offered, next to what each provider has installed or pinned. A VM artifact can be
  imported by digest as a new Proxmox template (never modifying or deleting existing ones) via a
  fenced, resumable durable job, verified by a streaming SHA-256, Proxmox's own sha256 check and
  the layer digest; the executor is off until `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`.
- Sandbox template images can be updated from **Template updates** (`docs/template-updates.md` §5,
  off until `PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`): **Update** mirrors the offered
  `ghcr.io/mutex-solutions/sandbox-<name>@<digest>` (re-verified) into the sandbox registry as
  `library/sandbox-<name>:<tag>` with the same digest — a fenced, resumable durable job running the
  trusted `sandbox-import` worker in a new copy mode with pod-owned credentials, the destination digest
  checked by the portal — then points the prepull DaemonSets at it; the existing adoption job switches
  templates once every Ready node has pulled it. **Rollback** points the prepull back at the previous
  pin. Same-digest updates are refused, one job per image at a time, audited
  (`admin:service:manage`). Installer: stage 55 creates a Zot `mirror` account (read/create on
  `library/sandbox-*` only), a ValidatingAdmissionPolicy limiting the portal to init-container image
  changes to mirrored digest refs of the same repository and, only once a dry-run probe shows that
  policy enforced, a Role (get/patch on the two prepull DaemonSets); stage 60 hands the account over as the write-only `secret.registryMirror`. Each switch is
  recorded as a `sandboxBuiltinPin`; `make upgrade` reads the pins through the admin API and renders
  the newer of `images.yaml` and the pin, printing the line to commit, and stops (instead of rolling
  back) when it cannot read them; `DELETE /api/v1/admin/template-updates/sandbox/pins/{id}` lets
  `images.yaml` win again. The worker gets only an anonymous short-lived GHCR token pull-scoped to the one public package, checked never to be or contain the GitHub PAT (used for discovery only; private packages are refused, `ghcr_package_not_public`). The admission policy compares fields by name, so stage
  55 first checks the sandbox cluster's OpenAPI schemas and stops the upgrade on any field the policy
  does not cover; prepull init containers now run non-root with a read-only root and no capabilities.
  **Upgrade note**: the first `make upgrade` with this change restarts the sandbox registry (Zot) pod
  once, because its users Secret gains the `mirror` account; pulls fail for the restart's few seconds.
  Residual risk: the `mirror` account may create a new `library/sandbox-<name>` repository (it cannot
  overwrite or delete an image). With the flag turned off, a sandbox job that is queued, or whose
  runner is gone (expired lease), ends `failed` `feature_disabled` (or keeps a failure code already
  recorded; one already pinned is acknowledged as succeeded; one whose lease expired on its last attempt
  ends `job_dead` through the dead-job pass) without running further: its worker pods
  and their Secrets are deleted and a pin whose prepull switch never landed is restored, so it no
  longer blocks its image or the pin delete; a job another replica (flag on) holds is not touched.
  A failed job keeps its image busy (a new update or a pin delete answers 409) until its cleanup
  has run; that cleanup (the job's worker pods, a partial prepull switch, the pin) needs only the
  provider's cluster, so it also runs when the provider lost its mirror account or moved its
  template registry.
- Add a per-provider **attach policy** for new projects: `config.attachToNewProjects`
  (default `true` when absent, so every existing provider keeps attaching new projects). With
  it off, a new project's first bootstrap skips that provider. Set it on the provider's new
  **Projects** tab or with `PUT /api/v1/admin/service/{id}/attach-policy`, a single-key write
  that changes nothing else and does not race the connection save. A non-boolean value is
  refused there and on the provider create/connection save.
- Add **Attach to existing projects** (follow-up of #215): from a provider's Projects tab,
  attach it to all projects, the projects of picked organizations, or picked projects. It runs
  as a fenced, resumable PostgreSQL durable job (`providerattach.run/v1`), at most one per
  provider, with 1–4 projects at a time and at most 5000 projects to attach per run (planned
  in resumable batches), and goes through the same path as the per-project attach. Already
  attached projects are skipped with no provider call; projects being deleted are skipped;
  suspended projects are attached and stay suspended. The provider is re-read per project: one
  disabled or deleted mid-run ends the run and the rest can be retried. Results per project use
  fixed codes only and are kept 30 days. The tab shows progress, the failed projects and their
  reason, and offers cancel and retry-failed. Start, cancel and retry are audited
  (`BULK_ATTACH`, with actor, provider, scope and counts). Start and retry need
  `admin:service:manage` and `admin:project:update`, cancel `admin:service:manage`; reading
  needs `admin:service:read` (and `admin:project:read` for the project picker and preview).
  API: `/api/v1/admin/service/{id}/bulk-attach…` (`docs/cloud-integration.md`).
- Add the **template lifecycle** to Template updates (`docs/template-updates.md` §4): every import lands
  *staged* (hidden); an automatic, durable **smoke test** boots a throwaway clone in pool
  `pc-templates` on the provider's smoke network (`config.templateSmoke`), with fresh cloud-init and an
  ephemeral SSH key, checks the guest agent, `cloud-init status`, root-disk growth (and kubelet +
  containerd for Kubernetes node images) and always destroys the clone; **Publish** (only after a PASS
  of exactly that template and digest) makes the newest version the default for new VMs and keeps older
  ones; **Withdraw** (reason required) takes a bad version out of the catalog — falling back to the
  previous one — keeps its template, and blocklists its digest for import, smoke and publish. Withdrawing
  the last published version of an OS needs an explicit forced confirmation. A withdraw covers every
  provider holding a copy of the digest; a copy that could not be changed yet (provider unreachable)
  shows *Withdraw incomplete* and the same action finishes it. A publish requires the template to be
  unchanged since its passing smoke test. All actions need
  `admin:service:manage`, are audited, and stay behind `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED`. The
  admin page gains an *Imported templates* tab with the state of each template and its actions.
- The first public Kubernetes node VM image,
  `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` (tag
  `ubuntu-2604-k8s-1.35.10-20261112032843-8a2a737b331b`), was published on 2026-11-12
  through the manually dispatched VM artifact publisher. It is a separate QCOW2 artifact for
  reviewed hypervisor import: it is not part of the app chart, the installer BOM or the
  `v0.5.0` tag. No kubeadm join or NodeReady is claimed for it; node acceptance stays
  the runtime checks in `docs/public-vm-sanitization.md`.

### Changed

- `images/build.py build` never prunes a template the portal imported (marker `pc-tu-import`, or a
  `pc-src-` tag from an earlier import), nor the newest published version of an OS on a node.
- `pve.DiscoverTemplates` skips templates tagged `pc-tu-withdrawn`, even when `pc-public` is present.

### Fixed

- The per-project admin attach runs for at most 2 minutes (its refusal of a project
  being deleted is listed under Breaking changes). A project's bindings are now only
  appended atomically: a project save (rename, member change, activation) no longer
  rewrites `services` or `provisionFailures` from a stale read,
  so it cannot drop a provider binding added meanwhile. Operator attaches and the first
  bootstrap of one project are serialized under a per-project lock; the binding write runs
  on the lock's own session with the ownership check in the same statement, so a writer
  that lost the lock writes nothing. Attaching never stores a copy of a Proxmox provider's
  default quota on the project: as before, a project without its own quota follows the
  provider default live. An operator attach of a project never opened marks its binding in the same write, so
  the project's first open still provisions every provider whose policy is on.
- The month-end forecast is now a real projection instead of always equal to the
  current month's cost (#217). `forecastedMonthEndCosts` in `GET /project/{id}/cost-info`,
  `GET /bill/{bp}/cost-info` and `GET /admin/billing-profile/{id}/cost-info` is the
  current cost plus what the charge job will still add before the UTC month end for the
  resources it bills now, rated read-only with the same engine and rules. Tax,
  savings-contract discounts, price-adjustment rules and future price or resource
  changes are not modeled. `forecastApproximate`, `forecastApproximateReasons`
  (`unrated`, `usage`, `stale`, `unavailable`, `external_billing`) and
  `forecastUnratedTypes` say when part of it is held at its current cost. A resolution
  is cached 60 s per profile and month and bounded to 3 s; a timeout, error or a profile
  with more than 2000 billable resources gives `unavailable` (forecast = current), never
  a failed read. The admin profile list keeps forecast = current, flagged approximate.
  The consoles label the figure as an estimate and explain why it is approximate
  (`docs/billing.md`).
- The client console's server field helpers moved out of the Servers page module, so
  the page hot-reloads with React Fast Refresh during development (#218).
- Kubernetes node-image sanitizer (`--k8s-os ubuntu-2604`) fixes for the real Ubuntu
  26.04 / Kubernetes 1.35.10 template, which the previous code refused (fail closed) on
  a read-only inventory (#219):
  - only the exact canonical `/dev/disk/by-uuid/<uuid>` root fstab spec is resolved,
    and it must be the inspected root; dpkg `hold ok installed` is accepted for the
    pinned Kubernetes packages; only active swap rows count; only an empty
    `.kubelet-keep` placeholder is ignored in the static manifests;
  - machine identity is reset in the copy (owner decisions): `builder` leaves every
    supplementary group and must own an unshared primary group, the sudo policy is
    bound to reviewed hashes (any `/etc/sudoers-rs`, active `/etc/sudo.conf` line or
    userdb record refuses), snapd is reset to unregistered (a session macaroon or a
    store user refuses), and the cloud-init default user is the upstream `ubuntu`;
  - the inventory mounts the root read-only, and the root is remounted read-write only
    after the policy accepts.

  The template apt-holds `cloud-init`, `openssh-server`, `qemu-guest-agent`, `snapd`
  and `kubernetes-cni` besides the Kubernetes packages; consumers must
  `apt-mark unhold` them to receive security updates. Runtime acceptance (cold boot,
  two-clone snapd re-registration) is not claimed by this source change.

## [0.4.0] - 2026-11-12

First stable ProxCloud release, superseding the `v0.4.0-rc.1` prerelease. It is
broker-free: fresh installs need only a writable PostgreSQL primary for application
data and durable jobs, with no RabbitMQ. It builds with Go 1.27.4 and adds a
fresh-scan security gate. "Stable" here does not mean full acceptance.
Live installer (P6) and split/managed-service (P7) acceptance were **not**
performed; the owner deferred them. The job scheduler and debug triggers stay off by
default. Production mail delivery and the managed-sandbox saved lifecycle remain
held. Existing broker-era installations must explicitly drain/reconcile and retain
their RabbitMQ data before upgrading, because the new API does not consume old
queues. Under the security gate policy, HIGH/CRITICAL findings without an upstream
fix are tracked, not blocking, until 2026-12-12. Raw upstream bases are report-only.
The GPU/CUDA code-interpreter template is neither built nor scanned by the gate.
The `v0.4.0` tag publishes the images and app chart `0.4.0`. Verify that they can
be pulled before installing with `images.build: false`.

### Security

- Add a fresh-scan security gate (`security-scan` workflow) for every PR and `main`
  push. It checks out the exact head commit, not GitHub's synthetic merge commit. It
  uses a checksum-verified Trivy 0.76.2 binary and a vulnerability database that must
  be less than 24 hours old. Scan targets:
  - source lockfiles, including development dependencies;
  - six locally built product images: API, customer and admin consoles, sandbox
    proxy, runtime and importer;
  - eleven non-GPU sandbox template outputs;
  - thirteen deduplicated upstream base/builder/donor images, read as registry
    blobs and never executed;
  - a nondefault controlled-base prototype.

  Nothing is pushed or published. A required `security-gate` job needs every scan
  leg to succeed. On PRs, a decorated, size-bounded comment lists the CVE tables for
  each target.
- Apply the owner-approved gate policy (2026-11-12):
  - fixable HIGH/CRITICAL findings and end-of-life image OSes block;
  - HIGH/CRITICAL findings without an upstream fix are listed and counted as
    tracked, but do not block until 2026-12-12 (UTC); after that date they block
    again until re-reviewed;
  - raw upstream base findings are report-only, but missing, stale or invalid base
    scans still fail;
  - exactly one expiring VEX (until 2026-12-31) covers CVE-2025-47273, where
    setuptools 70.3.0 appears only in pip 26.3's vendored SBOM and the vulnerable
    module is not shipped.

  No other ignore, allowlist, suppression or severity change exists.
- Remediate fixable HIGH/CRITICAL findings in the eleven non-GPU built-in sandbox
  templates and ship the rebuilt templates as this release's digest pins. Every rebuilt template
  applies its distribution's available package updates. Per template:
  - base: Node 22.23.5 with npm 11.21.2, corepack 0.36.0 and yarn 1.22.22 replaces
    the obsolete Node/npm payload; pip 26.3, setuptools 84.2.1, wheel 0.48.0 and
    packaging 26.3 are upgraded;
  - node: moves to npm 11.21.2 (undici 6.29.1, brace-expansion 5.0.12);
  - browser: upgrades setuptools 84.2.1, virtualenv 21.15.6 (bundling pip 26.3),
    certifi, filelock, msgpack and urllib3, and drops the stale virtualenv seed
    cache;
  - code-interpreter: moves to Node 22, npm 11.21.2 and pip 26.3, and smoke-tests
    every installed kernel during the build;
  - mcp-gateway: installs its npm servers as one private project with
    `@modelcontextprotocol/sdk` 1.32.2 and `basic-ftp` 6.2.6 overrides, and the
    build checks every installed copy. It also refreshes the Node 22 donor and
    moves to uv 0.13.4.
- Refresh `libxslt` in both console images (CVE-2026-87636, CVE-2026-9947). Pin
  fixed transitive brace-expansion, fast-uri and systeminformation versions in both
  SPA lockfiles, and nanoid and source-map-js in the customer console. The sandbox
  runtime donor moves from BusyBox to an upgraded Alpine 3.24.4, so its BusyBox/musl
  packages are visible to scans.

### Changed

- Build and test with Go 1.27.4, the current stable release, in all six release Go
  builder stages (now `golang:1.27.4-alpine`) and the hosted test workflow. The module
  minimum rises to `go 1.26.0`, which Testcontainers 0.45 and `golang.org/x/crypto`
  0.58 require, so source builds with Go 1.25 no longer work. The compiler pin and
  the module minimum remain separate, tested declarations.
- Update Testcontainers core and PostgreSQL modules to 0.45.0 and `golang.org/x/crypto`
  to 0.58.0 (with `x/net` 0.62.0 and `x/sync` 0.24.0) as one coherent graph. Indirect
  modules move too, for example go-jose 4.1.6, docker/cli 29.2.0, `x/sys` 0.49.0 and
  `x/text` 0.43.0. pgx 5.12.0 and the AWS SDK versions already on `main` are kept.
  This supersedes the separate dependency updates in #182, #183 and #32.
- Move the API runtime image from Debian bookworm-slim to an upgraded Alpine 3.24.4.
  The image still runs as UID 10001, and the Go binary stays static. It ships bash
  and the PostgreSQL 18 `psql` client for config-as-code seeding with password or
  TLS authentication. That client cannot seed through Kerberos/GSSAPI; for that, use
  a suitable `psql` elsewhere, such as in the database pod.
- Run the tagged build-archive integration test in hosted CI.

### Fixed

- Grow only a freshly created native OS-template base to a verified minimum 20 GiB
  before template conversion or first-boot package provisioning. Preserve larger
  upstream disks, validate exact ownership/digest/readback and bound failure cleanup
  to the confirmed new VM; do not resize existing templates or tenant disks.
- Kubernetes node-template builds:
  - The final cleanup provisioner returns without shutting the guest down. Packer
    can then remove its temporary script before the Proxmox builder stops and
    converts the VM (#204).
  - After a successful build, only this run's final template is discovered. The
    search polls for at most 120 seconds and rejects pre-existing VMIDs and any
    ambiguous or mismatched candidate (#208).
- Before building, the OS, Kubernetes and sandbox template workflows disable any
  inherited sparse checkout and verify that the full source is present (#206).

### Added

- Export the Ubuntu 26.04 / Kubernetes 1.35.10 node template line through the
  public VM image pipeline as `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s`.
  The export has its own `--k8s-os ubuntu-2604` sanitizer path and
  `vm-k8s-export-ubuntu-2604-…` artifact name:
  - The only input is the raw 20 GiB root disk. Its hash is bound to the
    parent-reviewed source before any image parser runs. EFI vars and cloud-init
    media are never exported.
  - A pure copy policy checks a read-only inventory before any write. Unknown or
    unexpected facts make it refuse rather than delete. These include existing
    cluster state, swap, registry credentials, proxies, lab addresses,
    systemd/snapd secrets, unknown accounts and unreviewed cloud-init drop-ins.
  - Kubelet enablement and the exact Kubernetes packages are preserved.
  - The publisher refuses any other Kubernetes OS or version, and any mix of
    native and Kubernetes names and manifests.

  This release does not include runtime acceptance of the Kubernetes image (real
  inventory, sanitize, postscan, cold boot, anonymous pull). Its publication is a
  separate, manually dispatched step that the `v0.4.0` tag does not perform.

- Define portable native VM-disk QCOW2 artifacts for public GHCR distribution,
  with strict versioned manifests, checksums, unsigned exporter facts and explicit
  sanitization/pull/import/boot gates. Add a copy-only offline sanitizer and a
  manual sealed three-file staging → hosted ORAS publisher bridge, without giving
  the image runner PVE-root or package-write access. Publication plumbing does not
  imply a public artifact exists; package visibility and anonymous retrieval remain
  separately verified.
- Add evaluation-only security recipes. None of them is integrated into a default
  image or published, and none waives a scan finding:
  - an OpenSSH client backport (CVE-2026-60002);
  - a source-built GitHub CLI;
  - an MIT krb5 1.22.3 source build;
  - a nondefault controlled base-template prototype with five optional variant
    recipes.

  The OpenSSH, GitHub CLI and krb5 workflows run only on PRs that touch them and
  produce artifacts only.

- Add native OS-template choices for Ubuntu 26.04 and Debian 12 while retaining
  Ubuntu 24.04 and Debian 13. New Kubernetes node builds select Ubuntu 26.04 by
  default, retain explicit 24.04 and verify the exact image-builder v0.1.55 commit.
  Bind OS target, seed, checksum, names and tags; export immutable upstream objects
  instead of trusting cached worktree bytes. Debian Kubernetes nodes remain unsupported
  by this pinned Proxmox route. Keep serial canary and cleanup/publication gates.

## [0.4.0-rc.1] - 2026-11-11

First broker-free ProxCloud release candidate, **not a stable release**; published
as a non-latest GitHub prerelease. The five source security fixes cover SMTP
credential TLS, stored-profile order read authorization, custom-role assignment
permission, native sandbox traffic tokens and exact SSO alias retirement; details
and limits are below. This is not live installer (P6) or split/managed-service (P7)
acceptance. Scheduling remains
off, mail delivery and saved sandbox lifecycle remain held, and existing broker
data still requires explicit migration/retention evidence.

### Changed

- Offer NGINX Gateway Fabric 2.8.1 with Gateway API Standard 1.6.3 for new managed
  Kubernetes clusters (tenant chart 0.1.10). Keep the controller opt-in, users owning
  application Gateway/Routes/TLS, and existing ingress-nginx traffic unchanged.
  Validate actual chart/Kubernetes capability before effects, preserve sparse-client
  addon choices and refuse implicit legacy controller migration or downgrade.

- Refresh installation, developer, product-help, CI/release and support documentation
  against current configuration. Distinguish preview/source-build state from
  versioned publication and live acceptance, and document static template networking,
  scheduler defaults and intentional sandbox safety holds. No release is created by
  this documentation update.
- Replace application RabbitMQ dispatch with primary PostgreSQL durable native
  billing jobs: frozen original cycles, fenced at-least-once workers, atomic
  all-provider financial receipt/ACK, transactional enqueue, bounded retry/dead
  letters and audited operator replay. Reject old fanout flags until legacy queues
  are drained/reconciled; scheduler stays off by default. Optional read-only pool
  serves lag-tolerant history only. Fresh Compose/chart/installer defaults are
  broker-free; existing broker state still requires explicit drain/reconcile and
  retention. Bounded mail outbox machinery and opt-in enqueue producers are
  separate from production delivery, which remains disabled.

### Fixed

- Select Node 24.22.2 and stable NGINX 1.30.6 for both frontend image recipes,
  align the hosted Node patch, and guard the declarations in data-only CI tests.
  Preserve app locks, static serving and runtime public-config behavior; distinguish
  actual musl/runtime checks from source pins, image builds and live acceptance.

- Select supported Go 1.26.11 explicitly for all six Go builder stages and the
  test workflow, keeping the module language minimum and dependency graph separate.
  Add source-declaration regression coverage and document the seven output artifacts,
  external envd inputs, image-build gaps and remaining runtime acceptance limits.

- Update managed PostgreSQL 15–18 image-map patch targets to 15.19, 16.15, 17.11
  and 18.6 in unpublished database chart 0.5.1. New installer DBaaS providers
  offer 17/18 with 18 as default; existing name-matched providers and database
  chart pins/data remain untouched. Require actual offline Helm/catalog regression
  checks in CI; no existing PostgreSQL major upgrade is performed.

- Pin both frontend Vite builds to security-floor patch 8.1.6 and installer
  ansible-core to 2.18.18 without unrelated lock graph churn or live installer
  execution. These narrow fixes do not certify other dependencies or runtime images.

- Persist exact organization SSO alias retirement on the primary before broker
  deletion, and refuse retained sessions even when no configured providers remain.
  Keep legitimate realm-level social/local sign-ins compatible, make cleanup retries
  idempotent and document mixed-version, cached-rule outage and recovery limits.

- Bind client order reads to the stored billing profile's current organization
  read permission, return an opaque 404 when denied and serialize a typed order.
  Require role-management permission for all custom organization role assignments
  while preserving ordinary MEMBER invitations; reject unknown roles and malformed
  role requests. Private native sandbox services on port 49983 now require their
  traffic token; only actual envd-serving pods retain envd's own-auth exemption.
  Add native repository and proxy regression coverage; no held feature is enabled.

- Require verified configured-host TLS before legacy SMTP credential authentication,
  including LOGIN-only and AUTH-stripped relays. Guard LOGIN independently against
  plaintext and host mismatch; preserve explicit NoAuth relay compatibility and
  PLAIN preference. No outbox delivery, relay configuration or live send is enabled.

- Protect full and sampled Proxmox SERVER/VOLUME/IMAGE reconciliation from
  filtered, malformed or unreadable VM inventories. Same-credential scoped proof
  and whole-row CAS preserve cache/history on unknown reads; identity retirement
  requires complete existing ACL/node authority. Delay protection healing until
  proof succeeds. Orphan IP cleanup now requires authoritative VM/address non-use
  and exact allocation/archive receipts instead of age or bulk NIC release;
  ambiguous reuse, other guest NICs and legacy/BeforeRow allocations stay held.
- Fail closed on permission-filtered PVE NETWORK inventories: prove effective
  same-token visibility and revalidate omitted networks/subnets before cache
  writes, preserving tenant rows and NAS exports on unknown results while genuine
  delete-of-vanished still archives normally. Surface typed repair warnings.
- Refuse conflicting TrueNAS provider owners on the same project's NAS backend and
  parent dataset before share/export mutations or duplicate share caching, rather
  than letting different provider settings repeatedly rewrite the same exports.

Before `0.4.0-rc.1`, ProxCloud was an untagged development line: a self-service
private-cloud and billing portal for Proxmox VE. History from the product this
codebase was forked from is not carried over.
