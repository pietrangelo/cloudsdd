# RFC 019: Rosette Consolidation of the Provider Layer

- **Status:** Proposed
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-09
- **Depends on:** [RFC 011](011-provider-hardening-and-test-coverage.md) §1.1H,
  [RFC 018](018-build-pipeline.md) — in flight, Azure section incomplete

## 1. Problem

RFC 011 §1.1H stated the rule this codebase has followed since: **a check
that lives in one provider is a check the next provider forgets.** The
answer it established was a set of leaf packages —
`internal/provider/{compute,container,decode,network,pipeline}` — holding
the shape and the rules, with each provider decoding through its own
`decode.Decoder` so only the error prefix stays local.

The rule holds. The practice has drifted, and it drifts in a predictable
place: **whenever a feature ships cloud by cloud.** RFC 018 built
`build_pipeline` on AWS, then GCP, then Azure. That is the right delivery
order and it produced three copies of the same seam, because the second
provider is written by reading the first and the third by reading the
second.

This is not a hypothetical cost. Three concrete symptoms exist today:

- `buildRevision` is **byte-identical** in all three providers — body *and*
  its six-line doc comment. Three places must be edited to change one
  decision.
- There are **three exported `ErrPipelineNotResolved` sentinels** for one
  condition. A caller asking "did the pipeline hand-off fail?" across
  clouds must know all three, which is exactly the coupling
  `errors.Is` exists to remove — and it contradicts this codebase's own
  pattern, where `pipeline.ErrImageNameMalformed` and its neighbours are
  single shared sentinels.
- `AWSProvider.Validate` repeats the same four-line region check **five
  times**, and `GCPProvider.Validate` already demonstrates the fix.

Left alone, the next two `todo.md` items add a sixth copy of the region
block and a fourth copy of the pipeline seam. The cost is not the
duplicated lines; it is that a security rule tightened in one copy is a
security rule silently weaker in the other two.

Separately, one **layering inversion** has accumulated:
`internal/provider/aws` imports `internal/engine`. A provider depending on
the engine that drives it inverts the dependency the architecture is built
on, and it is AWS-only — so it also reads as an accident rather than a
decision.

### 1.1 What this RFC is not

It is worth being precise, because "refactor for beauty" is how a working
system gets churned.

**No behaviour changes.** Not one. Every existing provider test must pass
with no assertion edited — only a sentinel rename. A test whose
expectations need changing is proof this RFC overstepped.

**No new abstractions where the duplication is only apparent.** The
strongest finding of the review was a negative one, recorded in §2.5.

## 2. Proposed Architecture

Four phases. Each is independently landable and independently revertible.

### 2.1 The invariant this refactor must not break

`internal/provider/{compute,container,decode,network,pipeline}` have **zero
internal imports**. They are pure domain packages: no Pulumi, no cloud SDK,
no engine. That is why they carry coverage floors of 95–100% while the
three Pulumi-bound provider packages sit at 65–73%, and it is the single
structural reason this codebase's business logic is testable at all.

Every consolidation below is constrained by it. Concretely, a shared helper
in one of those packages **takes a plain function and a string, never a
`*decode.Decoder` or a `provider.NetworkScope`**. Where a helper genuinely
needs those types, it goes somewhere else (§2.4) rather than costing a leaf
package its leafness.

`scripts/coverage-gate.sh` gains a check for this, so the invariant is
enforced rather than remembered.

### 2.2 Phase 1 — the `build_pipeline` seam

Five duplications collapse.

**Two methods on `spec.Resolved`** (`internal/spec/spec.go`, beside the
struct). Both nil-receiver-safe, both adding **zero imports** — `spec`
continues to import only `internal/schedule`:

```go
// CommitOr returns the commit the Engine resolved, or fallback when it
// supplied none.
func (r *Resolved) CommitOr(fallback string) string

// Complete reports whether r carries both halves of the §2.4.1 hand-off.
func (r *Resolved) Complete() bool
```

`CommitOr` absorbs the three `buildRevision` copies. `Complete` absorbs the
three-clause guard that opens each `pipelineImage`. Putting them on
`Resolved` rather than in `pipeline` is deliberate: the question "what did
the Engine resolve?" is a question about `Resolved`, and asking it there
introduces no dependency in either direction.

**Three additions to `internal/provider/pipeline/types.go`:**

```go
// One sentinel. Providers wrap it, so the prefix stays local and
// errors.Is works across clouds.
var ErrNotResolved = errors.New("build_pipeline: `pipeline` reached the provider unresolved")

// One name, agreed by three clouds and currently a bare literal in AWS.
const GeneratedDockerfileName = "Dockerfile.cloudsdd"

// The shared decode skeleton.
func DecodeAndValidate(
    props  map[string]any,
    decode func(map[string]any, any) error,
    prefix string,
) (*BuildPipelineProperties, error)
```

`DecodeAndValidate` performs the three steps all three providers perform
identically: decode, `Validate()`, and the early Dockerfile generation that
refuses an unsupported runtime version at plan time rather than after a
build has been provisioned and started. The `decode` parameter is a plain
func — `dec.Properties` satisfies it — which is what keeps `pipeline` a
leaf package.

AWS's `codeBuildSourceType` check **stays in `aws/pipeline.go`**, called
after the shared helper. It is a real CodeBuild constraint, not a shared
rule, and GCP and Azure are right not to have it.

### 2.3 Phase 2 — `AWSProvider.Validate` adopts the shape GCP already has

`aws/provider.go` repeats this five times, once per resource type:

```go
if r.Scope.Region == "" { return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired) }
if err := validateAWSRegionFormat(r.Scope.Region); err != nil { ... }
return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)
```

and the `len(r.Scope.Zones) > 0` refusal three times. The switch's plot —
*which resource type is this?* — is buried under placement boilerplate that
is the same in every branch.

`GCPProvider.Validate` hoists that block to **after** the switch, leaving
each case holding only what is type-specific. There is nothing to invent
here: AWS diverges from the codebase's own better pattern, and Phase 2 is
AWS adopting it.

One asymmetry has to be handled rather than hoisted over. AWS has a global
resource type, `cross_account_role`, which must *refuse* a region; GCP has
none. That case returns directly from inside the switch, which is also the
honest reading — a global resource is genuinely not on the regional path.

A `refuseZones(r)` helper absorbs the three-way repeat, keeping each case's
distinct *reason* in its own comment, since Fargate, CodeBuild and S3
refuse zones for three different reasons worth preserving.

### 2.4 Phase 3 — network helpers, and the layering inversion

**The helpers.** `networkScope` and `addressPolicy` are identical in all
three providers (Azure differs only by import alias). `shortHash` and
`scopeName` are identical in GCP and Azure. `resourceScope` is identical
modulo one provider constant.

`networkScope` and `addressPolicy` deserve a second look, because they are
not merely duplicated — they are pure conversions *into*
`internal/provider/network`'s own types. They are that package's
constructors, written outside it three times.

The obvious destination is therefore `internal/provider/network`, and it is
**wrong**: they take `provider.NetworkScope` and `spec.Policies`, so moving
them there would cost that package its leaf property (§2.1) to save nine
lines. They go to `internal/provider` instead — the root package, which
already defines `NetworkScope` and already imports `spec`. No cycle, no
invariant broken.

**The inversion.** `internal/provider/aws/target.go` imports
`internal/engine` for two types defined in `internal/engine/target.go`:
`DeploymentTarget` (with `AWSTargetConfig`) and `TargetProviderFactory`.

Read plainly, both are *provider-selection* concepts: a `DeploymentTarget`
names credentials a provider should assume, and a `TargetProviderFactory`
returns a `provider.CloudProvider`. Its signature already mentions
`provider` and not the engine. They move to `internal/provider`, which the
engine already imports, and `aws/target.go` drops the `engine` import.

This is the phase most likely to touch unrelated call sites, which is why
it is sequenced after the Azure work rather than before it.

### 2.5 What is deliberately not consolidated

The most useful result of the review was finding duplication that should
survive. Recorded here so a future pass does not "fix" it:

**The name sanitizers.** `gcp.bucketToken`, `azure.acrGroupToken`,
`azure.acrAlphanumericToken`, `gcp.artifactRepositoryID` and
`gcp.googleAccountID` look like five copies of one function. They are not.
ACR admits letters and digits and *not even hyphens*; Artifact Registry
admits hyphens and must start with a letter; Cloud Storage admits hyphens
in a globally-shared namespace. They differ in charset, in budget
semantics, in truncation and in fallback. A single helper parameterised on
a charset predicate would take five explicit, individually-commented rules
and hide them behind an argument — trading Clarity of Intent for a line
count. **Creativity is the dimension that says stop**: knowing where an
abstraction stops paying is part of the craft.

**The per-cloud `declare*` bodies.** They encode real platform differences
RFC 018 §2.8.1 already argues at length.

**`decodeContainerServiceProperties`.** Three copies share a four-step
skeleton with genuinely different per-cloud tails. It is the same shape as
§2.2 and a smaller win, so it is folded into Phase 4 rather than given its
own phase — and if Phase 4 runs long it is the item to drop.

**RFCs 001–010's varied structure.** RFCs 013 onward share a canonical
eight-section form; the early ones do not. Rewriting them would normalise
the record of how this project actually thought, which is worth more than
consistency.

### 2.6 Phase 4 — the documentation debt

RFC 018 owes three documentation items (`todo.md` 93–95) and one of its
own: it is the only RFC since 011 with **no `## 8. Implementation notes`**,
despite having accumulated two decisions that did not survive contact —
§2.4.1's commit-SHA hand-off replacing a digest, and §2.8.1's reversal of
the Premium SKU premise. Both are recorded inline today, where the next
reader will not look for them.

Phase 4 adds that section, adds a `README.md` with an RFC index (eighteen
RFCs with no map is its own Storytelling defect), and then discharges
items 93–95 — which by then describe RFC 018 and RFC 019's shapes together.

## 3. Impacted JSON Schema

**None.** No property is added, removed, renamed or revalidated. The
Specification a user writes today is byte-for-byte the Specification they
write after this RFC, and `docs/openapi.yaml` is unchanged.

This is the clearest statement of the RFC's scope: a change that touched
the schema would be a change that touched behaviour.

## 4. Security Considerations

A refactor is a security event, because it moves checks.

**Consolidation is a net gain, and that is the point.** After §2.2, one
function enforces `Source.Validate` — the HTTPS-scheme refusal, the
userinfo-credential refusal (RFC 018 §2.6), the revision grammar and the
`image_name` grammar. Today three functions do, and three functions can
diverge. The threat this closes is real and is the one RFC 011 §1.1H
named: a rule tightened in one provider staying loose in the others.

**No check may be dropped in the move.** The verification in §5 is
structured around this: each provider's existing tests must pass unchanged,
and every error sentinel keeps its identity through wrapping so
`errors.Is` continues to match where tests assert on it.

**The prefix must survive.** Error messages are how a user finds the
offending resource. `DecodeAndValidate` takes the prefix explicitly rather
than inferring it, so `aws:`, `gcp:` and `azure:` stay exactly where they
are today.

**One sentinel widens no authority.** Replacing three
`ErrPipelineNotResolved` values with one wrapped value changes what a
caller can *detect*, never what a provider will *do*. The refusal itself —
never guessing a registry, never falling back to `latest` (RFC 017 §2.4) —
is untouched.

**Phase 3 moves no credential logic.** `aws/target.go`'s STS assume-role
flow is unchanged; only the package the two types are declared in moves.
Session duration, external ID and the "never long-lived credentials"
guarantee of RFC 004 §2.1 are not in scope.

`gosec` and `govulncheck` run at the end of every phase, per
`<testing_and_security>`.

## 5. Testing Plan

The test suite is the safety net, so the plan is mostly *what must not
change*.

**The decisive check.** Every existing test in `internal/provider/aws`,
`internal/provider/gcp` and `internal/provider/azure` passes with **no
assertion modified** — the sole permitted edit is
`ErrPipelineNotResolved` → `pipeline.ErrNotResolved`. Any test needing a
changed expectation means behaviour moved and the change is rejected.

**New table-driven tests** (Go's native paradigm, per
`<testing_and_security>`):

- `internal/spec/spec_test.go`: `CommitOr` — resolved commit, empty commit,
  and the nil receiver; `Complete` — all three failure shapes.
- `internal/provider/pipeline/types_test.go`: `DecodeAndValidate` — prefix
  propagation, a decode failure, a `Validate` failure, and the early
  Dockerfile-generation refusal for an unsupported runtime version.
- `internal/provider/aws/provider_test.go`: the hoisted region path reached
  from every resource type, and `cross_account_role` still refusing a
  region.

**Structural tests**, because §2.1's invariant should be enforced rather
than remembered:

```bash
# The five leaf packages must have zero internal imports.
for d in compute container decode network pipeline; do
  go list -f '{{join .Imports "\n"}}' ./internal/provider/$d | grep -c '^cloudsdd/'
done   # every line must print 0

# The layering inversion must be gone (Phase 3).
go list -f '{{join .Imports "\n"}}' ./internal/provider/aws | grep cloudsdd/internal/engine
# must print nothing
```

**Coverage floors move up, never down.** Logic migrates *out* of the
Pulumi-bound packages (aws 65, azure 69, gcp 73) *into* fully unit-testable
ones (`spec` 90, `pipeline` 95). `scripts/coverage-gate.sh` ratchets
upward only, and this RFC should raise floors rather than invoke the
Pulumi-bound exception — if a floor needs *lowering*, something went wrong.

**Fuzzing** is unchanged and must stay green: `FuzzProperties`
(`internal/provider/decode`), `FuzzParseImage`
(`internal/provider/container`), `FuzzDerive` (`internal/provider/network`).

## 6. Rollout

Ordered so the in-flight RFC 018 work is written **once**, against the
final shape:

1. **Phase 1** — the `build_pipeline` seam (§2.2).
2. **Phase 2** — `AWSProvider.Validate` (§2.3).
3. **RFC 018 Azure items 79–81** — wiring `build_pipeline` into the Azure
   dispatch and `container.go`. These call exactly what Phases 1 and 2
   change, which is why they follow rather than precede.
4. **Phase 3** — network helpers and the layering inversion (§2.4).
5. **RFC 018 Volumes section** — unchanged, carried over as planned.
6. **Phase 4** — RFC 018 §8, `README.md`, `docs/architecture.md`,
   `docs/cli.md`, the NLP prompt, and `decodeContainerServiceProperties`
   (§2.6).

Phases 1 and 2 are the only ones that block RFC 018. Phases 3 and 4 could
slip without stalling the feature.

Per `<context_freshness_policy>`, each `todo.md` item ends with
`repomix-output.xml` regenerated and a handoff for `/clear`.

## 7. Open Questions

1. **Should `refuseZones` be shared across the three providers rather than
   AWS-local?** All three refuse zones for some types. Proposed answer:
   **no, not yet.** Each provider's refusal carries a different reason in
   its comment, and a shared helper would either lose those or take them as
   a parameter, which is a worse trade for one `if`. Revisit if a fourth
   provider appears.

2. **Should `internal/provider` (the root) hold the network helpers, or
   does that make it a grab-bag?** §2.4 argues for it on the grounds that
   it already owns `NetworkScope`. The risk is a package that accretes
   anything needing both `spec` and `provider` types. Proposed answer:
   accept it now, and split out `internal/provider/scope` if it exceeds
   roughly 150 lines.

3. **Does `DecodeAndValidate`'s plain-func parameter obscure the call
   site?** `pipeline.DecodeAndValidate(props, dec.Properties, "aws")` reads
   less directly than the method call it replaces. Proposed answer: accept
   it — the alternative costs `pipeline` its leaf property (§2.1), which is
   worth far more than one line of directness.

## 8. Implementation notes

*To be filled in as the phases land, per the convention RFC 011 established
and RFC 018 omitted (§2.6).*
