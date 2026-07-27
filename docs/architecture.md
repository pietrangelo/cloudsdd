# CloudSDD Architecture

> Current state of the system. Design decisions are tracked in the RFCs
> under `docs/rfc/`; this document reflects what has **actually been
> implemented**, not proposals.

## Overview

CloudSDD converts an SDD Specification (JSON) into operations on cloud
providers, through a cloud-agnostic Go engine. The intended end-to-end
flow is:

```
NL request ──(not yet implemented)──► JSON Specification ──► Engine.Validate
                                                                      │
                                                                      ▼
                                                                Engine.Plan
                                                                      │
                                                                      ▼
                                                                Engine.Apply
```

The "JSON Specification → Engine → Provider" part is implemented for AWS
(RFC 002, 003, 004). Natural-language translation → Specification and the
HTTP API layer remain out of scope and do not yet have a dedicated RFC.

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
├── cmd/                    # Executable entry points (empty: no RFC has defined them yet)
├── internal/
│   ├── spec/                # Specification types, strict parsing, domain validation
│   ├── provider/             # CloudProvider interface, Diff/Result types
│   │   └── aws/                # Concrete AWS implementation (RFC 002/003/004)
│   └── engine/                # Engine interface, DefaultEngine, DeploymentTarget (RFC 004)
├── pkg/                     # Empty: no public type exposed yet
├── LICENSE                  # GNU AGPLv3 (or later), full text
└── docs/
    ├── rfc/001-004...        # Foundation RFC + AWS provider + cross-account + multi-account (approved)
    ├── architecture.md        # This document
    ├── api.md                 # HTTP API status (not yet implemented) + programmatic usage
    ├── dependency-licenses.md # Third-party license audit vs. AGPLv3
    └── openapi.yaml            # OpenAPI schema (Specification + AWS properties, no paths)
```

## Package `internal/spec`

Represents and validates the SDD Specification (`Specification`,
`Resource`, `Policies`).

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
  enums, `max_cost_monthly` positive if present. `Resource.Account` (RFC
  004 §2.2) is optional: it references, by name, a `DeploymentTarget`
  resolved by the `Engine`, never an ARN or a credential.
- **`ParseAndValidate`**: combines the two steps.
- `Resource.Properties` remains `map[string]any`: a generic JSON transport
  container. Typed and validated decoding of properties specific to each
  `ResourceType`/provider is the responsibility of each concrete
  `CloudProvider`.

The parser is covered by a native Go fuzz test (`FuzzParse`), run against
malformed input, nested JSON, unknown fields, and empty input: the
invariant being verified is the absence of panics, not the acceptance of
the input.

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
  `bucket_name`, `region`, `versioning`/`encryption`/`block_public_access`
  as `*bool` with a secure default when absent — `encryption` and
  `block_public_access` default to `true`, `versioning` defaults to
  `false`) and `cross_account_role` → cross-account IAM role
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
  `object_storage` (the resource's region) and enforces fail-closed
  behavior on `cross_account_role` when `AllowedRegions` is empty (RFC
  003 §2.3: IAM is global on AWS, so the region constraint translates
  into an `aws:RequestedRegion` `Condition` on the generated policy).
- **State**: local filesystem backend (`CLOUDSDD_STATE_DIR`, default
  `~/.cloudsdd/state`), passphrase-based secrets provider
  (`CLOUDSDD_PULUMI_PASSPHRASE`, mandatory — `NewProvider` fails
  explicitly if absent), one Pulumi stack per `Resource.ID`.
- **Credentials**: defaults to the standard AWS SDK credential chain (RFC
  002 §2.4). With `Resource.Account` set, the Engine instead resolves a
  `DeploymentTarget` and uses credentials obtained via STS AssumeRole
  (`NewTargetProviderFactory`, RFC 004 §4), passed explicitly to the
  Pulumi AWS provider (never written to disk).

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

## Security

Measures active as of today (see also RFC 001 §3, 002 §2.4-2.5, 003
§2.2-2.3, 004 §3):

| OWASP API Top 10 threat | Current mitigation |
|---|---|
| Mass Assignment | Strict JSON decoding (`DisallowUnknownFields`) + two-tier validation, both in `internal/spec` and in every provider (`internal/provider/aws`) |
| Hostile input / parser crash | Native Go fuzz test on `spec.Parse` |
| Credentials in the Specification | Denylist of credential-like field names in `decodeProperties`, independent of the schema |
| Confused deputy (cross-account) | `external_id` mandatory on `cross_account_role` (RFC 003) and on AWS `DeploymentTarget` (RFC 004) |
| Cross-account privilege escalation | No full wildcard (`*`) permissions on `cross_account_role`; `AdministratorAccess` explicitly discouraged for `DeploymentTarget`s (RFC 004 §3, principles also valid for GCP/Azure once implemented) |
| Region bypass / uncontrolled cost | `Policies.AllowedRegions` enforced by every provider; `cross_account_role` is fail-closed if `AllowedRegions` is empty |
| BOLA | Not yet applicable: no multi-tenant storage/state layer exists yet (note: the stack-per-resource design in RFC 002 §2.3 has no tenant namespacing, open question) |
| Unrestricted Resource Consumption | Not yet applicable at the HTTP/API level (it does not exist yet); at the provider level, a cap of 20 entries on IAM lists (RFC 003) |

Scans run before this commit: `gosec ./...` (0 issues; 2 false positives
suppressed with `#nosec` and inline justification — an env var name
flagged as G101, a path from an env var flagged as G703 path-traversal
despite being operator-controlled, not Specification input),
`govulncheck ./...` (0 vulnerabilities reachable from the code;
`go.opentelemetry.io/otel` and `github.com/klauspost/compress` proactively
upgraded to the versions with fixes; one unreachable vulnerability with no
available fix remains in `golang.org/x/crypto/openpgp`, a transitive
dependency not invoked by our code).

## Testing

- Table-driven tests for `internal/spec` (90.9%), `internal/engine`
  (93.2%, including a test `mockProvider` and `DeploymentTarget`
  resolution), and `internal/provider/aws` (61.4%).
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

See RFC 001 §4-5, RFC 002 §6, RFC 003 §5-6, RFC 004 §5-6. In summary, not
yet implemented: GCP/Azure providers, other AWS `ResourceType`s
(`relational_database`, `compute_instance`, `container_service`),
resolution of `provider: "agnostic"`, references/dependencies between
resources in the same Specification, cost estimation and enforcement of
`max_cost_monthly`, multi-tenant isolation of state (real BOLA), HTTP API
layer, NL → Specification translation.
