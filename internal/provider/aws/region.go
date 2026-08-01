// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"fmt"

	"cloudsdd/internal/provider"
)

// validateRegionAllowed checks that region is included in allowed, when
// the latter is non-empty (RFC 001 §3, RFC 002 §2.5). An empty allowed
// means "no region constraint declared": it is not this check's job to
// decide whether that is acceptable for a given ResourceType (RFC 003
// §2.3 imposes a stricter constraint specific to cross_account_role,
// checked separately in AWSProvider.Validate).
//
// The rule itself now lives on the cloud-agnostic contract
// (provider.ValidateRegionAllowed, RFC 011 §2.3) so GCP and Azure enforce
// it too; this wrapper preserves the "aws: " prefix and the package-local
// ErrRegionNotAllowed sentinel that existing callers and tests match on.
func validateRegionAllowed(region string, allowed []string) error {
	if err := provider.ValidateRegionAllowed(region, allowed); err != nil {
		return fmt.Errorf("aws: region %q not in allowed_regions %v: %w", region, allowed, ErrRegionNotAllowed)
	}
	return nil
}
