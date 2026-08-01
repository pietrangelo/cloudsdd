#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Pietrangelo Masala
#
# Enforces a per-package coverage floor (RFC 011 §5.1).
#
# CLAUDE.md targets >90%. Three packages are held to a lower floor because
# their remaining uncovered statements are the Pulumi Automation API
# surface — upsertStack, Plan, Apply, Destroy — which shells out to the
# `pulumi` binary and talks to a real cloud control plane. Those paths are
# covered by the build-tagged integration tests instead
# (go test -tags=integration ./internal/provider/...).
#
# Floors ratchet upward only: raise them when coverage improves, never
# lower them to make a red build green.

set -euo pipefail

PROFILE="${1:-coverage.out}"

# package:minimum
FLOORS=(
  "cloudsdd/cmd/cloudsdd:88"
  "cloudsdd/internal/config:82"
  "cloudsdd/internal/engine:93"
  "cloudsdd/internal/nlp:96"
  "cloudsdd/internal/provider:100"
  "cloudsdd/internal/provider/decode:95"
  "cloudsdd/internal/provider/pulumiutil:100"
  "cloudsdd/internal/spec:89"
  "cloudsdd/internal/state:85"
  # Pulumi-bound: see the note above.
  "cloudsdd/internal/provider/aws:58"
  "cloudsdd/internal/provider/azure:58"
  "cloudsdd/internal/provider/gcp:57"
)

if [[ ! -f "$PROFILE" ]]; then
  echo "coverage profile $PROFILE not found" >&2
  exit 1
fi

# Build a "package<TAB>percent" table from the per-function profile.
COVERAGE=$(go tool cover -func="$PROFILE" \
  | awk '$1 != "total:" {
      n = split($1, parts, ":")
      path = parts[1]
      sub(/\/[^\/]*$/, "", path)          # strip the file name
      gsub(/%/, "", $NF)
      # Weight each function equally is wrong; accumulate statements instead
      # is not available here, so aggregate with the cover tool per package
      # below. This branch only collects package names.
      pkgs[path] = 1
    }
    END { for (p in pkgs) print p }')

status=0

for entry in "${FLOORS[@]}"; do
  pkg="${entry%:*}"
  floor="${entry##*:}"

  actual=$(go test -cover "$pkg" 2>/dev/null \
    | grep -oE 'coverage: [0-9.]+%' \
    | grep -oE '[0-9.]+' || true)

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
for pkg in $COVERAGE; do
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
