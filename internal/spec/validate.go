// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"
)

// resourceIDPattern mirrors the pattern defined in the JSON Schema
// (docs/rfc/001-core-architecture-and-json-schema.md, $defs.resource.id).
var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("resourceid", validateResourceID); err != nil {
		// This error can only happen for a duplicate/invalid tag name at
		// compile time: a bug in the code, not a runtime condition.
		panic(fmt.Sprintf("spec: failed to register the resourceid validator: %v", err))
	}
	return v
}

func validateResourceID(fl validator.FieldLevel) bool {
	return resourceIDPattern.MatchString(fl.Field().String())
}

// Validate applies domain rules to the already-decoded Specification
// (second tier of validation on top of Parse's strict decoding,
// RFC 001 §3 "Two-tier validation").
func Validate(s *Specification) error {
	if err := validate.Struct(s); err != nil {
		return fmt.Errorf("spec: validation failed: %w", err)
	}
	return nil
}
