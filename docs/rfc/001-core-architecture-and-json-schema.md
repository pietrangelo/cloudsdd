# RFC 001: Core Architecture and Universal JSON Schema

- **Status:** Approved
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-07-27

## 1. Problem

CloudSDD must turn natural-language requests into infrastructure that is
actually provisioned on cloud providers, in a cloud-agnostic, secure,
repeatable, and verifiable way. This requires:

1. A **formal intermediate representation** (the "Specification") that
   captures the user's intent unambiguously, independent of the target
   cloud provider.
2. A **Go engine** capable of:
   - validating the Specification against a strict schema (defense against
     hostile input, Mass Assignment, unexpected fields);
   - resolving the Specification into concrete resources on one or more
     cloud providers (AWS, GCP, Azure, etc.) through a common abstraction;
   - applying the state idempotently (create/update/no-op) and reporting a
     plan/diff before execution, following the Terraform/Pulumi model.
3. A clean boundary between "NL → Specification translation" (out of scope
   in this RFC, will be a separate module, e.g. LLM-assisted) and
   "Specification → Infrastructure" (the Go engine, the subject of this
   RFC).

This RFC covers exclusively the **foundation**: project structure,
universal JSON schema, and the engine's core interfaces. It does not yet
cover the implementation of individual providers or the diffing/planning
logic, which will be the subject of future RFCs.

## 2. Proposed Architecture

### 2.1 Project structure

```
cloudsdd/
├── cmd/                    # Entry points for executables (e.g. cmd/cloudsddctl)
├── internal/               # Private application code, not importable
│   ├── spec/               # Parsing, validation, and types of the SDD Specification
│   ├── engine/              # Orchestration: receives the Specification, invokes providers
│   ├── provider/            # Concrete CloudProvider implementations (aws, gcp, ...)
│   ├── api/                 # HTTP layer (net/http or Chi), routing, middleware
│   └── security/            # Rate limiting, authz (BOLA), auth middleware
├── pkg/                    # Public, reusable code (client SDK, shared types)
├── docs/
│   ├── rfc/                 # Design RFCs (this document)
│   ├── architecture.md      # Always up-to-date overview (created on approval)
│   └── api.md                # API documentation (created on approval)
└── go.mod
```

Rationale: `internal/` prevents implementation details (e.g. a specific
provider) from being imported by external modules, keeping the public
surface limited to `pkg/`.

### 2.2 High-level flow

```
NL request ──(out of scope)──► JSON Specification ──► [1] Validation
                                                             │
                                                             ▼
                                                   [2] Planning (diff between
                                                       desired and current state)
                                                             │
                                                             ▼
                                                   [3] Apply (CloudProvider.Apply)
                                                             │
                                                             ▼
                                                   Updated state + report
```

### 2.3 Core interfaces (design, not implementation)

The following interfaces define the system's main contracts. They are
presented here as a **design specification** for the purpose of
architectural approval — the concrete implementation will happen only
after approval.

```go
// internal/spec: typed representation of the SDD Specification.
type Specification struct {
    SDDVersion string     `json:"sdd_version" validate:"required,eq=1.0"`
    Intent     Intent     `json:"intent" validate:"required,oneof=deploy update destroy plan"`
    Resources  []Resource `json:"resources" validate:"required,dive"`
    Policies   Policies   `json:"policies"`
}

type Resource struct {
    ID         string         `json:"id" validate:"required,alphanum_dash"`
    Type       ResourceType   `json:"type" validate:"required"`
    Provider   string         `json:"provider" validate:"required"` // "agnostic" | "aws" | "gcp" | "azure"
    Properties map[string]any `json:"properties" validate:"required"`
}

type Policies struct {
    MaxCostMonthly  *float64 `json:"max_cost_monthly,omitempty" validate:"omitempty,gt=0"`
    AllowedRegions  []string `json:"allowed_regions,omitempty"`
}

// internal/provider: cloud-agnostic abstraction.
type CloudProvider interface {
    Name() string
    Validate(ctx context.Context, r Resource) error
    Plan(ctx context.Context, r Resource) (Diff, error)
    Apply(ctx context.Context, r Resource) (Result, error)
    Destroy(ctx context.Context, r Resource) error
}

// internal/engine: orchestrator that uses CloudProvider without knowing its internals.
type Engine interface {
    Validate(ctx context.Context, spec Specification) error
    Plan(ctx context.Context, spec Specification) ([]Diff, error)
    Apply(ctx context.Context, spec Specification) ([]Result, error)
}
```

Key design points:

- `Resource.Properties` is intentionally `map[string]any` at the raw
  Specification level, but **each provider** will define a typed and
  validated struct for its own `ResourceType` (e.g.
  `RelationalDatabaseProps`), so as to block Mass Assignment before it
  reaches business logic. The generic map exists only as a JSON transport
  container, never as a type operated on directly.
- `provider: "agnostic"` delegates the choice of concrete provider to the
  engine, based on `Policies.AllowedRegions` and cost/availability
  criteria (resolution logic, detailed in a future RFC).
- Every operation is `context.Context`-aware to support timeouts,
  cancellation, and tenant/identity propagation (required for BOLA).

### 2.4 Universal JSON Schema (v1.0)

The following schema is the draft JSON Schema (draft 2020-12) that will
validate every incoming Specification, in addition to struct-level
validation via `go-playground/validator`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cloudsdd.dev/schema/v1.0/specification.json",
  "title": "CloudSDD Specification",
  "type": "object",
  "additionalProperties": false,
  "required": ["sdd_version", "intent", "resources"],
  "properties": {
    "sdd_version": { "const": "1.0" },
    "intent": { "type": "string", "enum": ["deploy", "update", "destroy", "plan"] },
    "resources": {
      "type": "array",
      "minItems": 1,
      "items": { "$ref": "#/$defs/resource" }
    },
    "policies": { "$ref": "#/$defs/policies" }
  },
  "$defs": {
    "resource": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "type", "provider", "properties"],
      "properties": {
        "id": { "type": "string", "pattern": "^[a-zA-Z0-9_-]{1,63}$" },
        "type": { "type": "string", "enum": ["relational_database", "object_storage", "compute_instance", "container_service"] },
        "provider": { "type": "string", "enum": ["agnostic", "aws", "gcp", "azure"] },
        "properties": { "type": "object" }
      }
    },
    "policies": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "max_cost_monthly": { "type": "number", "exclusiveMinimum": 0 },
        "allowed_regions": { "type": "array", "items": { "type": "string" } }
      }
    }
  }
}
```

Note: the `resources[].type` and `provider` enum lists in this draft are
intentionally minimal (only the example from CLAUDE.md); they will be
extended via dedicated RFCs as providers and concrete resource types are
added.

## 3. Security Considerations

- **Two-tier validation**: JSON Schema (structure/types/enum) +
  `go-playground/validator` (domain rules) before any field reaches
  business logic. `additionalProperties: false` at every level blocks
  Mass Assignment right from parsing.
- **BOLA**: every `Resource.ID` will be namespaced per tenant at the
  storage/state level (implementation detail in a future RFC on the state
  module); the `Engine` will never accept a tenant ID coming from the
  request body, only from the authentication context (`context.Context`).
- **Rate limiting**: planned in `internal/security` using
  `golang.org/x/time/rate`, applied at the HTTP middleware level before a
  request reaches the `Engine`.
- **Resource consumption**: limits on incoming JSON payload size and on
  the maximum number of `resources` per Specification (value to be
  defined in the implementation RFC for the API layer).
- **Fuzzing**: the Specification parser (`internal/spec`) will be the
  first target of `go test -fuzz`, given its direct exposure to hostile
  input.

## 4. Out of Scope (for future RFCs)

- Natural language → JSON Specification translation.
- Concrete implementation of providers (AWS/GCP/Azure) via the Pulumi Go
  SDK.
- Diffing/planning algorithm and state file management.
- Authentication/authorization (tenant scheme, JWT/OIDC).
- HTTP API layer (routing, middleware, concrete rate limiting).

## 5. Open Questions

1. Will the state (current state of applied resources) be persisted
   locally, on a remote backend (e.g. S3-compatible), or both,
   configurable?
2. `provider: "agnostic"` requires a resolution policy: explicit user
   preference, minimum cost, or first available region in
   `allowed_regions`?
3. Schema versioning: `sdd_version` as a hard constraint (`const: "1.0"`)
   or multi-version support with automatic migration from the start?
