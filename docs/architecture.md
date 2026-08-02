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
│   │   ├── aws/              # AWS implementation (RFC 002/003/004/007/012/013/016/017)
│   │   ├── gcp/              # GCP implementation (RFC 008/012/013/016/017)
│   │   ├── azure/            # Azure implementation (RFC 008/012/013/015/016/017)
│   │   ├── compute/          # Cloud-agnostic compute_instance shape, shared by all three (RFC 013)
│   │   ├── container/        # Cloud-agnostic container_service shape and image rules (RFC 017)
│   │   ├── decode/           # Shared strict property decoder + credential-name denylist (RFC 011)
│   │   ├── network/          # Address derivation and conflict checking for scope networks (RFC 016)
│   │   └── pulumiutil/       # Diff/Result mapping from Pulumi operation summaries
│   └── engine/               # Engine interface, DefaultEngine, agnostic resolution, DeploymentTarget
├── pkg/                      # Reserved for public types; empty, with a README explaining why (see pkg/README.md)
├── scripts/
│   └── coverage-gate.sh      # Per-package coverage floors enforced by CI (RFC 011 §5.1)
├── .github/workflows/ci.yml  # Build, vet, gofmt, tidy, test+coverage gate, fuzz smoke, gosec, govulncheck
├── LICENSE                   # GNU AGPLv3 (or later), full text
└── docs/
    ├── rfc/001-017...         # Foundation + AWS + scoping + CLI + scheduling + compute + resolution + networking
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
    EnsureNetwork(ctx context.Context, s NetworkScope, p spec.Policies) error
}
```

`EnsureNetwork` and `DestroyNetwork` (RFC 016 §2.2, §2.6) exist because a
shared network outlives
and precedes the resources in it, so it cannot be declared inside any one
resource's Pulumi program — stack identity is per-resource, and a program
cannot create something a different stack also needs. The Engine calls it
once per distinct `NetworkScope` before applying anything in that scope,
which puts the one ordering constraint in the one component that can see
every resource in a Specification. `NetworkScope` carries the provider and
the account as well as environment and region, because neither two
providers nor two accounts ever share a network.

Teardown is the mirror image and needs one more thing: proof that the
scope is empty. `Engine.ReapNetworks` asks a `ScopeOccupancy` function —
injected via `WithScopeOccupancy`, the same dependency-injection pattern
RFC 004 used for `DeploymentTarget`s — and destroys only the networks
whose scope holds nothing. **Absent that function it removes nothing**,
which is the safe direction: an Engine that cannot prove a scope is empty
leaves an orphaned network, which costs a little and is fixable, rather
than cutting live resources off from everything they talk to. An error
reading the ledger is likewise not evidence of emptiness and stops the
reap.

The CLI supplies the function from `internal/state`, and calls
`ReapNetworks` *after* the destruction has been recorded — the ledger is
what answers the question, so it has to be current first. That ordering
also makes the operation idempotent: a rerun finds the networks gone.

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

### `internal/provider/network` (RFC 016, partial)

Decides the address range of every network CloudSDD creates. Pure — no
clock, no environment, no I/O, no cloud — for the same reason
`internal/schedule` is: every decision about *addresses* belongs in one
testable place, and a rule spread across three providers is three rules
that will disagree.

`Derive(Scope, Policy)` carves a configurable base block (default
`10.0.0.0/8`) into `/20` slots and picks one by hashing
`(account, environment, region)`. The hash is fixed and golden-tested,
because a deployed range cannot be recomputed without rebuilding the
network and everything inside it: changing the algorithm has to break a
test rather than re-address production.

Determinism rather than an allocator is a deliberate trade. CloudSDD's
state is a local file, so two engineers deploying two environments from
two machines cannot coordinate through it; the same scope yielding the
same range everywhere is what lets them not need to. Hashing 4096 slots
is not collision-proof, so `Check` derives every known scope and refuses
when two in the *same account* overlap, naming both and the override —
RFC 014's "report the ambiguity, never guess" applied to addresses.

Two accounts deriving the same range is **not** a conflict and is not
reported. Accounts never share a network (RFC 016 §2.1), so there is
nothing to collide; a warning that fires on correct behaviour is how real
warnings come to be ignored.

**Status: complete** (RFC 016 §6 steps 1-6) on all three providers.

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

### `internal/provider/container`

Holds the cloud-agnostic shape of a `container_service` — `image`, `port`,
`size`, `replicas`, `public` — and the image rules every provider applies
(RFC 017 §2.2, §2.4). Shared for the same reason `compute` is; what stays
per-provider is the mapping from `size` onto CPU and memory, which Fargate,
Cloud Run and Container Apps quantise differently.

`Replicas` is `*int` and `Public` is `*bool` so absence activates the
default rather than the zero value. It matters more here than elsewhere: an
absent `replicas` means one, an explicit `0` means none — the state a power
schedule puts the service in outside working hours — and a plain `int`
could not tell them apart.

There is deliberately no `env` or configuration field. Anything of that
shape is where credentials get pasted, and offering it without a secrets
story would be offering the paste.

**The image rules** are two independent axes, and reading them as one is
the mistake worth naming:

- **Mutability** — what runs must be what was reviewed. With no allowlist,
  every image must carry a digest; a tag is accepted only from a registry
  the operator named, because naming it is how they take responsibility for
  what its tags point at. `latest`, explicit or implied by an absent tag,
  is refused in every combination.
- **Origin** — where the artifact came from. When
  `policies.allowed_registries` is present it constrains *every* image,
  digest or not.

That last clause is a deliberate reading of RFC 017 §2.4, whose prose ("a
digest is accepted from anywhere") would otherwise let any digest bypass
the allowlist — which would make the RFC's own threat-table row about
attacker-controlled registries name a mitigation that does not mitigate.
An allowlist something can step around is not a policy.

`ParseImage` is stricter than a registry would be, and is fuzzed
(`FuzzParseImage`) on a property stronger than "does not panic": a registry
it reports must appear in the input, and a reference that parses must
survive being recomposed and parsed again. This is the one property in the
schema whose value decides what code runs, so a reference that parses two
ways must not parse at all.

Enforcement lives at the Engine (`validateImagePolicy`), beside
`AllowedRegions` and schedule compilation, for the RFC 011 §2.3 reason: a
check that lives only inside providers is a check the next provider
forgets. It fails closed — a missing or non-string `image` is an error, not
a skip — because a policy check that passes when it cannot read its input
passes hardest exactly when something is wrong.

### `internal/provider/aws` (RFC 002, 003, 004, 007, 012, 013, 016, 017)

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
- **The scope network (RFC 016 §2.3)**: `EnsureNetwork` upserts a stack
  named for the scope alone — `stackNameFor(account, environment, region, "")`
  — declaring a VPC at the derived range, private subnets in two
  availability zones (RDS refuses a subnet group with fewer), and the DB
  subnet group.

  **Egress (RFC 017 §2.7)**: two subnets tagged `CloudSDDTier=public`
  hold an internet gateway and **one NAT gateway for the whole scope**,
  and the private subnets route `0.0.0.0/0` through it. RFC 016 shipped the
  network with no route out, arguing that a database and a
  Session-Manager-reached VM both work without one. The database does; the
  VM does not, because the SSM agent has to reach the SSM endpoints before
  a session exists — so an instance booted into a subnet where nothing
  could talk to it. One NAT rather than one per zone is the cost trade RFC
  016 §7.1 priced. The tier spans two availability zones because an
  internet-facing load balancer refuses to exist in one (RFC 017 §2.3.1);
  only the first subnet holds the NAT, so this is not one gateway per zone.
  Nothing but the gateway and a load balancer is ever placed there, and no
  subnet assigns a public address on launch: a route out is not a route in.

  Resource programs find that network by **tag**, not through a Pulumi
  `StackReference` as RFC 016 §2.2 originally proposed. A StackReference
  couples a resource stack to another stack's *name inside the state
  backend*, and CloudSDD's backend is a local directory a user can move,
  share or lose independently of the cloud; a tag lives in the account
  next to the thing it describes. `relational_database` and
  `compute_instance` both look up `ManagedBy=cloudsdd` plus their scope,
  and a miss is an error naming the scope rather than a silent fall back
  to the default VPC — falling back is the behaviour this RFC exists to
  remove. The DB subnet group's physical name is derived from the scope on
  both sides, so the two stacks agree without talking to each other.

  `relational_database` gains a security group admitting only the engine's
  port from the VPC's own range, replacing a posture where "private" meant
  "reachable by everything else in the account".
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
- **`container_service` is ECS on Fargate** (RFC 017 §2.6), and it is the
  resource type that needs the most surrounding infrastructure: a cluster,
  a task definition, two IAM roles, a log group, two security groups, a
  load balancer, a target group and its listeners.

  *Two roles, not one.* The **execution role** belongs to the ECS agent —
  it pulls the image and opens the log stream before the container starts
  — and carries the AWS-managed `AmazonECSTaskExecutionRolePolicy`. The
  **task role** is what the container itself can do, and nothing is
  attached to it (RFC 017 §2.6). Conflating them is how a workload ends up
  able to read every log group in the account.

  *The perimeter is two tiers.* The load balancer's group admits `443`
  from the internet on a public service, or the VPC's own range on a
  private one. The service's group admits the container port **from the
  balancer's security group** — by group, not by CIDR, because a CIDR rule
  covering the subnet would admit everything else that happens to sit in
  it. The task runs in a private subnet with `assignPublicIp: false` and
  pulls its image through the scope's NAT.

  *Public needs a `domain`* (RFC 017 §2.3.1). ACM will not issue a
  certificate for an ALB's own `*.elb.amazonaws.com` name and AWS has no
  equivalent of Cloud Run's `*.run.app`, so a public service with no
  hostname could only be served over plain HTTP — which §2.3 refuses.
  `ErrPublicRequiresDomain` refuses the Specification instead. Given a
  domain, CloudSDD looks up its Route 53 hosted zone, issues the
  certificate, creates the DNS validation record, waits for issuance
  through `acm.CertificateValidation` (attaching a still-pending
  certificate to a listener fails), and creates an alias record pointing
  the domain at the balancer — without which the certificate is valid and
  the hostname resolves nowhere.

  Port 80 carries a **redirect** listener, never a forward. Leaving it
  closed would be safe too, and would mean every plain-HTTP client gets a
  connection refused rather than an upgrade; what must never happen is
  port 80 serving the application. The HTTPS listener pins
  `ELBSecurityPolicy-TLS13-1-2-2021-06`, because the AWS default still
  admits TLS 1.0 and 1.1.

  The public subnet tier gained a **second availability zone** for this:
  an internet-facing ALB refuses to be created with subnets in one zone.
  Still one NAT gateway — the extra subnet is empty until a balancer needs
  it, and RFC 016 §7.1's cost argument is unchanged.
- **Power scheduling (RFC 012 §4.1)**: EventBridge Scheduler with
  *universal targets* — `arn:aws:scheduler:::aws-sdk:rds:{start,stop}DBInstance`
  for a database, `…:aws-sdk:ec2:{start,stop}Instances` for a VM (RFC 013),
  and `…:aws-sdk:ecs:updateService` for a container service (RFC 017 §2.5)
  — so a schedule is pure configuration.

  The container service is the one target whose two rules call the **same
  API**: it has no power state, only a replica count, so "off" is a desired
  count of zero rather than a stopped task. `scheduleTarget` therefore
  carries a payload per action rather than one shared payload — "off" is
  not always the absence of an argument. The obvious alternative, a Lambda
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

### `internal/provider/gcp` (RFC 008, 012, 013, 016, 017)

Cloud Storage and Cloud SQL, both private by default; encryption at rest
is unconditional on GCP, so a Specification asking to disable it is
refused rather than quietly ignored.

**The scope network (RFC 016 §2.3)** is a custom-mode VPC — auto mode
would create a subnet per region out of ranges Google picks, which is the
overlap the address plan exists to prevent — with a workload subnet
carrying `PrivateIpGoogleAccess`, a reserved peering range, and a
`servicenetworking` connection.

**Egress (RFC 017 §2.7)** is a Cloud Router with a Cloud NAT, restricted to
the scope's own subnet (`LIST_OF_SUBNETWORKS`, not every subnet in the
network — identical behaviour today with one subnet, and the thing that
keeps it identical when a later RFC adds one that should not reach the
internet). No public tier and no external address on any instance: GCP
expresses egress as a property of a router rather than of a subnet, and a
Cloud NAT is regional, so the per-zone multiplication AWS has to refuse
does not arise.

That connection is the whole point. Cloud SQL was previously declared with
`Ipv4Enabled: false` and no `PrivateNetwork`, which does not make an
instance private so much as **unaddressable**: a private IP requires a
peered VPC, so the database came up with no path to it of any kind. The
peering range is carved from the scope's own /20 rather than left to
Google to choose, so the whole scope stays inside the range the address
plan assigned it.

Unlike AWS, no lookup and no invoke are needed: GCP resource names are
unique within a project and resolvable by name, so a resource program
derives the same network name from the same scope the network stack used.
The priority-0 deny rule RFC 013 added is kept on `compute_instance` as
defence in depth, retargeted at the scope network — it existed to
neutralise the `default` network's `default-allow-ssh`, and the scope
network ships no rules at all.

**`container_service` is Cloud Run v2** (RFC 017 §2.6), the first provider
to implement the type. Three things are worth naming.

*Public means two gates, not one.* Cloud Run decides reachability with an
ingress setting and callability with an IAM policy, and they are
independent. `public: false` sets `INGRESS_TRAFFIC_INTERNAL_ONLY` and
declares no binding; `public: true` sets `INGRESS_TRAFFIC_ALL` **and**
grants `roles/run.invoker` to `allUsers`. Opening only the first produces a
service that is reachable and answers 403 to everyone — a deployment that
looks finished and serves nobody. HTTPS needs no configuration: the
built-in endpoint is TLS with a Google-managed certificate and there is no
plain-HTTP listener to disable.

*Omitting the identity is not the same as withholding one.* A Compute
Engine instance with no `service_account` block gets no identity, which is
why `compute_instance` omits it. Cloud Run with no service account falls
back to the **default compute service account**, which carries Editor on
the whole project — a standing credential on the one resource type that
runs arbitrary code. So a dedicated account is declared explicitly, with
no role bound to it.

The service joins the scope's own subnet through direct VPC egress with
`ALL_TRAFFIC`, not `PRIVATE_RANGES_ONLY`. The weaker setting reaches the
database just as well and lets internet-bound traffic leave from Google's
shared pool; routing everything through the scope's Cloud NAT is what
makes the scope leave from one address an account-level control can see.

*A power schedule does not apply here* (RFC 017 §2.5). Cloud Run bills per
request and idles to zero between them, so there is no running state to
switch off and no saving left to deliver. An explicit schedule on a Cloud
Run service is refused (`ErrCloudRunNotSchedulable`); one inherited from
`policies.schedule` is accepted, declares nothing, and is reported in the
plan as inapplicable. §2.5 originally proposed setting max instances to
zero — step 2 found Cloud Run reads a zero ceiling as *unset* and applies
its own default, which would uncap the service rather than stop it.

*A `domain` is optional here and required on AWS*, which is worth stating
rather than papering over (RFC 017 §2.3.1). Cloud Run serves the service on
`*.run.app` with a Google-managed certificate, so absence means "use that
endpoint" — not "there is no HTTPS", which is what the same absence would
mean on an ALB. When a domain is given it becomes a `cloudrun.DomainMapping`
with `certificateMode: AUTOMATIC`; `NONE` would map the hostname and serve
no certificate for it, which is the plain-HTTP outcome §2.3 refuses.

`replicas` is a **ceiling** here rather than a fleet size, because Cloud
Run's floor is zero and that is the point of the platform. An explicit
`replicas: 0` is refused (`ErrZeroReplicasUnsupported`): Cloud Run reads a
zero `maxInstanceCount` as unset and applies its own default ceiling, so
honouring the request would uncap the service rather than stop it. Refused
rather than substituted, per RFC 012 §1.3.

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

### `internal/provider/azure` (RFC 008, 012, 013, 015, 016, 017)

Blob storage and Flexible Server databases, in a resource group per
resource since Azure has no ambient container and no default network.

**Databases are VNet-integrated on both engines** (RFC 015 §2.2), now in
the scope's shared network rather than one of their own (RFC 016 §2.3).
The scope's VNet carries a general subnet for compute plus one subnet
delegated per engine — a subnet delegated to `Microsoft.DBforMySQL`
cannot host a PostgreSQL server or a VM — and one private DNS zone per
engine, shared by every database of that engine in the scope. Both
engines' subnets are declared whether or not a database of that engine
exists: the network is built before the resources in it, so it cannot
know, and an empty subnet costs nothing.

This replaced two unsatisfying postures. MySQL exposes no
`PublicNetworkAccessEnabled` at all, so it had a public endpoint that only
the absence of a firewall rule kept closed; PostgreSQL had that flag set
false, which is genuinely private but leaves no path to the server at all.

**Egress (RFC 017 §2.7)** is a NAT gateway with a static Standard-SKU
address — the gateway rejects a dynamic or Basic one at create time, and
Azure's default allocation is dynamic — associated with **the general
subnet only**. The delegated database subnets are deliberately left
without: a managed flexible server never pulls a package, so an egress
path from its subnet is one only an exfiltrating query would use. Azure
needs no public tier for this, since the gateway is a resource associated
with a subnet rather than one living in it.

The security group is attached to the **subnet**, not to each network
interface as RFC 013 did — at scope level the perimeter is a property of
the network, so a resource cannot end up outside it by forgetting to
attach one. Discovery is by derived name through `LookupSubnet` and
`GetDnsZone`: Azure resource IDs carry the subscription, which a resource
program cannot compute, so unlike GCP the IDs must be looked up even
though the names are derived.

**`container_service` is Container Apps** (RFC 017 §2.6), the last of the
three, and with it no `ResourceType` in the schema is advertised and
unimplemented.

*Private means two settings, not one.* The environment's
`internalLoadBalancerEnabled` decides whether an external endpoint exists
at all; the app's `externalEnabled` decides whether this app uses one.
Setting only the second would leave an environment a later app could take
an external endpoint from by accident.

*HTTPS comes from stating a value, not from leaving it alone.* Azure's
`allowInsecureConnections` defaults to **true**, which serves plain HTTP
alongside HTTPS rather than redirecting. It is set false explicitly, which
is the whole of §2.3's "plain HTTP redirects rather than being served" on
this provider — the built-in `*.azurecontainerapps.io` endpoint carries a
managed certificate, so a `domain` is optional here as on GCP.

*The workload profile is an addressing constraint, not a performance one.*
A Container Apps environment is injected into a delegated subnet, and a
consumption-only environment requires that subnet to be at least a `/23`.
The RFC 016 address plan carves each scope's `/20` into `/24`s, so a
consumption-only environment does not fit the plan. Naming a workload
profile lowers the requirement to a `/27`. The alternative — widening
every scope's subnets for a resource type most scopes will not hold —
would re-address every network already deployed.

The scope network therefore gained a fourth subnet, delegated to
`Microsoft.App/environments` at index 3, alongside the general subnet and
the two engine subnets. Declared with the network rather than with the
first container service, for the reason the engine subnets already are:
the network is built before the resources in it and cannot know what the
scope will hold.

*A power schedule uses the app's own `start` and `stop` actions.* RFC 017
§2.5 proposed setting min and max replicas to zero, which would need a
PATCH with a body — and the RFC 012 §4.3 runbook deliberately cannot do
that, because it POSTs an action and interpolates nothing from the
Specification into script text. Container Apps offers `start` and `stop`
verbs of its own, so the schedule reuses the machinery the databases
already have: a stopped app runs no replicas and bills for none, with no
`deallocate` distinction to get wrong.

The identity is **user-assigned** rather than system-assigned, with no role
assignments, so the absence of permissions is a property of a resource a
test can point at rather than of one Azure generates. `replicas` sets both
`minReplicas` and `maxReplicas`: a floor below the requested count would
mean a Specification asking for three replicas usually running one.

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
| A shared network destroyed under live resources | Teardown is gated on the ledger showing the scope empty (RFC 016 §2.6). No occupancy check configured, or a ledger that cannot be read, means nothing is reaped — an orphaned network costs money and is fixable, the opposite default cuts running resources off from everything |
| A stale ledger orphaning a network | **Accepted, and new.** The ledger is now load-bearing for a destroy decision, not only for translation context. A ledger deleted by hand leaves networks standing rather than deleting them, which is the direction the failure should fall |
| Sealed environments by default (RFC 005 §2.5) | `Scope.Sealed` defaults to `true`; `cross_account_role`, the only ResourceType inherently cross-account, is rejected unless `Scope.Sealed: false` is explicit (`ErrSealedCrossAccountRole`) |
| Cross-account/cross-environment state collision | Pulumi stack identity is now `(Account, Environment, Region, Resource.ID)`, not `Resource.ID` alone (RFC 005 §2.6): closes a gap where the same `Resource.ID` applied to two accounts/environments could silently share one local stack |
| BOLA | Not yet applicable: no multi-tenant storage/state layer exists yet (note: the stack-per-scope design in RFC 002 §2.3 / RFC 005 §2.6 has no tenant namespacing, open question) |
| Scheduling privilege escalation (RFC 012 §7) | The identity a power schedule acts through is scoped to one resource and two or three actions on every provider: AWS start/stop on one instance ARN, GCP a two-permission custom role, Azure a role scoped to the single server. A scheduling feature that provisioned a broadly-privileged role would be a worse trade than the money it saves |
| Confused deputy (AWS scheduler service principal) | `aws:SourceAccount` and `ArnLike` `aws:SourceArn` conditions on the execution role's trust policy (RFC 012 §4.1) |
| SSRF to credential theft (RFC 013 §2.2) | IMDSv2 required on every EC2 instance, with `httpPutResponseHopLimit = 1` so a container on the host cannot reach the metadata service either |
| Image supply chain (RFC 013 §4) | AMI lookups filter on owner ID as well as name pattern; a name-only filter would let any account publishing a matching public AMI be selected |
| Container image mutated after review (RFC 017 §2.4) | A digest is required unless the registry is in `policies.allowed_registries`; `latest`, explicit or implied, is refused in every combination. Enforced at the Engine, so no provider can omit it, and fail-closed when the `image` property cannot be read |
| Container image from an attacker-controlled registry | When `allowed_registries` is set it constrains every image, digest included — an allowlist something can step around is not a policy |
| Egress becoming ingress (RFC 017 §2.7) | The AWS public subnet tier holds the NAT gateway and nothing else; no subnet on any provider assigns a public address on launch, and Azure's delegated database subnets get no NAT at all |
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
| `internal/provider/container` | 98.2% | 98% |
| `internal/nlp` | 97.0% | 96% |
| `internal/schedule` | 96.8% | 96% |
| `internal/provider/decode` | 95.8% | 95% |
| `internal/engine` | 95.6% | 95% |
| `internal/provider/network` | 93.7% | 93% |
| `internal/config` | 93.5% | 93% |
| `cmd/cloudsdd` | 93.2% | 93% |
| `internal/state` | 91.7% | 91% |
| `internal/spec` | 90.0% | 90% |
| `internal/provider/azure` | 70.4% | 70% |
| `internal/provider/gcp` | 69.8% | 69% |
| `internal/provider/aws` | 65.5% | 65% |

The floors live in `scripts/coverage-gate.sh` and are enforced by CI. They
ratchet upward only, and a package with tests but no floor fails the gate,
so a new package cannot quietly skip it. The script computes each
percentage from the profile it is handed rather than re-running the suite
package by package, so the numbers CI enforces are the ones in the
artifact it archives.

- Every package meets CLAUDE.md's >90% target except the three provider
  packages, which sit below it for one reason: `Plan`/`Apply`/`Destroy`/`upsertStack`/`NewTargetProviderFactory`
  drive the Pulumi Automation API, which shells out to the `pulumi` binary
  and talks to a real cloud control plane. The pure logic — property
  decoding and validation, IAM policy documents, ResourceType dispatch,
  schedule rendering, Diff/Result mapping — is covered by table-driven
  tests, including Pulumi resource *declaration* through `pulumi.WithMocks`,
  which needs neither Docker nor the CLI.
- Native Go fuzz targets, all five run for 60s per CI job: `spec.Parse`
  (`FuzzParse`), the provider property decoder (`FuzzProperties`), the
  schedule compiler (`FuzzCompile`), the network address derivation
  (`FuzzDerive`), and the container image parser (`FuzzParseImage`). For
  the first three the invariant is the absence of panics; the last two
  assert something stronger, because their failure mode is not a crash.
  A malformed range is a VPC the cloud rejects after the user approved the
  plan, so every scope must yield an aligned `/20` inside the base block.
  A misparsed image is a container pulled from a host the user never
  named, so a reported registry must appear in the input and a reference
  that parses must survive being recomposed and parsed again.
- Integration tests (`testcontainers-go` + LocalStack) present behind the
  `integration` build tag
  (`go test -tags=integration ./internal/provider/aws/... -run TestIntegration`):
  Plan → Apply → verify via SDK → Destroy cycle for `object_storage`.
  Require Docker and the `pulumi` CLI; not run by the default suite, not
  run by CI at all, and present for AWS only — GCP and Azure have no
  integration test, which is a gap rather than a decision.

## Out of scope / next steps

Not yet implemented:

- **The system prompt and the last documentation pass** (RFC 017 step 6).
  `internal/nlp/prompt.go` lists `container_service` as a valid type but
  describes none of its properties, so the translator has to guess at
  `image`, `port` and the rest. Steps 1 to 5 have landed: all three
  providers implement the type and compose with RFC 012, so no
  `ResourceType` in the schema is advertised and unimplemented — the first
  time that has been true.
- References/dependencies between resources in the same Specification —
  which is also why `Destroy` walks the resource list in reverse rather
  than in dependency order.
- Cost policy. `max_cost_monthly` was removed in RFC 011 §2.9: it was
  validated but never enforced, so it read as a guarantee and provided
  none. Real enforcement needs a pricing model and its own RFC.
- Portable region names. `agnostic` today still requires a
  provider-specific region string, which makes it that provider spelled
  indirectly rather than portability (RFC 014 §7.1).
- GCP and Azure integration tests. Only AWS has one, and CI runs none of
  them, which is what the `internal/provider/*` coverage floors are really
  measuring around.
- Multi-tenant isolation of state; zone-aware HA placement for any
  `ResourceType` (`compute_instance` consumes `Scope.Zones` but places a
  single machine); and a `DeploymentTarget` keyed by
  `(Account, Environment)` pairs — today `Environment` is a
  CloudSDD-enforced logical boundary within shared credentials, not a new
  credential-scoping mechanism.
