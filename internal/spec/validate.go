// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"

	"cloudsdd/internal/schedule"
)

// resourceIDPattern mirrors the pattern defined in the JSON Schema
// (docs/rfc/001-core-architecture-and-json-schema.md, $defs.resource.id).
var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// scopeNamePattern constrains Scope.Environment (RFC 005 §2.2): same
// charset as resourceIDPattern, shorter max length appropriate for a
// label rather than an identifier.
var scopeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("resourceid", validateResourceID); err != nil {
		// This error can only happen for a duplicate/invalid tag name at
		// compile time: a bug in the code, not a runtime condition.
		panic(fmt.Sprintf("spec: failed to register the resourceid validator: %v", err))
	}
	if err := v.RegisterValidation("scopename", validateScopeName); err != nil {
		panic(fmt.Sprintf("spec: failed to register the scopename validator: %v", err))
	}
	// The schedule tags (RFC 012 §2.1) delegate to internal/schedule
	// rather than restating its formats here: the package that compiles a
	// time is the package that decides what a valid time looks like.
	for tag, fn := range map[string]func(string) bool{
		"clocktime":    schedule.ValidClockTime,
		"ianatz":       schedule.ValidTimezone,
		"scheduledate": schedule.ValidDate,
	} {
		if err := v.RegisterValidation(tag, stringValidator(fn)); err != nil {
			panic(fmt.Sprintf("spec: failed to register the %s validator: %v", tag, err))
		}
	}
	return v
}

// stringValidator adapts a plain string predicate to validator.Func.
func stringValidator(fn func(string) bool) validator.Func {
	return func(fl validator.FieldLevel) bool { return fn(fl.Field().String()) }
}

func validateResourceID(fl validator.FieldLevel) bool {
	return resourceIDPattern.MatchString(fl.Field().String())
}

func validateScopeName(fl validator.FieldLevel) bool {
	return scopeNamePattern.MatchString(fl.Field().String())
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
