# RFC 003: Cross-Account Role and Permission Management (AWS)

- **Status:** Approved (2026-07-27)
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-07-27
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md) (approved),
  [RFC 002](002-aws-provider.md) (approved)

## 0. Decisions already gathered

Before writing this RFC, 3 scoping questions were put to the user.
Answers received, binding for the design below:

1. **Goal:** both — (a) CloudSDD must be able to *assume* a role in a
   target AWS account to provision resources on behalf of that account,
   and (b) resources managed by CloudSDD must be able to *grant* access to
   external AWS accounts (e.g. cross-account bucket policy).
2. **Modeling:** a new dedicated `ResourceType` in the Specification (not
   a nested property on existing resources).
3. **Region constraint:** IAM is a global AWS service (a role does not
   "live" in a region), so the constraint translates to: the actions
   allowed by the role stay confined within the existing
   `Policies.AllowedRegions` perimeter (RFC 001 §2.3), rather than a new
   "role region" concept.

## 1. Problem

CloudSDD must be able to express, in the Specification, IAM trust
relationships between AWS accounts: an account granting another account
the ability to assume a role with restricted permissions. This covers two
real usage patterns:

- **External grant**: the account managed by CloudSDD exposes a role that
  an external account (partner, another team, another environment) can
  assume to operate on specific resources, with minimal permissions.
- **Cross-account deploy**: CloudSDD itself assumes a role in a target
  account to apply a Specification to it, without the "primary"
  credentials (RFC 002 §2.4) having permanent direct access to that
  account.

## 2. Proposed Architecture

### 2.1 New `ResourceType`: `cross_account_role`

Extension of the core schema (RFC 001 §2.4, which explicitly anticipated
`resources[].type` as an enum "to be extended via dedicated RFCs"):

```go
// internal/spec/spec.go
const ResourceTypeCrossAccountRole ResourceType = "cross_account_role"
```

No other field of `spec.Resource`/`Specification` changes besides the
enum: the resource remains an element of the same `resources[]` list,
with provider-specific `Properties` just as already happens for
`object_storage`.

### 2.2 AWS Properties: `CrossAccountRoleProperties`

```go
// internal/provider/aws/crossaccountrole.go
type CrossAccountRoleProperties struct {
    Enabled          *bool    `json:"enabled,omitempty"`
    RoleName         string   `json:"role_name" validate:"required,min=1,max=64"`
    TrustedAccountID string   `json:"trusted_account_id" validate:"required,numeric,len=12"`
    ExternalID       string   `json:"external_id" validate:"required,min=8"`
    Permissions      []string `json:"permissions" validate:"required,min=1,max=20,dive,required,ne=*"`
    ResourceARNs     []string `json:"resource_arns" validate:"required,min=1,max=20,dive,required"`
}
```

Design notes:

- **`Enabled` (defaults to `true` if absent, `*bool` for the same reason
  as `S3Properties` in RFC 002 §2.6): explicit toggle for the grant**.
  The `ResourceType` is already opt-in by itself (the user must declare
  it explicitly in `resources[]`, it is never forced onto other
  resources), but this field covers a different use case: temporarily
  suspending cross-account access **without removing the block from the
  Specification** (which would mean losing the configuration and having
  to recreate it from scratch later). When `Enabled == false`,
  `AWSProvider.Plan`/`Apply` compute the destruction of the underlying IAM
  resources (role/policy) if they already exist, but the block stays
  declared and can be re-enabled with a single field. `Validate` requires
  no other change elsewhere: the other validations (§2.2 below) remain
  active even when `Enabled: false`, because a temporarily disabled
  configuration must still be internally consistent for when it is
  re-enabled.
- **`ExternalID` mandatory** (not optional): this is the standard AWS
  mitigation for the *confused deputy problem* in cross-account trust
  policies (a third party who knows the role's ARN cannot assume it
  without also knowing the `ExternalID`, which the tenant generates and
  communicates out of band). Making it optional would shift onto every
  Specification author the responsibility of remembering it; fail-closed
  is preferred.
- **`Permissions` forbids `"*"`** (validator `ne=*`): a fully wildcard IAM
  action cannot be expressed with this ResourceType. Service-scoped
  wildcards are allowed (`"s3:Get*"`), not the total action. Maximum of
  20 actions to limit the size/complexity of the generated policy
  (defense against Unrestricted Resource Consumption, consistent with
  CLAUDE.md).
- **`ResourceARNs` mandatory, no implicit `Resource: "*"`**: this RFC
  requires the user to explicitly list the ARNs of the resources the role
  grants access to (they may include wildcards within the ARN itself,
  e.g. `arn:aws:s3:::my-bucket/*`, but not a generic `"*"` as the entire
  policy). A **symbolic** reference to another `Resource.ID` in the same
  Specification (e.g. "give me the real ARN of bucket `app-data` once
  created") is **out of scope here**: see §2.4.
- Decoding/validation uses the same two-tier pattern already used for
  `S3Properties` (RFC 002 §2.6): re-marshal + `DisallowUnknownFields` +
  `validator`.

### 2.3 Pulumi mapping and region enforcement

Pulumi AWS Classic resources generated:

- `iam.NewRole`: trust policy (`AssumeRolePolicyDocument`) with
  `Principal.AWS = arn:aws:iam::<trusted_account_id>:root` and
  `Condition.StringEquals["sts:ExternalId"] = ExternalID`.
- `iam.NewRolePolicy` (inline, not managed, to avoid a shared policy
  reusable elsewhere): `Action = Permissions`,
  `Resource = ResourceARNs`, plus a second `Condition` block that applies
  the region constraint decided in point 3 of §0:
  `Condition.StringEquals["aws:RequestedRegion"] = Policies.AllowedRegions`.

`AWSProvider.Validate` (signature with `spec.Policies` as per RFC 002
§2.5) **rejects** a `cross_account_role` resource if
`Policies.AllowedRegions` is empty: a cross-account role with no explicit
region constraint is an exposure this RFC does not want to allow by
default (fail-closed, consistent with the "allowed_regions perimeter"
choice from §0.3).

### 2.4 Out of scope in this RFC: symbolic references and real cross-account deploy

The "both" answer from §0.1 ideally also implies that CloudSDD *assumes*
the role to apply other resources in the target account. Analyzing the
impact, this requires two architectural extensions that are **not**
contained in this RFC:

1. **References between resources in the same Specification** (e.g.
   `resource_refs: ["app-data"]` instead of literal ARNs): today
   `CloudProvider` (RFC 002 §2.5) operates on one `Resource` at a time and
   has no visibility into the other resources of the `Specification`, nor
   does the `Engine` know the shape of `Properties` (it is
   provider-specific by design, RFC 001 §2.3). A generic
   dependency/output mechanism between resources would be needed
   (an application graph, not just list order) — useful beyond IAM too
   (e.g. a `compute_instance` referencing an `object_storage`), so it
   deserves a dedicated RFC rather than an ad hoc solution here.
2. **Per-resource/per-account AWS credentials**: `AWSProvider` today is a
   single instance with a single credential chain (RFC 002 §2.4). For
   CloudSDD to actually assume this role in order to apply *other*
   resources of the same Specification in the target account requires the
   provider to know, resource by resource, which credential context to
   use — a broader design change than the single ResourceType.

This RFC therefore provides the **primitive** (the cross-account role,
created/planned/destroyed like any other resource, with the ARN exposed
in `Result.Details` after Apply), which fully covers the "external grant"
case (§0.1a) right away, and lays the groundwork for the "cross-account
deploy" case (§0.1b), which remains blocked on a future RFC dedicated to
resource dependencies and multi-account credential resolution. It is not
presented as implemented before it actually is.

## 3. Security Considerations

- **Confused deputy**: `ExternalID` mandatory (§2.2).
- **Least privilege by construction**: no `Permissions: ["*"]`, no
  implicit `ResourceARNs` of `"*"`, hard cap of 20 entries per list
  (§2.2).
- **Fail-closed on region**: a `cross_account_role` without
  `allowed_regions` configured is rejected in `Validate`, not applied
  with a permissive default (§2.3).
- **Inline, non-managed policy**: prevents the generated policy from
  being reusable/attackable by other IAM resources outside CloudSDD's
  control (§2.3).
- **Mass Assignment**: same two-tier validation already applied to
  `S3Properties` (§2.2).
- **No implicit escalation**: this RFC introduces no way for a tenant to
  obtain credentials for CloudSDD's own "primary" account — the created
  role grants access *from* an external account *to* explicitly listed
  resources, never the other way around.

## 4. Testing

- **Unit (table-driven):** `CrossAccountRoleProperties` validation
  (missing ExternalID rejected, `Permissions` with `"*"` rejected, empty
  `ResourceARNs` rejected, more than 20 entries rejected,
  `trusted_account_id` not 12 digits rejected).
- **Unit:** `Validate` rejects `cross_account_role` when
  `Policies.AllowedRegions` is empty.
- **Integration (LocalStack, as in RFC 002 §5):** Plan → Apply → verify via
  `aws-sdk-go-v2/service/iam` that the generated trust policy and
  permission policy exactly match what was declared → Destroy.

## 5. Out of Scope (for future RFCs)

- Symbolic references between resources in the same Specification
  (`resource_refs` instead of literal ARNs) and, more generally, a
  dependency/output mechanism between resources in the `Engine`.
- Per-resource/per-account AWS credential resolution (real cross-account
  deploy, where CloudSDD assumes the role and uses it to apply other
  resources of the same Specification).
- Cross-account support on providers other than AWS (GCP: cross-project
  IAM bindings; Azure: cross-tenant role assignments) — an analogous
  pattern, not covered here.
- Automatic rotation or expiration of cross-account roles.

## 6. Open Questions

1. Do you confirm the phasing in §2.4 (this RFC covers only the creation
   of the `cross_account_role` primitive with explicit ARNs; real
   cross-account deploy via assume-role requires a follow-up RFC on
   resource dependencies + multi-account credentials)?
2. Should `ExternalID` be mandatory and generated/provided by the caller
   of the Specification (no default), or would you prefer CloudSDD to
   generate it automatically when absent (which would then require
   returning it in `Result.Details` so it can be communicated to the
   external account)?
3. The cap of 20 entries for `Permissions`/`ResourceARNs` is arbitrary: is
   it fine as an initial limit, or would you prefer a different value?
