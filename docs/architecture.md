# CloudSDD Architecture

> Current state of the system. Design decisions are tracked in the RFCs
> under `docs/rfc/`; this document reflects what has **actually been
> implemented**, not proposals.

## Overview

CloudSDD converts an SDD Specification (JSON) into operations on cloud
providers, through a cloud-agnostic Go engine. The intended end-to-end
flow is:

```
NL request ──(CLI NLP Translation)──► JSON Specification ──► Engine.Validate
                                                                      │
                                                                      ▼
                                                                Engine.Plan
                                                                      │
                                                                      ▼
                                                                Engine.Apply
```

The full path is implemented: the CLI (`cobra`, RFC 006) translates natural
language through a configurable AI provider (RFC 010), and the Engine applies
the resulting Specification via AWS (RFC 002-004, 007), GCP, and Azure
(RFC 008) providers. Deployed resources are recorded in a local ledger
(RFC 009) that is fed back to the translator as context. RFC 011 hardened
the providers added after RFC 006 and added the CI that had been missing.

The original HTTP API layer concept was replaced by this CLI-first approach;
`docs/api.md` and `docs/openapi.yaml` are retained only as a record of that
earlier direction and describe nothing that exists.

## License

CloudSDD is licensed under the **GNU Affero General Public License v3.0
(or later)** — see [`LICENSE`](../LICENSE). Every `.go` source file carries
an `SPDX-License-Identifier: AGPL-3.0-or-later` header. All 157 external Go
modules currently pulled in (production build + the `integration`-tagged
test-only Docker/testcontainers-go path) were audited with `go-licenses`
and found to use AGPLv3-compatible licenses (Apache-2.0, MIT, BSD-2/3-Clause,
ISC, MPL-2.0) — see [`docs/dependency-licenses.md`](dependency-licenses.md)
for the full breakdown and the process for vetting new dependencies.

## Module structure

```
cloudsdd/
├── cmd/
│   └── cloudsdd/            # CLI entry point (cobra root, deploy, destroy commands)
├── internal/
│   ├── nlp/                 # LLM translation interface from Natural Language to Specification
│   ├── schedule/            # Power-schedule model and compiler (RFC 012)
│   ├── spec/                # Specification types, strict parsing, domain validation
│   ├── provider/             # CloudProvider interface, Diff/Result types
│   │   └── aws/                # Concrete AWS implementation (RFC 002/003/004)
│   └── engine/                # Engine interface, DefaultEngine, DeploymentTarget (RFC 004)
├── pkg/                     # Empty: no public type exposed yet
├── LICENSE                  # GNU AGPLv3 (or later), full text
└── docs/
    ├── rfc/001-012...        # Foundation + AWS + scoping + CLI + scheduling (approved)
    ├── architecture.md        # This document
    ├── cli.md                 # CLI commands and usage guide
    ├── dependency-licenses.md # Third-party license audit vs. AGPLv3
    └── openapi.yaml            # OpenAPI schema (Specification + AWS properties, no paths)
```

## Package `internal/spec`

Represents and validates the SDD Specification (`Specification`,
`Resource`, `Scope`, `Policies`).

- **`Parse(io.Reader) (*Specification, error)`**: strict JSON decoding
  (`json.Decoder.DisallowUnknownFields`) that rejects leftover JSON data
  after the first value. This is the first line of defense against
  **Mass Assignment**: any field not anticipated by the schema causes an
  explicit error instead of being silently ignored or assigned.
- **`Validate(*Specification) error`**: second tier of validation, via
  `go-playground/validator`, applying domain rules (`validate` tags on
  the structs): `sdd_version` fixed to `"1.0"`, `intent` constrained to
  an enum, at least one resource, resource `id` constrained to the
  pattern `^[a-zA-Z0-9_-]{1,63}$` (custom `resourceid` tag), `type`
  (including `cross_account_role`, RFC 003) and `provider` constrained to
  enums. `Resource.Account` (RFC
  004 §2.2) is optional: it references, by name, a `DeploymentTarget`
  resolved by the `Engine`, never an ARN or a credential.
- **`Resource.Scope` (RFC 005 §2.2)**: cloud-agnostic "where" for a
  resource, orthogonal to `Account` ("which credentials"):
  `Environment` (free-form label, custom `scopename` tag, max 32 chars),
  `Region`/`Regions` (mutually exclusive — `excluded_with` — `Regions`
  requires 2-10 entries; a single desired region belongs in `Region`),
  `Zones` (max 10, forwarded but not yet consumed by any `ResourceType`),
  and `Sealed` (`*bool`, `EffectiveSealed()` defaults to `true`: "sealed
  unless otherwise specified"). Region/zone *format* is intentionally not
  validated here — it is provider-specific (AWS/GCP/Azure region strings
  differ in shape) and checked by each `CloudProvider` instead.
- **`ParseAndValidate`**: combines the two steps.
- `Resource.Properties` remains `map[string]any`: a generic JSON transport
  container. Typed and validated decoding of properties specific to each
  `ResourceType`/provider is the responsibility of each concrete
  `CloudProvider`.

The parser is covered by a native Go fuzz test (`FuzzParse`), run against
malformed input, nested JSON, unknown fields, and empty input: the
invariant being verified is the absence of panics, not the acceptance of
the input.

## Package `internal/schedule`

Models *when* a resource is powered on, and compiles that model into a
provider-agnostic rule set (RFC 012).

`Compile(*Schedule) ([]Rule, error)` is a pure function: no clock is read,
no environment consulted, no I/O performed. Every decision about time in
CloudSDD is made here, which makes the whole temporal behaviour of the
system table-testable without a cloud account, and leaves each provider
with a mechanical translation of an already-validated rule set into its
native scheduling primitive.

A `Rule` stays structured — action, weekdays, hour, minute, timezone, and
an optional `[ValidFrom, ValidTo)` validity interval — rather than
carrying a cron string, because the dialects differ: AWS uses a six-field
`cron()` expression with a mandatory year, GCP and Azure use five-field
unix cron, and Azure does not use cron at all. Rendering belongs to the
provider; deciding when things happen belongs here.

Two compilation decisions are worth recording:

- **Exception windows segment the weekly rhythm** rather than competing
  with it. A window suspends the base rules for its duration via the
  validity interval, so there is never a stop rule live inside an
  `always_on` window.
- **`always_off` emits a *daily* stop**, not a single one. AWS
  automatically restarts an RDS instance that has been stopped for more
  than seven days, so a one-shot stop at the start of a two-week company
  shutdown would leave it running, and billing, for the second week.

The compiler is covered by a native fuzz target (`FuzzCompile`) over the
time, date, and mode strings, on the same reasoning as `spec`'s
`FuzzParse`.

## Package `internal/provider`

Defines the `CloudProvider` contract that every concrete backend (AWS,
GCP, Azure, ...) implements:

```go
type CloudProvider interface {
    Name() string
    Validate(ctx context.Context, r spec.Resource, p spec.Policies) error
    Plan(ctx context.Context, r spec.Resource, p spec.Policies) (Diff, error)
    Apply(ctx context.Context, r spec.Resource, p spec.Policies) (Result, error)
    Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error
}
```

`spec.Policies` is passed to every method (RFC 002 §2.5) precisely so a
provider can enforce constraints such as `Policies.AllowedRegions`, which
would otherwise remain unenforceable at the provider level.

### `internal/provider/aws` (RFC 002, 003, 004)

First concrete implementation of `CloudProvider`, based on the Pulumi
Automation API (inline program in Go; no `pulumi` process is shelled out
manually by our code — the Automation API requires it to be installed on
the machine, but invokes it itself).

- **Supported ResourceTypes**: `object_storage` → S3 (`S3Properties`:
  `bucket_name`, `versioning`/`encryption`/`block_public_access` as
  `*bool` with a secure default when absent — `encryption` and
  `block_public_access` default to `true`, `versioning` defaults to
  `false`; `region` moved out of `S3Properties` into `Resource.Scope.Region`,
  RFC 005 §2.3) and `cross_account_role` → cross-account IAM role
  (`CrossAccountRoleProperties`: `enabled` as a kill switch,
  `trusted_account_id`, `external_id` mandatory against the confused
  deputy problem, `permissions`/`resource_arns` with no full wildcard,
  cap of 20 entries). Other `ResourceType`s return
  `ErrUnsupportedResourceType`.
- **Mass Assignment at the provider level**: `decodeProperties`
  re-marshals `Resource.Properties` and re-decodes it with
  `DisallowUnknownFields`, then validates it with
  `go-playground/validator` (same two-tier pattern as `internal/spec`);
  it rejects upfront keys whose names are reminiscent of credentials
  (`access_key`, `secret`, `password`, `token`, ...) regardless of the
  schema.
- **Policy enforcement**: `Validate` applies `Policies.AllowedRegions` to
  `object_storage` (`Resource.Scope.Region`, format-checked against
  `validateAWSRegionFormat`) and enforces fail-closed behavior on
  `cross_account_role` when `AllowedRegions` is empty (RFC 003 §2.3: IAM
  is global on AWS, so the region constraint translates into an
  `aws:RequestedRegion` `Condition` on the generated policy).
- **Scope enforcement (RFC 005 §2.5)**: `object_storage` requires
  `Scope.Region` (`ErrRegionRequired` if absent) and rejects `Scope.Zones`
  (`ErrZonesNotSupported`: no zone-aware HA logic exists yet).
  `cross_account_role` is global — it rejects any `Scope.Region`/`Regions`/`Zones`
  (`ErrGlobalResourceScoped`) — and, because it is inherently cross-account
  by design, requires `Scope.Sealed` to be explicitly `false`
  (`ErrSealedCrossAccountRole` otherwise): "sealed unless otherwise
  specified" made concrete and enforced, not just a naming convention.
- **State**: local filesystem backend (`CLOUDSDD_STATE_DIR`, default
  `~/.cloudsdd/state`), passphrase-based secrets provider
  (`CLOUDSDD_PULUMI_PASSPHRASE`, mandatory — `NewProvider` fails
  explicitly if absent), one Pulumi stack per `(Account, Environment,
  Region, Resource.ID)` scope (`stackNameFor`, RFC 005 §2.6 — segments
  joined by `::`, empty ones omitted, so an unscoped resource still gets
  the bare `Resource.ID`, unchanged from before this RFC). This closes a
  state-isolation gap: before RFC 005, the stack name was `Resource.ID`
  alone, so the same ID applied to two different `DeploymentTarget`
  accounts silently shared one local Pulumi stack.
- **Credentials**: defaults to the standard AWS SDK credential chain (RFC
  002 §2.4). With `Resource.Account` set, the Engine instead resolves a
  `DeploymentTarget` and uses credentials obtained via STS AssumeRole
  (`NewTargetProviderFactory`, RFC 004 §4), passed explicitly to the
  Pulumi AWS provider (never written to disk).
- **Power scheduling (RFC 012 §4.1)**: EventBridge Scheduler with
  *universal targets* — `arn:aws:scheduler:::aws-sdk:rds:{start,stop}DBInstance`
  — so a schedule is pure configuration. The obvious alternative, a Lambda
  calling the RDS API, would mean shipping, versioning and patching a code
  artifact for something that changes no logic. The schedules are declared
  inside the same Pulumi program as the instance, so they share its stack
  identity and the existing `Destroy` path removes them.

  The execution role carries exactly two actions on exactly one instance
  ARN. Its trust policy names `scheduler.amazonaws.com` with
  `aws:SourceAccount` and an `ArnLike` `aws:SourceArn` prefix condition —
  mandatory, not optional hardening, since a service principal will
  otherwise assume the role on behalf of whoever asks. Both the account
  and the region are parsed out of the instance ARN rather than read
  through an `aws:getCallerIdentity` invoke: the schedules necessarily
  live where the instance does, and the program stays invoke-free.

## Package `internal/engine`

`DefaultEngine` orchestrates `Validate`/`Plan`/`Apply` on a
`Specification`, maintaining an immutable registry (copied in `New`) of
`CloudProvider` indexed by `spec.Provider`.

Relevant behavior:

- `Validate` first runs `spec.Validate` (domain), then delegates
  per-resource validation to the resolved provider, also passing
  `s.Policies`.
- `Plan`/`Apply` call `Validate` as a precondition (fail-fast: no call to
  a provider happens on an invalid Specification).
- **Power schedules (RFC 012 §2.3)**: `Validate` resolves each resource's
  effective schedule (`spec.EffectiveSchedule`: the resource's own if it
  declares one, otherwise `Policies.Schedule`) and compiles it *before*
  delegating to any provider. This follows the enforcement lift RFC 011
  §2.3 applied to `AllowedRegions` — a check that lives only inside
  providers is a check the next provider forgets. Providers keep their own
  as defense in depth. A resource type with no power state rejects an
  *explicit* schedule (`ErrResourceNotSchedulable`) but merely skips an
  *inherited* one, which is what lets a Specification hold both an object
  store and a database.
- **Multi-region fan-out (RFC 005 §2.4.2)**: for each resource, the Engine
  calls `effectiveRegions(r.Scope)` — `Scope.Regions` when set,
  `[Scope.Region]` for a single region, or `[""]` for an unscoped
  resource — and invokes the resolved provider's `Validate`/`Plan`/`Apply`
  once per entry, via `scopedResource(r, region)` (a copy of `r` with
  `Scope.Region` set to that single value and `Scope.Regions` cleared, so
  no `CloudProvider` implementation ever has to handle the plural form).
  `provider.Diff`/`provider.Result` each get a `Region` field, set by the
  Engine after the call. An unscoped resource, or one with a single
  `Scope.Region`, still produces exactly one `Diff`/`Result`, unchanged
  from before this RFC. This keeps multi-region logic cloud-agnostic and
  written once, rather than duplicated in every provider package.
- If `Resource.Provider == "agnostic"` (and `Resource.Account` is empty),
  the Engine returns `ErrAgnosticResolutionNotImplemented`: the automatic
  provider resolution policy is an open question from RFC 001 (§5, point
  2) and has not yet been decided or implemented.
- Unregistered providers produce `ErrProviderNotFound`.
- **`DeploymentTarget` (RFC 004)**: when `Resource.Account` is set, the
  Engine instead resolves a `DeploymentTarget` registered via
  `engine.WithDeploymentTargets` — never part of the JSON Specification,
  so as to keep "what to create" separate from "how to authenticate" (RFC
  004 §2.1). A target with `Enabled == false` produces
  `ErrDeploymentTargetDisabled` without contacting real infrastructure
  (kill switch, RFC 004 §2.2); an unknown target produces
  `ErrDeploymentTargetNotFound`; a provider/target mismatch produces
  `ErrDeploymentTargetProviderMismatch`. The `CloudProvider` scoped to the
  target is built by a `TargetProviderFactory` registered via
  `engine.WithTargetProviderFactory` (implemented by the concrete
  provider package, e.g. `aws.NewTargetProviderFactory` — the Engine
  itself stays cloud-agnostic and knows nothing about STS/AssumeRole) and
  cached for the rest of the Engine's lifetime.

### Scheduling on GCP and Azure (RFC 012 §4.2, §4.3)

`internal/provider/gcp` uses a Cloud Scheduler job calling the Cloud SQL
Admin API (`settings.activationPolicy`: `ALWAYS`/`NEVER`) with an OAuth
token minted for a dedicated service account, bound to a custom role
carrying `cloudsql.instances.get` and `cloudsql.instances.update`. Cloud
SQL has no resource-level IAM, so the binding is necessarily
project-wide — which is precisely why it must not be
`roles/cloudsql.admin`.

A Cloud Scheduler job has no start or expiry date, so **exception windows
cannot be expressed on GCP** and are rejected with
`ErrScheduleExceptionsUnsupported`. Dropping them silently would leave an
environment running through a shutdown the user believed they had
scheduled, and the failure would surface as an invoice rather than an
error.

`internal/provider/azure` uses an Automation Account with a
system-assigned identity, a runbook, and `automation.Schedule` resources
(whose `StartTime`/`ExpiryTime` do support exception windows). It is the
only provider needing a deployed code artifact, so the runbook is a fixed
constant in the repository: the action and the target arrive as runbook
*parameters*, never as interpolated script text. The identity is bound to
a custom role scoped to the single server, with read/start/stop and
nothing else.

Azure schedules are anchored rather than purely recurrent, so the
provider computes the first occurrence against a clock (a `timeNow` seam,
frozen in tests) and declares the resource with
`pulumi.IgnoreChanges([]string{"startTime"})` — otherwise every apply
would recompute the anchor and show a spurious diff.

## Security

Measures active as of today (see also RFC 001 §3, 002 §2.4-2.5, 003
§2.2-2.3, 004 §3, 005 §3):

| OWASP API Top 10 threat | Current mitigation |
|---|---|
| Mass Assignment | Strict JSON decoding (`DisallowUnknownFields`) + two-tier validation, both in `internal/spec` and in every provider (`internal/provider/aws`) |
| Hostile input / parser crash | Native Go fuzz test on `spec.Parse`, extended (RFC 005) with seeds carrying `scope` |
| Credentials in the Specification | Denylist of credential-like field names in `decodeProperties`, independent of the schema |
| Confused deputy (cross-account) | `external_id` mandatory on `cross_account_role` (RFC 003) and on AWS `DeploymentTarget` (RFC 004) |
| Cross-account privilege escalation | No full wildcard (`*`) permissions on `cross_account_role`; `AdministratorAccess` explicitly discouraged for `DeploymentTarget`s (RFC 004 §3, principles also valid for GCP/Azure once implemented) |
| Region bypass / uncontrolled cost | `Policies.AllowedRegions` enforced by every provider, per effective region when a resource is multi-region (RFC 005 §2.4.2); `cross_account_role` is fail-closed if `AllowedRegions` is empty |
| Sealed environments by default (RFC 005 §2.5) | `Scope.Sealed` defaults to `true`; `cross_account_role`, the only ResourceType inherently cross-account, is rejected unless `Scope.Sealed: false` is explicit (`ErrSealedCrossAccountRole`) |
| Cross-account/cross-environment state collision | Pulumi stack identity is now `(Account, Environment, Region, Resource.ID)`, not `Resource.ID` alone (RFC 005 §2.6): closes a gap where the same `Resource.ID` applied to two accounts/environments could silently share one local stack |
| BOLA | Not yet applicable: no multi-tenant storage/state layer exists yet (note: the stack-per-scope design in RFC 002 §2.3 / RFC 005 §2.6 has no tenant namespacing, open question) |
| Scheduling privilege escalation (RFC 012 §7) | The identity a power schedule acts through is scoped to one resource and two or three actions on every provider: AWS start/stop on one instance ARN, GCP a two-permission custom role, Azure a role scoped to the single server. A scheduling feature that provisioned a broadly-privileged role would be a worse trade than the money it saves |
| Confused deputy (AWS scheduler service principal) | `aws:SourceAccount` and `ArnLike` `aws:SourceArn` conditions on the execution role's trust policy (RFC 012 §4.1) |
| Terminal escape injection via prompt text | `schedule.Describe` strips control characters from `Window.Reason`, which originates in a natural language prompt, travels through the ledger, and is printed to the operator's terminal before the confirmation gate |
| Silent schedule degradation | A rule a provider cannot express is a `Validate` error, never a dropped rule (RFC 012 §1.3). The failure mode of a silently broken schedule is a bill rather than an alert, so it must surface while somebody is watching |
| Unrestricted Resource Consumption | Not yet applicable at the HTTP/API level (it does not exist yet); at the provider level, a cap of 20 entries on IAM lists (RFC 003), `Scope.Regions`/`Zones` capped at 10 entries (RFC 005 §3), `schedule.exceptions` capped at 12 windows, which bounds the number of scheduling resources one Specification can provision (RFC 012 §2.2) |

Scans run before this commit (RFC 005): `gosec ./...` (0 issues; 2 false
positives suppressed with `#nosec` and inline justification — an env var
name flagged as G101, a path from an env var flagged as G703
path-traversal despite being operator-controlled, not Specification
input), `govulncheck ./...` (0 vulnerabilities reachable from the code;
one unreachable vulnerability with no available fix remains in a
transitive dependency not invoked by our code).

## Testing

- Table-driven tests for `internal/spec` (89.7%), `internal/engine`
  (94.3%, including a test `mockProvider`, `DeploymentTarget` resolution,
  and multi-region fan-out), and `internal/provider/aws` (63.1%).
- Native Go fuzz test for `spec.Parse`.
- `internal/provider/aws` coverage is lower than the others because
  `Plan`/`Apply`/`Destroy`/`upsertStack`/`NewTargetProviderFactory`
  require, respectively, the `pulumi` CLI to be installed and AWS
  credentials/network access for STS: not runnable in a sandboxed
  environment without these external dependencies. The pure logic
  (Properties decoding/validation, IAM policy document construction,
  ResourceType dispatch, Diff/Result mapping) is instead covered with
  table-driven tests, including tests that exercise Pulumi resource
  declaration via `pulumi.WithMocks` (no need for Docker or the pulumi
  CLI for these).
- Integration tests (`testcontainers-go` + LocalStack) present behind the
  `integration` build tag
  (`go test -tags=integration ./internal/provider/aws/... -run TestIntegration`):
  Plan → Apply → verify via SDK → Destroy cycle for `object_storage`.
  Require Docker and the `pulumi` CLI; not run by the default suite nor
  verified in environments lacking these dependencies.

## Out of scope / next steps

summary, not yet implemented: other AWS/GCP/Azure
`ResourceType`s (`compute_instance`,
`container_service`), resolution of `provider: "agnostic"`,
references/dependencies between resources in the same Specification, cost
policy (`max_cost_monthly` was removed in RFC 011 §2.9 — it was validated
but never enforced; real enforcement needs a pricing model and its own
RFC), multi-tenant isolation
of state, network-layer sealing (VPC/security-group isolation per Environment, RFC
005 §5 — no networked `ResourceType` exists yet), zone-aware HA placement
logic for any concrete `ResourceType`, and a `DeploymentTarget` keyed by
`(Account, Environment)` pairs (today `Environment` is a CloudSDD-enforced
logical boundary within shared credentials, not a new credential-scoping
mechanism).
