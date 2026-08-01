// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
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
func declareRelationalDatabase(ctx *pulumi.Context, id string, p relationalDatabaseProperties, net scopeNetwork, opts ...pulumi.ResourceOption) (*rds.Instance, error) {
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

	// 2a. A security group admitting the engine's port from the scope's
	// own range, and from nothing else.
	//
	// Before RFC 016 the instance had no security group of its own and
	// lived in the account's default VPC, which meant "private" was
	// "reachable by everything else in the account". Scoping ingress to
	// the VPC's CIDR is what makes an application in this environment able
	// to connect while an application in another one cannot.
	sg, err := ec2.NewSecurityGroup(ctx, id+"-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(net.vpcID),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: database access from this environment only", id)),
		Ingress: ec2.SecurityGroupIngressArray{
			ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String("tcp"),
				FromPort:   pulumi.Int(enginePort(p.Engine)),
				ToPort:     pulumi.Int(enginePort(p.Engine)),
				CidrBlocks: pulumi.StringArray{pulumi.String(net.cidr)},
			},
		},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the security group for %q: %w", id, err)
	}

	args := &rds.InstanceArgs{
		// The scope's network, not the account's default VPC (RFC 016).
		DbSubnetGroupName:   pulumi.String(net.dbSubnetName),
		VpcSecurityGroupIds: pulumi.StringArray{sg.ID()},

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

// enginePort is the port a database engine listens on.
//
// A table rather than opening the whole range: the security group admits
// exactly the port the engine uses, so a second service accidentally
// started inside the same VPC is not reachable through the database's
// rule.
func enginePort(engine string) int {
	switch engine {
	case "postgres":
		return 5432
	default:
		// mysql and mariadb both speak the MySQL protocol on 3306, and
		// the validator tag admits no other engine.
		return 3306
	}
}
