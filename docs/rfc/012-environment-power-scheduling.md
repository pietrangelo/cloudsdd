# RFC 012: Environment Power Scheduling

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 002](002-aws-provider.md), [RFC 005](005-account-environment-region-scoping.md),
  [RFC 006](006-cli-entrypoint.md), [RFC 007](007-aws-relational-database.md),
  [RFC 008](008-gcp-azure-providers.md), [RFC 009](009-global-state-tracking.md),
  [RFC 011](011-provider-hardening-and-test-coverage.md)

## 1. Problem

A non-production environment is billed 24/7 and used for roughly 45 hours a
week. Everything CloudSDD provisions today runs continuously: there is no way to
express *"this environment is up Monday to Friday from 08:00 to 19:00 and off at
the weekend"*, so a `dev` database costs the same as a production one.

This is the first CloudSDD feature whose direct outcome is cost reduction rather
than correctness or security. It is also the first feature whose behavior is
*temporal*: the specification no longer describes only what infrastructure
exists, but when it is powered on.

Three constraints shape everything below.

### 1.1 There is no CloudSDD daemon

`cloudsdd` is invoked, applies a specification, and exits (RFC 006). Nothing
survives the process. A schedule implemented as "the CLI stops the database when
you run it" is not a schedule; a schedule implemented as a local `cron` entry
silently stops working the moment the operator's laptop is closed or the CI
runner is decommissioned — and the failure mode is *paying for infrastructure
that was supposed to be off*, which is invisible until the invoice arrives.

The schedule must therefore be **materialized as cloud-native resources in the
target account**, provisioned by the same Pulumi program as the resource it
governs, and must keep working with no CloudSDD process anywhere.

### 1.2 The AI must not invent times

Deciding that a team's working day starts at 08:00 is a cost and availability
decision belonging to the user, not a translation the model can perform. A
mistranslated region provisions a bucket in the wrong place; a mistranslated
*stop time* takes an environment down while people are working in it.

The translator therefore emits the *intent* to schedule. The concrete times are
elicited from the user, exactly as the request that motivated this RFC asked for
("chiedere gli orari di avvio e di spegnimento all'utente").

### 1.3 Silence is not an acceptable outcome

RFC 011 §1.1A catalogued the failure mode this project treats as the worst one:
the user receives infrastructure that differs from what they described, with no
error. Scheduling multiplies the opportunities for this. Every provider gap in
§4 is therefore a hard `Validate` error, never a dropped rule.

## 2. Proposed Architecture

```
  Specification                internal/schedule            provider
 ┌──────────────┐   Effective  ┌───────────────┐  []Rule   ┌──────────────────┐
 │ policies.    │──Schedule───▶│               │──────────▶│ AWS  EventBridge │
 │   schedule   │              │   Compile()   │           │ GCP  Cloud Sched │
 │ resource.    │              │  (pure func)  │           │ AZ   Automation  │
 │   schedule   │              └───────────────┘           └──────────────────┘
 └──────────────┘                     ▲
        ▲                             │
        │  elicitation                │  validated once, in the Engine,
   cmd/cloudsdd  ◀── user             │  before any provider is reached
```

The design deliberately concentrates all reasoning about time into a single
pure function, and leaves each provider with a mechanical translation of an
already-validated rule set into its native primitive.

### 2.1 Schema (`internal/spec`)

Additive. A schedule is declared once for the whole specification and inherited
by every schedulable resource; a resource may override it or opt out.

```json
{
  "sdd_version": "1.0",
  "intent": "deploy",
  "policies": {
    "allowed_regions": ["eu-central-1"],
    "schedule": {
      "enabled": true,
      "timezone": "Europe/Rome",
      "start": "08:00",
      "stop": "19:00",
      "days": ["mon", "tue", "wed", "thu", "fri"],
      "exceptions": [
        { "from": "2026-12-24", "to": "2027-01-06", "mode": "always_off", "reason": "company shutdown" },
        { "from": "2026-09-12", "to": "2026-09-14", "mode": "always_on",  "reason": "release weekend" }
      ]
    }
  },
  "resources": [
    {
      "id": "app-db",
      "type": "relational_database",
      "provider": "aws",
      "scope": { "environment": "dev", "region": "eu-central-1" },
      "properties": { "engine": "postgres", "version": "15" }
    },
    {
      "id": "always-up-db",
      "type": "relational_database",
      "provider": "aws",
      "scope": { "environment": "prod", "region": "eu-central-1" },
      "schedule": { "enabled": false },
      "properties": { "engine": "postgres", "version": "15" }
    }
  ]
}
```

`Policies.Schedule` and `Resource.Schedule` are both `*Schedule`. The pointer is
load-bearing: it distinguishes *absent* (inherit) from *present and disabled*
(opt out), the same tri-state pattern RFC 011 §2.5 established for
`deletion_protection`.

```go
// Schedule describes when a resource is powered on. It is declared once in
// Policies and inherited by every schedulable resource in the
// Specification; a Resource may override it wholesale.
type Schedule struct {
    // Enabled turns scheduling on. Absent means false: an environment that
    // was never asked to shut down never shuts down.
    Enabled *bool `json:"enabled,omitempty"`

    // Timezone is an IANA zone name (e.g. "Europe/Rome"). Fixed offsets
    // are rejected: they silently drift by an hour across a DST boundary,
    // which would start the environment an hour late for half the year.
    Timezone string `json:"timezone,omitempty" validate:"omitempty,ianatz"`

    // Start and Stop are wall-clock times in Timezone, "HH:MM".
    Start string `json:"start,omitempty" validate:"omitempty,clocktime"`
    Stop  string `json:"stop,omitempty"  validate:"omitempty,clocktime"`

    // Days lists the weekdays the environment is powered on. Absent means
    // Monday to Friday, which makes the weekend off by construction.
    Days []Weekday `json:"days,omitempty" validate:"omitempty,max=7,unique,dive,oneof=mon tue wed thu fri sat sun"`

    // Exceptions are date ranges where the weekly rhythm is suspended.
    Exceptions []ScheduleWindow `json:"exceptions,omitempty" validate:"omitempty,max=12,dive"`
}

// ScheduleWindow suspends the weekly rhythm over an inclusive date range.
type ScheduleWindow struct {
    From   string       `json:"from" validate:"required,datetime=2006-01-02"`
    To     string       `json:"to"   validate:"required,datetime=2006-01-02"`
    Mode   ScheduleMode `json:"mode" validate:"required,oneof=always_on always_off"`
    Reason string       `json:"reason,omitempty" validate:"omitempty,max=128"`
}
```

Two new validators join `resourceid` and `scopename` in
`internal/spec/validate.go`:

| Tag | Rule |
|---|---|
| `clocktime` | `^([01]\d\|2[0-3]):[0-5]\d$` |
| `ianatz` | `time.LoadLocation` succeeds **and** the value is not `"Local"` or a fixed offset |

Resolution is wholesale, not field-by-field:

```go
// EffectiveSchedule returns the Schedule governing r: its own if it
// declares one, otherwise the Specification-wide default.
//
// A Resource that declares a Schedule replaces the inherited one entirely
// rather than merging field by field. A partial override would let a
// resource inherit a stop time its author never saw, which is exactly the
// class of surprise §1.2 exists to prevent.
func EffectiveSchedule(r Resource, p Policies) *Schedule
```

**Defaults, stated once:**

| Field | Absent means | Rationale |
|---|---|---|
| `enabled` | `false` | Opt-in. Existing specifications are unaffected. |
| `days` | `mon`–`fri` | The requested default. The weekend needs no rule: no start fires, and Friday's stop leaves the environment down. |
| `start` / `stop` / `timezone` | **elicited from the user** (§5) | Never defaulted. §1.2. |

### 2.2 The compiler (`internal/schedule`)

`Compile` turns a `Schedule` into a provider-agnostic rule set. It is a pure
function of its input — no clock, no environment, no I/O — so the entire
temporal logic of the project is table-testable without a cloud account.

```go
// Action is what a Rule does when it fires.
type Action string

const (
    ActionStart Action = "start"
    ActionStop  Action = "stop"
)

// Rule is one provider-agnostic scheduling instruction.
//
// A Rule stays structured rather than carrying a cron string: AWS uses a
// six-field cron() expression with a mandatory year, GCP and Azure use
// five-field unix cron. Rendering the expression is each provider's job;
// deciding when things happen is this package's.
type Rule struct {
    Name      string     // deterministic, stable across applies
    Action    Action
    Days      []Weekday  // empty means every day
    Hour, Min int
    Timezone  string
    ValidFrom *time.Time // nil: unbounded
    ValidTo   *time.Time // nil: unbounded
}

func Compile(s Schedule) ([]Rule, error)
```

Algorithm:

1. **Base rhythm.** Emit a `start` rule at `start` and a `stop` rule at `stop`,
   both on `days`, both unbounded.
2. **Reject ambiguity.** A window with `from > to`, or two windows that
   intersect, is an error. Inventing a precedence rule would mean the user
   cannot predict the outcome from reading their own specification.
3. **Segment.** Split the base rules around every window using
   `ValidFrom`/`ValidTo`, so the weekly rhythm is *suspended* for the window's
   duration rather than competing with it. Two windows produce three base
   segments.
4. **`always_on` window** → one **daily** `start` rule bounded to the window,
   and no `stop` rule inside it.
5. **`always_off` window** → one **daily** `stop` rule bounded to the window,
   and no `start` rule inside it.
6. **Cap.** At most 12 windows (schema-enforced), which bounds the generated
   resource count at `2 × (12+1) + 12 = 38` in the worst case.

Step 5 says *daily*, not *once*, for a concrete reason: **AWS automatically
restarts an RDS instance that has been stopped for 7 days.** A one-shot stop at
the start of a two-week company shutdown leaves the instance running for the
second week, billing quietly. A daily stop rule re-stops it the morning after
each automatic restart. This is the single most important operational detail in
this RFC, and the reason the compiler emits recurring rather than one-off rules.

`Describe(s Schedule) string` renders the summary shown to the user before the
confirmation gate:

```
Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off
  2026-09-12 to 2026-09-14: always on (release weekend)
  2026-12-24 to 2027-01-06: always off (company shutdown)
```

### 2.3 Validation in the Engine

`DefaultEngine.Validate` resolves and compiles the effective schedule for every
resource **before** delegating to any provider, following the enforcement lift
RFC 011 §2.3 applied to `AllowedRegions`: a check that lives only in providers
is a check a future provider forgets. Providers keep their own validation as
defense in depth, for the case where one is used directly outside the Engine.

New sentinel errors in `internal/engine/errors.go`, wrapped with the resource ID
in the style of the existing ones.

## 3. Which resources are schedulable

Only `relational_database` has a power state today. `object_storage` and
`cross_account_role` do not — an S3 bucket cannot be switched off, and an IAM
role costs nothing. `compute_instance` and `container_service` are declared in
the schema (RFC 001) but not implemented by any provider yet; they are the
obvious next consumers and the compiler output requires no change to serve them.

The distinction that makes the feature usable in practice:

| Case | Behavior |
|---|---|
| **Explicit** `resource.schedule` on a non-schedulable type | Error (`ErrResourceNotSchedulable`). The user asked for something specific that cannot happen. |
| **Inherited** `policies.schedule` on a non-schedulable type | Skipped, and reported in the plan output. |

Without the second row, no specification could contain both a bucket and a
database — which is most of them.

## 4. Provider mapping

Each provider translates `[]Rule` into its native primitive. Where a provider
cannot express a rule, it fails `Validate` rather than dropping it (§1.3).

| Provider | Primitive | Validity windows | Code artifact |
|---|---|---|---|
| **AWS** | EventBridge Scheduler, universal target `arn:aws:scheduler:::aws-sdk:rds:{start,stop}DBInstance` | `StartDate` / `EndDate` — **yes** | None |
| **GCP** | Cloud Scheduler → HTTP `PATCH` on the Cloud SQL Admin API (`settings.activationPolicy`: `ALWAYS` / `NEVER`), OAuth service account | **No** — a Cloud Scheduler job has no validity window | None |
| **Azure** | Automation Account (system-assigned identity) + runbook + `automation.Schedule` | `StartTime` / `ExpiryTime` — **yes** | Inline PowerShell runbook |

### 4.1 AWS (`internal/provider/aws/schedule.go`)

EventBridge Scheduler's *universal targets* call any AWS API directly with an
execution role. No Lambda, no deployment package, no code to version or patch —
the schedule is pure configuration. This is what makes AWS the reference
implementation.

- `declareSchedule` is called from `resourceProgram` **inside the same Pulumi
  program** as the database, so the schedules share the resource's stack
  identity and the existing `Destroy` path tears them down with no extra work.
- One `scheduler.Schedule` per compiled `Rule`, named deterministically from
  `Rule.Name` so a re-apply updates rather than duplicates.
- `ScheduleExpressionTimezone` carries the IANA zone name; the cron expression
  is rendered in AWS's six-field dialect.
- `FlexibleTimeWindow: { Mode: "OFF" }`. A start-of-workday action must be
  exact, not smeared across a window.
- Execution role: `rds:StartDBInstance` and `rds:StopDBInstance` on the instance
  ARN only. No wildcard action, no wildcard resource.
- Trust policy on `scheduler.amazonaws.com` with `aws:SourceAccount` (from
  `aws.GetCallerIdentity`) and `aws:SourceArn` scoped to
  `arn:aws:scheduler:{region}:{account}:schedule/default/{id}-*` — the
  confused-deputy guard. The wildcard is necessary: conditioning on a concrete
  schedule ARN would be circular, since the schedules reference the role.

### 4.2 GCP

Cloud Scheduler posts to the Cloud SQL Admin API with an OAuth token from a
dedicated service account holding `roles/cloudsql.admin` on that instance alone.
Also no code artifact.

A Cloud Scheduler job has no start or expiry date, so **exception windows cannot
be expressed**. Any compiled rule carrying `ValidFrom` or `ValidTo` is rejected
with `ErrScheduleExceptionsUnsupported`. The base weekly rhythm — the common
case, and the one the original request centers on — works fully. Closing the gap
needs a guard the scheduler invokes (Cloud Functions or Workflows), which brings
a code artifact into a project that currently ships none; it is deferred to its
own RFC rather than smuggled in here.

### 4.3 Azure

Azure has no equivalent of a universal target: something must call the ARM API.
An Automation Account with a system-assigned identity and an inline PowerShell
runbook is the standard, least-surprising choice, and `automation.Schedule`
supports `StartTime`/`ExpiryTime`, so exception windows work.

It is the only provider requiring an embedded code artifact, so it lands last
(§8) and its runbook is a fixed, reviewed constant in the repository — never
assembled from specification input.

## 5. CLI: eliciting the times

Elicitation runs after `translator.Translate` and before `eng.Plan`, so the
final times appear in the printed specification the user approves — the
specification stays the single source of truth, including the parts the user
typed at a prompt.

Triggered when the effective schedule is enabled and any of `timezone`,
`start`, or `stop` is missing:

```
This environment will be shut down outside working hours.
  Start time (HH:MM)  [required]: 08:00
  Stop time  (HH:MM)  [required]: 19:00
  Timezone            [Europe/Rome]:
  Days                [mon-fri]:

Schedule: Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off
```

- The timezone default is the operator's own `time.Local`, shown for
  confirmation rather than assumed silently.
- Input is read with the whole-line reader `run.go` already uses for the
  confirmation gate, not `fmt.Scanln`.
- Flags for non-interactive use: `--schedule-start`, `--schedule-stop`,
  `--schedule-timezone`, `--schedule-days`, and `--no-schedule` to strip a
  schedule the translator proposed.
- **Non-interactive with times missing is a hard error.** With `--yes` or on
  EOF, CloudSDD refuses rather than picking a default. An invented stop time is
  an outage.
- The plan output prints `schedule.Describe(...)` per resource before the
  confirmation gate, so the user approves the schedule in words, not in cron.

## 6. NLP prompt (`internal/nlp/prompt.go`)

The shared `systemPrompt` (one prompt, three translators — RFC 011 §2.6) gains
the `schedule` block, plus one rule stated in the imperative:

> If the user asks for a schedule without stating times, emit
> `"schedule": {"enabled": true}` and nothing else. NEVER invent `start`,
> `stop`, `timezone`, or a date range.

`exceptions` is documented so that "keep it up during the release weekend"
becomes an `always_on` window rather than being dropped, and so a follow-up
prompt can reason about a schedule already recorded in the ledger.

## 7. Security Considerations

- **Least privilege.** The AWS execution role can perform exactly two actions on
  exactly one instance ARN; the GCP service account holds `roles/cloudsql.admin`
  on one instance; the Azure identity is scoped to the target resource. A
  scheduling feature that provisions a broadly-privileged role would be a worse
  trade than the money it saves.

- **Confused deputy.** `scheduler.amazonaws.com` is an AWS service principal
  that will assume the role on behalf of *whoever* asks unless constrained. The
  `aws:SourceAccount` and `aws:SourceArn` conditions (§4.1) are mandatory, not
  optional hardening.

- **Not a `sealed` violation.** RFC 005 §2.5 forbids a resource from declaring
  trust toward a different Account or Environment. The scheduling role trusts an
  AWS *service* principal within the resource's own account, so `sealed` stays
  `true` and no specification needs to weaken it to get a schedule.

- **Availability is the blast radius.** Stopping a database destroys no data,
  but it does terminate connections, and a mistake here takes an environment
  down during working hours. Mitigations: scheduling is opt-in; times are never
  inferred; the plan renders the schedule in prose before the confirmation gate;
  and `resource.schedule: {"enabled": false}` gives a production resource an
  explicit, reviewable opt-out that a specification-wide policy cannot override.

- **DST correctness.** IANA zone names are required and fixed offsets rejected.
  The zone name is passed through to the provider primitive
  (`ScheduleExpressionTimezone`, Cloud Scheduler `time_zone`, Automation
  `TimeZone`); CloudSDD never precomputes a UTC offset, which would be correct
  for half the year.

- **Overnight windows.** `start > stop` (e.g. `22:00` → `06:00`) is accepted and
  compiled as-is: the start rule fires in the evening and the stop rule the
  following morning. It is called out here because it is the one input where the
  naive reading ("stop must be after start") would reject a legitimate request.

- **RDS 7-day auto-restart** (§2.2 step 5) is a documented cloud behavior the
  compiler works around, not an assumption. Tested explicitly.

- **Prompt injection.** `reason` strings flow into the ledger and back into the
  system prompt on the next invocation. They are length-capped at 128 characters
  and remain inside the untrusted-data delimiters `withLedgerContext` already
  establishes (RFC 011 §4). They are never interpolated into an IAM policy, a
  resource name, or the Azure runbook.

- **Cost is a security-adjacent property here.** The failure mode of a silently
  broken schedule is a bill, not an alert. This is why §1.3 admits no silent
  degradation: a rule that cannot be honored must fail loudly at `Validate`,
  when someone is watching.

## 8. Testing Plan

Table-driven and native, per CLAUDE.md, with a >90% coverage target.

| Package | Focus |
|---|---|
| `internal/schedule` | The behavioral specification. Weekday default; weekend fully off; `always_on` suspends the stop rule; `always_off` emits a *daily* stop; overlapping and inverted windows rejected; overnight windows; segmentation with 0, 1, and 2 windows; deterministic rule names across repeated compiles. Pure logic — must clear >90% with no excuses. |
| `internal/spec` | `clocktime` and `ianatz` validators, including fixed-offset and `"Local"` rejection; `EffectiveSchedule` inheritance and wholesale override; unknown fields inside `schedule` rejected by the existing strict decoding. |
| `internal/engine` | Schedule validated before provider dispatch; a resource with an unschedulable type and an explicit schedule fails; the same type with an inherited schedule passes. |
| `internal/provider/aws` | `pulumi.WithMocks` assertions on the real inputs: one schedule per rule, expected cron dialect and timezone, `FlexibleTimeWindow.Mode == "OFF"`, and an IAM policy naming exactly the instance ARN with exactly two actions and no wildcard. |
| `internal/provider/gcp` | Base rhythm produces the expected jobs; any rule with a validity window fails `Validate` with the explicit error. |
| `internal/provider/azure` | Schedules carry `StartTime`/`ExpiryTime`; the runbook content is the fixed constant, unaffected by specification input. |
| `cmd/cloudsdd` | Elicitation with a scripted stdin; answers appear in the printed specification; `--yes` without times fails; `--schedule-*` flags bypass the prompt; `--no-schedule` strips a translator-proposed schedule. |
| `internal/nlp` | Prompt parity across the three translators after the addition. |

A native fuzz target over `Compile` inputs (`HH:MM` strings, date strings,
window sets) guards the parsing surface, consistent with the existing
`internal/spec/fuzz_test.go`.

## 9. Rollout

| Step | Content | Gate |
|---|---|---|
| 1 | `internal/schedule` compiler, `spec` fields and validators | Table-driven tests, `go vet` |
| 2 | Engine validation, CLI elicitation and flags, NLP prompt | Full suite, CLI tests with scripted stdin |
| 3 | AWS EventBridge Scheduler and IAM role | `pulumi.WithMocks` assertions |
| 4 | GCP Cloud Scheduler (base rhythm; exceptions rejected) | Same |
| 5 | Azure Automation | Same |
| 6 | `docs/cli.md`, `docs/architecture.md`, coverage floor | CI green, `gosec`, `govulncheck` |

Steps 1–3 are independently useful: after step 3 the feature is complete for
AWS, which is the only provider with a schedulable resource type in production
use today.

## 10. Impacted JSON Schema

| Change | Field | Compatibility |
|---|---|---|
| Added | `policies.schedule` | Additive; absent means no scheduling |
| Added | `resources[].schedule` | Additive; absent means inherit |
| Behavior | Unknown fields inside `schedule` | Rejected, per the existing strict decoding (RFC 011 §2.1) |

No existing specification changes meaning. The ledger schema is unaffected: a
schedule is part of `spec.Resource` and is recorded through the existing path.

## 11. Open Questions — resolved at approval

1. **Should `enabled: true` also require an explicit acknowledgment for a
   resource whose `scope.environment` looks like production?** **No.** RFC 011
   §7.2 rejected implicit behavior keyed on the free-form environment string,
   and that reasoning still holds. The confirmation gate is the control, and
   `"schedule": {"enabled": false}` is the reviewable opt-out.
2. **Should `compute_instance` scheduling ship in this RFC?** **No.** No
   provider implements the type yet; adding it would mean shipping an
   untestable path. `ResourceType.SupportsSchedule` already returns true for it,
   so the day a provider lands, scheduling is not silently rejected.
3. **GCP exception windows** (§4.2): **reject now**, revisit in a dedicated RFC
   rather than introducing the project's first deployed code artifact here.

## 12. Implementation notes

Deviations and findings worth recording against the plan above.

- **A shared-stdin bug surfaced immediately.** The elicitation prompt and the
  existing confirmation gate each built their own `bufio.Reader` over
  `cmd.InOrStdin()`. The first buffered the answer meant for the second, so
  every scheduled deploy was cancelled with "operation cancelled by user".
  `runIntent` now creates one reader and threads it through both.

- **The AWS provider stayed invoke-free.** The trust policy needs the account
  and the region, which the obvious implementation reads through
  `aws:getCallerIdentity` and `aws:getRegion`. Both are Pulumi *invokes*, which
  would have had to be threaded through the assumed-credential provider options
  of RFC 004 and mocked separately in tests. The schedules necessarily live in
  the same account and region as the instance they manage, so both values are
  parsed out of the instance ARN instead (`parseARN`).

- **EventBridge `StartDate` is exclusive-ish.** It is documented as the date
  "after which the schedule can begin invoking its target", and exception
  windows open at local midnight with rules that fire at local midnight —
  exactly the boundary case. The compiler expresses clean half-open intervals
  and the AWS provider backdates `StartDate` by one second
  (`scheduleStartDate`), so the adaptation lives where the API semantics do.

- **Azure schedules are anchored, not purely recurrent.** `automation.Schedule`
  requires a `StartTime` at least five minutes in the future, so the provider
  computes the first occurrence against a clock — a `timeNow` seam, frozen in
  tests. Recomputing it on every apply would show a spurious diff, so the
  resource is declared with `pulumi.IgnoreChanges([]string{"startTime"})`. A
  rule whose window has already closed is skipped rather than declared, since
  Azure would reject a start time in the past.

- **`declare*` signatures changed to return their resource.** The AWS, GCP and
  Azure database declarations previously returned only an error. The schedules
  need the instance ARN, project, or resource ID, so each now returns the
  created resource (Azure via a small `databaseServer` struct carrying the
  engine, ID and resource group, since the API version and the RBAC action
  namespace both depend on the engine).

- **`time/tzdata` is embedded in the CLI.** `schedule.Compile` resolves IANA
  zone names, and a stripped container or a Windows host has no zoneinfo to
  read. The alternative failure mode — a schedule that compiles against the
  wrong DST rules — is worse than the ~450 KB.

- **`Describe` sanitizes `Window.Reason`.** It is model-generated text printed
  straight to the operator's terminal before the confirmation gate; without
  stripping control characters, an escape sequence could repaint or hide the
  very plan output the gate exists to let the user read. Not anticipated in §7
  as drafted; added during implementation.

- **Coverage.** `internal/schedule` reaches 96.8%, `internal/spec` 90.0%,
  `internal/engine` 94.9%, `cmd/cloudsdd` 92.3%. The three provider packages
  rose to 65–69% (from 57–59%): the new scheduling code sits below the Pulumi
  Automation API boundary and is therefore reachable by `pulumi.WithMocks`
  tests, unlike `upsertStack`/`Plan`/`Apply`/`Destroy`. Floors in
  `scripts/coverage-gate.sh` were ratcheted up accordingly.

- **`docs/openapi.yaml` still carried `max_cost_monthly`**, removed from the
  schema by RFC 011 §2.9. Corrected while adding the `schedule` component,
  since that document claims to mirror `internal/spec`.

- **Scans.** `gosec ./...` and `govulncheck ./...` are both clean; the new code
  added no suppressions.
