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
# binary and talks to a real cloud control plane. Those paths are covered
# by the build-tagged integration tests instead
# (go test -tags=integration ./internal/provider/...).
#
# What remains uncovered in internal/state is of the same kind, at a
# smaller scale: I/O failures that can only happen *after* a file handle
# is open — a failed Chmod, Write, Sync or Close on the ledger's temp
# file. Reaching them needs a filesystem that fails mid-write, and the
# seam that would fake one is a worse trade than the four statements.
#
# Floors ratchet upward only: raise them when coverage improves, never
# lower them to make a red build green.

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
  "cloudsdd/internal/provider/decode:95"
  "cloudsdd/internal/provider/network:93"
  "cloudsdd/internal/provider/pulumiutil:100"
  "cloudsdd/internal/schedule:96"
  "cloudsdd/internal/spec:90"
  "cloudsdd/internal/state:91"
  # Pulumi-bound: see the note above.
  "cloudsdd/internal/provider/aws:65"
  "cloudsdd/internal/provider/azure:71"
  "cloudsdd/internal/provider/gcp:70"
)

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

exit $status
