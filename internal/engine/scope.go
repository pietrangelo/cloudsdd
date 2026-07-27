// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import "cloudsdd/internal/spec"

// effectiveRegions expands a Scope into the list of regions the Engine
// must fan out Validate/Plan/Apply calls to (RFC 005 §2.4.2): Regions
// when set (multi-region), a single-element list for Region, or a single
// empty string for an unscoped resource (preserving pre-RFC-005 behavior:
// exactly one call per resource).
func effectiveRegions(s spec.Scope) []string {
	if len(s.Regions) > 0 {
		return s.Regions
	}
	if s.Region != "" {
		return []string{s.Region}
	}
	return []string{""}
}

// scopedResource returns a copy of r scoped to a single effective region
// (RFC 005 §2.4.2): Scope.Region is set to region and Scope.Regions is
// cleared, so every CloudProvider call only ever sees one region, never
// the plural form.
func scopedResource(r spec.Resource, region string) spec.Resource {
	r.Scope.Region = region
	r.Scope.Regions = nil
	return r
}
