// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/network"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/privatedns"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/privatelink"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// The Azure Files posture RFC 020 §2.6 specifies.
const (
	// SMB 3.1.1 with AES-256-GCM is the only pairing that encrypts on the
	// wire. Each is the sole entry in its list: an older dialect allowed
	// beside it is one a client can negotiate down to.
	fileSMBVersion    = "SMB3.1.1"
	fileSMBEncryption = "AES-256-GCM"

	fileMinTLSVersion = "TLS1_2"

	// fileSoftDeleteDays is the only undo Azure Files has for a scope
	// destroyed with its data in it (RFC 020 §2.9).
	fileSoftDeleteDays = 7

	// defaultFileShareQuotaGB is the share quota when size_gb is absent
	// (RFC 020 §2.7).
	defaultFileShareQuotaGB = 100

	// filePrivateDNSZone is the private DNS zone Azure resolves an account's
	// file endpoint through once it has a private endpoint. Not ours to
	// choose.
	filePrivateDNSZone = "privatelink.file.core.windows.net"
)

// storageAccountNamePrefix plus 16 hex digits is exactly the 24
// characters Azure allows.
const storageAccountNamePrefix = "cloudsdd"

// storageAccountNameFor is the storage account holding a scope's shares.
//
// An account name is drawn from a namespace shared with every Azure
// customer, so the subscription is part of the derivation, as it is for
// ACR (RFC 018 §2.8.1). The NUL separator keeps a subscription ending
// where a scope begins from hashing as another pair, and 64 bits of digest
// suit a namespace that large better than ShortHash's 32.
func storageAccountNameFor(subscriptionID string, s provider.NetworkScope) string {
	sum := sha256.Sum256([]byte(subscriptionID + "\x00" + provider.ScopeName(s)))
	return storageAccountNamePrefix + hex.EncodeToString(sum[:8])
}

// fileShareNameFor is the share holding one volume, within the scope's
// account.
//
// A volume name admits uppercase and underscores and a share name does
// not, so the readable part is lossy. The hash of the unfolded name is
// always appended, which keeps "Cache" and "cache" two shares and leaves
// the name starting and ending with a letter or digit even when the fold
// leaves nothing.
func fileShareNameFor(volume string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(volume) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case !strings.HasSuffix(b.String(), "-"):
			// Azure rejects consecutive hyphens, so a run of separators
			// folds to one.
			b.WriteByte('-')
		}
	}

	readable := strings.Trim(b.String(), "-")
	if readable == "" {
		return provider.ShortHash(volume)
	}
	return readable + "-" + provider.ShortHash(volume)
}

// fileShareQuotaGB applies size_gb as the share quota, defaulting when
// absent (RFC 020 §2.7).
func fileShareQuotaGB(v spec.Volume) int {
	if v.SizeGB == 0 {
		return defaultFileShareQuotaGB
	}
	return v.SizeGB
}

// declareScopeFilesystems declares the scope's storage account, one share
// per volume, and the private endpoint that is the only way in (RFC 020
// §2.6).
//
// A scope whose resources mount nothing declares nothing, and does not
// even ask for the subscription.
func declareScopeFilesystems(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	vnet *network.VirtualNetwork,
	subnet *network.Subnet,
	volumes []spec.Volume,
) error {
	if len(volumes) == 0 {
		return nil
	}

	config, err := core.GetClientConfig(ctx)
	if err != nil {
		return fmt.Errorf("azure: failed to resolve the subscription for the filesystems of scope %q: %w",
			provider.ScopeName(s), err)
	}

	account, err := declareScopeStorageAccount(ctx, s, rg, config.SubscriptionId)
	if err != nil {
		return err
	}

	for _, v := range volumes {
		share := fileShareNameFor(v.Name)
		if _, err := storage.NewShare(ctx, scopeNetworkName(s)+"-share-"+share, &storage.ShareArgs{
			Name:               pulumi.String(share),
			StorageAccountName: account.Name,
			Quota:              pulumi.Int(fileShareQuotaGB(v)),
		}); err != nil {
			return fmt.Errorf("azure: failed to declare the file share for volume %q in scope %q: %w",
				v.Name, provider.ScopeName(s), err)
		}
	}

	return declareFilePrivateEndpoint(ctx, s, rg, vnet, subnet, account)
}

// declareScopeStorageAccount is the account every share of the scope sits
// in. Every hardening value is stated, because Azure's default for each is
// the more open one.
func declareScopeStorageAccount(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	subscriptionID string,
) (*storage.Account, error) {
	account, err := storage.NewAccount(ctx, scopeNetworkName(s)+"-files", &storage.AccountArgs{
		Name:                   pulumi.String(storageAccountNameFor(subscriptionID, s)),
		ResourceGroupName:      rg.Name,
		Location:               rg.Location,
		AccountTier:            pulumi.String("Standard"),
		AccountReplicationType: pulumi.String("LRS"),
		// No path from the internet: the private endpoint is the only way
		// in, and the account key alone is not a perimeter.
		PublicNetworkAccessEnabled: pulumi.Bool(false),
		MinTlsVersion:              pulumi.String(fileMinTLSVersion),
		ShareProperties: &storage.AccountSharePropertiesArgs{
			Smb: &storage.AccountSharePropertiesSmbArgs{
				Versions:               pulumi.StringArray{pulumi.String(fileSMBVersion)},
				ChannelEncryptionTypes: pulumi.StringArray{pulumi.String(fileSMBEncryption)},
			},
			RetentionPolicy: &storage.AccountSharePropertiesRetentionPolicyArgs{
				Days: pulumi.Int(fileSoftDeleteDays),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the storage account for scope %q: %w",
			provider.ScopeName(s), err)
	}
	return account, nil
}

// declareFilePrivateEndpoint places the account's file endpoint in the
// scope's general subnet and makes its hostname resolve there from inside
// the VNet.
//
// The general subnet, because every other subnet in the scope is delegated
// to one service and a delegated subnet refuses a private endpoint.
func declareFilePrivateEndpoint(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	vnet *network.VirtualNetwork,
	subnet *network.Subnet,
	account *storage.Account,
) error {
	name := scopeNetworkName(s)

	zone, err := privatedns.NewZone(ctx, name+"-file-dns", &privatedns.ZoneArgs{
		Name:              pulumi.String(filePrivateDNSZone),
		ResourceGroupName: rg.Name,
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the file dns zone for scope %q: %w", provider.ScopeName(s), err)
	}

	if _, err := privatedns.NewZoneVirtualNetworkLink(ctx, name+"-file-dns-link",
		&privatedns.ZoneVirtualNetworkLinkArgs{
			Name:                pulumi.String("file-link"),
			PrivateDnsZoneName:  zone.Name,
			ResourceGroupName:   rg.Name,
			VirtualNetworkId:    vnet.ID(),
			RegistrationEnabled: pulumi.Bool(false),
		}); err != nil {
		return fmt.Errorf("azure: failed to link the file dns zone for scope %q: %w", provider.ScopeName(s), err)
	}

	if _, err := privatelink.NewEndpoint(ctx, name+"-file-pe", &privatelink.EndpointArgs{
		Name:              pulumi.String(name + "-file-pe"),
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		SubnetId:          subnet.ID(),
		PrivateServiceConnection: &privatelink.EndpointPrivateServiceConnectionArgs{
			Name:                        pulumi.String(name + "-file"),
			PrivateConnectionResourceId: account.ID(),
			SubresourceNames:            pulumi.StringArray{pulumi.String("file")},
			// A manual connection waits for approval in the portal, and
			// until then the share is unreachable from the scope too.
			IsManualConnection: pulumi.Bool(false),
		},
		PrivateDnsZoneGroup: &privatelink.EndpointPrivateDnsZoneGroupArgs{
			Name:              pulumi.String("file"),
			PrivateDnsZoneIds: pulumi.StringArray{zone.ID()},
		},
	}); err != nil {
		return fmt.Errorf("azure: failed to declare the file private endpoint for scope %q: %w",
			provider.ScopeName(s), err)
	}
	return nil
}

// scopeShares is what a Container Apps environment needs to mount a
// scope's shares: the account, the key its storage links authenticate
// with, and the share behind each volume. The zero value mounts nothing.
type scopeShares struct {
	account string
	key     string
	mounts  []mountedShare
}

// mountedShare is a volume paired with the share the scope's network
// stack built for it.
type mountedShare struct {
	volume spec.Volume
	share  string
}

// lookupMountedShares finds the scope account and the share behind each
// of a resource's volumes (RFC 020 §2.8).
//
// It is the mirror of declareScopeFilesystems: the network stack created
// these, this stack only finds them, and the two meet at the names
// storageAccountNameFor and fileShareNameFor derive for both. The account
// is looked up once, because its key is one credential for the scope.
func lookupMountedShares(ctx *pulumi.Context, s provider.NetworkScope, volumes []spec.Volume) (scopeShares, error) {
	if len(volumes) == 0 {
		return scopeShares{}, nil
	}

	config, err := core.GetClientConfig(ctx)
	if err != nil {
		return scopeShares{}, fmt.Errorf("azure: failed to resolve the subscription for the volumes of scope %q: %w",
			provider.ScopeName(s), err)
	}

	name := storageAccountNameFor(config.SubscriptionId, s)
	rg := scopeResourceGroupName(s)
	account, err := storage.LookupAccount(ctx, &storage.LookupAccountArgs{Name: name, ResourceGroupName: &rg})
	if err != nil {
		return scopeShares{}, fmt.Errorf(
			"%w: scope %q has no storage account named %q. The scope's network is provisioned "+
				"before the resources in it, so this means it was removed out of band: %w",
			ErrFilesystemMissing, provider.ScopeName(s), name, err)
	}

	mounts := make([]mountedShare, 0, len(volumes))
	for _, v := range volumes {
		share := fileShareNameFor(v.Name)
		if _, err := storage.LookupShare(ctx, &storage.LookupShareArgs{Name: share, StorageAccountName: name}); err != nil {
			return scopeShares{}, fmt.Errorf(
				"%w: volume %q in scope %q has no share named %q in account %q. The scope's network "+
					"is provisioned before the resources in it, so this means it was removed out of band: %w",
				ErrFilesystemMissing, v.Name, provider.ScopeName(s), share, name, err)
		}
		mounts = append(mounts, mountedShare{volume: v, share: share})
	}
	return scopeShares{account: name, key: account.PrimaryAccessKey, mounts: mounts}, nil
}
