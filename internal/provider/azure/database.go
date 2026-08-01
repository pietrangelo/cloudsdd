// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/mysql"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/postgresql"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// backupRetentionDays is the automated-backup window applied to every
// managed database, matching the AWS provider's 7-day default.
const backupRetentionDays = 7

// relationalDatabaseProperties represents the Properties of a Resource
// with Type: "relational_database" on Azure (RFC 008).
type relationalDatabaseProperties struct {
	Engine           string `json:"engine" validate:"required,oneof=postgres mysql"`
	Version          string `json:"version" validate:"required"`
	HighAvailability bool   `json:"high_availability,omitempty"`
}

// decodeRelationalDatabaseProperties decodes and validates Properties via
// the shared strict decoder (RFC 011 §2.1).
func decodeRelationalDatabaseProperties(props map[string]any) (*relationalDatabaseProperties, error) {
	var p relationalDatabaseProperties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// declareRelationalDatabase provisions the Flexible Server matching the
// requested engine.
//
// RFC 011 §1.1A3: this previously always called
// postgresql.NewFlexibleServer regardless of Engine, and decoded
// HighAvailability without ever reading it — so a request for an HA MySQL
// instance produced a single-node PostgreSQL one.
func declareRelationalDatabase(ctx *pulumi.Context, id, location string, p relationalDatabaseProperties) error {
	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		Location: pulumi.String(location),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare resource group for %q: %w", id, err)
	}

	pwd, err := random.NewRandomPassword(ctx, id+"-pwd", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return err
	}

	switch strings.ToLower(p.Engine) {
	case "postgres":
		return declarePostgresFlexibleServer(ctx, id, rg, pwd, p)
	case "mysql":
		return declareMySQLFlexibleServer(ctx, id, rg, pwd, p)
	default:
		// Unreachable via decode (the oneof tag rejects it first); kept
		// so a future engine added to the tag cannot silently fall
		// through to the wrong server type.
		return fmt.Errorf("azure: unsupported database engine %q (supported: postgres, mysql)", p.Engine)
	}
}

func declarePostgresFlexibleServer(ctx *pulumi.Context, id string, rg *core.ResourceGroup, pwd *random.RandomPassword, p relationalDatabaseProperties) error {
	args := &postgresql.FlexibleServerArgs{
		ResourceGroupName:     rg.Name,
		Location:              rg.Location,
		Version:               pulumi.String(p.Version),
		AdministratorLogin:    pulumi.String("masteruser"),
		AdministratorPassword: pwd.Result,
		SkuName:               pulumi.String("B_Standard_B1ms"),
		StorageMb:             pulumi.Int(32768),
		BackupRetentionDays:   pulumi.Int(backupRetentionDays),
		// The previous comment here claimed Flexible Server was "private
		// by default without firewall rules". It is not: the default is
		// public network access enabled, with connections merely blocked
		// by the absence of firewall rules. Setting this establishes the
		// posture the comment asserted (RFC 011 §1.1E).
		PublicNetworkAccessEnabled: pulumi.Bool(false),
	}

	if p.HighAvailability {
		args.HighAvailability = &postgresql.FlexibleServerHighAvailabilityArgs{
			Mode: pulumi.String("ZoneRedundant"),
		}
		args.GeoRedundantBackupEnabled = pulumi.Bool(true)
	}

	if _, err := postgresql.NewFlexibleServer(ctx, id+"-server", args); err != nil {
		return fmt.Errorf("azure: failed to declare postgresql server %q: %w", id, err)
	}
	return nil
}

func declareMySQLFlexibleServer(ctx *pulumi.Context, id string, rg *core.ResourceGroup, pwd *random.RandomPassword, p relationalDatabaseProperties) error {
	args := &mysql.FlexibleServerArgs{
		ResourceGroupName:     rg.Name,
		Location:              rg.Location,
		Version:               pulumi.String(p.Version),
		AdministratorLogin:    pulumi.String("masteruser"),
		AdministratorPassword: pwd.Result,
		SkuName:               pulumi.String("B_Standard_B1ms"),
		BackupRetentionDays:   pulumi.Int(backupRetentionDays),
		Storage: &mysql.FlexibleServerStorageArgs{
			SizeGb:          pulumi.Int(32),
			AutoGrowEnabled: pulumi.Bool(true),
		},
	}

	if p.HighAvailability {
		args.HighAvailability = &mysql.FlexibleServerHighAvailabilityArgs{
			Mode: pulumi.String("ZoneRedundant"),
		}
		args.GeoRedundantBackupEnabled = pulumi.Bool(true)
	}

	// Note: unlike its PostgreSQL counterpart, the MySQL Flexible Server
	// resource in pulumi-azure v5 exposes no PublicNetworkAccessEnabled
	// field; network isolation is achieved via DelegatedSubnetId, which
	// requires a VNet this RFC does not yet model. Tracked as a follow-up
	// rather than silently claimed.
	if _, err := mysql.NewFlexibleServer(ctx, id+"-server", args); err != nil {
		return fmt.Errorf("azure: failed to declare mysql server %q: %w", id, err)
	}
	return nil
}
