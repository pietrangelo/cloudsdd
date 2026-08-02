// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/acm"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/lb"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/route53"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Fargate constants.
const (
	launchTypeFargate = "FARGATE"
	networkModeAwsVpc = "awsvpc"

	// targetTypeIP is mandatory for awsvpc networking: a Fargate task has
	// an elastic network interface rather than an instance to register.
	targetTypeIP = "ip"

	// logRetentionDays bounds what a container's output costs to keep.
	// Thirty days is long enough to investigate an incident and short
	// enough that a chatty service does not accumulate a bill nobody
	// chose.
	logRetentionDays = 30
)

// Listener ports and protocols.
const (
	portHTTPS = 443
	portHTTP  = 80

	protocolHTTP  = "HTTP"
	protocolHTTPS = "HTTPS"
	protocolTCP   = "tcp"

	// tlsPolicy excludes TLS 1.0 and 1.1. The AWS default policy still
	// admits them, so leaving it unset would serve a public endpoint over
	// protocol versions with known weaknesses.
	tlsPolicy = "ELBSecurityPolicy-TLS13-1-2-2021-06"

	// redirectPermanent is the status a plain-HTTP request is answered
	// with. §2.3 requires HTTP to redirect rather than be served.
	redirectPermanent = "HTTP_301"
)

// executionRolePolicy is the AWS-managed policy an ECS task execution role
// needs: it pulls the image and writes the log stream, and it is used by
// the ECS agent rather than by the container.
//
// It is distinct from the task role, which is what the *container* can do
// and which CloudSDD leaves empty (RFC 017 §2.6). Conflating the two is
// how a workload ends up able to read every log group in the account.
const executionRolePolicy = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"

// fargateResources maps a cloud-agnostic size onto a Fargate CPU/memory
// pair.
//
// Fargate accepts only certain combinations — 256 CPU units admit 512,
// 1024 or 2048 MiB and nothing else — so an arbitrary-looking pair here is
// a task definition the API rejects after the user approved the plan.
func fargateResources(size container.Size) (cpu, memory string, err error) {
	switch size {
	case container.SizeSmall:
		return "256", "512", nil
	case container.SizeMedium:
		return "512", "1024", nil
	case container.SizeLarge:
		return "1024", "2048", nil
	default:
		return "", "", fmt.Errorf("aws: %w: %q", ErrUnsupportedContainerSize, size)
	}
}

// decodeContainerServiceProperties decodes and validates Properties as a
// cloud-agnostic container.Properties, then applies the AWS-specific
// ingress rules.
func decodeContainerServiceProperties(props map[string]any, allowedRegistries []string) (*container.Properties, error) {
	var p container.Properties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	if _, _, err := fargateResources(p.Size); err != nil {
		return nil, err
	}
	if err := container.ValidateImage(p.Image, allowedRegistries); err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	if err := p.ValidateIngress(); err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}

	// The AWS half of RFC 017 §2.3.1. ACM will not issue a certificate for
	// an ALB's own *.elb.amazonaws.com name and there is no AWS equivalent
	// of Cloud Run's *.run.app, so a public service with no hostname could
	// only be served over plain HTTP — which §2.3 refuses. Refusing the
	// Specification instead follows RFC 012 §1.3: a request a provider
	// cannot express is an error, never a silent downgrade.
	if p.EffectivePublic() && p.Domain == "" {
		return nil, ErrPublicRequiresDomain
	}
	return &p, nil
}

// hostedZoneName derives the Route 53 zone a domain's records live in.
//
// The heuristic is one label up: "api.acme.example" is served from the
// "acme.example" zone, which is how all but a handful of deployments are
// arranged. A domain with two labels or fewer is already an apex and is
// used as-is.
//
// It is a heuristic, and the error path says so — a subdomain delegated to
// its own zone would not be found, and the message names the zone that was
// looked for rather than reporting that the domain does not exist.
func hostedZoneName(domain string) string {
	labels := strings.Split(strings.TrimSuffix(domain, "."), ".")
	if len(labels) <= 2 {
		return domain
	}
	return strings.Join(labels[1:], ".")
}

// containerDefinition renders the single-container definition ECS takes as
// a JSON document.
func containerDefinition(id, image, region, logGroup string, port int) (string, error) {
	definitions := []map[string]any{{
		"name":      id,
		"image":     image,
		"essential": true,
		"portMappings": []map[string]any{{
			"containerPort": port,
			"protocol":      protocolTCP,
		}},
		"logConfiguration": map[string]any{
			"logDriver": "awslogs",
			"options": map[string]string{
				"awslogs-group":         logGroup,
				"awslogs-region":        region,
				"awslogs-stream-prefix": managedByValue,
			},
		},
	}}

	encoded, err := json.Marshal(definitions)
	if err != nil {
		return "", fmt.Errorf("aws: failed to render the container definition for %q: %w", id, err)
	}
	return string(encoded), nil
}

// declareContainerService registers the ECS service and everything it
// needs to be reachable (RFC 017 §2.6).
func declareContainerService(
	ctx *pulumi.Context,
	r spec.Resource,
	net scopeNetwork,
	p container.Properties,
	rules []schedule.Rule,
	opts ...pulumi.ResourceOption,
) (*ecs.Service, error) {
	id := r.ID
	region := r.Scope.Region

	cpu, memory, err := fargateResources(p.Size)
	if err != nil {
		return nil, err
	}

	albSG, serviceSG, err := declareContainerSecurityGroups(ctx, id, net, p, opts...)
	if err != nil {
		return nil, err
	}

	balancer, targetGroup, err := declareContainerIngress(ctx, id, net, p, albSG, opts...)
	if err != nil {
		return nil, err
	}

	taskDefinition, err := declareContainerTask(ctx, id, region, cpu, memory, p, opts...)
	if err != nil {
		return nil, err
	}

	cluster, err := ecs.NewCluster(ctx, id+"-cluster", &ecs.ClusterArgs{
		Name: pulumi.String(id + "-cloudsdd"),
		Tags: pulumi.StringMap{"Name": pulumi.String(id), tagManagedBy: pulumi.String(managedByValue)},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the cluster for %q: %w", id, err)
	}

	service, err := ecs.NewService(ctx, id, &ecs.ServiceArgs{
		Name:           pulumi.String(id),
		Cluster:        cluster.Arn,
		TaskDefinition: taskDefinition.Arn,
		LaunchType:     pulumi.String(launchTypeFargate),
		DesiredCount:   pulumi.Int(p.EffectiveReplicas()),
		NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
			Subnets:        pulumi.ToStringArray(net.subnetIDs),
			SecurityGroups: pulumi.StringArray{serviceSG.ID()},
			// The task sits in a private subnet with no address of its
			// own. It pulls its image through the scope's NAT (RFC 017
			// §2.7) and is reached only through the load balancer.
			AssignPublicIp: pulumi.Bool(false),
		},
		LoadBalancers: ecs.ServiceLoadBalancerArray{
			&ecs.ServiceLoadBalancerArgs{
				TargetGroupArn: targetGroup.Arn,
				ContainerName:  pulumi.String(id),
				ContainerPort:  pulumi.Int(p.Port),
			},
		},
		Tags: pulumi.StringMap{"Name": pulumi.String(id), tagManagedBy: pulumi.String(managedByValue)},
	}, append(opts, pulumi.DependsOn([]pulumi.Resource{balancer}))...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the container service %q: %w", id, err)
	}

	// The schedule shares the service's Pulumi program, and so its stack,
	// which is what makes the existing Destroy path tear both down
	// (RFC 012 §4.1).
	if err := declareContainerSchedule(ctx, id, cluster, service, p.EffectiveReplicas(), rules, opts...); err != nil {
		return nil, err
	}
	return service, nil
}

// declareContainerSecurityGroups builds the two-tier perimeter: what may
// reach the load balancer, and what may reach the container.
//
// The container's own port is never open to anything but the load
// balancer's group, which is what makes "the container is reachable only
// through the ingress" true rather than merely intended (RFC 017 §2.3).
func declareContainerSecurityGroups(
	ctx *pulumi.Context,
	id string,
	net scopeNetwork,
	p container.Properties,
	opts ...pulumi.ResourceOption,
) (albSG, serviceSG *ec2.SecurityGroup, err error) {
	// Where callers may come from. A private service admits its own
	// network and nothing else; a public one is the deliberate exception,
	// and it is deliberate only on the encrypted port.
	ingressCIDR := net.cidr
	if p.EffectivePublic() {
		ingressCIDR = defaultRoute
	}

	ingressRules := ec2.SecurityGroupIngressArray{
		&ec2.SecurityGroupIngressArgs{
			Protocol:   pulumi.String(protocolTCP),
			FromPort:   pulumi.Int(portHTTPS),
			ToPort:     pulumi.Int(portHTTPS),
			CidrBlocks: pulumi.StringArray{pulumi.String(ingressCIDR)},
		},
	}
	// Port 80 is opened only where something is listening on it, and what
	// listens there answers 301 rather than serving the application.
	if p.EffectivePublic() {
		ingressRules = append(ingressRules, &ec2.SecurityGroupIngressArgs{
			Protocol:   pulumi.String(protocolTCP),
			FromPort:   pulumi.Int(portHTTP),
			ToPort:     pulumi.Int(portHTTP),
			CidrBlocks: pulumi.StringArray{pulumi.String(ingressCIDR)},
		})
	} else {
		// A private service has no certificate to serve, so its internal
		// listener is HTTP on port 80, reachable from the scope network
		// alone. Replacing the HTTPS rule rather than adding to it: there
		// is nothing on 443.
		ingressRules = ec2.SecurityGroupIngressArray{
			&ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String(protocolTCP),
				FromPort:   pulumi.Int(portHTTP),
				ToPort:     pulumi.Int(portHTTP),
				CidrBlocks: pulumi.StringArray{pulumi.String(ingressCIDR)},
			},
		}
	}

	albSG, err = ec2.NewSecurityGroup(ctx, id+"-alb-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(net.vpcID),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: load balancer ingress", id)),
		Ingress:     ingressRules,
		Egress: ec2.SecurityGroupEgressArray{
			&ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String(net.cidr)},
			},
		},
	}, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the load balancer security group for %q: %w", id, err)
	}

	serviceSG, err = ec2.NewSecurityGroup(ctx, id+"-svc-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(net.vpcID),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: reachable from the load balancer only", id)),
		Ingress: ec2.SecurityGroupIngressArray{
			&ec2.SecurityGroupIngressArgs{
				Protocol: pulumi.String(protocolTCP),
				FromPort: pulumi.Int(p.Port),
				ToPort:   pulumi.Int(p.Port),
				// By group, not by CIDR. A CIDR rule would admit anything
				// else that happened to sit in the same subnet.
				SecurityGroups: pulumi.StringArray{albSG.ID()},
			},
		},
		// Outbound is open: the task pulls its image and reaches whatever
		// the application talks to. It leaves through the scope's NAT, so
		// "open" means one observable address rather than an anonymous
		// one.
		Egress: ec2.SecurityGroupEgressArray{
			&ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String(defaultRoute)},
			},
		},
	}, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the service security group for %q: %w", id, err)
	}
	return albSG, serviceSG, nil
}

// declareContainerIngress builds the load balancer, its target group and
// its listeners, plus — for a public service — the certificate and the DNS
// records that make the domain resolve to it.
func declareContainerIngress(
	ctx *pulumi.Context,
	id string,
	net scopeNetwork,
	p container.Properties,
	albSG *ec2.SecurityGroup,
	opts ...pulumi.ResourceOption,
) (*lb.LoadBalancer, *lb.TargetGroup, error) {
	public := p.EffectivePublic()

	// An internet-facing balancer lives in the public tier, which RFC 017
	// §2.7 created for the NAT and which spans two zones for exactly this
	// reason. An internal one lives in the private subnets with the
	// workloads.
	subnets := net.subnetIDs
	if public {
		if len(net.publicSubnetIDs) < publicSubnetCount {
			return nil, nil, fmt.Errorf(
				"aws: scope network has %d public subnets, need %d for an internet-facing load balancer: %w",
				len(net.publicSubnetIDs), publicSubnetCount, ErrPublicSubnetsMissing)
		}
		subnets = net.publicSubnetIDs
	}

	balancer, err := lb.NewLoadBalancer(ctx, id+"-alb", &lb.LoadBalancerArgs{
		LoadBalancerType: pulumi.String("application"),
		Internal:         pulumi.Bool(!public),
		Subnets:          pulumi.ToStringArray(subnets),
		SecurityGroups:   pulumi.StringArray{albSG.ID()},
		// A header the balancer cannot parse is a request two hops might
		// read differently, which is the shape of a request-smuggling bug.
		DropInvalidHeaderFields: pulumi.Bool(true),
	}, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the load balancer for %q: %w", id, err)
	}

	targetGroup, err := lb.NewTargetGroup(ctx, id+"-tg", &lb.TargetGroupArgs{
		Port:     pulumi.Int(p.Port),
		Protocol: pulumi.String(protocolHTTP),
		VpcId:    pulumi.String(net.vpcID),
		// Mandatory for awsvpc networking: a Fargate task registers an
		// elastic network interface, not an instance.
		TargetType: pulumi.String(targetTypeIP),
		HealthCheck: &lb.TargetGroupHealthCheckArgs{
			Enabled:  pulumi.Bool(true),
			Protocol: pulumi.String(protocolHTTP),
			Path:     pulumi.String("/"),
			Matcher:  pulumi.String("200-399"),
		},
	}, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the target group for %q: %w", id, err)
	}

	forward := lb.ListenerDefaultActionArray{
		&lb.ListenerDefaultActionArgs{
			Type:           pulumi.String("forward"),
			TargetGroupArn: targetGroup.Arn,
		},
	}

	if !public {
		// No certificate exists for an internal service: there is no
		// hostname to issue one for, and `domain` requires `public`. The
		// listener is HTTP, reachable from the scope network alone.
		if _, err := lb.NewListener(ctx, id+"-http", &lb.ListenerArgs{
			LoadBalancerArn: balancer.Arn,
			Port:            pulumi.Int(portHTTP),
			Protocol:        pulumi.String(protocolHTTP),
			DefaultActions:  forward,
		}, opts...); err != nil {
			return nil, nil, fmt.Errorf("aws: failed to declare the internal listener for %q: %w", id, err)
		}
		return balancer, targetGroup, nil
	}

	certificateArn, err := declareContainerCertificate(ctx, id, p.Domain, balancer, opts...)
	if err != nil {
		return nil, nil, err
	}

	if _, err := lb.NewListener(ctx, id+"-https", &lb.ListenerArgs{
		LoadBalancerArn: balancer.Arn,
		Port:            pulumi.Int(portHTTPS),
		Protocol:        pulumi.String(protocolHTTPS),
		SslPolicy:       pulumi.String(tlsPolicy),
		CertificateArn:  certificateArn,
		DefaultActions:  forward,
	}, opts...); err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the https listener for %q: %w", id, err)
	}

	// Port 80 redirects rather than serving. Leaving it closed would be
	// safe and would also mean every plain-HTTP client gets a connection
	// refused instead of an upgrade, which is a worse outcome than the
	// redirect for something that exists to be reached (RFC 017 §2.3).
	if _, err := lb.NewListener(ctx, id+"-http-redirect", &lb.ListenerArgs{
		LoadBalancerArn: balancer.Arn,
		Port:            pulumi.Int(portHTTP),
		Protocol:        pulumi.String(protocolHTTP),
		DefaultActions: lb.ListenerDefaultActionArray{
			&lb.ListenerDefaultActionArgs{
				Type: pulumi.String("redirect"),
				Redirect: &lb.ListenerDefaultActionRedirectArgs{
					Port:       pulumi.String(fmt.Sprint(portHTTPS)),
					Protocol:   pulumi.String(protocolHTTPS),
					StatusCode: pulumi.String(redirectPermanent),
				},
			},
		},
	}, opts...); err != nil {
		return nil, nil, fmt.Errorf("aws: failed to declare the http redirect for %q: %w", id, err)
	}

	return balancer, targetGroup, nil
}

// declareContainerCertificate issues the ACM certificate for domain,
// validates it through DNS, and points the domain at the balancer.
//
// The zone lookup is what makes this possible without asking the user to
// paste records by hand: emitting validation records for a human to create
// would make `apply` block on an action CloudSDD cannot observe. It is
// also the constraint RFC 017 §2.3.1 states rather than leaves to be
// discovered — the domain has to be served from a Route 53 zone in this
// account.
func declareContainerCertificate(
	ctx *pulumi.Context,
	id, domain string,
	balancer *lb.LoadBalancer,
	opts ...pulumi.ResourceOption,
) (pulumi.StringOutput, error) {
	zoneName := hostedZoneName(domain)
	private := false

	zone, err := route53.LookupZone(ctx, &route53.LookupZoneArgs{
		Name:        &zoneName,
		PrivateZone: &private,
	}, nil)
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf(
			"aws: no public Route 53 hosted zone %q found for domain %q. A public container service "+
				"needs its certificate validated through DNS, which requires the zone to be in this "+
				"account: %w", zoneName, domain, err)
	}

	certificate, err := acm.NewCertificate(ctx, id+"-cert", &acm.CertificateArgs{
		DomainName:       pulumi.String(domain),
		ValidationMethod: pulumi.String("DNS"),
	}, opts...)
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("aws: failed to declare the certificate for %q: %w", id, err)
	}

	// One domain and no subject alternative names, so there is exactly one
	// validation option and indexing it is safe.
	option := certificate.DomainValidationOptions.Index(pulumi.Int(0))
	validationRecord, err := route53.NewRecord(ctx, id+"-cert-validation", &route53.RecordArgs{
		ZoneId:  pulumi.String(zone.ZoneId),
		Name:    option.ResourceRecordName().Elem(),
		Type:    option.ResourceRecordType().Elem(),
		Records: pulumi.StringArray{option.ResourceRecordValue().Elem()},
		Ttl:     pulumi.Int(60),
		// A re-apply reissues the same validation record; refusing to
		// overwrite would fail the second deploy of an unchanged service.
		AllowOverwrite: pulumi.Bool(true),
	}, opts...)
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("aws: failed to declare the validation record for %q: %w", id, err)
	}

	// The listener must wait for issuance, not merely for the certificate
	// resource to exist: attaching one still PENDING_VALIDATION fails. The
	// validation's own ARN output is what carries that dependency.
	validation, err := acm.NewCertificateValidation(ctx, id+"-cert-ready", &acm.CertificateValidationArgs{
		CertificateArn:        certificate.Arn,
		ValidationRecordFqdns: pulumi.StringArray{validationRecord.Fqdn},
	}, opts...)
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("aws: failed to declare certificate validation for %q: %w", id, err)
	}

	// Without this the certificate is valid and the hostname resolves
	// nowhere: a deployment that reports success and serves nothing.
	if _, err := route53.NewRecord(ctx, id+"-alias", &route53.RecordArgs{
		ZoneId: pulumi.String(zone.ZoneId),
		Name:   pulumi.String(domain),
		Type:   pulumi.String("A"),
		Aliases: route53.RecordAliasArray{
			&route53.RecordAliasArgs{
				Name:                 balancer.DnsName,
				ZoneId:               balancer.ZoneId,
				EvaluateTargetHealth: pulumi.Bool(true),
			},
		},
	}, opts...); err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("aws: failed to declare the alias record for %q: %w", id, err)
	}

	return validation.CertificateArn, nil
}

// declareContainerTask builds the log group, the two roles and the task
// definition.
func declareContainerTask(
	ctx *pulumi.Context,
	id, region, cpu, memory string,
	p container.Properties,
	opts ...pulumi.ResourceOption,
) (*ecs.TaskDefinition, error) {
	logGroupName := "/cloudsdd/" + id
	if _, err := cloudwatch.NewLogGroup(ctx, id+"-logs", &cloudwatch.LogGroupArgs{
		Name:            pulumi.String(logGroupName),
		RetentionInDays: pulumi.Int(logRetentionDays),
	}, opts...); err != nil {
		return nil, fmt.Errorf("aws: failed to declare the log group for %q: %w", id, err)
	}

	assumeRole, err := ecsAssumeRolePolicy()
	if err != nil {
		return nil, err
	}

	// The execution role belongs to the ECS agent: it pulls the image and
	// opens the log stream before the container starts.
	executionRole, err := iam.NewRole(ctx, id+"-exec-role", &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(assumeRole),
		Description:      pulumi.String(fmt.Sprintf("CloudSDD %s: pulls the image and writes logs", id)),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the execution role for %q: %w", id, err)
	}
	if _, err := iam.NewRolePolicyAttachment(ctx, id+"-exec-role-policy", &iam.RolePolicyAttachmentArgs{
		Role:      executionRole.Name,
		PolicyArn: pulumi.String(executionRolePolicy),
	}, opts...); err != nil {
		return nil, fmt.Errorf("aws: failed to attach the execution policy for %q: %w", id, err)
	}

	// The task role is what the *container* can do, and nothing is
	// attached to it (RFC 017 §2.6). It is declared rather than omitted so
	// a workload that later needs a permission has somewhere to receive
	// it, and so the absence is visible in the plan rather than implied.
	taskRole, err := iam.NewRole(ctx, id+"-task-role", &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(assumeRole),
		Description:      pulumi.String(fmt.Sprintf("CloudSDD %s: no permissions are attached", id)),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the task role for %q: %w", id, err)
	}

	definition, err := containerDefinition(id, p.Image, region, logGroupName, p.Port)
	if err != nil {
		return nil, err
	}

	taskDefinition, err := ecs.NewTaskDefinition(ctx, id+"-task", &ecs.TaskDefinitionArgs{
		Family:                  pulumi.String(id),
		Cpu:                     pulumi.String(cpu),
		Memory:                  pulumi.String(memory),
		NetworkMode:             pulumi.String(networkModeAwsVpc),
		RequiresCompatibilities: pulumi.StringArray{pulumi.String(launchTypeFargate)},
		ExecutionRoleArn:        executionRole.Arn,
		TaskRoleArn:             taskRole.Arn,
		ContainerDefinitions:    pulumi.String(definition),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the task definition for %q: %w", id, err)
	}
	return taskDefinition, nil
}

// ecsAssumeRolePolicy is the trust policy both roles carry: only the ECS
// task service may assume them.
func ecsAssumeRolePolicy() (string, error) {
	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Action":    "sts:AssumeRole",
			"Principal": map[string]string{"Service": "ecs-tasks.amazonaws.com"},
		}},
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("aws: failed to render the ecs assume-role policy: %w", err)
	}
	return string(encoded), nil
}
