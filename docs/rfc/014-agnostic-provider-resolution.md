# RFC 014: Agnostic Provider Resolution

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 004](004-multi-account-deployer-access.md),
  [RFC 005](005-account-environment-region-scoping.md),
  [RFC 006](006-cli-entrypoint.md),
  [RFC 011](011-provider-hardening-and-test-coverage.md),
  [RFC 013](013-compute-instance.md)

## 1. Problem

CloudSDD's stated purpose, in the first line of `CLAUDE.md`, is to be a
"cloud-agnostic CLI tool and engine". The schema has offered
`provider: "agnostic"` since RFC 001 and the shared system prompt lists it as a
valid value. It has never worked:

```
Error: engine: agnostic provider resolution not implemented: resource "app-db"
```

RFC 001 §5 left the resolution policy as open question 2 — "explicit user
preference, minimum cost, or first available region in `allowed_regions`?" —
and it has stayed open through thirteen RFCs while every other part of the
system matured around it. This is the last of the three values the system
prompt advertises and nothing delivers; RFC 013 closed `compute_instance`,
and `container_service` is a resource type rather than a design decision.

The feature the project is named after is the one that returns an error.

## 2. Proposed Architecture

### 2.1 The principle

**"Agnostic" means resolved by a declared rule, never guessed.**

An engine that quietly picked a cloud would be the same defect RFC 011 spent
its length closing, at the largest possible scale: infrastructure that is not
what the user described, in a place they did not choose. So resolution must be

1. **deterministic** — the same Specification resolves the same way on every
   machine, every run;
2. **explainable** — the plan says which provider was chosen and why;
3. **approved** — the Specification the user confirms names a concrete
   provider, never `agnostic`;
4. **refusable** — where the input genuinely does not determine a cloud,
   CloudSDD says so instead of choosing.

### 2.2 The candidate filter is `Validate`, not a capability table

The obvious implementation is a table mapping ResourceType to the providers
that support it. The obvious implementation is wrong: a hand-maintained table
is a second source of truth that drifts from the `switch` statements it
mirrors, and drift between a declared capability and the real one is precisely
the RFC 011 §1.1 failure mode.

There is already a method that answers "can this provider express this
resource, in this region, under these policies?" without contacting
infrastructure — `CloudProvider.Validate`, whose contract has said exactly that
since RFC 001 §2.3.

**A candidate is a registered provider whose `Validate` returns nil.**

This reuses one code path for both resolution and validation, so a provider
cannot advertise a capability its validation rejects. It also means resolution
automatically accounts for everything `Validate` already checks: ResourceType
support, property decoding, `Policies.AllowedRegions`, the zone rules of RFC
013 §2.3, and region format.

### 2.3 Region format is already a resolution signal

The three region formats are mutually exclusive, which turns out to settle most
of the problem before any preference is consulted:

| Provider | Pattern | Example | Matches the others? |
|---|---|---|---|
| AWS | `^[a-z]{2}-[a-z]+-\d$` | `eu-central-1` | no |
| GCP | `^[a-z]+-[a-z]+\d+$` | `europe-west1` | no |
| Azure | `^[a-z][a-z0-9]{2,}$` | `westeurope` | no |

So `provider: "agnostic"` with `scope.region: "eu-central-1"` has exactly one
candidate, and needs no preference, no configuration and no tie-break. The same
holds for a `cross_account_role`, which only AWS implements.

This is worth stating plainly because it inverts the expected shape of the
feature: in the common case, agnostic resolution is *determined by the
Specification the user already wrote*, and the preference order below exists
for the genuinely ambiguous remainder — a resource with no region, or one whose
region several providers would accept.

A consequence to accept: writing `agnostic` with an AWS-shaped region is not
portability, it is AWS spelled differently. Portable regions
(`eu-central` → per-provider) are a real feature and a much larger one; §7.1
defers them.

### 2.4 Resolution algorithm

For each resource with `provider: "agnostic"` and no `Account` (an account
already names its provider, RFC 004 §2.2, and that path is unchanged):

1. **Candidates** = registered providers, in a **sorted, deterministic order**,
   whose `Validate` returns nil.
2. **Exactly one** → resolved.
3. **More than one** → apply the first match from the declared preference:
   1. `policies.provider_preference` — an ordered list in the Specification;
   2. `defaults.provider` in `~/.cloudsdd/config.yaml`;
   3. otherwise **error**, naming every candidate and both ways to choose.
4. **None** → error, quoting each provider's own rejection.

Step 1's ordering is called out because `DefaultEngine.providers` is a Go map,
and ranging over a map is randomised. Resolution that depended on map order
would produce a different cloud on different runs of the same Specification —
the exact opposite of the determinism §2.1 requires.

Step 4's error is the valuable one. Today an unsupported resource produces one
provider's complaint; here the user sees all three, which is usually enough to
show what is actually wrong:

```
Error: engine: resource "app-cdn": no provider can deploy this resource
  aws:   unsupported resource type: "container_service"
  gcp:   unsupported resource type: "container_service"
  azure: unsupported resource type: "container_service"
```

### 2.5 Providers that are not configured are not candidates

RFC 011 §2.8 made provider construction lazy so an AWS-only deploy would not
demand GCP and Azure credentials. Agnostic resolution needs candidates, so the
CLI must attempt to build every provider when any resource is agnostic.

A provider that fails to construct — no passphrase, no credentials — is
**excluded from candidacy and reported**, never silently dropped:

```
Note: azure is not available as a candidate (CLOUDSDD_PULUMI_PASSPHRASE is not set)
Resolved: app-db -> gcp (only candidate for region europe-west1)
```

Silently excluding it would make the resolution depend on invisible local
state, and the same Specification would resolve differently on a colleague's
machine with no way to see why.

### 2.6 The user approves a concrete Specification

Resolution runs in the Engine's `Validate`, before `Plan`. The resolved
provider is written into the resource, so:

- the rendered Specification the CLI prints names `aws`, not `agnostic`;
- the plan prints one resolution line per agnostic resource, with its reason;
- the ledger records the concrete provider, so a later prompt referring to the
  resource resolves against where it actually lives.

`Plan` and `Apply` therefore never see an agnostic resource, and
`ErrAgnosticResolutionNotImplemented` is deleted rather than moved.

## 3. Impacted JSON Schema

| Change | Field | Compatibility |
|---|---|---|
| Newly usable | `resources[].provider: "agnostic"` | Additive: previously a hard error |
| Added | `policies.provider_preference` | Optional ordered list of `aws`, `gcp`, `azure` |
| Behavior | The Specification the user approves | Now always names a concrete provider |

Configuration gains `defaults.provider`, absent by default.

## 4. Security Considerations

- **Resolution must not widen the blast radius.** A resolved provider is
  subject to exactly the same `Validate` it would face if named explicitly —
  that is the point of §2.2 — so `allowed_regions`, sealed-by-default and the
  secure defaults of every resource type apply unchanged.

- **Determinism is a security property here, not just a usability one.** A
  Specification that resolves to AWS in review and to Azure in production is a
  change of blast radius nobody approved. Hence the sorted candidate order and
  the refusal to break ties implicitly.

- **The confirmation gate stays the control.** Because resolution happens
  before `Plan`, the concrete provider is on screen before anything is applied.
  A user who disagrees declines, exactly as with any other plan.

- **No credential probing.** Candidacy is decided by whether a provider
  *constructs*, not by attempting a call to the cloud. CloudSDD does not go
  looking for which clouds the operator has access to; that would turn a deploy
  into a reconnaissance sweep across three providers.

- **Prompt influence.** The translator chooses whether to emit `agnostic`, and
  it is influenced by the ledger, which is attacker-influenceable (RFC 011 §4).
  Resolution cannot make that worse than an explicit provider would: either way
  the plan is shown and gated. It is called out so the reasoning is on record.

## 5. Testing Plan

| Package | Focus |
|---|---|
| `internal/engine` | Single candidate by region format, by type support; ambiguity refused without a preference; `policies.provider_preference` honored in order; config default as fallback; zero candidates reports every provider's reason; resolution is stable across repeated runs (the map-order regression); resolved provider written into the spec before `Plan`. |
| `internal/engine` | An agnostic resource with `Account` set still resolves through the DeploymentTarget path, unchanged. |
| `cmd/cloudsdd` | All providers attempted when a spec is agnostic; a provider that fails to construct is excluded *and reported*; the printed Specification names a concrete provider; the resolution reason appears before the confirmation gate. |
| `internal/config` | `defaults.provider` parsed, absent by default, invalid value rejected. |
| `internal/spec` | `provider_preference` validated (`dive,oneof=aws gcp azure`, no duplicates, never contains `agnostic`). |

## 6. Rollout

| Step | Content | Gate |
|---|---|---|
| 1 | `policies.provider_preference`, `defaults.provider`, validation | Table-driven tests |
| 2 | Engine resolution, deterministic ordering, error aggregation | Full suite |
| 3 | CLI: build all candidates, report exclusions, render the resolution | CLI tests |
| 4 | NLP prompt guidance on when to emit `agnostic` | Prompt parity tests |
| 5 | `docs/cli.md`, `docs/architecture.md`, `docs/openapi.yaml`, coverage floors | CI green, `gosec`, `govulncheck` |

## 7. Open Questions

1. **Portable regions.** `scope.region: "eu-central"` mapped per provider would
   make `agnostic` mean portability rather than "one cloud, determined by the
   region I happened to write" (§2.3). It is a substantial feature — a curated
   region equivalence table, kept current across three clouds — and deserves
   its own RFC. Proposed answer: **defer**.
2. **Cost-based resolution.** RFC 001 §5 floated "minimum cost" as a criterion.
   It needs the pricing model RFC 011 §2.9 deferred, and it would make
   resolution non-deterministic as prices move. Proposed answer: **no**,
   and note it in the RFC that eventually introduces cost policy.
3. **Should `defaults.provider` also apply to explicitly-named resources?**
   No — it is a tie-break for `agnostic`, not a rewrite of what the user wrote.
4. **`container_service`** remains advertised and unimplemented after this RFC.
   Proposed answer: its own RFC, now the last of the three.

## 8. Implementation notes

Deviations and findings worth recording against the plan above.

- **`Resolve` joined the `Engine` interface.** The CLI needs the resolved
  Specification *and* the reasons before it renders anything, which a method
  hanging off `*DefaultEngine` could not provide through the interface the CLI
  holds. `Validate` also calls it, so an Engine driven directly still never
  hands a provider an agnostic resource; the call is a no-op once nothing is
  agnostic.

- **Multi-region resolution requires one provider for *all* regions.** §2.4
  described candidacy per resource; the implementation probes every region the
  resource fans out to (RFC 005 §2.4.2) and requires the provider to accept
  each. Accepting a provider that covers only some of them would fail halfway
  through an apply, after part of the infrastructure exists.

- **The candidate probe reuses `ValidateRegionAllowed` explicitly.** The Engine
  enforces `AllowedRegions` centrally (RFC 011 §2.3) *and* each provider does
  in its own `Validate`. Resolution calls the central check itself rather than
  relying on the provider's, so a provider that somehow omitted it could not
  become a candidate for a region policy forbids.

- **`buildEngine` distinguishes explicit from candidate failures**, which §2.5
  implied without spelling out. A provider the user named by hand that fails to
  construct is still a hard error — they asked for it specifically — while one
  attempted only as an agnostic candidate becomes a reported exclusion. Losing
  that distinction would have turned RFC 011 §2.8's laziness fix into a
  regression.

- **A test-ordering trap worth recording.** `newHarness` rewires
  `providerFactories` itself, so tests installing their own factories must do
  it *after* the harness, not before. The first version of the resolution tests
  silently exercised the permissive fakes and reported ambiguity where the
  region should have decided.

- **Coverage.** `internal/engine` 96.0%, `cmd/cloudsdd` 93.5%,
  `internal/config` 84.8%; floors ratcheted to match. `gosec` and
  `govulncheck` clean, no new suppressions.

- **`ErrAgnosticResolutionNotImplemented` is deleted**, not deprecated. It was
  the last of the three capabilities the system prompt advertised and nothing
  delivered; only `container_service` remains (§7.4).
