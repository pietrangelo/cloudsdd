// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// relationalDatabaseProperties represents the Properties of a Resource
// with Type: "relational_database" on AWS (RFC 007).
//
// DeletionProtection and SkipFinalSnapshot are *bool, not bool: their
// absence activates a secure default (Effective*, below) rather than the
// zero-value of a plain bool, which for DeletionProtection would silently
// mean "unprotected" (RFC 011 §2.5). Same tri-state pattern as
// S3Properties.
type relationalDatabaseProperties struct {
	Engine           string `json:"engine" validate:"required,oneof=postgres mysql mariadb"`
	Version          string `json:"version" validate:"required"`
	HighAvailability bool   `json:"high_availability,omitempty"`

	// DeletionProtection guards the instance against accidental deletion.
	// Defaults to true: RFC 011 §1.1E found this shipped hardcoded-off
	// "for simplified teardown", which turns a mistranslated destroy
	// prompt into unrecoverable data loss.
	DeletionProtection *bool `json:"deletion_protection,omitempty"`

	// SkipFinalSnapshot, when true, discards the database at destroy time
	// without taking a final snapshot. Defaults to false.
	SkipFinalSnapshot *bool `json:"skip_final_snapshot,omitempty"`
}

// EffectiveDeletionProtection returns DeletionProtection, or true if
// absent (RFC 011 §2.5: secure by default).
func (p relationalDatabaseProperties) EffectiveDeletionProtection() bool {
	return boolOrDefault(p.DeletionProtection, true)
}

// EffectiveSkipFinalSnapshot returns SkipFinalSnapshot, or false if absent
// (RFC 011 §2.5: a final snapshot is taken unless explicitly waived).
func (p relationalDatabaseProperties) EffectiveSkipFinalSnapshot() bool {
	return boolOrDefault(p.SkipFinalSnapshot, false)
}

// decodeRelationalDatabaseProperties decodes and validates Properties as
// relationalDatabaseProperties. It previously hand-rolled a permissive
// json.Unmarshal, bypassing the Mass Assignment defense every other AWS
// resource type applies (RFC 011 §1.1C); it now goes through the shared
// strict decoder like the rest.
func decodeRelationalDatabaseProperties(props map[string]any) (*relationalDatabaseProperties, error) {
	var p relationalDatabaseProperties
	if err := decodeProperties(props, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// declareRelationalDatabase registers the RDS instance and returns it, so
// that resources referring to it — the power schedules of RFC 012 §4.1 —
// can be declared against its ARN and identifier.
func declareRelationalDatabase(ctx *pulumi.Context, id string, p relationalDatabaseProperties, opts ...pulumi.ResourceOption) (*rds.Instance, error) {
	// 1. Generate a secure random password for the master user.
	pwd, err := random.NewRandomPassword(ctx, id+"-pwd", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	}, opts...)
	if err != nil {
		return nil, err
	}

	// 2. Provision the RDS instance.
	instanceClass := "db.t3.micro"
	if p.HighAvailability {
		instanceClass = "db.t3.small" // HA generally requires non-micro
	}

	args := &rds.InstanceArgs{
		Engine:                           pulumi.String(p.Engine),
		EngineVersion:                    pulumi.String(p.Version),
		InstanceClass:                    pulumi.String(instanceClass),
		AllocatedStorage:                 pulumi.Int(20),
		MaxAllocatedStorage:              pulumi.Int(100), // Auto-scaling
		Username:                         pulumi.String("masteruser"),
		Password:                         pwd.Result,
		MultiAz:                          pulumi.Bool(p.HighAvailability),
		PubliclyAccessible:               pulumi.Bool(false), // Private by default
		StorageEncrypted:                 pulumi.Bool(true),  // Encryption at rest
		IamDatabaseAuthenticationEnabled: pulumi.Bool(true),  // State-of-the-art IAM auth
		BackupRetentionPeriod:            pulumi.Int(7),      // Automated backups
		ApplyImmediately:                 pulumi.Bool(true),
		DeletionProtection:               pulumi.Bool(p.EffectiveDeletionProtection()),
		SkipFinalSnapshot:                pulumi.Bool(p.EffectiveSkipFinalSnapshot()),
	}

	// RDS requires a snapshot identifier whenever a final snapshot is to
	// be taken, which is now the default.
	if !p.EffectiveSkipFinalSnapshot() {
		args.FinalSnapshotIdentifier = pulumi.String(id + "-final-snapshot")
	}

	return rds.NewInstance(ctx, id, args, opts...)
}
