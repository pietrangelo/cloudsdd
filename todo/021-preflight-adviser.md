# Implementation Plan: RFC 021 (The Pre-Flight Adviser)

Ordered per [RFC 021](../docs/rfc/021-preflight-adviser.md) §6, which puts
the safety gate before the optimisation: Phases 1 and 2 change no
behaviour and are independently revertible, Phase 3 is the first
behaviour change and the one to review hardest, and the intent
corroboration that the originating proposal listed first arrives in
Phase 4.

Items are ordered tests-first per `<tdd_policy>`. A test item may leave
its package not compiling until the implementation item that follows it;
every **implementation** item must leave the tree compiling and
`go test ./... -count=1` green.

RFC 021 §2.1 governs every item below: the adviser may raise a gate,
never lower one. Any item that makes an existing check conditional on an
adviser response is wrong, whatever it says here.

**Other open plans:** [RFC 020](020-scope-owned-filesystems.md) still has
Phases 2 (final ratchet) through 5 outstanding. It is unaffected by this
plan — the two touch no common file except `scripts/coverage-gate.sh`
and `docs/architecture.md`, both at the end of each.

## Phase 1: `internal/judge` — the vocabulary and the transport (RFC 021 §6 step 1)

- [x] Create `internal/judge/judge_test.go`: 3 tests (33 rejection rows, each a sentinel) + `FuzzDecodeDecision`; red on the undefined vocabulary only.
- [x] Create `internal/judge/judge.go`: `Question`, `Answer`, `Decision`, the `Judge` interface, the sentinels, and the strict decoder (unknown fields rejected) satisfying the tests above. No HTTP in this file. *(98.6%; 25 mutants killed; 4 rows added to pin per-primitive strictness — RFC 021 §8.)*
- [x] Update `scripts/coverage-gate.sh`: add the `internal/judge` floor (98, from 98.6%). *(Added: the gate fails any package without a floor, so leaving this to the Phase 3 gate item would keep CI red for every item in between.)* *(Done: 98.6% ≥ 98; a floor of 99 fails.)*
- [x] Create `internal/judge/jev_test.go`: against an `httptest` server pointed at by `CLOUDSDD_TYPESAFE_ENDPOINT`, per RFC 011 §5 and the shape of `internal/nlp/http_test.go`. Assert the request body field by field (`model`, `state`, and each question's `type`/`instructions`/`criteria`) so a silently-renamed wire field fails; the happy path for each of the three primitives; `401`/`422`/`429`/`529` each mapped to their sentinel; one backoff retry on `429` then success; a body exceeding `maxResponseBytes`; a truncated body; and the deadline. *(Red on the undefined client only; compiles and vets against a throwaway stub — RFC 021 §8.)*
- [x] Create `internal/judge/jev.go`: the `net/http` client — `POST /v1/systemone`, bearer auth from `TYPESAFE_API_KEY`, the 3 s timeout of §2.5, `maxResponseBytes`, and a single backoff retry on `429`/`529` inside that budget. `encoding/json` and `net/http` only: no new module dependency (§1.1). *(99.2%; 18 mutants killed; 7 rows added to `jev_test.go` — RFC 021 §8.)*
- [ ] Create `internal/judge/factory.go`: `NewJudge(provider, model)`, rejecting an unknown provider the way `nlp.NewTranslator` does. This file is what makes §2.2's departure from the originating proposal real — a second System One model is a fourth file here, not a migration.

## Phase 2: `internal/preflight` — the domain (RFC 021 §6 step 2)

- [ ] Create `internal/preflight/summary_test.go`: assert `Summarize` emits exactly the seven fields of §2.6, and that a sentinel seeded into `Resource.Properties`, `Resource.Account` and `Resource.Resolved` never appears in the marshalled bytes (§4.2). This is the test that keeps the projection a projection.
- [ ] Create `internal/preflight/summary.go`: `Summary`, `Change`, and `Summarize`. Built field by field from `spec.Specification` and `[]provider.Diff` — never `json.Marshal` of either.
- [ ] Create `internal/preflight/questions_test.go`: pin the question set and the rubric — `action`'s five options including the load-bearing `other` (§2.3), `directive`, `unrequested_destruction`, `production_impact`, and `operational_risk`'s five ordered levels in order.
- [ ] Create `internal/preflight/questions.go`: those questions, with the §2.3/§2.4 instructions and criteria verbatim.
- [ ] Create `internal/preflight/preflight_test.go`: the §2.4 verdict table row by row against a fake `Judge` — each threshold at, just below, and just above its boundary; `production_impact` firing only in conjunction with a real removal in the diff; an adviser error and a disabled adviser both yielding `Proceed` with the deterministic concerns intact; and `Destructive` alone over a diff with no adviser configured at all.
- [ ] Create `internal/preflight/preflight.go`: `Verdict`, `Concern`, `Destructive`, `Review`, and the thresholds as named constants each carrying the reasoning from §2.4 and §7.1 — they are chosen, not measured, and the file must say so.

## Phase 3: The blast-radius gate (RFC 021 §6 step 3 — first behaviour change)

- [ ] Update `cmd/cloudsdd/run_test.go`: a flagged plan refuses under `--yes`; `--force` overrides a halt and is reported; an adviser failure prints the §2.5 abstention note and proceeds; a `Proceed` verdict leaves today's output byte-identical.
- [ ] Update `cmd/cloudsdd/run.go`, `cmd/cloudsdd/deploy.go` and `cmd/cloudsdd/destroy.go`: call `preflight.Review` between `eng.Plan` and `confirm`, register `--force`, and make `--yes` no longer imply consent for a flagged plan. The failure message must name `--force` (§2.4).

## Phase 4: Intent corroboration (RFC 021 §6 step 4)

- [ ] Update `cmd/cloudsdd/run_test.go`: a disagreement prompts **before** `Translate` is called — asserted by the fake translator recording that it was never invoked, not by output alone; confidence below 0.80 abstains; `other` and a `directive` above threshold are carried into the Phase 3 verdict.
- [ ] Update `cmd/cloudsdd/run.go`: ask the §2.3 questions after the prompt is assembled and before `translator.Translate`. The existing post-translation `sddSpec.Intent != intent` check is not touched — this is a cheaper, earlier, independent reading, not a replacement.

## Phase 5: Configuration (RFC 021 §6 step 5)

- [ ] Update `internal/config/config_test.go`: `preflight` absent means disabled; enabled with `TYPESAFE_API_KEY` absent is a hard `LoadConfig` error (§2.5's one deliberate fail-closed case); an unknown `preflight.provider` is rejected.
- [ ] Update `internal/config/config.go`: the `PreflightConfig` block of §2.8, its defaults, its validation, and the one-line notice on a run where it is absent. The API key is read from the environment only and never from `config.yaml`.

## Phase 6: The gate and the documentation (RFC 021 §6 step 6)

- [ ] Update `scripts/coverage-gate.sh`: add the `internal/preflight` floor (`internal/judge` already has one, from Phase 1), and confirm the existing structural checks still hold.
- [ ] Update `docs/architecture.md`: the adviser, the two packages and why they are two, and §2.1's rule.
- [ ] Update `docs/cli.md`: `--force`, what `--yes` now means, the `preflight` config block, and the §4.1 disclosure of what leaves the machine.
- [ ] Update `docs/rfc/021-preflight-adviser.md`: fill in `## 8. Implementation notes` with the decisions that did not survive contact, and set the status to Implemented.
