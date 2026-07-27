// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package aws implements provider.CloudProvider for AWS (RFC 002), via the
// Pulumi Automation API, as a concrete "backend" for the SDD Specification.
package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"
)

var validate = newValidator()

// awsRegionPattern mirrors the format of an AWS region (e.g.
// "eu-central-1"), used by the custom "awsregion" validator.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d$`)

// s3BucketNamePattern applies a conservative subset of S3 naming rules
// (3-63 characters, lowercase/digits/hyphens, does not start/end with a
// hyphen): no dots, to avoid the well-known virtual-hosted-style + TLS
// complications with dots in the bucket name.
var s3BucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	mustRegister(v, "awsregion", func(fl validator.FieldLevel) bool {
		return awsRegionPattern.MatchString(fl.Field().String())
	})
	mustRegister(v, "s3bucketname", func(fl validator.FieldLevel) bool {
		return s3BucketNamePattern.MatchString(fl.Field().String())
	})
	return v
}

func mustRegister(v *validator.Validate, tag string, fn validator.Func) {
	if err := v.RegisterValidation(tag, fn); err != nil {
		// This error can only happen for a duplicate/invalid tag name at
		// compile time: a bug in the code, not a runtime condition (same
		// pattern as internal/spec/validate.go).
		panic(fmt.Sprintf("aws: failed to register validator %q: %v", tag, err))
	}
}

// credentialLikeFieldNames are key-name substrings that, if present in
// Properties, are rejected upfront as defense in depth against credentials
// being embedded in the Specification (RFC 002 §2.4): even if such a field
// were not part of any known struct (and would therefore already be
// rejected by DisallowUnknownFields), the explicit check makes the intent
// non-negotiable and independent of any single resource's schema.
var credentialLikeFieldNames = []string{
	"access_key", "secret_key", "secret", "password", "token", "session_token", "private_key",
}

// decodeProperties re-marshals props (coming from spec.Resource.Properties,
// a generic map[string]any) and re-decodes it in strict mode into out,
// then validates it with go-playground/validator. Same two-tier pattern
// already used in internal/spec.Parse/Validate, applied here to block
// Mass Assignment at the provider level too (RFC 002 §2.6, RFC 003 §2.2).
func decodeProperties(props map[string]any, out any) error {
	if err := rejectCredentialLikeKeys(props); err != nil {
		return err
	}

	raw, err := json.Marshal(props)
	if err != nil {
		return fmt.Errorf("aws: failed to marshal properties: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("aws: unknown or malformed property: %w", err)
	}

	if err := validate.Struct(out); err != nil {
		return fmt.Errorf("aws: property validation failed: %w", err)
	}
	return nil
}

func rejectCredentialLikeKeys(props map[string]any) error {
	for key := range props {
		lower := strings.ToLower(key)
		for _, forbidden := range credentialLikeFieldNames {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("aws: property key %q looks like a credential and is rejected", key)
			}
		}
	}
	return nil
}

func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
