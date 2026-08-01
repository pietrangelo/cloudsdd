// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/management"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/mysql"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/postgresql"
	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// backupRetentionDays is the automated-backup window applied to every
// managed database, matching the AWS provider's 7-day default.
const backupRetentionDays = 7

// lockLevelCanNotDelete permits reads and modifications but forbids
// deletion. See declareDeletionLock for why the stronger ReadOnly level
// is not used.
const lockLevelCanNotDelete = "CanNotDelete"

// relationalDatabaseProperties represents the Properties of a Resource
// with Type: "relational_database" on Azure (RFC 008).
type relationalDatabaseProperties struct {
	Engine           string `json:"engine" validate:"required,oneof=postgres mysql"`
	Version          string `json:"version" validate:"required"`
	HighAvailability bool   `json:"high_availability,omitempty"`

	// DeletionProtection guards the server against accidental deletion.
	// Defaults to true, matching AWS and GCP (RFC 015 §2.1). It arrived
	// later than on the other two providers, which is the defect RFC 015
	// records: the property was documented as universal while Azure
	// rejected it as unknown.
	DeletionProtection *bool `json:"deletion_protection,omitempty"`
}

// EffectiveDeletionProtection returns DeletionProtection, or true if
// absent (secure by default).
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
	return &p, nil
}

// declareRelationalDatabase provisions the Flexible Server matching the
// requested engine.
//
// RFC 011 §1.1A3: this previously always called
// postgresql.NewFlexibleServer regardless of Engine, and decoded
// HighAvailability without ever reading it — so a request for an HA MySQL
// instance produced a single-node PostgreSQL one.
func declareRelationalDatabase(ctx *pulumi.Context, id, location string, p relationalDatabaseProperties) (databaseServer, error) {
	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		Location: pulumi.String(location),
	})
	if err != nil {
		return databaseServer{}, fmt.Errorf("azure: failed to declare resource group for %q: %w", id, err)
	}

	pwd, err := random.NewRandomPassword(ctx, id+"-pwd", &random.RandomPasswordArgs{
		Length:  pulumi.Int(32),
		Special: pulumi.Bool(false),
	})
	if err != nil {
		return databaseServer{}, err
	}

	// Both engines are VNet-integrated, so the network is built once here
	// rather than inside each declare function (RFC 015 §2.2). Only the
	// delegation name and the DNS suffix differ, and those come from a
	// lookup rather than a branch.
	net, err := declareDatabaseNetwork(ctx, id, rg, p)
	if err != nil {
		return databaseServer{}, err
	}

	var (
		server  databaseServer
		declErr error
	)
	switch strings.ToLower(p.Engine) {
	case "postgres":
		server, declErr = declarePostgresFlexibleServer(ctx, id, rg, pwd, net, p)
	case "mysql":
		server, declErr = declareMySQLFlexibleServer(ctx, id, rg, pwd, net, p)
	default:
		// Unreachable via decode (the oneof tag rejects it first); kept
		// so a future engine added to the tag cannot silently fall
		// through to the wrong server type.
		return databaseServer{}, fmt.Errorf("azure: unsupported database engine %q (supported: postgres, mysql)", p.Engine)
	}
	if declErr != nil {
		return databaseServer{}, declErr
	}

	if err := declareDeletionLock(ctx, id, server, p); err != nil {
		return databaseServer{}, err
	}
	return server, nil
}

// serverOptions returns the resource options every Flexible Server
// carries.
//
// pulumi.Protect is half of deletion protection (RFC 015 §2.1) and it is
// the half that stops *CloudSDD itself*: Pulumi refuses the delete at
// plan time, so `cloudsdd destroy` fails atomically rather than part-way
// through. The Azure-side management lock cannot do this, because it is a
// resource in this same stack and a destroy would remove it first.
func serverOptions(p relationalDatabaseProperties) []pulumi.ResourceOption {
	return []pulumi.ResourceOption{pulumi.Protect(p.EffectiveDeletionProtection())}
}

// declareDeletionLock is the other half: a management lock that blocks
// deletion through the portal, the CLI, or anything else that never
// consults CloudSDD's state file (RFC 015 §2.1).
//
// The level is CanNotDelete and never ReadOnly. ReadOnly is the stronger
// lock and that is exactly why it is wrong: RFC 012 provisions a runbook
// whose entire job is to stop and start this server on a schedule, and a
// ReadOnly lock would turn a power schedule into a stream of failed jobs
// whose only symptom is an unchanged bill.
//
// Nothing is declared when protection is off, rather than a lock at some
// weaker level: a lock left behind would make the documented escape hatch
// a lie.
func declareDeletionLock(ctx *pulumi.Context, id string, server databaseServer, p relationalDatabaseProperties) error {
	if !p.EffectiveDeletionProtection() {
		return nil
	}

	if _, err := management.NewLock(ctx, id+"-lock", &management.LockArgs{
		Scope:     server.id,
		LockLevel: pulumi.String(lockLevelCanNotDelete),
		Notes:     pulumi.String("CloudSDD deletion_protection"),
	}); err != nil {
		// Creating a lock needs Microsoft.Authorization/locks/write, which
		// Owner and User Access Administrator hold and Contributor does
		// not. Failing here is deliberate: the alternative is provisioning
		// a database the user was told is protected and is not.
		return fmt.Errorf("azure: failed to declare the deletion lock for %q "+
			"(creating a management lock requires Microsoft.Authorization/locks/write, "+
			"which the Contributor role does not grant; set deletion_protection: false "+
			"to deploy without one): %w", id, err)
	}
	return nil
}

func declarePostgresFlexibleServer(ctx *pulumi.Context, id string, rg *core.ResourceGroup, pwd *random.RandomPassword, net databaseNetwork, p relationalDatabaseProperties) (databaseServer, error) {
	args := &postgresql.FlexibleServerArgs{
		ResourceGroupName:     rg.Name,
		Location:              rg.Location,
		Version:               pulumi.String(p.Version),
		AdministratorLogin:    pulumi.String("masteruser"),
		AdministratorPassword: pwd.Result,
		SkuName:               pulumi.String("B_Standard_B1ms"),
		StorageMb:             pulumi.Int(32768),
		BackupRetentionDays:   pulumi.Int(backupRetentionDays),
		// RFC 011 §1.1E set PublicNetworkAccessEnabled: false here, after
		// finding a comment claiming Flexible Server was "private by
		// default without firewall rules" when the default is a public
		// endpoint. That was correct but left the server with no network
		// path at all. RFC 015 §2.2 replaces it with VNet integration,
		// which is both private and reachable from its own network — and
		// is the same mechanism MySQL is now obliged to use, so the two
		// engines stop differing.
		DelegatedSubnetId: net.subnetID,
		PrivateDnsZoneId:  net.privateDNSID,
	}

	if p.HighAvailability {
		args.HighAvailability = &postgresql.FlexibleServerHighAvailabilityArgs{
			Mode: pulumi.String("ZoneRedundant"),
		}
		args.GeoRedundantBackupEnabled = pulumi.Bool(true)
	}

	server, err := postgresql.NewFlexibleServer(ctx, id+"-server", args, serverOptions(p)...)
	if err != nil {
		return databaseServer{}, fmt.Errorf("azure: failed to declare postgresql server %q: %w", id, err)
	}
	return databaseServer{engine: "postgres", id: server.ID(), resourceGroup: rg}, nil
}

func declareMySQLFlexibleServer(ctx *pulumi.Context, id string, rg *core.ResourceGroup, pwd *random.RandomPassword, net databaseNetwork, p relationalDatabaseProperties) (databaseServer, error) {
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
		// MySQL Flexible Server exposes no PublicNetworkAccessEnabled at
		// all, so until RFC 015 this server had a public endpoint that
		// nothing but the absence of a firewall rule kept closed — one
		// portal click from open, while docs/cli.md claimed the resource
		// was not publicly reachable. VNet integration removes the public
		// endpoint rather than leaving it unfirewalled.
		DelegatedSubnetId: net.subnetID,
		PrivateDnsZoneId:  net.privateDNSID,
	}

	if p.HighAvailability {
		args.HighAvailability = &mysql.FlexibleServerHighAvailabilityArgs{
			Mode: pulumi.String("ZoneRedundant"),
		}
		args.GeoRedundantBackupEnabled = pulumi.Bool(true)
	}

	server, err := mysql.NewFlexibleServer(ctx, id+"-server", args, serverOptions(p)...)
	if err != nil {
		return databaseServer{}, fmt.Errorf("azure: failed to declare mysql server %q: %w", id, err)
	}
	return databaseServer{engine: "mysql", id: server.ID(), resourceGroup: rg}, nil
}
