# RFC 011: Provider Hardening, Policy Enforcement, and Test Coverage

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 002](002-aws-provider.md), [RFC 005](005-account-environment-region-scoping.md),
  [RFC 006](006-cli-entrypoint.md), [RFC 007](007-aws-relational-database.md),
  [RFC 008](008-gcp-azure-providers.md), [RFC 009](009-global-state-tracking.md),
  [RFC 010](010-configurable-ai-providers.md)

## 1. Problem

RFCs 006–010 delivered the CLI, the AWS relational database, the GCP and Azure
providers, the global state ledger, and configurable AI providers. The
architecture is sound and matches what those RFCs described. The
*implementations*, however, diverged from three binding project invariants:

1. **"The underlying provider implementations MUST automatically enforce the
   best state-of-the-art configurations ... without the user needing to
   explicitly ask for them."** (CLAUDE.md, CRITICAL REQUIREMENT)
2. **"Mass Assignment prevention: Incoming Go structs must actively block
   injection of unexpected fields."** (CLAUDE.md, security)
3. **"No error is ever ignored."** (CLAUDE.md, Go standards)

The result is a set of defects where **the infrastructure a user receives is not
the infrastructure they described**, which is the one failure mode an
intent-driven system cannot tolerate. This RFC proposes the consolidation and
hardening needed to close them, plus the test coverage that would have caught
them.

This is a remediation RFC. It introduces no new resource types and no new
user-facing schema beyond §2.5 and §2.7.

### 1.1 Defects motivating this RFC

Grouped by the invariant they violate. File references are to the current
working tree.

#### A. Wrong infrastructure is provisioned (silent)

| # | Location | Defect |
|---|---|---|
| A1 | `internal/provider/gcp/storage.go:31` | Bucket `Location` is hardcoded to `"EU"`, while `Validate` *requires* `r.Scope.Region`. A user asking for `us-east1` receives an EU bucket. RFC 005's region scoping is silently discarded, with data-residency consequences. |
| A2 | `internal/provider/gcp/database.go:46` | `DatabaseVersion` is built as `POSTGRES_%s` regardless of `p.Engine`. A request for MySQL silently provisions PostgreSQL. |
| A3 | `internal/provider/azure/database.go:48` | Always calls `postgresql.NewFlexibleServer` regardless of `p.Engine`; `p.HighAvailability` is decoded and then never read. Requesting an HA MySQL instance yields a single-node Postgres one. |
| A4 | `internal/provider/azure/storage.go:38` | The storage account name is derived from `id`; the decoded, required `p.BucketName` is never used. `strings.ReplaceAll(id, "-", "")` also neither lowercases nor strips underscores — which `spec` permits in IDs — so `My_Bucket` yields a name Azure rejects, and truncation to 24 chars can silently collide two distinct resources onto one account. |

#### B. Crash paths

| # | Location | Defect |
|---|---|---|
| B1 | `internal/provider/azure/provider.go:97,100` | `resourceProgram` discards the decode error (`s3p, _ :=`) and dereferences `*s3p` inside the Pulumi closure. `Plan`/`Apply` happen to call `Validate` first; `Destroy` (line 153) does not — so `cloudsdd destroy` **panics** on any Azure resource with invalid properties. |
| B2 | `internal/nlp/openai.go:92` | `resp.Choices[0]` with no length check panics on an empty completion. |
| B3 | `internal/config/config.go:41,61`, `internal/nlp/anthropic.go:34` | The default model is `claude-3-5-sonnet-20241022`, **retired 2025-10-28**. A fresh install writes it to `~/.cloudsdd/config.yaml` and every `deploy`/`destroy` fails with a 404. The tool does not work out of the box. |

#### C. Mass-assignment defense not applied

`internal/provider/aws/decode.go:76` (`decodeProperties`) implements exactly the
pattern RFC 002 §2.6 specified: re-marshal → `DisallowUnknownFields` →
`go-playground/validator` → `rejectCredentialLikeKeys`. It is used by
`objectstorage.go` and `crossaccountrole.go`.

Four newer decoders bypass it entirely and hand-roll `json.Unmarshal` with no
strict mode, no validator run, and no credential rejection:
`aws/database.go:21`, `gcp/storage.go:18`, `gcp/database.go:21`,
`azure/storage.go:20`, `azure/database.go:22`. Their `validate:"required"`
struct tags are decorative — nothing ever invokes the validator. Unknown
properties are silently dropped rather than rejected, and a property named
`password` or `secret_key` passes straight through on those paths.

#### D. Declared policies that do nothing

| # | Location | Defect |
|---|---|---|
| D1 | `spec/spec.go:130` | `Policies.AllowedRegions` is enforced only by AWS (`aws/provider.go:102,117`). GCP and Azure never call any equivalent. A spec pinning `allowed_regions: ["eu-central-1"]` deploys anywhere on those two providers. |
| D2 | `spec/spec.go:129` | `Policies.MaxCostMonthly` is declared, validated (`gt=0`), documented in the CLAUDE.md schema example — and referenced in **zero** non-test files. It is dead weight that reads as a guarantee. |
| D3 | `spec/spec.go:46` | `Intent` is parsed and validated against `oneof=deploy update destroy plan`, then never consulted by the engine. `Apply` will happily apply a `destroy`-intent spec. `cmd/cloudsdd/destroy.go:55` papers over one direction by force-overwriting `Intent` after translation; `deploy.go` has no equivalent guard. |

#### E. Secure-by-default escape hatches

Three settings ship insecure defaults with a comment acknowledging it:

| Location | Setting | Comment in source |
|---|---|---|
| `gcp/storage.go:34` | `ForceDestroy: true` | "For simplified teardown" |
| `gcp/database.go:61` | `DeletionProtection: false` | "For simplified teardown" |
| `aws/database.go:65` | `SkipFinalSnapshot: true` | "For simplified teardown during dev" |

Each converts an accidental `destroy` into unrecoverable data loss. Separately,
`azure/database.go:56` carries the comment "Flexible server is private by
default without firewall rules" — but Azure Flexible Server defaults to
**public network access enabled**; absent firewall rules block connections
without removing the public endpoint. The comment asserts a posture the code
does not establish.

#### F. Unbounded and unchecked I/O

| # | Location | Defect |
|---|---|---|
| F1 | `internal/nlp/ollama.go:92` | Uses `http.DefaultClient` (no timeout) with the CLI's `context.Background()` (no deadline). A hung Ollama blocks the CLI indefinitely. |
| F2 | `internal/nlp/ollama.go:99,104` | `io.ReadAll` on the error path and the JSON decode are both unbounded, against "treat every input as hostile". |
| F3 | Multiple | Ignored errors: `config.go:44,45` (`yaml.Marshal`, `os.WriteFile` — a config write can fail silently), `azure/provider.go:44` (`os.MkdirAll`, which the GCP twin *does* check), `deploy.go:47,48,55` / `destroy.go:47,48,57` (`ReadLedger`, `json.Marshal`, `Scanln`), `ollama.go:85`, and every hand-rolled decoder in §1.1C. |

#### G. Ledger correctness

| # | Location | Defect |
|---|---|---|
| G1 | `state/ledger.go:87,114` | Both `RecordDeployment` and `RecordDestruction` match purely on `er.ID == nr.ID`, ignoring account, environment, and region. Deploying `app-db` to dev and to prod collapses to a single entry; destroying it in dev deletes the prod entry too. This contradicts RFC 005's scoping model and corrupts the very context RFC 009 exists to provide. |
| G2 | `deploy.go:102-107` | `eng.Apply` returns `(results, err)`; on partial failure the CLI returns early and records **nothing**, so resources that were actually created go untracked. `destroy.go` has the same shape. |
| G3 | `state/ledger.go:21` | RFC 009 §2.3 specified "file locking to prevent corruption". The implementation uses a package-level `sync.Mutex`, which is process-local. Two concurrent `cloudsdd` invocations can still interleave read-modify-write and lose entries. |
| G4 | `state/ledger.go:110` | `RecordDestruction` builds `updated` via `var updated []spec.Resource`; when everything is destroyed it stays `nil` and marshals as `"resources": null` rather than `[]`. |

#### H. Duplication with functional divergence

| # | Location | Defect |
|---|---|---|
| H1 | `nlp/{anthropic,openai,ollama}.go` | The system prompt is triplicated. The Anthropic copy documents the valid resource types, provider values, scope shape, and per-type properties. The OpenAI and Ollama copies have `"properties": {}` and **no type list at all** — so those providers emit specs that fail `ParseAndValidate` far more often. This is a capability gap, not cosmetic duplication. |
| H2 | `gcp/provider.go:136`, `azure/provider.go:124` | The change-summary→action mapping is copy-pasted, with a comment conceding why: "Convert changes manually for brevity since summarizeChangeSummary is private in aws pkg." Both copies also never emit `delete`. |
| H3 | `cmd/cloudsdd/{deploy,destroy}.go` | ~90% identical (122 / 119 lines): prompt joining, config load, translator construction, ledger read, three provider constructions, engine wiring, and the confirmation gate are all duplicated. |

#### I. Test coverage

CLAUDE.md sets a >90% target. Current state:

| Package | Coverage |
|---|---|
| `internal/spec` | 89.7% |
| `internal/engine` | 80.4% |
| `internal/provider/aws` | 55.6% |
| `internal/nlp` | 3.6% |
| `cmd/cloudsdd` | 0.0% |
| `internal/config` | 0.0% |
| `internal/state` | 0.0% |
| `internal/provider/gcp` | 0.0% |
| `internal/provider/azure` | 0.0% |

Every defect in §1.1A–B is the kind a single table-driven test would have
caught. There is also no CI: no `.github/`, and `gosec`/`govulncheck` are run
only ad hoc.

## 2. Proposed Architecture

### 2.1 Shared strict property decoding (`internal/provider/decode`)

Promote `decodeProperties` out of `internal/provider/aws` into a new
cloud-agnostic package `internal/provider/decode`, so the mass-assignment
defense is a property of *the provider contract*, not of one provider.

```go
// Package decode implements the strict, two-tier property decoding every
// CloudProvider must apply before acting on Resource.Properties (RFC 002
// §2.6, RFC 011 §2.1).
package decode

// Properties re-marshals props and re-decodes it in strict mode into out,
// then validates out with go-playground/validator.
func Properties(props map[string]any, out any) error
```

Behavior is the union of what `aws/decode.go` does today:

1. `rejectCredentialLikeKeys` — reject any key containing `access_key`,
   `secret_key`, `secret`, `password`, `token`, `session_token`, `private_key`.
2. Re-marshal → `json.Decoder` with `DisallowUnknownFields` → decode.
3. `validator.Struct(out)`.

Provider-specific validators (`awsregion`, `s3bucketname`) stay registrable via
an exported hook so AWS keeps its tags, and GCP/Azure can register their own
(`gcpregion`, `azurelocation`, `azureaccountname`).

`internal/provider/aws/decode.go` becomes a thin wrapper preserving the `aws:`
error prefixes so existing AWS tests and error strings are unchanged. All five
hand-rolled decoders in §1.1C are replaced with calls to it.

**Consequence to accept:** properties that are currently silently dropped will
start being rejected. That is the intended behavior per CLAUDE.md, and it is
better surfaced at `Validate` time than as missing infrastructure.

### 2.2 Shared plan-summary mapping (`internal/provider/pulumiutil`)

Move `summarizeChangeSummary` from `aws/stack.go` to a shared
`internal/provider/pulumiutil` package, used by all three providers. The shared
version fixes the two bugs in the copies: it emits `provider.ActionDestroy` when
the summary contains deletes, and it returns the typed `provider.Action`
constants rather than stringly-typed values.

Providers also set `Diff.Region` and `Result.Region` themselves rather than
relying on the engine's backfill, so the `CloudProvider` contract is honored
uniformly. The engine's backfill in `default.go` stays as a safety net.

### 2.3 Cross-provider policy enforcement

`validateRegionAllowed` moves from `internal/provider/aws/region.go` to a shared
location and is invoked by GCP's and Azure's `Validate` on every resource type,
closing D1. The AWS-specific *format* check (`validateAWSRegionFormat`) stays in
the AWS package; GCP and Azure get their own format validators.

To guarantee this cannot regress, the enforcement point moves up: the engine
performs the `AllowedRegions` check itself in `DefaultEngine.Validate`, before
delegating to the provider. Providers keep their own check as defense in depth
(a provider used directly, outside the engine, must still be safe), but a new
provider can no longer forget it and silently bypass the policy.

### 2.4 `Intent` enforcement (D3)

`DefaultEngine` gains an intent gate:

| Method | Accepts `Intent` | Rejects with |
|---|---|---|
| `Plan` | any | — (planning is side-effect free) |
| `Apply` | `deploy`, `update` | `ErrIntentMismatch` |
| `Destroy` | `destroy` | `ErrIntentMismatch` |

This makes the declared intent load-bearing and removes the need for
`destroy.go`'s post-translation `sddSpec.Intent = spec.IntentDestroy`
overwrite — which today masks an LLM that misread the user's request. After
this change, a `destroy` prompt that the translator renders as `intent: deploy`
is a hard error the user sees, rather than a silent coercion.

`IntentPlan` is accepted by `Plan` only; `Apply` on a `plan`-intent spec is an
error.

### 2.5 Secure-by-default reversal (E)

The three escape hatches become **spec-driven properties with secure
defaults**, rather than hardcoded convenience:

| Property | Type | Default | Applies to |
|---|---|---|---|
| `force_destroy` | `*bool` | `false` | `object_storage` (GCP; AWS S3 equivalent) |
| `deletion_protection` | `*bool` | `true` | `relational_database` (all providers) |
| `skip_final_snapshot` | `*bool` | `false` | `relational_database` (AWS) |

Pointer types plus the existing `boolOrDefault` helper (`aws/decode.go:110`)
give the three-state semantics the codebase already uses: unset → secure
default, explicitly set → user's choice. A user who genuinely wants disposable
dev infrastructure must now say so, which is exactly the CLAUDE.md requirement
read in the right direction: the *secure* configuration is what you get without
asking.

Additionally, Azure PostgreSQL Flexible Server gets
`PublicNetworkAccessEnabled: false` so the code establishes the posture its
comment claims, and Azure Storage gets a default-deny `NetworkRules` block.

These properties are added to the NLP system prompt (§2.6) so the translator can
set them when a user explicitly asks for disposable infrastructure.

### 2.6 Single shared NLP system prompt (H1)

Extract one `systemPrompt` constant into `internal/nlp`, built from the
Anthropic version (the only complete one), extended with the properties from
§2.5. All three translators use it. The ledger-context suffix
(`anthropic.go:80` and its two copies) is extracted alongside it as a single
`withLedgerContext(prompt, contextJSON string) string`.

This closes a real capability gap: OpenAI and Ollama currently receive no list
of valid resource types or providers.

### 2.7 Scope-aware ledger (G)

The ledger's identity key becomes the full RFC 005 scope tuple rather than the
bare ID:

```go
// ledgerKey identifies a deployed resource uniquely across the scoping
// dimensions RFC 005 established. Matching on ID alone (RFC 009's original
// implementation) collapses the same logical resource deployed to different
// environments or regions into one entry.
type ledgerKey struct {
    Account     string
    Environment string
    Region      string
    ID          string
}
```

Four further changes:

- **Record from results, not from the spec** (G2). `RecordDeployment` takes
  `[]provider.Result` so only resources that actually reached
  `StatusApplied` are recorded, and a partial failure still records what
  succeeded. The CLI records **before** returning the apply error.
- **Cross-process file locking** (G3), delivering what RFC 009 §2.3 promised.
  An `O_CREATE|O_EXCL` lockfile at `~/.cloudsdd/ledger.lock` with a bounded
  retry, plus a stale-lock timeout, keeps the dependency footprint at zero
  (consistent with the project's AGPLv3 dependency audit) while making
  concurrent invocations safe.
- **Atomic writes.** Write to a temp file in the same directory and
  `os.Rename` over the target, so an interrupted write cannot truncate the
  ledger.
- **`Resources` always non-nil** (G4), so the JSON is always `[]`, never
  `null`.

Ledger schema version is bumped and a migration path is included: a ledger
without the scope fields is read as `{Account: "", Environment: "", Region: "",
ID: id}`, which is exactly its current meaning, so existing files keep working.

### 2.8 CLI consolidation (H3)

Extract the shared body of `deploy.go` and `destroy.go` into a single
`runIntent(cmd, args, intent)` helper covering prompt joining, config load,
translator construction, ledger read, provider construction, engine wiring,
plan display, confirmation, execution, and ledger update.

Two behavioral additions:

- **`--yes` / `-y`** to skip the interactive confirmation, so the CLI is usable
  from CI. Today `fmt.Scanln` on a non-TTY returns an error and the run is
  cancelled, with no way to proceed.
- **Confirmation reads the whole line** rather than one whitespace-separated
  token, and accepts `y`/`yes` case-insensitively. The default stays "no" on
  any other input, including EOF.

Provider construction becomes lazy: only providers actually referenced by the
spec are constructed. Today `deploy.go:58-71` unconditionally builds AWS, GCP,
and Azure providers, so a user deploying only to AWS must still set
`CLOUDSDD_PULUMI_PASSPHRASE` for GCP and Azure — both `NewProvider`s hard-fail
without it. This is a usability bug the consolidation resolves for free.

### 2.9 `MaxCostMonthly` (D2)

Two options; this RFC proposes **(a)**.

**(a) Remove it from the schema.** Cost estimation requires per-provider pricing
data, per-SKU rate cards, and a currency/period model — a substantial feature
deserving its own RFC. Leaving a validated-but-unenforced `max_cost_monthly` in
the public schema is worse than not having it: it reads as a guarantee and
provides none. Remove the field, and note in `docs/architecture.md` that cost
policy is deferred to a future RFC.

**(b) Keep it and enforce it** — requires the pricing model above, and is out of
scope for a remediation RFC.

If the user prefers (b), the field stays as-is and a follow-up RFC 012 is
tracked. This RFC's other work is independent either way.

## 3. Impacted JSON Schema

| Change | Field | Compatibility |
|---|---|---|
| Added | `properties.force_destroy` (`object_storage`) | Additive; default `false` |
| Added | `properties.deletion_protection` (`relational_database`) | Additive; default `true` |
| Added | `properties.skip_final_snapshot` (`relational_database`, AWS) | Additive; default `false` |
| Removed | `policies.max_cost_monthly` | **Breaking** — a spec setting it will now be rejected by `DisallowUnknownFields`. Only under option (a) in §2.9. |
| Behavior | `intent` | Now enforced (§2.4). A spec whose `intent` contradicts the operation is rejected. |
| Behavior | Unknown properties on RDS / GCP / Azure resources | Now rejected rather than silently dropped (§2.1). |

The ledger file schema changes additively (§2.7); existing files are migrated on
read.

## 4. Security Considerations

- **Mass assignment (§2.1).** Closing §1.1C is the primary security outcome.
  After this RFC, every resource type on every provider passes through
  `DisallowUnknownFields` + validator + credential-key rejection. The check
  becomes structurally hard to skip because there is exactly one decode path.

- **Policy bypass (§2.3).** `AllowedRegions` is a stated security and cost
  control (RFC 001 §3). Enforcing it in the engine rather than per-provider
  makes bypass a design error rather than an omission.

- **Prompt injection via the ledger.** `deploy.go:49` injects the ledger JSON
  into the LLM **system** prompt, and the ledger is populated from prior LLM
  output — a self-referential channel where attacker-influenced resource IDs or
  properties become system-prompt content. Mitigations adopted here: the ledger
  is delimited and explicitly labelled as untrusted data in the prompt; a size
  cap is applied before injection; and the translator's output is still gated by
  `spec.ParseAndValidate`, so injected content cannot produce a spec the schema
  rejects. It **can** still influence which valid resources are proposed, which
  is why the `Plan` → confirmation gate (§2.8) remains mandatory and is never
  skipped by default.

- **Unbounded input (§1.1F).** Ollama responses get an `io.LimitReader` cap and
  the client a timeout. Anthropic and OpenAI SDK clients get explicit request
  timeouts. This closes the CLI's only indefinite-hang path.

- **Credential exposure.** Error strings in the translators
  (`anthropic.go:132`, `openai.go:95`, `ollama.go:111`) embed the raw model
  output on a parse failure. Since the model output is derived from a prompt
  containing the ledger, this can echo infrastructure detail into logs. The raw
  output is truncated and the full text moved behind a `--verbose` flag.

- **Destructive-default reversal (§2.5).** `SkipFinalSnapshot: true` and
  `DeletionProtection: false` mean a `destroy` — including one produced by a
  mistranslated prompt — is unrecoverable. Reversing these defaults is the
  highest-value change in this RFC measured by worst-case blast radius.

- **Ledger locking (§2.7).** Concurrent invocations currently race on
  read-modify-write of `ledger.json`. A lost write desynchronizes the AI's view
  of deployed infrastructure from reality, which can lead to a subsequent
  `destroy` targeting the wrong resource set.

## 5. Testing Plan

Per CLAUDE.md: table-driven, native Go, >90% coverage.

| Package | Focus |
|---|---|
| `internal/provider/decode` | Unknown-field rejection, credential-key rejection, validator invocation, per-provider tag registration. Native fuzz target over arbitrary `map[string]any`. |
| `internal/config` | Default creation, malformed YAML, unwritable dir, model default, error propagation on marshal/write. |
| `internal/state` | Scope-keyed upsert and delete; two environments sharing an ID stay distinct; partial-failure recording; concurrent access via the lockfile; `null` vs `[]`; legacy-file migration. |
| `internal/provider/gcp` | Region honored (A1); engine honored (A2); policy enforcement (D1); secure defaults (E). |
| `internal/provider/azure` | `Destroy` with invalid properties errors rather than panics (B1); engine and HA honored (A3); bucket name used and normalized (A4); policy enforcement (D1). |
| `internal/nlp` | `httptest`-backed fakes for all three translators: markdown-fence stripping, empty-choices guard (B2), oversized response, timeout, prompt parity across the three. |
| `internal/engine` | Intent gate (§2.4); engine-level `AllowedRegions` enforcement (§2.3). |
| `cmd/cloudsdd` | `runIntent` with fake providers and a fake translator; `--yes`; confirmation parsing; lazy provider construction; ledger written on partial failure. |

Integration tests via `testcontainers-go` are **not** proposed here: the
providers' external dependency is the cloud control plane, not a containerizable
service. The Pulumi Automation API boundary is exercised via the existing
`internal/provider/aws/integration_test.go` build-tag pattern, extended to GCP
and Azure.

### 5.1 CI

Add `.github/workflows/ci.yml` running, on push and PR:

```
go build ./...
go vet ./...
go test -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out          # enforced floor
gosec ./...
govulncheck ./...
```

The coverage floor starts at the level reached by §5 and ratchets upward only.
This is the missing feedback loop that let §1.1's defects land.

## 6. Rollout

Ordered so each step is independently verifiable and the highest-risk defects
land first.

| Step | Content | Gate |
|---|---|---|
| 1 | §1.1A + §1.1B (wrong infrastructure, crashes) | `go build`, targeted regression tests |
| 2 | §2.1 shared decode; §2.3 policy enforcement; §2.4 intent; §2.5 secure defaults; §1.1F I/O bounding and ignored errors | Full suite + `gosec`, `govulncheck` |
| 3 | §2.2, §2.6, §2.7, §2.8 (consolidation, ledger, CLI) | Full suite |
| 4 | §5 test coverage to >90% | Coverage floor |
| 5 | §5.1 CI, `docs/cli.md`, `docs/architecture.md` update | CI green |

`docs/cli.md` does not currently exist despite being required by CLAUDE.md
workflow rule 4; it is written in step 5. `docs/api.md` and `docs/openapi.yaml`
describe an HTTP layer that the amended CLAUDE.md no longer mentions (they
reference Chi, BOLA, and rate limiting, all removed from the project mandate);
step 5 marks them explicitly deferred rather than leaving them as apparent
current-state documentation.

## 7. Open Questions — resolved at approval

1. **§2.9 — `max_cost_monthly`:** **removed** (option (a)). Cost enforcement
   needs per-provider pricing data and is deferred to its own RFC.
2. **§2.5 — default strictness:** **uniform secure defaults**, with no implicit
   per-environment relaxation. An implicit rule keyed on a free-form
   `scope.environment` string would itself be a footgun.
3. **§2.4 — intent coercion:** **confirmed.** `destroy.go`'s silent `Intent`
   overwrite is now a hard error, surfacing translator misreads the user
   previously never saw.

## 8. Implementation notes

Deviations and findings worth recording against the plan above.

- **§2.2 was narrower than described.** `summarizeChangeSummary` in
  `internal/provider/aws/stack.go` was already correct — it handled the delete
  case and returned typed `provider.Action` values. Only the GCP and Azure
  hand-copies were degraded. The shared `pulumiutil` package therefore lifts
  the AWS implementation unchanged rather than fixing it.

- **Coverage: >90% was met everywhere it is reachable.** `internal/provider/{aws,gcp,azure}`
  cap at 57–59%. Their remaining uncovered statements are exactly `upsertStack`,
  `Plan`, `Apply`, and `Destroy` — the Pulumi Automation API surface, which
  shells out to the `pulumi` binary and requires a live cloud control plane.
  Everything below that boundary *is* covered, including the `declare*`
  functions, via Pulumi's `pulumi.WithMocks` monitor: the tests assert the
  actual resource inputs (bucket location, database version, deletion
  protection), which verifies the §1.1A fixes far more directly than
  asserting on intermediate return values. `scripts/coverage-gate.sh` records
  a per-package floor and the reason for the lower three.

- **`cmd/cloudsdd` needed a seam.** `runIntent` reaches the translator and the
  cloud providers through the package-level `newTranslator` and
  `providerFactories` indirections so the flow can be driven with fakes.

- **`CLOUDSDD_HOME` was added.** `internal/config` and `internal/state` both
  resolved paths through `os.UserHomeDir()`, which made them untestable without
  touching the invoking user's real home. `config.Dir()` now honours
  `CLOUDSDD_HOME` and `internal/state` routes through it, giving one definition
  of where CloudSDD keeps local files.

- **A race was found in the test mocks, not the product.** Pulumi registers
  resources concurrently; the first version of the mock monitor appended to a
  shared slice unguarded. Caught by `go test -race`, which the new CI runs.

- **`go mod tidy` promoted two dependencies.** `sashabaranov/go-openai` and
  `gopkg.in/yaml.v3` were marked indirect but are directly imported by
  `internal/nlp` and `internal/config`. CI now fails on an untidy `go.mod`.

- **gosec findings.** Eleven at the start, zero at the end: two G101
  false positives on the `CLOUDSDD_PULUMI_PASSPHRASE` env-var *name*
  (annotated to match the existing AWS convention), three G304 on paths
  derived from the operator-controlled home directory (annotated), and six
  genuine G104 unhandled-`Close()` errors in the new ledger code, which were
  fixed by restructuring the atomic write into `writeTempAndRename` with a
  joined close error.
