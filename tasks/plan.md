# Audit remediation plan

## Status and source of truth

Prepared on 2026-10-15 for the audit of commit
`e6efaca96e1bb4ca073c9c62df67f8ba9766ba0f`.

The initial request authorized creating issues and this plan. Implementation was
subsequently authorized; current completion and evidence live in GitHub Issues.
Start new implementation only within the user's authorized scope. Once a scope is
approved, continue within it without repeatedly requesting approval.

Tasks and their acceptance criteria live in
[GitHub Issues](https://github.com/anesthetised/lotsman/issues). This file is the
execution order and coordination guide, not a second completion checklist.
Issues #6 and #7 already existed and are reused; #8 through #22 were created for
the remaining audit findings. Read the current issue before starting: its state or
the implementation may have changed since this snapshot.

## Approved follow-up slice (2026-10-16)

The user approved nftables peer authorization for #14 and disabling the downstream
interface before teardown for #8. Implement these sequentially from current main,
with packet regressions in disposable containers. No push, PR, or benchmark is
part of this slice. The earlier authorization paragraph describes the original
plan preparation, not this follow-up.

- #14: the Linux firewall starts with no authorized peers. `Route` grants access
  after installing rules; `Unroute` revokes access before deleting rules. Failed
  rule cleanup remains retryable while the peer stays blocked after revocation.
  Local verification: `just test`, `just lint`, explicit gofmt check,
  `just test-integration`, and amd64/arm64 builds passed. The packet regression
  failed on the original implementation and when the outbound authorization
  check was removed in a disposable review copy. It covers never-assigned,
  revoked, reauthorized, and repeatedly failed rule-cleanup cases. Kernel rule
  deletion failures were injected at the call boundary, not produced by Linux.
- #8: Linux `Teardown` must disable downstream ingress before removing protection;
  failure to disable it preserves rules and returns an error. The daemon closes
  downstream and retries cleanup once before closing upstreams, preserving route
  ownership evidence. Packet tests cover the boundary after teardown but before
  downstream close, including partial setup, and inspect protection before the
  link-down call. Disable failures are injected at the call boundary, not produced
  by Linux. Lifecycle tests cover zero, one, and repeated teardown failures.
  Local verification: `just test`, `just lint`, explicit gofmt check,
  `just test-integration`, and amd64/arm64 builds passed with both fixes applied.
  The shutdown packet regression failed before the fix for complete and partial
  startup. In disposable review copies, skipping link-down broke the packet and
  preservation tests; restoring the old daemon stop broke the cleanup retry tests.
  The existing routing-ownership and live-flow-pinning regressions also passed.

## Approved remaining audit work (2026-10-16)

The user authorized parallel follow-up work on #10, #13, #19, #20, #21, and #22.
Use GPT-6-Luna at medium effort for the focused #22 test repair and GPT-6.1-Sol
at medium effort for the other implementation/research assignments. The parent
reviews and integrates each issue-sized slice. Keep #10 and #13 network contract
changes sequential; independent research can proceed without edit ownership.
Assignments stop at their focused acceptance/checkpoint. No push, PR, benchmark,
or dependency upgrade is included.

Approved behavior choices:

- #10: support identical provider tunnel IPs by binding health probes to their
  intended interface and selecting routes by source IP and output interface.
  Host processes bound only to a provider source IP no longer implicitly select
  that provider's route; they must also bind the interface.
- #13: honor the exact downstream listen address and address family. In particular,
  `0.0.0.0` is IPv4-only and `[::]` is IPv6-only. The default no longer opens an
  additional IPv6 socket. Keep upstream binds unchanged. The simpler downstream
  bind loses default-bind UDP batching/offload optimizations; throughput impact
  is unmeasured and no benchmark is included.
- #20: reject subnet prefixes with host bits set, with a clear validation error;
  do not normalize them silently.

## Working rules

- Use the existing package boundaries and dependencies first. Read the relevant
  code and callers before choosing a fix. Apply `ponytail` at full intensity,
  `caveman` for concise handoffs, and the relevant implementation, test, git, and
  review skills available in the session.
- The parent alone creates or assigns subagents, with at most three active at
  once. Announce each assignment's scope, model, and effort. Subagents never
  delegate. Default focused implementation/review to GPT-6.1-Sol, medium effort;
  use a cheaper capable model for simple tasks and escalate only with a concrete
  reason. Do not assume model-switch controls exist.
- Assign one issue-sized slice at a time. Use separate `fix/<issue>-<slug>`
  branches/worktrees when practical. Shared files still require exclusive
  ownership even across worktrees; do not create competing implementations.
- Only the parent commits, using Conventional Commits and an accurate
  `Co-Authored-By` trailer. Subagents may edit their assigned files and run agreed
  checks. No push without an explicit request; no PR without user approval.
- Consult the user before architectural decisions, dependency changes, or other
  consequential behavior choices. Present alternatives, trade-offs, and a
  recommendation. An issue's suggested approach is not an approved decision.
- Preserve unrelated/uncommitted work. The parent owns `CHANGELOG.md`, shared
  operations/architecture documentation, this plan, and final integration.
  Subagents propose documentation entries in their handoff instead of colliding
  on shared documents.
- Run networking tests only in disposable containers/network namespaces, never
  against the developer host's routes/firewall. Do not use host networking.
  Benchmarks require a separately agreed workload and resource budget.
- Stop when the assigned acceptance criteria and checks pass. Do not broaden a
  fix into speculative refactoring, dependency upgrades, or feature work.

## Baseline and evidence limits

At the audited commit, `go vet ./...`, Linux integration-tag vet,
`go test -race ./...`, formatting, `go mod verify`, and the Docker Linux
integration/end-to-end suite passed. These passes do not invalidate the findings.
The latest CI run inspected during the audit covered an older commit, `0ca3e06`.
Check CI for the actual implementation commit before claiming it is green.

Temporary audit reproductions are not committed tests and may no longer exist.
Recreate focused regressions in the repository. Some audit tests asserted that a
bug exists; convert them into assertions of the required corrected behavior.

- #8: the shutdown window is established by code order; no packet leak was captured.
- #10: kernel lookup showed both probes selecting one upstream; independent
  provider failure still needs an end-to-end regression.
- #14: kernel lookup plus firewall inspection demonstrated a bypass path; add an
  actual packet test before claiming the forwarding behavior is fixed.
- #7 and #12: failures were injected into the fake dataplane; no real kernel
  failure was injected. Test eventual convergence, not only call counts.
- #19: malformed settings were accepted and dangerous allocation arithmetic was
  inspected. Do not reproduce an out-of-memory allocation; test early rejection.
- #22: an isolated overlay replacing both name regexes with `.*` left tests green.
  Repair the fixtures and repeat that bounded mutation before calling coverage fixed.

## Task index

P1 means urgent correctness/security remediation; P2 means a concrete defect to
address after its prerequisites. Scope estimates count production/test file groups,
not elapsed hours: S = about 1-2 files, M = about 3-5. Split larger changes.

| Issue | Priority | Owner lane | Scope | Required result / verification focus |
|---|---|---|---|---|
| [#7](https://github.com/anesthetised/lotsman/issues/7) Retry failed route cleanup | P1 | A | M | Retain retry state until removal succeeds; one-shot and repeated cleanup failures converge. |
| [#6](https://github.com/anesthetised/lotsman/issues/6) Removed profiles | P1 | A | S | Previously routed peers lose routes/pins when their profile disappears; upstream stays configured in the regression. |
| [#8](https://github.com/anesthetised/lotsman/issues/8) Shutdown fail-closed | P1 | A, exclusive handoff | M | Downstream traffic cannot escape while rules are torn down; deterministic Linux packet test. |
| [#9](https://github.com/anesthetised/lotsman/issues/9) Routing ownership | P1 | B | M | Foreign rules/routes survive setup, reload, failed startup, teardown, and uninstall. |
| [#10](https://github.com/anesthetised/lotsman/issues/10) Duplicate upstream IPs | P1 | B, exclusive handoff | M+ | Approved overlap policy; correct independent probe routing or explicit early rejection. Split if cross-package work exceeds five files. |
| [#11](https://github.com/anesthetised/lotsman/issues/11) Secret file modes | P1 | C | M | Database, relevant sidecars, and backups stay private, including migration in an existing searchable directory. |
| [#12](https://github.com/anesthetised/lotsman/issues/12) Health application retry | P2 | A | M | Failed `SetAlive(false)` is retried and stale pins are eventually removed. |
| [#13](https://github.com/anesthetised/lotsman/issues/13) Listen address | P1 | B, exclusive handoff | M | Actual sockets honor supported addresses, or unsupported settings are explicitly rejected after approval. |
| [#14](https://github.com/anesthetised/lotsman/issues/14) Unrouted-peer bypass | P2 | B | M | Blocked peers cannot use connected/main-table upstream routes; verify packets with an upstream /24. |
| [#15](https://github.com/anesthetised/lotsman/issues/15) CLI argument rejection | P2 | C | S | Malformed `user rm alice phone -profile nl` fails before any deletion; documented syntax still works. |
| [#16](https://github.com/anesthetised/lotsman/issues/16) Atomic identity creation | P2 | C | S | Concurrent first creators/readers see one complete identity without overwriting existing keys. |
| [#17](https://github.com/anesthetised/lotsman/issues/17) IP reuse | P2 | A | S | Replacing a peer between reconciles preserves the new peer's route and revokes the old key in the same successful pass. |
| [#18](https://github.com/anesthetised/lotsman/issues/18) Reload thresholds | P2 | A | S | Added/replaced upstreams use the incoming health settings, alongside retained upstreams. |
| [#19](https://github.com/anesthetised/lotsman/issues/19) Upstream validation | P2 | C, handoff | M | Reject unsafe junk parameters, missing required peer keys, and invalid effective MTU before device startup. |
| [#20](https://github.com/anesthetised/lotsman/issues/20) Subnet host bits | P2 | C | M | Approved rejection/normalization behavior; gateway and allocations remain valid. |
| [#21](https://github.com/anesthetised/lotsman/issues/21) Advertised endpoint | P2 | C | M | Reject empty host and invalid numeric ports while preserving valid address forms. |
| [#22](https://github.com/anesthetised/lotsman/issues/22) Validation tests | P2 | C | S | Tests reach the intended validator; weakening regex validation makes them fail. |

## Execution order and ownership

These are proposed assignments for a future authorized run, not active agents.
The parent may reduce concurrency to avoid conflicts or unnecessary cost.
Prioritize P1 work as soon as its dependencies, decisions, and file ownership are
ready; wave labels must not delay an otherwise unblocked security fix.

### Wave 1: containment and retry foundations

Run up to three lanes concurrently, one issue at a time within each lane:

| Lane | Ordered queue | Exclusive files during the assignment |
|---|---|---|
| A: daemon reconciliation | #7, then #6, then #12 | `internal/daemon/daemon.go`, `daemon_test.go`, and `internal/dataplane/fake/*` when needed |
| B: Linux routing | #9, then #14 | `internal/dataplane/linux/*`, `deploy/uninstall.sh`; changes to the dataplane interface require a coordinated handoff |
| C: secrets and test foundations | #11, then #16, then #22 | `internal/store/*`, `internal/daemon/identity.go`, a dedicated `identity_test.go`, then `internal/config/config_test.go` |

For #11/#16, use dedicated identity tests rather than editing lane A's
`daemon_test.go`. Existing tests can be read and run without acquiring edit ownership.
If #9 needs a user decision, pause its dependent edits and continue independent
authorized work; do not let another agent choose the answer on the user's behalf.

After the first 2-3 completed issues in a lane, the parent reviews the actual diffs,
regressions, and evidence before assigning the next batch. At the wave checkpoint:

- Verify cleanup retries do not falsely report a peer as blocked while leaving
  untracked forwarding state; reconcile removal paths consistently.
- Verify foreign-network preservation and blocked-peer behavior in Linux, using
  the revised real dataplane rather than relying solely on its fake.
- Verify key permissions at creation/migration and stable identity publication.
- Integrate approved slices and run the common verification gate below. Record
  exact commands/results and unresolved decisions in the issue handoffs.

### Wave 2: lifecycle, CLI, and validation

- Lane A: take #8 as soon as lane B releases Linux ownership; then #17 and #18.
  For #8, acquire `daemon.go`, the necessary Linux dataplane files, and the
  Linux end-to-end test file as one exclusive assignment. Coordinate the
  teardown contract; do not independently redesign it in two branches.
- Lane C: #15, then #20 and #21 after #22. Keep exclusive ownership of
  `internal/config/*` and `cmd/lotsman/main_test.go` while these are active.
  Acquire `store_test.go` explicitly for subnet allocation coverage.
- Lane B may prepare the alternatives and focused test designs for #10 and #13
  while awaiting the cross-package handoff. It must not edit lane A/C files.
- Run #19 only after lane A releases `daemon_test.go` and `upstream.go` is
  assigned explicitly; a dedicated upstream-validation test file can reduce
  contention, but does not authorize overlapping production edits.

At the checkpoint, verify malformed CLI input leaves stored users unchanged,
invalid configs fail for the correct reason, reload uses incoming settings, and
packet-level shutdown containment works. Keep the test-quality repair separate
from production validation changes so the regression is reviewable.

### Wave 3: approved cross-package network behavior

Run #10 and #13 sequentially after the parent transfers all affected daemon,
health, tunnel, config, dataplane, and test ownership. These tasks may alter
contracts across packages; no concurrent edits to those contracts are allowed.
Each task starts with its approved choice, a narrow regression, and an explicit
compatibility statement. Re-run existing tunnel interoperability, failover,
pinning, and Linux end-to-end coverage after integration.

## Decisions that remain open

The #10, #13, #14, and #20 choices are approved by the follow-ups above. Their
original alternatives below remain for context; consult current issues for the
other tasks' decisions and completion.

| Issue | Alternatives / trade-off | Recommended discussion starting point |
|---|---|---|
| #9 | Explicitly reserved/rejected occupied IDs are simpler; discovered or persisted allocation supports more host layouts but adds lifecycle state. | Prove ownership and refuse unsafe collisions first; agree on crash-leftover compatibility before implementation. |
| #10 | Reject duplicate tunnel IPs with a clear error, or support them through independently selected probe routes. Supporting them is useful for provider configs but touches more contracts. | Inspect how often real provider configs overlap, then choose deliberately; do not silently rewrite provider IPs. |
| #13 | Implement address-specific binding, or reject addresses the existing bind cannot honor. Rejection is smaller but does not deliver the documented address-specific behavior. | Prefer honoring the documented setting if existing library interfaces allow a bounded change; otherwise present explicit rejection as an interim option. |
| #14 | Approved: nftables peer authorization independent of route selection. | Preserve live-flow pinning; revoke before route cleanup and test actual packets with an upstream /24. |
| #20 | Reject host bits, or normalize with `Masked()`. Normalization is convenient but can alter the interpretation of existing configs. | Prefer explicit validation failure over silently changing a running deployment's address plan. |

For #6, the existing issue specifies blocking affected peers. Do not replace that
criterion with a different reload policy without consulting the user.

## Verification gate

Every fix needs a meaningful regression that fails before the fix and passes
after it, plus the focused package tests. For file permissions and concurrency,
assert externally observable results, not internal implementation details.

The parent serializes suites using fixed UDP ports: two simultaneous host test
processes can contend for `:51820`. Separate worktrees do not isolate ports.
Use separate container network namespaces for independent networking runs.

After integrating a coherent batch, run the repository gates:

```sh
just test
just lint
test -z "$(gofmt -l .)"
just test-integration
just build amd64
just build arm64
```

`just lint` prints formatting deviations; the explicit `test -z` makes them a
failure, matching CI. `just test-integration` uses a privileged disposable
container; never replace it with execution against the developer host network.
Run `go mod verify` if dependency files/cache integrity is relevant; it is not a
vulnerability scan. No dependency upgrade or benchmark is part of this plan.

Do not repeat broad checks after they pass unless new changes, failures, or
unresolved concerns justify another run. Update `CHANGELOG.md` and relevant
operations/architecture documentation for shipped behavior in the corresponding
fix, then review the consolidated diff before integration. Preserve required
coverage; #22 explicitly requires a bounded mutation check.

## Assignment and handoff template

The parent supplies each subagent:

```text
Issue: <URL and current acceptance criteria>
Authorized scope: <one concrete slice and expected outcome>
Base branch/commit and worktree: <exact values>
Exclusive editable files: <explicit list>
Dependencies already integrated: <issues/commits>
Relevant skills and evidence: <minimal references, no full-history dump>
Model/effort and time/resource budget: <agreed limits>
Checks: <focused commands; reserve fixed ports before running>
Pending decisions: <pause affected work until user answers>
Constraints: no delegation, commits, pushes, PRs, benchmarks, or unrelated edits
Stop: acceptance criteria pass, an external decision blocks progress, or budget is reached
```

The handoff must name changed files, explain behavior, provide regression and
test evidence, identify risks and pending decisions, and propose any changelog
entry. The parent reads the actual diff and validates claims before committing.
Do not mark an issue complete merely because code exists or a bug-asserting audit
test is green. Track completion in GitHub with the fixing commit/PR and evidence
once the change has actually been integrated under the repository workflow.

## Deferred investigation and exclusions

- IPv6 DNS on a client with native IPv6 may bypass the IPv4-only tunnel or fail.
  Official-client behavior was not tested. Coordinate a separate investigation
  with [#4](https://github.com/anesthetised/lotsman/issues/4); do not claim either
  leakage or leak prevention from the absence of `::/0` alone.
- A stale probe completing after an upstream replacement may affect the reused
  interface name. User-visible harm was not reproduced; do not silently expand
  the retry fixes into a reload redesign to address this hypothesis.
- CVE scanning, throughput benchmarking, a DNS forwarder, a web UI, and full IPv6
  support are outside this remediation scope. Existing unrelated issues remain
  untouched.
