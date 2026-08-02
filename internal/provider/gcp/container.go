// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/cloudrun"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/cloudrunv2"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
)

// Cloud Run ingress settings (RFC 017 §2.3).
const (
	// ingressAll exposes the service's built-in HTTPS endpoint. There is
	// no plain-HTTP listener to disable: Cloud Run serves the endpoint over
	// TLS with a Google-managed certificate and redirects HTTP, so "public
	// means HTTPS" needs no configuration to be true.
	ingressAll = "INGRESS_TRAFFIC_ALL"

	// ingressInternal restricts the service to traffic originating inside
	// the VPC, which is what makes an internal API expressible.
	ingressInternal = "INGRESS_TRAFFIC_INTERNAL_ONLY"
)

// vpcEgressAll routes every outbound connection through the scope's
// network, and therefore through the Cloud NAT RFC 017 §2.7 provisions.
//
// PRIVATE_RANGES_ONLY would be enough to reach a database, and would let
// internet-bound traffic leave from Google's shared egress pool instead.
// That is the same "a workload reaches the internet unnoticed" trade §2.7
// refused on the other two clouds: NAT'ing everything means the scope
// leaves from one address an account-level control can see and constrain.
const vpcEgressAll = "ALL_TRAFFIC"

// invokerRole is the one role granted on a public service, and it is
// granted to `allUsers` rather than to the service's own identity: it
// permits calling the service, nothing about what the service may do.
const (
	invokerRole = "roles/run.invoker"
	allUsers    = "allUsers"
)

// certificateModeAutomatic provisions and renews a Google-managed
// certificate for a mapped domain. The alternative, NONE, maps the domain
// and serves no certificate for it — the plain-HTTP outcome RFC 017 §2.3
// refuses.
const certificateModeAutomatic = "AUTOMATIC"

// runResources maps a cloud-agnostic size onto Cloud Run CPU and memory
// limits.
//
// The pairs are not arbitrary. Cloud Run refuses a container asking for
// more CPU than its memory supports — 4 vCPU needs at least 2Gi — so a
// table that looked tidier would produce a service that fails on apply
// after the user approved the plan.
func runResources(size container.Size) (cpu, memory string, err error) {
	switch size {
	case container.SizeSmall:
		return "1", "512Mi", nil
	case container.SizeMedium:
		return "2", "2Gi", nil
	case container.SizeLarge:
		return "4", "4Gi", nil
	default:
		return "", "", fmt.Errorf("gcp: %w: %q", ErrUnsupportedSize, size)
	}
}

// decodeContainerServiceProperties decodes and validates Properties as a
// cloud-agnostic container.Properties through the shared strict decoder.
//
// The image rules are applied here as well as at the Engine (RFC 011
// §2.3): a provider driven directly, outside the Engine, must be no less
// safe than one driven through it.
func decodeContainerServiceProperties(props map[string]any, allowedRegistries []string) (*container.Properties, error) {
	var p container.Properties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	if _, _, err := runResources(p.Size); err != nil {
		return nil, err
	}
	if err := container.ValidateImage(p.Image, allowedRegistries); err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	if err := p.ValidateIngress(); err != nil {
		return nil, fmt.Errorf("gcp: %w", err)
	}
	if p.EffectiveReplicas() == 0 {
		return nil, ErrZeroReplicasUnsupported
	}
	return &p, nil
}

// declareContainerService registers the Cloud Run service, the dedicated
// identity it runs as, and — when public — the IAM binding that lets the
// internet call it (RFC 017 §2.6).
func declareContainerService(
	ctx *pulumi.Context,
	id, region, networkName string,
	p container.Properties,
) (*cloudrunv2.Service, error) {
	cpu, memory, err := runResources(p.Size)
	if err != nil {
		return nil, err
	}

	// A dedicated identity with nothing attached, following RFC 013's
	// choice to give a VM no standing credential. Omitting the block
	// entirely would not do the same thing here as it does for a Compute
	// Engine instance: Cloud Run falls back to the default compute service
	// account, which carries the Editor role on the whole project — a
	// standing credential on the one resource type that runs arbitrary
	// code.
	account, err := serviceaccount.NewAccount(ctx, id+"-run-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(googleAccountID(id, "-run")),
		DisplayName: pulumi.String(fmt.Sprintf("CloudSDD %s container service", id)),
		Description: pulumi.String("Runs the container service; no roles are bound to it."),
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the identity for %q: %w", id, err)
	}

	ingress := ingressInternal
	if p.EffectivePublic() {
		ingress = ingressAll
	}

	service, err := cloudrunv2.NewService(ctx, id, &cloudrunv2.ServiceArgs{
		Name:     pulumi.String(id),
		Location: pulumi.String(region),
		Ingress:  pulumi.String(ingress),
		Template: &cloudrunv2.ServiceTemplateArgs{
			ServiceAccount: account.Email,
			Scaling: &cloudrunv2.ServiceTemplateScalingArgs{
				// Zero is the floor, which is Cloud Run's whole point:
				// nothing runs, and nothing bills, until a request arrives.
				// Replicas is therefore a ceiling here rather than a fleet
				// size — the cap that keeps a mistranslated prompt from
				// scaling into an invoice.
				MinInstanceCount: pulumi.Int(0),
				MaxInstanceCount: pulumi.Int(p.EffectiveReplicas()),
			},
			// Direct VPC egress into the scope's own subnet, so the service
			// sits beside the database it was deployed to talk to (RFC 016
			// §2.3) and leaves through the scope's NAT (RFC 017 §2.7).
			VpcAccess: &cloudrunv2.ServiceTemplateVpcAccessArgs{
				Egress: pulumi.String(vpcEgressAll),
				NetworkInterfaces: cloudrunv2.ServiceTemplateVpcAccessNetworkInterfaceArray{
					&cloudrunv2.ServiceTemplateVpcAccessNetworkInterfaceArgs{
						Network:    pulumi.String(networkName),
						Subnetwork: pulumi.String(networkName + "-subnet"),
					},
				},
			},
			Containers: cloudrunv2.ServiceTemplateContainerArray{
				&cloudrunv2.ServiceTemplateContainerArgs{
					Image: pulumi.String(p.Image),
					Ports: cloudrunv2.ServiceTemplateContainerPortArray{
						&cloudrunv2.ServiceTemplateContainerPortArgs{
							ContainerPort: pulumi.Int(p.Port),
						},
					},
					Resources: &cloudrunv2.ServiceTemplateContainerResourcesArgs{
						Limits: pulumi.StringMap{
							"cpu":    pulumi.String(cpu),
							"memory": pulumi.String(memory),
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the container service %q: %w", id, err)
	}

	// Without this binding a service with INGRESS_TRAFFIC_ALL is reachable
	// on the network and rejects every caller with 403: Cloud Run's ingress
	// setting and its IAM policy are two separate gates, and "public" means
	// passing both. A private service gets no binding at all, which is why
	// this is inside the branch rather than granted unconditionally.
	if p.EffectivePublic() {
		if _, err := cloudrunv2.NewServiceIamMember(ctx, id+"-public-invoker",
			&cloudrunv2.ServiceIamMemberArgs{
				Location: service.Location,
				Name:     service.Name,
				Role:     pulumi.String(invokerRole),
				Member:   pulumi.String(allUsers),
			}); err != nil {
			return nil, fmt.Errorf("gcp: failed to declare the public invoker binding for %q: %w", id, err)
		}
	}

	// A domain is optional here, unlike AWS. Cloud Run already serves the
	// service on *.run.app with a Google-managed certificate, so absence
	// means "use that endpoint" rather than "there is no HTTPS" (RFC 017
	// §2.3.1). Present, it adds a mapping — with a managed certificate
	// too, so the promise is the same either way.
	if p.Domain != "" {
		if _, err := cloudrun.NewDomainMapping(ctx, id+"-domain", &cloudrun.DomainMappingArgs{
			Name:     pulumi.String(p.Domain),
			Location: pulumi.String(region),
			// The namespace is the project, taken from the service's own
			// output rather than from configuration: the provider resolves
			// the project from the ambient credentials, and reading it back
			// from the resource is how the two are guaranteed to agree
			// (the same approach declareDatabaseSchedule takes).
			Metadata: &cloudrun.DomainMappingMetadataArgs{
				Namespace: service.Project,
			},
			Spec: &cloudrun.DomainMappingSpecArgs{
				RouteName: service.Name,
				// AUTOMATIC provisions and renews a Google-managed
				// certificate. NONE would map the domain and serve no
				// certificate for it, which is the plain-HTTP outcome
				// §2.3 refuses.
				CertificateMode: pulumi.String(certificateModeAutomatic),
			},
		}); err != nil {
			return nil, fmt.Errorf("gcp: failed to map domain %q to %q: %w", p.Domain, id, err)
		}
	}

	return service, nil
}
