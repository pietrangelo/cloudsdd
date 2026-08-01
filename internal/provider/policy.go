// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package provider

import (
	"errors"
	"fmt"
)

// ErrRegionNotAllowed indicates that the region requested by a resource is
// not included in Policies.AllowedRegions (RFC 001 §3, RFC 002 §2.5).
var ErrRegionNotAllowed = errors.New("region not in allowed_regions")

// ValidateRegionAllowed checks that region is included in allowed, when the
// latter is non-empty (RFC 001 §3, RFC 011 §2.3). An empty allowed means
// "no region constraint declared": it is not this check's job to decide
// whether that is acceptable for a given ResourceType (RFC 003 §2.3 imposes
// a stricter constraint specific to cross_account_role, checked separately
// in AWSProvider.Validate).
//
// This lives on the cloud-agnostic contract rather than inside one
// provider: RFC 011 §1.1D1 found AllowedRegions was enforced only by AWS,
// so a spec pinning allowed_regions deployed anywhere at all on GCP and
// Azure. The Engine now applies it centrally (RFC 011 §2.3) and each
// provider keeps its own call as defense in depth, so a provider driven
// directly — outside the Engine — is still safe.
func ValidateRegionAllowed(region string, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	for _, a := range allowed {
		if a == region {
			return nil
		}
	}
	return fmt.Errorf("region %q not in allowed_regions %v: %w", region, allowed, ErrRegionNotAllowed)
}
