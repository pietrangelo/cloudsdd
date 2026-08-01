// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package decode implements the strict, two-tier property decoding that
// every CloudProvider must apply before acting on Resource.Properties
// (RFC 002 §2.6, RFC 011 §2.1).
//
// It was originally private to internal/provider/aws. RFC 011 §1.1C found
// that every decoder written after it (AWS RDS, GCP, Azure) had hand-rolled
// a permissive json.Unmarshal instead, so the Mass Assignment defense
// CLAUDE.md mandates was applied on exactly one provider. Lifting it here
// makes the strict path the only path: a provider cannot decode properties
// without going through Decoder.Properties.
package decode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-playground/validator/v10"
)

// credentialLikeFieldNames are key-name substrings that, if present in
// Properties, are rejected upfront as defense in depth against credentials
// being embedded in the Specification (RFC 002 §2.4): even if such a field
// were not part of any known struct (and would therefore already be
// rejected by DisallowUnknownFields), the explicit check makes the intent
// non-negotiable and independent of any single resource's schema.
var credentialLikeFieldNames = []string{
	"access_key", "secret_key", "secret", "password", "token", "session_token", "private_key",
}

// Decoder decodes and validates provider-specific Properties. Each
// provider owns one, so error messages carry that provider's prefix and
// custom validator tags stay scoped to the provider that defines them
// (no shared global validator, no cross-package init ordering).
type Decoder struct {
	prefix   string
	validate *validator.Validate
}

// New builds a Decoder whose errors are prefixed with prefix (e.g. "aws").
func New(prefix string) *Decoder {
	return &Decoder{
		prefix:   prefix,
		validate: validator.New(validator.WithRequiredStructEnabled()),
	}
}

// MustRegister registers a custom validator tag, panicking on failure.
// A duplicate or invalid tag name is a compile-time-class bug in the
// calling package, not a runtime condition (same pattern as
// internal/spec/validate.go).
func (d *Decoder) MustRegister(tag string, fn validator.Func) {
	if err := d.validate.RegisterValidation(tag, fn); err != nil {
		panic(fmt.Sprintf("%s: failed to register validator %q: %v", d.prefix, tag, err))
	}
}

// Properties re-marshals props (coming from spec.Resource.Properties, a
// generic map[string]any) and re-decodes it in strict mode into out, then
// validates it with go-playground/validator. Same two-tier pattern already
// used in internal/spec.Parse/Validate, applied here to block Mass
// Assignment at the provider level too (RFC 002 §2.6, RFC 003 §2.2).
//
// Unknown properties are rejected rather than silently dropped: on an
// intent-driven system, a property the provider does not understand means
// the user asked for something they are not going to get, which must
// surface at Validate time rather than as missing infrastructure.
func (d *Decoder) Properties(props map[string]any, out any) error {
	if err := d.rejectCredentialLikeKeys(props); err != nil {
		return err
	}

	raw, err := json.Marshal(props)
	if err != nil {
		return fmt.Errorf("%s: failed to marshal properties: %w", d.prefix, err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: unknown or malformed property: %w", d.prefix, err)
	}

	if err := d.validate.Struct(out); err != nil {
		return fmt.Errorf("%s: property validation failed: %w", d.prefix, err)
	}
	return nil
}

func (d *Decoder) rejectCredentialLikeKeys(props map[string]any) error {
	for key := range props {
		lower := strings.ToLower(key)
		for _, forbidden := range credentialLikeFieldNames {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("%s: property key %q looks like a credential and is rejected", d.prefix, key)
			}
		}
	}
	return nil
}

// BoolOrDefault resolves a tri-state *bool: absent means def, present
// means the caller's explicit choice. Used throughout the providers so an
// omitted security-relevant property activates a secure default rather
// than the false zero-value of a plain bool (RFC 002 §2.6, RFC 011 §2.5).
func BoolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
