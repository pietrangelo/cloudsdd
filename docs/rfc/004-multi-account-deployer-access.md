# RFC 004: CloudSDD Deployer Access to External Accounts/Projects/Subscriptions

- **Status:** Approved (2026-07-27)
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-07-27
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 002](002-aws-provider.md), [RFC 003](003-aws-cross-account-iam.md)
  (all approved)

## 1. Problem

RFC 003 §2.4 had explicitly deferred the case "CloudSDD assumes a role in
a target account to apply resources to it", flagging two architectural
gaps: (1) no dependency mechanism between resources, (2) no
per-resource/per-account credential resolution (`AWSProvider` today has a
single global credential chain, RFC 002 §2.4).

Feedback received from the user clarifies the scope of this second gap:

> The AWS account, as well as the accounts of the other providers, must be
> able to access all accounts with the account administrator role,
> otherwise [CloudSDD] would not be able to create the requested
> resources. Access must follow the best practices and state of the art
> of AWS but also of all other Cloud Providers, to guarantee maximum
> security.

This is a problem **distinct** from RFC 003: RFC 003 (`cross_account_role`)
models **inbound** access, narrow and minimal-permission, that an
*external* account/tenant obtains toward specific resources managed by
CloudSDD. This RFC models **outbound** access: how CloudSDD itself reaches
a target account/project/subscription with privileges broad enough to
create *any* resource requested by the Specification — necessarily
broader than an explicit list of 20 actions, but still bound by best
practice, not unrestricted, uncontrolled access.

## 2. Proposed Architecture

### 2.1 Separation between the Specification (desired state) and access configuration

Core principle: the SDD Specification (RFC 001) describes **what** to
create, not **how to authenticate** to create it. Mixing the two would
expose role ARNs, account IDs, and trust references to anyone who can
read or review a Specification (which might be shared more widely than a
deployment configuration would be). A second configuration artifact is
therefore proposed, **`DeploymentTarget`**, managed at the engine/CLI
level, not inside the validated JSON Schema of RFC 001 §2.4:

```go
// internal/engine/target.go
type DeploymentTarget struct {
    Name    string       // referenced by spec.Resource.Account
    Provider spec.Provider // aws | gcp | azure
    Enabled bool          // explicit toggle: defaults to false until activated
    AWS     *AWSTargetConfig
    // GCP/Azure: equivalent structs, introduced when those providers
    // exist (dedicated RFCs); the principles in §3 apply from now regardless.
}

type AWSTargetConfig struct {
    AccountID              string
    RoleARN                string
    ExternalID             string
    SessionDurationSeconds int32 // cap, e.g. 3600; no long-lived credentials
}
```

`DeploymentTarget` is loaded from a separate file (e.g.
`cloudsdd.targets.json`, `0600` permissions, **excluded from git**), not
from the HTTP request/Specification body — consistent with RFC 001 §3
(the tenant ID/authentication context comes only from `context.Context`,
never from the body). Analogy: it is conceptually a "kubeconfig" for
CloudSDD, not a Specification.

### 2.2 Minimal extension to `spec.Resource`

```go
// internal/spec/spec.go
type Resource struct {
    ID         string
    Type       ResourceType
    Provider   Provider
    Account    string `json:"account,omitempty" validate:"omitempty,max=64"` // name of a DeploymentTarget; absent = default credentials (RFC 002 §2.4), unchanged behavior
    Properties map[string]any
}
```

**Optionality (direct answer to your first point):** `Account` is
`omitempty` and optional. A resource without this field behaves exactly
as it does today (RFC 002 §2.4, default credential chain) — no existing
or future resource is required to go through a `DeploymentTarget`.
Multi-account access is therefore opt-in per resource, not a default
behavior.

**Easily enabled/disabled:** `DeploymentTarget.Enabled` is the kill
switch. `Engine.resolveProvider`, before delegating any operation on a
resource with `Account` set, checks `Enabled == true`; if `false`, it
returns an explicit error (`ErrDeploymentTargetDisabled`) without
attempting any AWS call. There is no need to remove or rotate the
underlying trust relationship to suspend access: a single boolean field
in the configuration file, separate from the Specification, immediately
deactivates every operation toward that target.

This also partially resolves gap (2) from RFC 003 §2.4: per-resource
credential resolution now exists. Gap (1) — references/dependencies
between resources in the same Specification — remains open, stays out of
scope here, and requires a dedicated RFC.

## 3. Security Principles (valid for AWS, GCP, Azure)

These principles are binding for the AWS implementation in this RFC, and
**for any future RFC** that introduces GCP/Azure support for this same
mechanism — recorded here because the request was explicitly to guarantee
them "in this case [AWS] but also for all other Cloud Providers":

1. **Never long-lived static credentials for a target.** Always
   short-lived role/identity assumption: `sts:AssumeRole` for AWS;
   Workload Identity Federation (not service account JSON keys) for GCP;
   Azure Lighthouse or Managed Identity/Service Principal with PIM for
   Azure.
2. **Never a literal "God-mode" role.** "Account administrator" in the
   request must be interpreted as "sufficient to create any
   infrastructure resource", not as unrestricted total access:
   - AWS: the managed **`PowerUserAccess`** policy (creates/manages
     almost all resources) instead of `AdministratorAccess` (which
     includes IAM/Organizations management and therefore privilege
     escalation), plus any additional targeted IAM permissions needed
     (e.g. to create the `cross_account_role` resources from RFC 003)
     with a **permission boundary** that prevents the role from
     self-modifying its own permissions or creating broader ones.
   - GCP: `roles/editor` on the target project instead of `roles/owner`
     (which includes IAM policy admin and billing).
   - Azure: `Contributor` on the target subscription/resource group
     instead of `Owner` (which includes RBAC role assignment).
3. **Confused deputy mitigated wherever a cross-boundary trust exists.**
   `sts:ExternalId` mandatory for AWS (same principle as RFC 003 §2.2);
   equivalent conditions for GCP impersonation and for Azure Lighthouse
   delegations.
4. **Defense in depth on region**, in addition to the constraint already
   introduced in RFC 003 §2.3 (`aws:RequestedRegion` in the role's
   policy): adoption of organizational guardrails — AWS Organizations
   SCPs, GCP Organization Policy Constraints, Azure Policy — is
   documented as a client-side prerequisite (not direct enforcement by
   CloudSDD, which has no visibility into the target's SCPs/Org Policy),
   as a second layer independent of the first.
5. **Native auditability of the target provider**: CloudTrail (AWS) /
   Cloud Audit Logs (GCP) / Activity Log (Azure) must be active in the
   target — a documented precondition, not actively verified by CloudSDD
   in this RFC (active verification is out of scope, see §5).
6. **Kill switch independent of the Specification** (§2.2):
   `DeploymentTarget.Enabled` deactivates access without touching the
   underlying trust relationship or the Specification.

## 4. Impact on `internal/engine`

`DefaultEngine.resolveProvider` (today in `internal/engine/default.go`)
must, when `r.Account != ""`:

1. Look up `r.Account` in the `DeploymentTarget` registry (new
   constructor field, analogous to the existing `providers` registry).
2. Reject with `ErrDeploymentTargetNotFound` if absent,
   `ErrDeploymentTargetDisabled` if `Enabled == false`.
3. Obtain assumed credentials (STS AssumeRole for AWS) and
   build/reuse an `AWSProvider` instance configured with that context,
   instead of the default instance in the `providers` registry.

This does not change the `CloudProvider` signature (RFC 002 §2.5, already
including `spec.Policies`): the change is entirely in resolving *which*
provider instance is used for the resource, not in the interface
contract.

## 5. Out of Scope (for future RFCs)

- Concrete GCP/Azure implementation of `DeploymentTarget` (the principles
  in §3 are binding from now, the code will come with the RFCs for the
  respective providers).
- Active verification, by CloudSDD, that CloudTrail/Audit Logs/Activity
  Log are actually enabled in the target (would require additional read
  permissions and dedicated logic).
- References/dependencies between resources in the same Specification
  (gap (1) from RFC 003 §2.4, still open).
- Automatic rotation of `ExternalID`/`RoleARN` in `DeploymentTarget`s.
- UI/CLI to manage the `cloudsdd.targets.json` file (manual file editing
  is assumed for now).

## 6. Open Questions

1. Do you confirm `PowerUserAccess` (+ permission boundary) as the target
   permission level for AWS, instead of `AdministratorAccess`? This
   choice aligns with "state of the art" but is more restrictive than a
   "true" administrator: if the Specification were to require, in the
   future, resources that `PowerUserAccess` cannot manage (typically
   IAM/Organizations operations), an explicit supplemental permission
   would be needed, to be evaluated case by case.
2. Is `cloudsdd.targets.json` as a local file with `0600` permissions
   sufficient for this stage, or is integration with an external secret
   manager needed from the start (same open question as RFC 003 §6.2,
   extended here to the whole targets file)?
3. Do you confirm that the GCP/Azure implementation should stay at the
   level of principles (§3) in this RFC, deferring the code to dedicated
   RFCs when those providers are introduced?
