// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/sql"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// relationalDatabaseProperties represents the Properties of a Resource
// with Type: "relational_database" on GCP (RFC 008).
type relationalDatabaseProperties struct {
	Engine           string `json:"engine" validate:"required,oneof=postgres mysql"`
	Version          string `json:"version" validate:"required"`
	HighAvailability bool   `json:"high_availability,omitempty"`

	// DeletionProtection guards the instance against accidental deletion.
	// Defaults to true (RFC 011 §2.5); it previously shipped hardcoded
	// false "for simplified teardown".
	DeletionProtection *bool `json:"deletion_protection,omitempty"`
}

// EffectiveDeletionProtection returns DeletionProtection, or true if
// absent (RFC 011 §2.5: secure by default).
func (p relationalDatabaseProperties) EffectiveDeletionProtection() bool {
	return boolOrDefault(p.DeletionProtection, true)
}

// decodeRelationalDatabaseProperties decodes and validates Properties via
// the shared strict decoder (RFC 011 §2.1).
func decodeRelationalDatabaseProperties(props map[string]any) (*relationalDatabaseProperties, error) {
	var p relationalDatabaseProperties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	// Fail at decode time rather than provisioning the wrong engine.
	if _, err := cloudSQLDatabaseVersion(p.Engine, p.Version); err != nil {
		return nil, err
	}
	return &p, nil
}

// cloudSQLDatabaseVersion maps the cloud-agnostic (engine, version) pair
// onto a Cloud SQL DatabaseVersion enum value (e.g. POSTGRES_15,
// MYSQL_8_0).
//
// RFC 011 §1.1A2: this was previously built as fmt.Sprintf("POSTGRES_%s",
// version) regardless of engine, so a request for MySQL silently
// provisioned PostgreSQL. An unsupported engine is now a hard error — the
// one thing an intent-driven system must never do is hand back different
// infrastructure than was asked for.
func cloudSQLDatabaseVersion(engine, version string) (string, error) {
	// Cloud SQL spells versions with underscores: "8.0" -> "8_0".
	normalized := strings.ReplaceAll(version, ".", "_")

	switch strings.ToLower(engine) {
	case "postgres":
		return "POSTGRES_" + normalized, nil
	case "mysql":
		return "MYSQL_" + normalized, nil
	default:
		return "", fmt.Errorf("gcp: unsupported database engine %q (supported: postgres, mysql)", engine)
	}
}

// declareRelationalDatabase registers the Cloud SQL instance and
// returns it, so the power schedules of RFC 012 §4.2 can be declared
// against its project and name.
func declareRelationalDatabase(ctx *pulumi.Context, id, region, networkName string, p relationalDatabaseProperties) (*sql.DatabaseInstance, error) {
	pwd, err := random.NewRandomPassword(ctx, id+"-pwd", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return nil, err
	}

	availabilityType := "ZONAL"
	if p.HighAvailability {
		availabilityType = "REGIONAL"
	}

	databaseVersion, err := cloudSQLDatabaseVersion(p.Engine, p.Version)
	if err != nil {
		return nil, err
	}

	instance, err := sql.NewDatabaseInstance(ctx, id, &sql.DatabaseInstanceArgs{
		DatabaseVersion: pulumi.String(databaseVersion),
		Region:          pulumi.String(region),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			Tier:             pulumi.String("db-f1-micro"),
			AvailabilityType: pulumi.String(availabilityType),
			IpConfiguration: &sql.DatabaseInstanceSettingsIpConfigurationArgs{
				Ipv4Enabled: pulumi.Bool(false), // No public address
				// The scope's network (RFC 016 §2.3). Before this,
				// Ipv4Enabled: false was set with no PrivateNetwork,
				// which does not make the instance private so much as
				// unaddressable: Cloud SQL needs a peered VPC to have a
				// private IP at all, so the database came up with no path
				// to it of any kind.
				PrivateNetwork: pulumi.String(networkName),
			},
			BackupConfiguration: &sql.DatabaseInstanceSettingsBackupConfigurationArgs{
				Enabled: pulumi.Bool(true),
			},
			DiskAutoresize: pulumi.Bool(true),
		},
		DeletionProtection: pulumi.Bool(p.EffectiveDeletionProtection()),
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare database instance %q: %w", id, err)
	}

	if _, err := sql.NewUser(ctx, id+"-user", &sql.UserArgs{
		Instance: instance.Name,
		Name:     pulumi.String("masteruser"),
		Password: pwd.Result,
	}); err != nil {
		return nil, fmt.Errorf("gcp: failed to declare database user for %q: %w", id, err)
	}
	return instance, nil
}
