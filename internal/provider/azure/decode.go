// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package azure implements provider.CloudProvider for Microsoft Azure (RFC
// 008), via the Pulumi Automation API.
package azure

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"

	"cloudsdd/internal/provider/decode"
)

// azureLocationPattern mirrors the format of an Azure location as the ARM
// API spells it: lowercase, no separators (e.g. "westeurope", "eastus2").
var azureLocationPattern = regexp.MustCompile(`^[a-z][a-z0-9]{2,}$`)

// azureStorageAccountPattern encodes Azure's storage account naming rule,
// which is stricter than S3's or GCS's: 3-24 characters, lowercase letters
// and digits only — no hyphens, underscores, or dots.
var azureStorageAccountPattern = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

// dec is the Azure provider's strict property decoder (RFC 011 §2.1).
// Before that RFC this package hand-rolled a permissive json.Unmarshal
// whose errors were discarded at the call site, which is what made the
// Destroy path panic (RFC 011 §1.1B1).
var dec = newDecoder()

func newDecoder() *decode.Decoder {
	d := decode.New("azure")
	d.MustRegister("azurelocation", func(fl validator.FieldLevel) bool {
		return azureLocationPattern.MatchString(fl.Field().String())
	})
	d.MustRegister("azurestorageaccount", func(fl validator.FieldLevel) bool {
		return azureStorageAccountPattern.MatchString(fl.Field().String())
	})
	return d
}

// validateAzureLocationFormat checks that location matches the Azure
// location format. Used directly rather than via a struct tag because
// Region lives on the cloud-agnostic spec.Scope (RFC 005 §2.2).
func validateAzureLocationFormat(location string) error {
	if !azureLocationPattern.MatchString(location) {
		return fmt.Errorf("azure: %q is not a valid Azure location", location)
	}
	return nil
}

func boolOrDefault(p *bool, def bool) bool { return decode.BoolOrDefault(p, def) }
