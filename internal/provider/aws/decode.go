// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package aws implements provider.CloudProvider for AWS (RFC 002), via the
// Pulumi Automation API, as a concrete "backend" for the SDD Specification.
package aws

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"

	"cloudsdd/internal/provider/decode"
)

// awsRegionPattern mirrors the format of an AWS region (e.g.
// "eu-central-1"), used by the custom "awsregion" validator.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d$`)

// s3BucketNamePattern applies a conservative subset of S3 naming rules
// (3-63 characters, lowercase/digits/hyphens, does not start/end with a
// hyphen): no dots, to avoid the well-known virtual-hosted-style + TLS
// complications with dots in the bucket name.
var s3BucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// dec is the AWS provider's strict property decoder (RFC 011 §2.1). The
// generic machinery — DisallowUnknownFields, validator, credential-key
// rejection — now lives in internal/provider/decode so every provider
// shares it; this package only contributes its AWS-specific tags.
var dec = newDecoder()

func newDecoder() *decode.Decoder {
	d := decode.New("aws")
	d.MustRegister("awsregion", func(fl validator.FieldLevel) bool {
		return awsRegionPattern.MatchString(fl.Field().String())
	})
	d.MustRegister("s3bucketname", func(fl validator.FieldLevel) bool {
		return s3BucketNamePattern.MatchString(fl.Field().String())
	})
	return d
}

// decodeProperties decodes and validates props into out in strict mode.
// Thin wrapper over decode.Decoder.Properties, kept so the existing AWS
// call sites and error prefixes are unchanged.
func decodeProperties(props map[string]any, out any) error {
	return dec.Properties(props, out)
}

// validateAWSRegionFormat checks that region matches the AWS region
// format (e.g. "eu-central-1"). Used directly (not via a struct tag)
// because Region now lives on spec.Scope (RFC 005 §2.2), a cloud-agnostic
// struct that cannot carry an AWS-specific validator tag.
func validateAWSRegionFormat(region string) error {
	if !awsRegionPattern.MatchString(region) {
		return fmt.Errorf("aws: %q is not a valid AWS region", region)
	}
	return nil
}

// boolOrDefault resolves a tri-state *bool (see decode.BoolOrDefault).
func boolOrDefault(p *bool, def bool) bool { return decode.BoolOrDefault(p, def) }
