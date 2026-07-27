# RFC 002: First Concrete Provider — AWS

- **Status:** Approved (2026-07-27)
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-07-27
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md) (approved)

## 1. Problem

RFC 001 defined the `provider.CloudProvider` interface, but no concrete
implementation exists yet: `resolveProvider` in the `Engine` returns
`ErrProviderNotFound` for any `provider` other than `"agnostic"`. A first
real backend is needed to validate end-to-end the design of RFC 001
(Specification → Validate → Plan → Apply → Destroy) against a real cloud
provider, and to surface any design gaps before replicating the pattern on
GCP/Azure.

This RFC covers the implementation of the **AWS** provider, with an
initial scope limited to **a single `ResourceType`**: `object_storage`
(S3). The other types (`relational_database`, `compute_instance`,
`container_service`) are explicitly deferred to future RFCs (§6), to keep
the first vertical increment small, testable, and reviewable.

## 2. Proposed Architecture

### 2.1 Provisioning mechanism: Pulumi Automation API

As stated in CLAUDE.md ("via the Pulumi Go SDK or IaC manifest
generation"), it is proposed to use the **Pulumi Automation API**
(`github.com/pulumi/pulumi/sdk/v3/go/auto`) in *inline program* mode (the
Pulumi "program" is generated at runtime in Go from the validated
`Resource`, not an external `.ts`/`.py` file). Rationale:

- No external `pulumi` CLI process to shell out to: the Automation API is
  a Go library, consistent with "Idiomatic Go" and with the absence of
  external tool dependencies in the execution path.
- Preview (`stack.Preview`) and Up (`stack.Up`) map naturally onto
  `CloudProvider.Plan` and `CloudProvider.Apply`.
- Reuses the Pulumi AWS Classic provider
  (`github.com/pulumi/pulumi-aws/sdk/v6/go/aws`), which in turn
  encapsulates standard AWS credential resolution (see §4).

**Discarded alternative:** generating Terraform/CloudFormation manifests
and shelling out to an external binary. Discarded because it introduces an
external process dependency (an attack surface for command injection if
parameters ever ended up in a CLI invocation) and complicates the
structured capture of Plan/Apply that the Automation API offers natively
in Go.

### 2.2 Package structure

```
internal/provider/aws/
├── provider.go       # AWSProvider: implements provider.CloudProvider
├── objectstorage.go  # S3Properties, decoding/validation, mapping onto pulumi-aws s3.Bucket
├── stack.go           # helpers: Pulumi project/stack name, local backend, secrets provider
└── *_test.go
```

`AWSProvider` implements `provider.CloudProvider` with `Name() == "aws"`
and is registered in the `map[spec.Provider]provider.CloudProvider` passed
to `engine.New` (no change to the `Engine` constructor).

### 2.3 Pulumi state backend

Every Pulumi operation requires a state backend and a secrets provider.
For this first increment it is proposed to use:

- **Backend:** local filesystem, `file://<CLOUDSDD_STATE_DIR>` (default
  `~/.cloudsdd/state` if the variable is not set). No dependency on a
  Pulumi Cloud account or a dedicated S3 state bucket at this stage.
- **Project/Stack:** a logical Pulumi project `cloudsdd-aws`, one **stack
  per `Resource.ID`** (e.g. stack `app-db`), to isolate Plan/Destroy per
  individual resource and preserve the 1:1 resource→stack pass-through
  already implicit in the `Engine`'s design.
- **Secrets provider:** `passphrase`, read exclusively from the
  `CLOUDSDD_PULUMI_PASSPHRASE` environment variable (never from
  `Properties`, never logged). Needed because the state may contain
  sensitive outputs (ARNs, endpoints). If the variable is missing, the
  provider fails explicitly at startup instead of falling back to a weak
  default.

Stack-per-resource without tenant namespacing is a **temporary** choice:
see open question §7.1 (BOLA).

### 2.4 AWS Credentials

The provider **never reads, accepts, or forwards** AWS credentials taken
from `Resource.Properties` or any field of the Specification. Resolution
is delegated entirely to the Pulumi AWS provider, which follows the
standard SDK credential chain (env vars `AWS_ACCESS_KEY_ID`/`AWS_PROFILE`,
shared config `~/.aws/config`, IAM role/instance profile). As defense in
depth, the decoding of `Properties` (§2.6) rejects upfront any key whose
name is reminiscent of credentials (`access_key`, `secret`, `password`,
`token`, case-insensitive) even if it were not a known schema field.

### 2.5 Policy enforcement — minimal interface change

RFC 001 §3 states that `allowed_regions` is a security/cost defense, but
`CloudProvider.Validate(ctx, r)` today does not receive `spec.Policies`:
the provider has no way to know which regions are allowed. Since only one
concrete provider exists (no implementation to migrate besides the test
mocks), it is proposed to extend the interface **now**, before adding more
providers makes the breaking change more costly:

```go
// internal/provider/provider.go — updated signature
type CloudProvider interface {
    Name() string
    Validate(ctx context.Context, r spec.Resource, p spec.Policies) error
    Plan(ctx context.Context, r spec.Resource, p spec.Policies) (Diff, error)
    Apply(ctx context.Context, r spec.Resource, p spec.Policies) (Result, error)
    Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error
}
```

`DefaultEngine` (in `internal/engine/default.go`) will pass `s.Policies`
to every call instead of just `r`. Impact: mechanical change to the 4
call sites in `default.go` and to the corresponding `mockProvider` in
existing tests; no behavior change for `spec`/JSON Schema.

`AWSProvider.Validate` will use `Policies.AllowedRegions` to reject a
resource whose `region` (S3 property, see §2.6) is not in the list, when
the list is non-empty. `MaxCostMonthly` is **not** enforced in this RFC:
cost estimation requires a dedicated module (pricing API or static
tables) that is out of scope here, see §6.

### 2.6 `ResourceType: object_storage` on AWS → S3

```go
// internal/provider/aws/objectstorage.go
type S3Properties struct {
    BucketName          string `json:"bucket_name" validate:"required,min=3,max=63,lowercase,dns_domain_label"`
    Region               string `json:"region" validate:"required"`
    Versioning           *bool  `json:"versioning,omitempty"`
    Encryption            *bool  `json:"encryption,omitempty"`
    BlockPublicAccess     *bool  `json:"block_public_access,omitempty"`
}
```

"Secure by default" design notes:

- `Versioning`, `Encryption`, `BlockPublicAccess` are `*bool` (not
  `bool`): the **absence** of the field activates the secure default
  (`Encryption = true`, `BlockPublicAccess = true`; `Versioning = false`
  because it carries a storage cost, not a security posture). A plain
  `bool` would use `false` as the zero-value, which for
  `block_public_access`/`encryption` would be a silent **insecure**
  default — exactly the kind of mistake this RFC wants to structurally
  rule out.
- Decoding: `Resource.Properties` (`map[string]any`) is re-marshaled to
  JSON and re-decoded with `json.Decoder.DisallowUnknownFields` into
  `S3Properties`, then validated with `go-playground/validator` — the
  same two-tier pattern as `internal/spec.Parse`/`Validate`, applied here
  to block Mass Assignment at the provider level too (consistent with RFC
  001 §3).
- Mapping onto Pulumi: `s3.NewBucketV2` (+ `s3.BucketVersioningV2`,
  `s3.BucketServerSideEncryptionConfigurationV2`,
  `s3.BucketPublicAccessBlock`) from the Pulumi AWS Classic package.

### 2.7 Diff/Result mapping

- `Plan` → `stack.Preview(ctx)`: the summary of Pulumi's
  `ResourceChanges` is inspected and reduced to a single
  `provider.Diff{Action: ...}` for the resource (create/update/noop);
  `Changes` reports the properties with a non-empty diff.
- `Apply` → `stack.Up(ctx)`: outcome reported as `provider.Result` with
  `Status: applied` if the Pulumi update finishes without errors,
  `failed` otherwise (the error is still propagated via the returned
  `error`, as per the existing signature).
- `Destroy` → `stack.Destroy(ctx)` followed by stack removal
  (`workspace.RemoveStack`) so as not to leave orphaned empty stacks in
  the local backend.

### 2.8 Impact on the JSON Schema

Extension of `$defs` (draft, will be formalized in `docs/openapi.yaml`
upon implementation) to validate `properties` when
`type == object_storage` and `provider == aws`:

```json
{
  "bucket_name": { "type": "string", "minLength": 3, "maxLength": 63 },
  "region": { "type": "string" },
  "versioning": { "type": "boolean" },
  "encryption": { "type": "boolean" },
  "block_public_access": { "type": "boolean" }
}
```

Note: JSON Schema validation of `properties` remains conditional on
`type`+`provider` (not generalizable until a second provider exists); the
implementation will still use the provider's two-tier validation (§2.6)
as the authority on domain rules, consistent with how `internal/spec`
already treats `Properties` as a transport container.

## 3. New Dependencies

| Module | Purpose | Weight |
|---|---|---|
| `github.com/pulumi/pulumi/sdk/v3` | Automation API (Plan/Apply/Destroy) | Heavy, but explicitly anticipated by CLAUDE.md |
| `github.com/pulumi/pulumi-aws/sdk/v6` | Typed AWS resources (S3, etc.) | Heavy, necessary |
| `github.com/aws/aws-sdk-go-v2` (+ `service/s3`) | Only in integration tests, to verify real state against LocalStack | Test only |
| `github.com/testcontainers/testcontainers-go` | LocalStack container for integration tests | Test only |

These are the only "heavy" dependencies in the project: the HTTP layer
(out of scope here) will remain on `net/http`/Chi as per CLAUDE.md. This
trade-off is flagged explicitly for approval, since CLAUDE.md generally
requires "zero heavy external dependencies".

## 4. Security Considerations

- **Credentials:** never in the Specification; resolved via the standard
  AWS SDK credential chain, delegated to the Pulumi provider (§2.4).
- **Mass Assignment:** strict decoding + validator at the provider level
  too, not only in `internal/spec` (§2.6).
- **Secure defaults:** S3 bucket created with encryption-at-rest and
  public-access blocking active by default; requires explicit action
  (explicit `false` in the Specification) to disable them (§2.6).
- **Policy enforcement:** `allowed_regions` applied in `Validate` before
  any Pulumi call (§2.5). `max_cost_monthly` remains unenforced — see §6,
  not to be presented as implemented.
- **State secrecy:** the local Pulumi state is encrypted with a
  passphrase (§2.3) and must be excluded from git (add the state dir to
  `.gitignore` at implementation time); never logged in plaintext in any
  error path.
- **BOLA:** not addressed in this RFC (stack-per-resource without
  tenant), explicitly flagged as an open question (§7.1) and blocking for
  multi-tenant production use.
- **gosec/govulncheck:** run as part of the standard process before
  presenting code; the new Pulumi dependencies will be checked with
  `govulncheck` like everything else.

## 5. Testing

- **Unit (table-driven):** `S3Properties` decoding/validation (valid/invalid
  bucket name, unknown field rejected, secure defaults applied when
  pointers are `nil`, region outside `allowed_regions` rejected).
- **Integration (`testcontainers-go` + LocalStack):** `localstack/localstack`
  container with S3 emulated; full cycle
  Plan → Apply → verify bucket via `aws-sdk-go-v2` → Destroy → verify
  removal. Gated behind the `integration` build tag
  (`go test -tags=integration ./...`) so as not to require Docker in the
  default suite.
- **No new fuzz target:** parsing/validation of `Properties` reuses the
  same pattern already fuzzed in `internal/spec`; a dedicated fuzz target
  will be considered only if decoding introduces non-trivial logic beyond
  the JSON re-marshal.
- >90% coverage target maintained for the new `internal/provider/aws`
  package.

## 6. Out of Scope (for future RFCs)

- `relational_database` (RDS), `compute_instance` (EC2),
  `container_service` (ECS/Fargate) on AWS.
- GCP/Azure providers.
- Resolution of `provider: "agnostic"` (depends on RFC 001 §5.2, still
  open).
- Cost estimation and enforcement of `max_cost_monthly`.
- Multi-tenant isolation of state (real BOLA, not just the design note in
  RFC 001).
- Remote state backend (S3-compatible) instead of the local filesystem.

## 7. Open Questions

1. **BOLA/multi-tenant:** is the `cloudsdd-aws/<resource-id>` Pulumi stack
   without a tenant prefix acceptable for this first increment
   (local/single-tenant use), given that it must be explicitly blocked
   before any multi-tenant exposure? Do you confirm it is acceptable to
   defer it to a dedicated RFC on the state/tenancy module?
2. **Secrets provider:** is the environment-variable passphrase
   sufficient for this stage, or is integration with an external secret
   manager (Vault/AWS KMS) needed from the start?
3. **`CloudProvider` interface change** (§2.5, adding `spec.Policies` to
   every method): do you confirm it is preferable to do this now, with
   only one provider to adapt, rather than introducing an alternative
   mechanism (e.g. `Policies` in `context.Context`)?
4. **Next AWS `ResourceType`** after `object_storage`: `relational_database`
   (RDS) or `compute_instance` (EC2)?
