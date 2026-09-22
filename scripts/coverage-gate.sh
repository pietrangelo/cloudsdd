#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Pietrangelo Masala
#
# Enforces a per-package coverage floor (RFC 011 §5.1).
#
# CLAUDE.md targets >90%, and every package meets it except the three
# provider packages. Those are held lower because their remaining
# uncovered statements are the Pulumi Automation API surface —
# upsertStack, Plan, Apply, Destroy — which shells out to the `pulumi`
# binary and talks to a real cloud control plane.
#
# An earlier version of this note claimed those paths were "covered by the
# build-tagged integration tests instead
# (go test -tags=integration ./internal/provider/...)". That was true of
# one package and read as true of three: only internal/provider/aws has an
# integration test, it exercises object_storage alone, and CI does not run
# the `integration` tag at all. So the honest statement is that this
# surface is covered by an integration test on AWS and by nothing on GCP
# or Azure — which is a gap worth closing, not a justification to cite.
#
# What remains uncovered in internal/state is of the same kind, at a
# smaller scale: I/O failures that can only happen *after* a file handle
# is open — a failed Chmod, Write, Sync or Close on the ledger's temp
# file. Reaching them needs a filesystem that fails mid-write, and the
# seam that would fake one is a worse trade than the four statements.
#
# Floors ratchet upward only: raise them when coverage improves, never
# lower them to make a red build green.
#
# One narrow exception, and it must be argued in the commit that uses it.
# The three provider packages are a growing body of Pulumi-driving code
# that no unit test can reach, wrapped around logic that is fully covered.
# Every RFC adding a method like EnsureNetwork or DestroyNetwork therefore
# *lowers* the ratio while improving the system, and padding tests to hold
# a number that measures the wrong thing is worse than moving the number.
# So: when a change adds Pulumi-bound surface to a provider package and
# the testable logic it adds is covered, the floor may be reset to the new
# actual — with the reason recorded here.
#
#   2026-08-01, RFC 016 §2.6: DestroyNetwork on all three providers.
#   azure 71 -> 70, gcp 70 -> 69. Every statement of the new method that
#   can be reached without the `pulumi` binary is tested (the region-less
#   no-op and the address-plan refusal); what remains is stack.Destroy
#   and its error wrap.
#
#   2026-08-09, RFC 019 Phase 1: the build_pipeline seam. gcp 73 -> 72.
#   This is the exception's mirror image and deserves stating as such,
#   because RFC 019 §5 predicted the opposite: it expected floors to
#   *rise* as logic migrated out of the Pulumi-bound packages, and it had
#   the arithmetic backwards. Phase 1 moved ten statements out of
#   internal/provider/gcp — buildRevision, the decode skeleton, the
#   hand-off guard — and every one of the ten was covered. The package
#   went 412/563 to 402/553: numerator and denominator each fell by ten
#   while the uncovered remainder held at exactly 151. No test was
#   deleted and no statement stopped being tested; the ten now live in
#   internal/provider/pipeline, which reads 96.1%. Removing covered code
#   from a package whose uncovered remainder is fixed lowers its ratio,
#   which is the clearest demonstration available that this ratio is a
#   proxy and not the thing itself.
#
#   2026-08-02, RFC 017 step 5: the Container Apps power schedule.
#   azure 70 -> 69. The exception is used here rather than in step 4,
#   where the shortfall was closed by writing the test resourceScope had
#   always been missing — coverage that was owed, not padding. Nothing of
#   that kind is left: every uncovered statement the schedule adds is an
#   `if err != nil` branch after a Pulumi resource declaration, which a
#   WithMocks monitor never fails, and the logic around them
#   (containerAppPowerTarget's actions, the API version, both verbs
#   reaching the runbook as parameters) is asserted.

set -euo pipefail

PROFILE="${1:-coverage.out}"

# package:minimum
FLOORS=(
  "cloudsdd/cmd/cloudsdd:93"
  "cloudsdd/internal/config:93"
  "cloudsdd/internal/engine:95"
  "cloudsdd/internal/nlp:96"
  "cloudsdd/internal/provider:100"
  "cloudsdd/internal/provider/compute:100"
  "cloudsdd/internal/provider/container:98"
  "cloudsdd/internal/provider/decode:95"
  "cloudsdd/internal/provider/network:93"
  "cloudsdd/internal/provider/pipeline:96"
  "cloudsdd/internal/provider/pulumiutil:100"
  "cloudsdd/internal/schedule:96"
  "cloudsdd/internal/spec:91"
  "cloudsdd/internal/state:91"
  # Pulumi-bound: see the note above.
  "cloudsdd/internal/provider/aws:70"
  "cloudsdd/internal/provider/azure:71"
  "cloudsdd/internal/provider/gcp:72"
)

# The leaf packages of RFC 011 §1.1H, which RFC 019 §2.1 makes a checked
# invariant rather than a remembered one: each must have zero internal
# imports. Their leafness is why they carry 95-100% floors while the
# Pulumi-bound packages sit at 70-72%, so it is the property every shared
# helper added to them has to preserve — a helper taking a *decode.Decoder
# or a provider.NetworkScope would read naturally and cost exactly this.
LEAVES=(compute container decode network pipeline)

# The Pulumi-bound provider packages, checked for the layering inversion
# RFC 019 §2.4 removed: none of them may depend on the engine that drives
# them. Only internal/provider/aws ever did, which is precisely what made
# it read as an accident rather than a decision — so the check covers all
# three, and the rule the next provider inherits is the fixed one.
PROVIDERS=(aws azure gcp)

if [[ ! -f "$PROFILE" ]]; then
  echo "coverage profile $PROFILE not found" >&2
  exit 1
fi

# Per-package coverage, computed from the profile this script was handed.
#
# Earlier versions took the profile only as proof that tests had run and
# then called `go test -cover` once per package, which ran the entire
# suite a second time — and, worse, measured something other than the
# artifact CI archives. The percentages now come from the same file.
#
# The profile's format is one line per block:
#   <file>:<startLine>.<col>,<endLine>.<col> <numStatements> <hitCount>
# so a package's coverage is its covered statements over its total, which
# is exactly what `go test -cover` reports. Counting blocks or averaging
# per-file percentages would both be wrong: blocks differ in size.
COVERAGE=$(awk '
  NR == 1 && /^mode:/ { next }
  {
    split($1, loc, ":")
    path = loc[1]
    sub(/\/[^\/]*$/, "", path)            # strip the file name
    total[path] += $2
    if ($3 > 0) covered[path] += $2
  }
  END {
    for (p in total) {
      pct = total[p] > 0 ? (covered[p] * 100.0 / total[p]) : 0
      printf "%s %.1f\n", p, pct
    }
  }' "$PROFILE")

status=0

for entry in "${FLOORS[@]}"; do
  pkg="${entry%:*}"
  floor="${entry##*:}"

  actual=$(awk -v pkg="$pkg" '$1 == pkg { print $2 }' <<<"$COVERAGE")

  if [[ -z "$actual" ]]; then
    echo "FAIL  $pkg — no coverage reported" >&2
    status=1
    continue
  fi

  if awk -v a="$actual" -v f="$floor" 'BEGIN { exit !(a + 0 < f + 0) }'; then
    printf 'FAIL  %-42s %6s%% < %s%% floor\n' "$pkg" "$actual" "$floor" >&2
    status=1
  else
    printf 'ok    %-42s %6s%% (floor %s%%)\n' "$pkg" "$actual" "$floor"
  fi
done

# Any package with tests that is not listed above is unguarded — catch it
# so new packages cannot skip the gate.
for pkg in $(awk '{ print $1 }' <<<"$COVERAGE"); do
  listed=0
  for entry in "${FLOORS[@]}"; do
    [[ "${entry%:*}" == "$pkg" ]] && listed=1 && break
  done
  if [[ $listed -eq 0 ]]; then
    echo "FAIL  $pkg — package has coverage but no floor in scripts/coverage-gate.sh" >&2
    status=1
  fi
done

# The leaf invariant. `.Imports` is the package's own import set and not
# its tests' — a test file may reach for another package without costing
# the shipped code its leafness, which is the property being guarded.
#
# The `|| true` matters: grep exits 1 when it matches nothing, and nothing
# is the passing case, so under `pipefail` the clean run is the one that
# would kill the script.
for leaf in "${LEAVES[@]}"; do
  pkg="cloudsdd/internal/provider/$leaf"
  imports=$(go list -f '{{join .Imports "\n"}}' "./internal/provider/$leaf" | grep '^cloudsdd/' || true)

  if [[ -n "$imports" ]]; then
    echo "FAIL  $pkg — leaf package (RFC 019 §2.1) must have no internal imports:" >&2
    while IFS= read -r imp; do
      echo "        imports $imp" >&2
    done <<<"$imports"
    status=1
  else
    printf 'ok    %-42s leaf, no internal imports\n' "$pkg"
  fi
done

# The inversion invariant. A provider depending on the engine that drives
# it inverts the dependency the architecture rests on; RFC 019 §2.4 moved
# DeploymentTarget and TargetProviderFactory into internal/provider so the
# edge could go, and this is what keeps it gone.
#
# `.Deps` and not `.Imports` here: reaching the engine through an
# intermediate package is the same inversion, and all three providers are
# clean transitively today, so the stronger form costs nothing. As above,
# it is the shipped code's graph and not its tests' — an engine-driving
# integration test is free to import both.
for p in "${PROVIDERS[@]}"; do
  pkg="cloudsdd/internal/provider/$p"
  inverted=$(go list -f '{{join .Deps "\n"}}' "./internal/provider/$p" | grep '^cloudsdd/internal/engine$' || true)

  if [[ -n "$inverted" ]]; then
    echo "FAIL  $pkg — must not depend on cloudsdd/internal/engine (RFC 019 §2.4)" >&2
    status=1
  else
    printf 'ok    %-42s no dependency on internal/engine\n' "$pkg"
  fi
done

exit $status
