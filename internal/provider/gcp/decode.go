// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package gcp implements provider.CloudProvider for Google Cloud (RFC
// 008), via the Pulumi Automation API.
package gcp

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"

	"cloudsdd/internal/provider/decode"
)

// gcpRegionPattern mirrors the format of a GCP region (e.g. "us-east1",
// "europe-west4", "asia-northeast1").
var gcpRegionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+\d+$`)

// gcsBucketNamePattern applies a conservative subset of GCS naming rules
// (3-63 characters, lowercase/digits/hyphens/underscores, does not
// start/end with a separator): no dots, to avoid the domain-named-bucket
// verification rules.
var gcsBucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,61}[a-z0-9]$`)

// dec is the GCP provider's strict property decoder (RFC 011 §2.1). Before
// that RFC this package hand-rolled a permissive json.Unmarshal that
// ignored its own errors and never ran the validator, so the
// `validate:"required"` tags on its property structs were decorative.
var dec = newDecoder()

func newDecoder() *decode.Decoder {
	d := decode.New("gcp")
	d.MustRegister("gcpregion", func(fl validator.FieldLevel) bool {
		return gcpRegionPattern.MatchString(fl.Field().String())
	})
	d.MustRegister("gcsbucketname", func(fl validator.FieldLevel) bool {
		return gcsBucketNamePattern.MatchString(fl.Field().String())
	})
	return d
}

// validateGCPRegionFormat checks that region matches the GCP region
// format. Used directly rather than via a struct tag because Region lives
// on the cloud-agnostic spec.Scope (RFC 005 §2.2).
func validateGCPRegionFormat(region string) error {
	if !gcpRegionPattern.MatchString(region) {
		return fmt.Errorf("gcp: %q is not a valid GCP region", region)
	}
	return nil
}

func boolOrDefault(p *bool, def bool) bool { return decode.BoolOrDefault(p, def) }
