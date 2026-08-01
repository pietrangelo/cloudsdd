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
RFC 012 added power scheduling, RFC 013 `compute_instance` on all three
clouds, and RFC 014 the resolution of `provider: "agnostic"` to a concrete
cloud.

The original HTTP API layer concept was replaced by this CLI-first approach.
`docs/api.md` is retained only as a record of that earlier direction and
describes nothing that exists. `docs/openapi.yaml` is still maintained — it
is the schema of the current Specification — but declares no paths, because
nothing serves it.

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
│   └── cloudsdd/             # CLI entry point (cobra root, deploy/destroy, run pipeline, schedule prompts)
├── internal/
│   ├── config/               # ~/.cloudsdd/config.yaml: AI provider, model, defaults.provider (RFC 010/014)
│   ├── nlp/                  # LLM translation from Natural Language to Specification
│   │                         #   (anthropic, openai, ollama backends behind one Translator)
│   ├── schedule/             # Power-schedule model and compiler (RFC 012)
│   ├── spec/                 # Specification types, strict parsing, domain validation
│   ├── state/                # Local ledger of deployed resources, fed back to the translator (RFC 009)
│   ├── provider/             # CloudProvider interface, Diff/Result types, AllowedRegions helper
│   │   ├── aws/              # AWS implementation (RFC 002/003/004/007/012/013)
│   │   ├── gcp/              # GCP implementation (RFC 008/012/013)
│   │   ├── azure/            # Azure implementation (RFC 008/012/013)
│   │   ├── compute/          # Cloud-agnostic compute_instance shape, shared by all three (RFC 013)
│   │   ├── decode/           # Shared strict property decoder + credential-name denylist (RFC 011)
│   │   └── pulumiutil/       # Diff/Result mapping from Pulumi operation summaries
│   └── engine/               # Engine interface, DefaultEngine, agnostic resolution, DeploymentTarget
├── pkg/                      # Empty: no public type exposed yet (untracked — git carries no empty directory)
├── scripts/
│   └── coverage-gate.sh      # Per-package coverage floors enforced by CI (RFC 011 §5.1)
├── .github/workflows/ci.yml  # Build, vet, gofmt, tidy, test+coverage gate, fuzz smoke, gosec, govulncheck
├── LICENSE                   # GNU AGPLv3 (or later), full text
└── docs/
    ├── rfc/001-014...         # Foundation + AWS + scoping + CLI + scheduling + compute + resolution
    ├── architecture.md        # This document
    ├── cli.md                 # CLI commands and usage guide
    ├── dependency-licenses.md # Third-party license audit vs. AGPLv3
    ├── api.md                 # Superseded HTTP-API direction, retained for history
    └── openapi.yaml           # OpenAPI schema of the Specification (no paths — nothing serves it)
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

## Packages `internal/nlp`, `internal/config`, `internal/state`

The three packages that sit around the engine rather than inside it.

`internal/nlp` translates a prompt into a Specification behind one
`Translator` interface, with Anthropic, OpenAI and Ollama backends chosen
by configuration (RFC 010) so an operator forbidden from sending
architecture to a third party can run entirely locally. The prompt is
built in one place, shared by all three backends: three copies of it would
mean a capability documented to the model on one provider and invisible on
another (RFC 011 §1.1H1).

`internal/config` reads `~/.cloudsdd/config.yaml` (`0600`, created on
first run): the AI provider and model, and `defaults.provider`, the
machine-wide tie-break for agnostic resolution. A `defaults.provider` that
does not name a real cloud is an error at load time, not a silent fallback.

`internal/state` maintains the ledger of what has been deployed
(`~/.cloudsdd/ledger.json`, `0600`), keyed by
`(account, environment, region, id)` so the same resource ID in `dev` and
`prod` stays two records (RFC 011 §2.7). Writes go through a temp file and
a rename, under a cross-process lockfile with a staleness timeout, because
two `cloudsdd` runs are a normal thing to have. Only resources that
actually applied are recorded, so a partially-failed run still tracks what
it created. The ledger is fed back to the translator as context, and is
treated as **untrusted input** on the way: explicitly delimited, labelled,
and size-capped, with everything the model returns still gated by strict
schema validation (RFC 011 §4).

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

### `internal/provider/decode` and `internal/provider/pulumiutil`

Two pieces every provider needs identically, extracted so they cannot
drift (RFC 011 §1.1H). `decode` re-marshals `Resource.Properties` and
re-decodes it with `DisallowUnknownFields` before running
`go-playground/validator` over it, and rejects credential-shaped keys
independently of the schema; each provider constructs its own `Decoder`
so error prefixes stay provider-specific. `pulumiutil` maps a Pulumi
operation summary onto `provider.Diff`/`provider.Result`.

### `internal/provider/compute`

Holds the cloud-agnostic shape of a `compute_instance` — `size`, `os`,
`disk_size_gb`, `public_ip` — and the single-zone placement rule (RFC 013
§2.1, §2.3).

The struct is shared rather than redeclared per provider for the reason
RFC 011 §1.1H recorded: three copies of a schema drift, and a drifted
schema means a Specification that is valid on one cloud and silently
different on another. Each provider still decodes it through its own
`decode.Decoder`, so error prefixes and provider-specific validator tags
stay where they belong. What is deliberately *not* shared is the mapping
onto SKUs and images, which is per-provider by nature.

`compute_instance` is also the first ResourceType to consume
`Scope.Zones`, which RFC 005 §2.4.3 introduced with no consumer. A VM
occupies exactly one zone, so more than one entry is an error rather than
a silent pick of the first.

### `internal/provider/aws` (RFC 002, 003, 004, 007, 012, 013)

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
  cap of 20 entries), plus `relational_database` → RDS (RFC 007) and
  `compute_instance` → EC2 (RFC 013), both described below.
  `container_service` — the one remaining `ResourceType` in the schema —
  returns `ErrUnsupportedResourceType` on every provider.
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
  for a database, `…:aws-sdk:ec2:{start,stop}Instances` for a VM (RFC 013)
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

### `internal/provider/gcp` (RFC 008, 012, 013)

Cloud Storage and Cloud SQL, both private by default; encryption at rest
is unconditional on GCP, so a Specification asking to disable it is
refused rather than quietly ignored.

Power scheduling uses a Cloud Scheduler job calling the Cloud SQL Admin
API (`settings.activationPolicy`: `ALWAYS`/`NEVER`) with an OAuth token
minted for a dedicated service account, bound to a custom role carrying
`cloudsql.instances.get` and `cloudsql.instances.update`. Cloud SQL has no
resource-level IAM, so the binding is necessarily project-wide — which is
precisely why it must not be `roles/cloudsql.admin`.

A Cloud Scheduler job has no start or expiry date, and a Compute Engine
instance schedule accepts one policy with one validity interval, so
**exception windows cannot be expressed on GCP** and are rejected with
`ErrScheduleExceptionsUnsupported`. Dropping them silently would leave an
environment running through a shutdown the user believed they had
scheduled, and the failure would surface as an invoice rather than an
error.

### `internal/provider/azure` (RFC 008, 012, 013, 015)

Blob storage and Flexible Server databases, in a resource group per
resource since Azure has no ambient container and no default network.

**Databases are VNet-integrated on both engines** (RFC 015 §2.2): a
virtual network, a subnet delegated to the engine's own service, a
private DNS zone and its link, then a server carrying `DelegatedSubnetId`
and `PrivateDnsZoneId`. Only the delegation name and the DNS suffix
differ between PostgreSQL and MySQL, so they come from a two-entry table
rather than a branch in each declare function — the point of the change
being that the two engines stop producing different resource graphs. The
address space is `10.1.0.0/16` against compute's `10.0.0.0/16`, because
overlapping ranges cannot be peered and peering is what the deferred
network model needs.

This replaced two unsatisfying postures. MySQL exposes no
`PublicNetworkAccessEnabled` at all, so it had a public endpoint that only
the absence of a firewall rule kept closed; PostgreSQL had that flag set
false, which is genuinely private but leaves no path to the server at all.

**`deletion_protection` is enforced twice** (RFC 015 §2.1), because
Flexible Server has no server-side equivalent of the flag AWS and GCP
pass straight to the API. A `CanNotDelete` management lock stops deletion
through any tool that never reads CloudSDD's state; `pulumi.Protect`
stops CloudSDD itself, which would otherwise delete the lock first — it
is a resource in the same stack — and the server second. Neither half
covers the other's case. The level is `CanNotDelete` and not the stronger
`ReadOnly` because RFC 012's runbook has to keep stopping and starting
the server, and a schedule broken that way reports itself as an unchanged
bill.

Creating the lock requires `Microsoft.Authorization/locks/write`, which
Contributor does not grant. The deploy fails with that named in the error
rather than provisioning a database documented as protected and isn't —
the same choice RFC 013 made for `encryption_at_host`.

Power scheduling uses an Automation Account with a system-assigned
identity, a runbook, and `automation.Schedule` resources (whose
`StartTime`/`ExpiryTime` do support exception windows). It is the only
provider needing a deployed code artifact, so the runbook is a fixed
constant in the repository: the action and the target arrive as runbook
*parameters*, never as interpolated script text. The identity is bound to
a custom role scoped to the single server, with read/start/stop and
nothing else.

Azure schedules are anchored rather than purely recurrent, so the provider
computes the first occurrence against a clock (a `timeNow` seam, frozen in
tests) and declares the resource with
`pulumi.IgnoreChanges([]string{"startTime"})` — otherwise every apply
would recompute the anchor and show a spurious diff.

### Compute instances across the three providers (RFC 013)

The security posture is the point of this resource type: every other type
in the schema is a managed service, whereas a VM runs arbitrary code with
an attached identity.

- **AWS**: IMDSv2 required with a hop limit of 1 — the control that turns
  an application SSRF from a credential compromise into a failed request —
  an encrypted root volume, a security group with *no ingress rules at
  all*, and an instance profile carrying only
  `AmazonSSMManagedInstanceCore` so an operator can open an audited shell
  without the workload gaining anything. The AMI lookup filters on owner ID
  as well as name; filtering on a name pattern alone would let any account
  publishing a matching public AMI be selected. It is the provider's only
  Pulumi invoke, unavoidable because AMI IDs are region-specific.
- **GCP**: Shielded VM (secure boot, vTPM, integrity monitoring), OS Login
  with project SSH keys blocked, serial console off, and no service account
  attached at all — the default compute identity would be a standing
  credential. The explicit deny-ingress rule at priority 0 is load-bearing:
  the `default` network ships `default-allow-ssh` and GCP firewall rules
  are allow-only, so nothing weaker actually closes the machine.
- **Azure**: Trusted Launch, encryption at host, a VNet/subnet/NSG of its
  own since Azure has no default network, and a generated ed25519 key pair
  whose private half never leaves the encrypted Pulumi state. The key
  exists only because the API rejects a Linux VM without one; access goes
  through the AAD login extension.

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
- **Agnostic resolution (RFC 014)**: `Resolve` binds every resource
  declaring `provider: "agnostic"` to a concrete one before anything is
  planned. A *candidate* is a registered provider whose `Validate` accepts
  the resource — deliberately not a capability table, which would be a
  second source of truth free to drift from the `switch` statements it
  mirrors, the exact RFC 011 §1.1 failure mode. Reusing `Validate` also
  means resolution inherits `AllowedRegions`, property decoding and the
  zone rules for free.

  Candidates are evaluated in sorted order, because `DefaultEngine.providers`
  is a map and ranging over one is randomised: resolution that depended on
  iteration order would send the same Specification to a different cloud on
  different runs. One candidate resolves; several require an explicit
  preference (`policies.provider_preference`, then the configured
  `defaults.provider`) and are otherwise refused with
  `ErrAmbiguousProvider`; none produces `ErrNoCandidateProvider` quoting
  every provider's own reason.

  Because the three region formats are mutually exclusive, a resource that
  names a region usually has exactly one candidate — so in the common case
  resolution is determined by the Specification the user already wrote.

  `Resolve` takes a `Specification` by value and returns a bound copy, so
  the checks and the operation must run against *the same* copy.
  `Plan`/`Apply`/`Destroy` therefore share an unexported `validated`, which
  performs every check `Validate` performs and returns the resolved
  Specification the checks ran against. Calling the exported `Validate` and
  then iterating one's own resources instead would leave `"agnostic"` in
  hand and fail on a registry lookup that cannot succeed — the registry is
  keyed by concrete providers only. The CLI resolves explicitly before
  planning (it has to: the user approves a Specification naming a real
  cloud), which is why this path is exercised only by an Engine driven
  directly, and is regression-tested as such.
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
| Publicly reachable database endpoint (RFC 015 §1) | Azure MySQL had a public endpoint held closed only by the absence of a firewall rule — one portal click from open — while `cli.md` claimed it was not publicly reachable. VNet integration removes the endpoint instead of leaving it unfirewalled, on both engines |
| Deletion protection that protects against everything except us | The Azure management lock is a resource in CloudSDD's own stack, so a destroy would delete it first. `pulumi.Protect` covers that path; the lock covers the portal. Shipping either alone would be protection with a hole exactly where the mistranslated-prompt risk lives (RFC 015 §2.1) |
| Protection that silently disables a cost control | The lock is `CanNotDelete`, never `ReadOnly`: `ReadOnly` forbids modification and would leave RFC 012's runbook unable to stop the server, reporting itself only as an unchanged bill |
| Sealed environments by default (RFC 005 §2.5) | `Scope.Sealed` defaults to `true`; `cross_account_role`, the only ResourceType inherently cross-account, is rejected unless `Scope.Sealed: false` is explicit (`ErrSealedCrossAccountRole`) |
| Cross-account/cross-environment state collision | Pulumi stack identity is now `(Account, Environment, Region, Resource.ID)`, not `Resource.ID` alone (RFC 005 §2.6): closes a gap where the same `Resource.ID` applied to two accounts/environments could silently share one local stack |
| BOLA | Not yet applicable: no multi-tenant storage/state layer exists yet (note: the stack-per-scope design in RFC 002 §2.3 / RFC 005 §2.6 has no tenant namespacing, open question) |
| Scheduling privilege escalation (RFC 012 §7) | The identity a power schedule acts through is scoped to one resource and two or three actions on every provider: AWS start/stop on one instance ARN, GCP a two-permission custom role, Azure a role scoped to the single server. A scheduling feature that provisioned a broadly-privileged role would be a worse trade than the money it saves |
| Confused deputy (AWS scheduler service principal) | `aws:SourceAccount` and `ArnLike` `aws:SourceArn` conditions on the execution role's trust policy (RFC 012 §4.1) |
| SSRF to credential theft (RFC 013 §2.2) | IMDSv2 required on every EC2 instance, with `httpPutResponseHopLimit = 1` so a container on the host cannot reach the metadata service either |
| Image supply chain (RFC 013 §4) | AMI lookups filter on owner ID as well as name pattern; a name-only filter would let any account publishing a matching public AMI be selected |
| Key material in the Specification | No SSH key property exists on any provider; the one key Azure's API demands is generated in-program and kept in encrypted state, and the credential-name denylist rejects a user-supplied `private_key` independently |
| Cost control that silently does not control cost | On Azure a scheduled stop **deallocates**: a VM in the `Stopped` state still bills for compute, so the obvious verb would run correctly and save nothing (RFC 013 §2.5) |
| Non-deterministic target cloud (RFC 014 §4) | Candidate providers are evaluated in sorted order and ties are refused rather than broken implicitly, so a Specification cannot resolve to one cloud in review and another in production |
| Reconnaissance during a deploy | Candidacy is decided by whether a provider *constructs*, never by probing the cloud for access; CloudSDD does not sweep three providers to discover what the operator can reach |
| Terminal escape injection via prompt text | `schedule.Describe` strips control characters from `Window.Reason`, which originates in a natural language prompt, travels through the ledger, and is printed to the operator's terminal before the confirmation gate |
| Silent schedule degradation | A rule a provider cannot express is a `Validate` error, never a dropped rule (RFC 012 §1.3). The failure mode of a silently broken schedule is a bill rather than an alert, so it must surface while somebody is watching |
| Unrestricted Resource Consumption | Not yet applicable at the HTTP/API level (it does not exist yet); at the provider level, a cap of 20 entries on IAM lists (RFC 003), `Scope.Regions`/`Zones` capped at 10 entries (RFC 005 §3), `schedule.exceptions` capped at 12 windows, which bounds the number of scheduling resources one Specification can provision (RFC 012 §2.2) |

Both scanners run in CI on every push and pull request, so the record
below is the current state of `main` rather than a snapshot taken at one
commit: `gosec ./...` (0 issues; 2 false positives suppressed with
`#nosec` and inline justification — an env var name flagged as G101, a
path from an env var flagged as G703 path-traversal despite being
operator-controlled, not Specification input), `govulncheck ./...` (0
vulnerabilities reachable from the code; one unreachable vulnerability
remains in a required module not invoked by our code).

## Testing

Table-driven throughout, per CLAUDE.md. Coverage as measured by
`go test -race -coverprofile` on the default (untagged) suite:

| Package | Coverage | Floor |
|---|---|---|
| `internal/provider` | 100.0% | 100% |
| `internal/provider/compute` | 100.0% | 100% |
| `internal/provider/pulumiutil` | 100.0% | 100% |
| `internal/nlp` | 97.0% | 96% |
| `internal/schedule` | 96.8% | 96% |
| `internal/engine` | 96.1% | 95% |
| `internal/provider/decode` | 95.8% | 95% |
| `cmd/cloudsdd` | 93.5% | 93% |
| `internal/spec` | 90.0% | 90% |
| `internal/state` | 85.9% | 85% |
| `internal/config` | 84.8% | 84% |
| `internal/provider/azure` | 71.2% | 71% |
| `internal/provider/gcp` | 71.0% | 70% |
| `internal/provider/aws` | 65.6% | 65% |

The floors live in `scripts/coverage-gate.sh` and are enforced by CI. They
ratchet upward only, and a package with tests but no floor fails the gate,
so a new package cannot quietly skip it.

- The three provider packages sit below CLAUDE.md's >90% target for one
  reason: `Plan`/`Apply`/`Destroy`/`upsertStack`/`NewTargetProviderFactory`
  drive the Pulumi Automation API, which shells out to the `pulumi` binary
  and talks to a real cloud control plane. The pure logic — property
  decoding and validation, IAM policy documents, ResourceType dispatch,
  schedule rendering, Diff/Result mapping — is covered by table-driven
  tests, including Pulumi resource *declaration* through `pulumi.WithMocks`,
  which needs neither Docker nor the CLI.
- Native Go fuzz targets, all three run for 60s per CI job: `spec.Parse`
  (`FuzzParse`), the provider property decoder (`FuzzProperties`), and the
  schedule compiler (`FuzzCompile`). The invariant is the absence of
  panics, not the acceptance of the input.
- Integration tests (`testcontainers-go` + LocalStack) present behind the
  `integration` build tag
  (`go test -tags=integration ./internal/provider/aws/... -run TestIntegration`):
  Plan → Apply → verify via SDK → Destroy cycle for `object_storage`.
  Require Docker and the `pulumi` CLI; not run by the default suite nor
  verified in environments lacking these dependencies.

## Out of scope / next steps

Not yet implemented:

- `container_service`, the one `ResourceType` in the schema no provider
  implements.
- **A network model.** "Private by default" is implemented on all three
  clouds as *no configured path*, and only AWS produces a database
  something can actually connect to: it sits in the account's default VPC,
  whereas Azure's is alone in a VNet of its own and GCP's has a private IP
  with no VPC offering private services access. Deciding what shares a
  network — which is also what `Environment` should mean at the network
  layer (RFC 005 §5) — is the largest open gap in the system and needs its
  own RFC. RFC 015 §1.1 scoped it out deliberately rather than solving a
  third of it inside a database change.
- References/dependencies between resources in the same Specification —
  which is also why `Destroy` walks the resource list in reverse rather
  than in dependency order.
- Cost policy. `max_cost_monthly` was removed in RFC 011 §2.9: it was
  validated but never enforced, so it read as a guarantee and provided
  none. Real enforcement needs a pricing model and its own RFC.
- Portable region names. `agnostic` today still requires a
  provider-specific region string, which makes it that provider spelled
  indirectly rather than portability (RFC 014 §7.1).
- Multi-tenant isolation of state; network-layer sealing per `Environment`
  (VPC/security-group, RFC 005 §5); zone-aware HA placement for any
  `ResourceType` (`compute_instance` consumes `Scope.Zones` but places a
  single machine); and a `DeploymentTarget` keyed by
  `(Account, Environment)` pairs — today `Environment` is a
  CloudSDD-enforced logical boundary within shared credentials, not a new
  credential-scoping mechanism.
