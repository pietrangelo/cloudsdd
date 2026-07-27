# RFC 005: Account, Environment, Region, and Zone Scoping (Sealed by Default)

- **Status:** Approved (2026-07-27)
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-07-27
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 002](002-aws-provider.md), [RFC 003](003-aws-cross-account-iam.md),
  [RFC 004](004-multi-account-deployer-access.md) (all approved)

## 1. Problem

User requirement (verbatim intent, cloud-agnostic — binding for every
provider, not just AWS):

> Every resource must be definable per account (dev, staging, production,
> etc.), per environment on the *same* account (dev, staging, production,
> etc.), per region, or multi-region/multi-zone (when applicable to the
> resource type). All environments must be **sealed unless otherwise
> specified**.

Today's schema only covers part of this:

- **Per account** (separate AWS account / GCP project / Azure
  subscription) exists via `Resource.Account` + `DeploymentTarget` (RFC
  004). Credentials differ, so accounts are already hard-isolated by AWS
  IAM/STS itself.
- **Per environment on the *same* account** does **not** exist. Nothing
  distinguishes a "dev" resource from a "staging" resource that share the
  same `Account` (or no `Account` at all, i.e. the default credential
  chain, RFC 002 §2.4).
- **Region** exists only as an ad hoc, provider-specific property
  (`S3Properties.Region`, RFC 002 §2.6) — not a core, cloud-agnostic
  concept, and not reusable by future resource types without duplicating
  the field and its validation in every provider package.
- **Multi-region / multi-zone** does not exist in any form.
- **"Sealed unless otherwise specified"** does not exist as an enforced
  rule. It is *implicitly* true only because the two mechanisms that cross
  an account boundary today (`cross_account_role`, RFC 003; `Account` +
  `DeploymentTarget`, RFC 004) both require an explicit, opt-in
  declaration. There is no field that lets a user state the boundary
  explicitly, and no validation rejects a resource that crosses it
  implicitly.

### 1.1 A concrete gap this RFC must also close

While designing the state-isolation side of this RFC, a pre-existing bug
surfaced: `stackNameFor(resourceID)` (`internal/provider/aws/stack.go`)
derives the Pulumi stack name **from the Resource ID alone**. A
`DeploymentTarget`-scoped `AWSProvider` (RFC 004 §4) reuses the *same*
`stateDir` as the base provider (`target.go`: `stateDir: base.stateDir`).
Consequence: a Specification that applies a resource with `id: "app-db"`
to `account: "staging"` and later a *different* resource, also
`id: "app-db"`, to `account: "production"`, would read/write the **same**
local Pulumi stack. This is a real sealing violation, already present for
the one dimension (`Account`) that exists today, not just a risk for the
new dimensions this RFC introduces. §2.6 below fixes it as part of the
same mechanism.

## 2. Proposed Architecture

### 2.1 Terminology

| Term | Meaning | Isolation guarantee |
|---|---|---|
| **Account** | A distinct cloud account/project/subscription (`Resource.Account` → `DeploymentTarget`, RFC 004). | Hard: enforced by the cloud provider's own IAM boundary (separate credentials). |
| **Environment** | A logical deployment stage (`dev`, `staging`, `production`, `qa`, ... — open-ended, not a fixed enum). May coincide 1:1 with an Account, or multiple Environments may share one Account. | Soft: enforced only by CloudSDD (state isolation + validation, §2.5–§2.6), *not* by the cloud provider, since credentials are shared. |
| **Region** | A physical cloud region (`eu-central-1`, `us-central1`, `westeurope`, ...). Format is provider-specific; the field itself is cloud-agnostic. | N/A — an addressing dimension, not a trust boundary. |
| **Zone** | An availability zone within a region. Relevant only to resource types that place infrastructure across AZs for HA (none implemented yet). | N/A — placement detail, not a trust boundary. |
| **Sealed** | The invariant that a resource must not create connectivity, trust, or state-sharing across an Account/Environment boundary other than its own, unless explicitly declared otherwise. | Enforced by CloudSDD validation (§2.5). |

**Account vs. Environment are orthogonal.** Case 1 from the requirement
(separate accounts per env) is `Account` alone, unchanged from RFC 004.
Case 2 (multiple environments inside one account) is new: `Account` is
absent or identical across resources, and `Environment` is the only thing
that distinguishes "dev" from "staging". Both may be combined (e.g. two
accounts, each internally split into `staging`/`production` via
`Environment`, for teams migrating gradually toward full account
separation).

### 2.2 Schema extension: `Scope`

A new struct groups the "where" concerns, kept separate from `Account`
("which credentials") and `Properties` ("what"), mirroring how `Policies`
is already its own struct (RFC 001 §2.3):

```go
// internal/spec/spec.go
type Resource struct {
    ID         string
    Type       ResourceType
    Provider   Provider
    Account    string         // RFC 004, unchanged: which DeploymentTarget
    Scope      Scope          // NEW
    Properties map[string]any
}

type Scope struct {
    // Environment is a free-form logical stage name (dev/staging/production/...).
    // Optional: absent means "unscoped", same behavior as today.
    Environment string `json:"environment,omitempty" validate:"omitempty,scopename"`

    // Region: single-region placement. Mutually exclusive with Regions.
    Region string `json:"region,omitempty" validate:"omitempty,excluded_with=Regions"`

    // Regions: multi-region placement, fanned out by the Engine (§2.4).
    // Mutually exclusive with Region. Min 2 (a single entry belongs in Region).
    Regions []string `json:"regions,omitempty" validate:"omitempty,min=2,max=10,excluded_with=Region"`

    // Zones: optional availability-zone pinning/spread, forwarded to
    // resource-type-specific HA logic (§2.4.3). Not fanned out.
    Zones []string `json:"zones,omitempty" validate:"omitempty,min=1,max=10"`

    // Sealed defaults to true when absent (fail-closed). A resource whose
    // ResourceType inherently crosses an Account/Environment boundary
    // (e.g. cross_account_role) must set this explicitly to false —
    // enforced per-ResourceType by each provider's Validate (§2.5).
    Sealed *bool `json:"sealed,omitempty"`
}

// EffectiveSealed returns the value of Sealed, or true if absent
// (fail-closed default, per the "sealed unless otherwise specified" requirement).
func (s Scope) EffectiveSealed() bool { return boolOrDefault(s.Sealed, true) }
```

`scopename` is a new `go-playground/validator` custom tag
(`internal/spec/validate.go`), reusing the same charset as the existing
`resourceid` tag (`^[a-zA-Z0-9_-]{1,32}$`) with a shorter max length
appropriate for a label rather than an identifier.

**`Region`/`Regions` format is deliberately not validated at this layer.**
AWS, GCP, and Azure region strings have different shapes
(`eu-central-1` vs. `us-central1` vs. `westeurope`); the core schema stays
cloud-agnostic (consistent with `provider: "agnostic"`, RFC 001 §2.3) and
delegates format validation to each provider's `Validate`, the same
pattern `AWSProvider` already applies via the `awsregion` custom
validator (RFC 002, `internal/provider/aws/decode.go`).

### 2.3 `S3Properties.Region` is superseded by `Scope.Region`

RFC 002 §2.6 put `Region` inside `S3Properties` because no cross-cutting
place existed yet for it. This RFC removes `S3Properties.Region` (no
back-compat shim: the project has no external consumers yet, and CLAUDE.md
directs against compatibility shims) and reads `r.Scope.Region` instead in
`AWSProvider.resourceProgram`/`Validate`. The `awsregion` format check
(§2.2) moves with it, applied to `Scope.Region` and to each element of
`Scope.Regions`.

### 2.4 Region/zone resolution

#### 2.4.1 Single region (unchanged behavior)

A resource with `Scope.Region` set (and no `Regions`) behaves exactly as
today's `S3Properties.Region` did: one Pulumi stack, one `aws:region`
config value (`setRegionConfig`, `internal/provider/aws/stack.go`).

#### 2.4.2 Multi-region: Engine-level fan-out, not a provider concern

`CloudProvider.Plan/Apply/Destroy` (RFC 001 §2.3) keep operating on **one
region per call** — no interface change. Multi-region fan-out happens in
`DefaultEngine`, so the logic is written once and applies to every
provider (AWS today, GCP/Azure later), not duplicated per package:

```go
// internal/engine/default.go (design sketch)
func effectiveRegions(s spec.Scope) []string {
    if len(s.Regions) > 0 {
        return s.Regions
    }
    if s.Region != "" {
        return []string{s.Region}
    }
    return []string{""} // unscoped, current behavior
}
```

For each resource, `DefaultEngine.Plan`/`Apply`/`Validate` iterate
`effectiveRegions(r.Scope)`, calling the resolved `CloudProvider` once per
region with a resource copy whose `Scope.Region` is set to that single
value and whose `Scope.Regions` is cleared. Results are aggregated: one
`provider.Diff`/`provider.Result` per region (§2.7 adds a `Region` field
to both so callers can tell them apart; today a Specification always
produces exactly one `Diff`/`Result` per `Resource.ID`, this RFC makes
that a 1\:N relationship when `Regions` is set).

`Policies.AllowedRegions` (RFC 001 §2.3) is checked against **every**
entry in `effectiveRegions`, not just one — a multi-region resource with
one non-compliant region is rejected entirely in `Validate` (fail-closed,
consistent with RFC 003 §2.3's approach), not partially applied.

#### 2.4.3 Zones: forwarded, not fanned out

Unlike regions, an availability zone is not stack-worthy — it is a
placement detail *within* one region's resources (e.g. RDS Multi-AZ, an
EC2 subnet choice), not an independent deployment target. `Scope.Zones`
is therefore passed through to the provider unchanged (already reachable
via `r.Scope` inside `spec.Resource`, no interface change needed) for
resource-type-specific HA logic to consume. **No resource type
implemented today is zone-aware** (`object_storage` is a global-within-region
S3 bucket; `cross_account_role` is IAM, global). Concretely in this RFC,
both existing types **reject** a non-empty `Scope.Zones` in `Validate`
(fail-closed: silently ignoring a user's explicit zone request would be
worse than an explicit error) — actual consumption arrives with whichever
future RFC introduces a zone-aware resource type (`relational_database`,
`compute_instance`, matching the example in CLAUDE.md's
`high_availability` field).

### 2.5 Sealing: a concrete, enforced validation rule

"Sealed unless otherwise specified" is implemented as: **`Scope.EffectiveSealed()
== true` (the default) forbids a resource from declaring any trust or
access relationship toward a different Account/Environment.** Today
exactly one `ResourceType` inherently does this: `cross_account_role`
(RFC 003), whose entire purpose is to grant a *different* AWS account
access. `AWSProvider.Validate` gains one rule:

```go
case spec.ResourceTypeCrossAccountRole:
    if r.Scope.EffectiveSealed() {
        return fmt.Errorf("aws: resource %q: %w", r.ID, ErrSealedCrossAccountRole)
    }
    // ... existing checks unchanged (RFC 003 §2.2, §2.3)
```

This makes the "otherwise specified" escape hatch explicit and auditable
at the Specification level: a reviewer sees `"sealed": false` right next
to the resource that needs it, instead of inferring intent from the mere
presence of `cross_account_role`. It costs nothing for existing
Specifications from RFC 003/004's test suites other than adding
`"scope": {"sealed": false}` to `cross_account_role` fixtures.

This RFC does **not** claim to enforce sealing at the network layer (VPC
peering, security groups, route tables) — no such resource type exists
yet. It **is** establishing a binding principle for every future RFC that
introduces networked resource types (analogous to how RFC 004 §3 bound
future provider RFCs to its credential principles): such resource types
must default to per-Environment network isolation (e.g. a `compute_instance`
must not default into a shared VPC across environments) and must expose
their own explicit opt-out, following the same `Scope.Sealed` convention.

### 2.6 State isolation: stack naming fix

`stackNameFor` (`internal/provider/aws/stack.go`) changes from
`resourceID` alone to a composite key derived from `Account`,
`Scope.Environment`, the single effective `Scope.Region` for that call
(§2.4.2), and `resourceID`, with empty segments omitted and the rest
joined by a separator not valid in any segment's charset (e.g. `"::"`,
disjoint from `resourceid`/`scopename`/region charsets):

```go
func stackNameFor(account, environment, region, resourceID string) string {
    parts := make([]string, 0, 4)
    for _, p := range []string{account, environment, region} {
        if p != "" {
            parts = append(parts, p)
        }
    }
    parts = append(parts, resourceID)
    return strings.Join(parts, "::")
}
```

A Specification using none of `Account`/`Scope.Environment`/`Scope.Region`
produces the exact same stack name as today (just `resourceID`) — existing
single-account, single-region tests are unaffected. This closes the gap
in §1.1: `account: "staging"` and `account: "production"` now resolve to
distinct Pulumi stacks even when `Resource.ID` collides, and the same
guarantee extends to `Environment` within one account.

Existing local Pulumi state created before this RFC (if any exists outside
test fixtures) would need manual stack renaming to migrate — acceptable
given the project has no production deployments yet (confirm in §6).

### 2.7 `provider.Diff`/`provider.Result`: `Region` field

```go
// internal/provider/provider.go
type Diff struct {
    ResourceID string
    Region     string `json:"region,omitempty"` // NEW: empty when unscoped, set otherwise
    Action     Action
    Changes    map[string]any
}

type Result struct {
    ResourceID string
    Region     string `json:"region,omitempty"` // NEW
    Status     Status
    Details    map[string]any
}
```

Populated by `DefaultEngine`'s fan-out loop (§2.4.2), not by individual
providers (keeps the change cloud-agnostic, in one place).

## 3. Security Considerations

- **Fail-closed sealing default**: `Scope.Sealed` absent ⇒ `true`. A
  cross-boundary resource must opt out explicitly (§2.5), never opt in by
  omission.
- **State isolation prevents cross-environment/cross-account collisions**:
  §2.6 closes a real bug where two different accounts could share one
  local Pulumi stack, which could cause CloudSDD to apply a diff computed
  against one account's actual state to a different account's
  credentials.
- **Mass Assignment**: `Scope` follows the same `additionalProperties:
  false` / `DisallowUnknownFields` two-tier validation as every other
  field (RFC 001 §3).
- **Unrestricted Resource Consumption**: `Scope.Regions` capped at 10
  entries (defense against a Specification fanning out one resource into
  an unbounded number of Plan/Apply calls, consistent with RFC 003 §2.2's
  20-entry caps for a similar reason).
- **`Policies.AllowedRegions` now applies per-entry** across
  `effectiveRegions`, not just to a single region value (§2.4.2):
  otherwise a multi-region resource could smuggle one non-compliant
  region past the existing check.
- **No new implicit privilege**: this RFC adds no new way to reach a
  different Account/Environment than already existed via `Account` +
  `DeploymentTarget` (RFC 004) and `cross_account_role` (RFC 003) — it
  only makes the existing implicit "sealed by construction" property of
  those two mechanisms explicit, checked, and extensible to `Environment`.

## 4. Testing

- **Unit (table-driven), `internal/spec`**: `Scope` validation (`Region`
  + `Regions` mutually exclusive, `Regions` min 2 / max 10, `Zones` max
  10, `scopename` charset/length for `Environment`, `EffectiveSealed`
  default).
- **Unit, `internal/engine`**: `effectiveRegions` (empty scope → `[""]`;
  single `Region` → 1 entry; `Regions` → N entries); fan-out produces one
  `Diff`/`Result` per region with `Region` populated; `Policies.AllowedRegions`
  rejects a multi-region resource with one non-compliant entry.
- **Unit, `internal/provider/aws`**: `stackNameFor` composite-key cases
  (all empty → bare `resourceID`, unchanged; each combination of
  account/environment/region present); `cross_account_role` rejected when
  `Sealed` is absent/true, accepted when explicitly `false`; `object_storage`/
  `cross_account_role` reject non-empty `Scope.Zones`; `cross_account_role`
  rejects non-empty `Scope.Region`/`Regions` (unchanged from RFC 003 §2.3's
  "IAM is global" rule, now expressed against `Scope` instead of
  implicitly).
- **Regression**: existing RFC 002/003/004 fixtures updated to use
  `Scope.Region` instead of the removed `S3Properties.Region`, and
  `cross_account_role` fixtures gain `"scope": {"sealed": false}`.
- **Fuzz**: extend the existing `internal/spec` fuzz target
  (`fuzz_test.go`) to include `Scope` in generated malformed input.

## 5. Out of Scope (for future RFCs)

- Network-layer sealing (VPC/subnet/security-group isolation per
  Environment) — no networked resource type exists yet; §2.5 records the
  binding principle for when one is introduced.
- Zone-aware placement logic for any concrete resource type (`Scope.Zones`
  is plumbing only in this RFC).
- Multi-region *replication* semantics beyond independent per-region
  fan-out (e.g. S3 Cross-Region Replication, DB read replicas) — this RFC
  gives N independent stacks per region, not a resource type that models
  replication between them.
- Symbolic cross-Environment resource references (would build on the
  still-open dependency-graph gap from RFC 003 §2.4/§5).
- Fixed enum of allowed `Environment` values — kept free-form (§6.1).
- Composite `DeploymentTarget` keyed by `(Account, Environment)` pairs to
  give same-account environments distinct credentials — out of scope
  here; `Environment` in this RFC is a CloudSDD-enforced *logical* boundary
  within shared credentials, not a new credential-scoping mechanism (that
  would be an extension of RFC 004, if ever needed).

## 6. Open Questions

1. **`Environment` as free-form vs. fixed enum.** This RFC keeps it
   free-form (`scopename` charset/length only), matching "dev, staging,
   production, **etc.**" from the requirement. Confirm, or would you
   prefer a closed enum (`dev|staging|qa|production`) enforced at the
   schema level?
2. **Engine-level multi-region fan-out** (§2.4.2) vs. pushing multi-region
   handling into each provider individually. This RFC centralizes it in
   `DefaultEngine` so GCP/Azure providers get it for free later. Confirm
   this is the right layer?
3. **Sealing enforcement scope for this RFC**: limited to rejecting
   `cross_account_role` unless `Sealed: false` (§2.5), with network-layer
   sealing deferred as a *principle* for future networked-resource RFCs.
   Confirm this phasing, or should this RFC attempt more (e.g. a
   Specification-wide check that rejects any two resources sharing an
   `Account` but declaring different `ResourceARNs`/trust that implies
   cross-`Environment` access)?
4. **Stack-naming migration** (§2.6): confirm no real local Pulumi state
   exists yet that would need manual renaming after this change lands.
