// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	ecsClusterToken        = "aws:ecs/cluster:Cluster"
	ecsServiceToken        = "aws:ecs/service:Service"
	ecsTaskDefinitionToken = "aws:ecs/taskDefinition:TaskDefinition"
	loadBalancerToken      = "aws:lb/loadBalancer:LoadBalancer"
	targetGroupToken       = "aws:lb/targetGroup:TargetGroup"
	listenerToken          = "aws:lb/listener:Listener"
	certificateToken       = "aws:acm/certificate:Certificate"
	certValidationToken    = "aws:acm/certificateValidation:CertificateValidation"
	route53RecordToken     = "aws:route53/record:Record"
	getZoneToken           = "aws:route53/getZone:getZone"
	logGroupToken          = "aws:cloudwatch/logGroup:LogGroup"
	rolePolicyAttachToken  = "aws:iam/rolePolicyAttachment:RolePolicyAttachment"
)

// testContainerImage is digest-pinned, which is what the image rules
// require when no registry is allow-listed.
const testContainerImage = "ghcr.io/acme/api@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func containerProps(mutate func(map[string]any)) map[string]any {
	props := map[string]any{
		"image": testContainerImage,
		"port":  8080,
		"size":  "small",
	}
	if mutate != nil {
		mutate(props)
	}
	return props
}

func containerResource(mutate func(*spec.Resource)) spec.Resource {
	r := spec.Resource{
		ID:         "api",
		Type:       spec.ResourceTypeContainerService,
		Provider:   spec.ProviderAWS,
		Scope:      spec.Scope{Region: "eu-central-1"},
		Properties: containerProps(nil),
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func declaredContainer(t *testing.T, mutate func(map[string]any)) []recordedResource {
	t.Helper()

	props, err := decodeContainerServiceProperties(containerProps(mutate), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, containerResource(nil), testNetwork(), *props, nil)
		return err
	})
}

// securityGroupByDescription finds one of the two groups by the substring
// its description carries. Both are ec2:SecurityGroup, so the type token
// alone cannot tell the perimeter from the workload.
func securityGroupByDescription(t *testing.T, recorded []recordedResource, want string) recordedResource {
	t.Helper()
	for _, r := range recorded {
		if r.Type == securityGroupToken &&
			strings.Contains(r.Inputs["description"].StringValue(), want) {
			return r
		}
	}
	t.Fatalf("no security group whose description contains %q", want)
	return recordedResource{}
}

// TestDeclareContainerServiceIsPrivateByDefault covers RFC 017 §2.3.
//
// A private service gets an internal load balancer, admits only the
// scope's own network, and issues no certificate — there is no hostname
// to issue one for, and `domain` requires `public`.
func TestDeclareContainerServiceIsPrivateByDefault(t *testing.T) {
	recorded := declaredContainer(t, nil)

	balancer := findResource(t, recorded, loadBalancerToken)
	if !balancer.Inputs["internal"].BoolValue() {
		t.Error("internal = false; a private service would be reachable from the internet")
	}
	// In the private subnets, with the workloads — not the public tier.
	subnets := balancer.Inputs["subnets"].ArrayValue()
	for _, s := range subnets {
		if strings.Contains(s.StringValue(), "pub") {
			t.Errorf("internal balancer placed in %q, want the private subnets", s.StringValue())
		}
	}

	alb := securityGroupByDescription(t, recorded, "load balancer ingress")
	for _, rule := range alb.Inputs["ingress"].ArrayValue() {
		for _, cidr := range rule.ObjectValue()["cidrBlocks"].ArrayValue() {
			if cidr.StringValue() == defaultRoute {
				t.Error("a private service admits 0.0.0.0/0; it must admit its own network only")
			}
		}
	}

	for _, token := range []string{certificateToken, route53RecordToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for a private service; there is no hostname to certify", token)
		}
	}
}

// TestDeclareContainerServicePublicIsHTTPSOnly covers the §2.3 promise
// that a public service is served over TLS and never as a raw open port.
//
// The redirect listener is the subtle half: leaving port 80 closed would
// also be safe, and would mean every plain-HTTP client gets a connection
// refused rather than an upgrade. What must never happen is port 80
// *serving* the application.
func TestDeclareContainerServicePublicIsHTTPSOnly(t *testing.T) {
	recorded := declaredContainer(t, func(p map[string]any) {
		p["public"] = true
		p["domain"] = "api.acme.example"
	})

	balancer := findResource(t, recorded, loadBalancerToken)
	if balancer.Inputs["internal"].BoolValue() {
		t.Error("internal = true; a public service is unreachable")
	}
	for _, s := range balancer.Inputs["subnets"].ArrayValue() {
		if !strings.Contains(s.StringValue(), "pub") {
			t.Errorf("public balancer placed in %q, want the public tier", s.StringValue())
		}
	}

	listeners := resourcesOfType(recorded, listenerToken)
	if len(listeners) != 2 {
		t.Fatalf("declared %d listeners, want 2 (https, and http redirecting to it)", len(listeners))
	}

	var https, http recordedResource
	for _, l := range listeners {
		switch l.Inputs["port"].NumberValue() {
		case portHTTPS:
			https = l
		case portHTTP:
			http = l
		}
	}

	if got := https.Inputs["protocol"].StringValue(); got != protocolHTTPS {
		t.Errorf("listener on %d speaks %q, want %q", portHTTPS, got, protocolHTTPS)
	}
	if got := https.Inputs["sslPolicy"].StringValue(); got != tlsPolicy {
		t.Errorf("ssl policy = %q, want %q — the AWS default still admits TLS 1.0", got, tlsPolicy)
	}
	if !https.Inputs["certificateArn"].HasValue() {
		t.Error("the https listener carries no certificate")
	}

	// The one assertion that makes "HTTP redirects rather than being
	// served" true: a forward action here would serve the application in
	// clear text on a public endpoint.
	actions := http.Inputs["defaultActions"].ArrayValue()
	if len(actions) != 1 {
		t.Fatalf("the http listener has %d default actions, want 1", len(actions))
	}
	action := actions[0].ObjectValue()
	if got := action["type"].StringValue(); got != "redirect" {
		t.Fatalf("the http listener action is %q, want a redirect — it must never serve the application", got)
	}
	redirect := action["redirect"].ObjectValue()
	if got := redirect["protocol"].StringValue(); got != protocolHTTPS {
		t.Errorf("redirect protocol = %q, want %q", got, protocolHTTPS)
	}
	if got := redirect["statusCode"].StringValue(); got != redirectPermanent {
		t.Errorf("redirect status = %q, want %q", got, redirectPermanent)
	}
}

// TestDeclareContainerServiceCertificateIsValidatedAndResolvable covers
// RFC 017 §2.3.1.
//
// Two records, and both matter. Without the validation record the
// certificate never leaves PENDING_VALIDATION and the listener cannot use
// it. Without the alias record the certificate is valid and the hostname
// resolves nowhere — a deployment that reports success and serves nothing.
func TestDeclareContainerServiceCertificateIsValidatedAndResolvable(t *testing.T) {
	recorded := declaredContainer(t, func(p map[string]any) {
		p["public"] = true
		p["domain"] = "api.acme.example"
	})

	certificate := findResource(t, recorded, certificateToken)
	if got := certificate.Inputs["domainName"].StringValue(); got != "api.acme.example" {
		t.Errorf("certificate domain = %q, want the requested one", got)
	}
	if got := certificate.Inputs["validationMethod"].StringValue(); got != "DNS" {
		t.Errorf("validation method = %q, want DNS — email validation needs a human", got)
	}
	if !hasResource(recorded, certValidationToken) {
		t.Error("no certificate validation; the listener would attach a pending certificate and fail")
	}

	records := resourcesOfType(recorded, route53RecordToken)
	if len(records) != 2 {
		t.Fatalf("declared %d route53 records, want 2 (validation and alias)", len(records))
	}

	var alias recordedResource
	for _, r := range records {
		if r.Inputs["type"].StringValue() == "A" {
			alias = r
		}
	}
	if got := alias.Inputs["name"].StringValue(); got != "api.acme.example" {
		t.Errorf("alias record name = %q, want the domain", got)
	}
	if n := len(alias.Inputs["aliases"].ArrayValue()); n != 1 {
		t.Errorf("alias record has %d targets, want 1 (the load balancer)", n)
	}
}

// TestDeclareContainerServicePerimeter covers the two-tier security group
// arrangement: the container's own port is reachable from the load
// balancer's group and from nothing else.
//
// By group rather than by CIDR is the assertion that matters. A CIDR rule
// covering the subnet would admit every other thing that happens to sit in
// it, which is the posture RFC 016 §1 was written to remove.
func TestDeclareContainerServicePerimeter(t *testing.T) {
	recorded := declaredContainer(t, nil)

	service := securityGroupByDescription(t, recorded, "reachable from the load balancer only")
	rules := service.Inputs["ingress"].ArrayValue()
	if len(rules) != 1 {
		t.Fatalf("the service group has %d ingress rules, want 1", len(rules))
	}

	rule := rules[0].ObjectValue()
	if got := rule["fromPort"].NumberValue(); got != 8080 {
		t.Errorf("ingress from port %v, want the container port", got)
	}
	if rule["cidrBlocks"].HasValue() && len(rule["cidrBlocks"].ArrayValue()) > 0 {
		t.Error("the service group admits a CIDR; anything else in the subnet would reach the container")
	}
	if n := len(rule["securityGroups"].ArrayValue()); n != 1 {
		t.Errorf("the service group admits %d security groups, want exactly the balancer's", n)
	}
}

// TestDeclareContainerServiceTaskRoleHasNothingAttached covers RFC 017
// §2.6's identity row.
//
// Two roles are declared and they are not interchangeable. The execution
// role belongs to the ECS agent and needs the managed policy to pull the
// image; the task role is what the container itself can do, and nothing is
// attached to it. Conflating them is how a workload ends up able to read
// every log group in the account.
func TestDeclareContainerServiceTaskRoleHasNothingAttached(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if n := len(resourcesOfType(recorded, iamRoleToken)); n != 2 {
		t.Fatalf("declared %d roles, want 2 (execution and task)", n)
	}

	attachments := resourcesOfType(recorded, rolePolicyAttachToken)
	if len(attachments) != 1 {
		t.Fatalf("declared %d policy attachments, want 1 — only the execution role gets one", len(attachments))
	}
	if got := attachments[0].Inputs["policyArn"].StringValue(); got != executionRolePolicy {
		t.Errorf("attached policy = %q, want %q", got, executionRolePolicy)
	}

	// Both roles trust the ECS task service and nothing else.
	for _, role := range resourcesOfType(recorded, iamRoleToken) {
		policy := role.Inputs["assumeRolePolicy"].StringValue()
		if !strings.Contains(policy, "ecs-tasks.amazonaws.com") {
			t.Errorf("role trust policy = %q, want it to name the ecs task service", policy)
		}
		if strings.Contains(policy, `"AWS"`) {
			t.Errorf("role trust policy = %q, want no account principal", policy)
		}
	}
}

// TestDeclareContainerServiceTask covers the task definition: Fargate
// requires awsvpc networking, a matched CPU/memory pair, and the service
// must place tasks in private subnets with no address of their own.
func TestDeclareContainerServiceTask(t *testing.T) {
	recorded := declaredContainer(t, func(p map[string]any) { p["replicas"] = 3 })

	task := findResource(t, recorded, ecsTaskDefinitionToken)
	if got := task.Inputs["networkMode"].StringValue(); got != networkModeAwsVpc {
		t.Errorf("network mode = %q, want %q — Fargate requires it", got, networkModeAwsVpc)
	}
	if got := task.Inputs["cpu"].StringValue(); got != "256" {
		t.Errorf("cpu = %q, want the small pair's", got)
	}
	if got := task.Inputs["memory"].StringValue(); got != "512" {
		t.Errorf("memory = %q, want the small pair's", got)
	}

	var definitions []map[string]any
	if err := json.Unmarshal([]byte(task.Inputs["containerDefinitions"].StringValue()), &definitions); err != nil {
		t.Fatalf("container definitions are not valid JSON: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("declared %d container definitions, want 1", len(definitions))
	}
	if got := definitions[0]["image"]; got != testContainerImage {
		t.Errorf("image = %v, want the requested one", got)
	}
	if definitions[0]["essential"] != true {
		t.Error("the container is not essential; a crash would leave the task running empty")
	}

	service := findResource(t, recorded, ecsServiceToken)
	if got := service.Inputs["desiredCount"].NumberValue(); got != 3 {
		t.Errorf("desired count = %v, want the requested replicas", got)
	}
	if got := service.Inputs["launchType"].StringValue(); got != launchTypeFargate {
		t.Errorf("launch type = %q, want %q", got, launchTypeFargate)
	}

	network := service.Inputs["networkConfiguration"].ObjectValue()
	if network["assignPublicIp"].BoolValue() {
		t.Error("assignPublicIp = true; the task would be addressable from outside the load balancer")
	}
	for _, s := range network["subnets"].ArrayValue() {
		if strings.Contains(s.StringValue(), "pub") {
			t.Errorf("task placed in %q, want a private subnet", s.StringValue())
		}
	}

	// A target group registering instances cannot work with awsvpc: a
	// Fargate task has an elastic network interface, not an instance.
	if got := findResource(t, recorded, targetGroupToken).Inputs["targetType"].StringValue(); got != targetTypeIP {
		t.Errorf("target type = %q, want %q", got, targetTypeIP)
	}

	if got := findResource(t, recorded, logGroupToken).Inputs["retentionInDays"].NumberValue(); got != logRetentionDays {
		t.Errorf("log retention = %v days, want %d", got, logRetentionDays)
	}
	if !hasResource(recorded, ecsClusterToken) {
		t.Error("no cluster was declared")
	}
}

// TestFargateResources: the pairs are not free choices. Fargate accepts
// only certain CPU/memory combinations, so an arbitrary-looking pair is a
// task definition the API rejects after the user approved the plan.
func TestFargateResources(t *testing.T) {
	// The memory values Fargate admits for each CPU size.
	valid := map[string][]string{
		"256":  {"512", "1024", "2048"},
		"512":  {"1024", "2048", "3072", "4096"},
		"1024": {"2048", "3072", "4096", "5120", "6144", "7168", "8192"},
	}

	tests := []struct {
		size    container.Size
		cpu     string
		memory  string
		wantErr bool
	}{
		{size: container.SizeSmall, cpu: "256", memory: "512"},
		{size: container.SizeMedium, cpu: "512", memory: "1024"},
		{size: container.SizeLarge, cpu: "1024", memory: "2048"},
		{size: "enormous", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.size), func(t *testing.T) {
			cpu, memory, err := fargateResources(tt.size)

			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedContainerSize) {
					t.Fatalf("fargateResources(%q) = %v, want ErrUnsupportedContainerSize", tt.size, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("fargateResources(%q) = %v", tt.size, err)
			}
			if cpu != tt.cpu || memory != tt.memory {
				t.Errorf("fargateResources(%q) = %q/%q, want %q/%q", tt.size, cpu, memory, tt.cpu, tt.memory)
			}
			// Both parse as integers, which is what the API expects, and
			// the pair is one Fargate actually admits.
			if _, err := strconv.Atoi(cpu); err != nil {
				t.Errorf("cpu %q is not an integer", cpu)
			}
			admitted, ok := valid[cpu]
			if !ok {
				t.Fatalf("cpu %q is not a Fargate size", cpu)
			}
			var found bool
			for _, m := range admitted {
				if m == memory {
					found = true
				}
			}
			if !found {
				t.Errorf("Fargate does not admit %s MiB with %s CPU units; valid: %v", memory, cpu, admitted)
			}
		})
	}
}

// TestHostedZoneName covers the zone derivation RFC 017 §2.3.1 describes
// as a heuristic.
func TestHostedZoneName(t *testing.T) {
	tests := []struct {
		domain string
		want   string
	}{
		{domain: "api.acme.example", want: "acme.example"},
		{domain: "api.eu.acme.example", want: "eu.acme.example"},
		// Already an apex: stripping a label would look for "example".
		{domain: "acme.example", want: "acme.example"},
		{domain: "localhost", want: "localhost"},
		// A trailing dot is a legal fully-qualified name and must not
		// produce an empty last label.
		{domain: "api.acme.example.", want: "acme.example"},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			if got := hostedZoneName(tt.domain); got != tt.want {
				t.Errorf("hostedZoneName(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

// TestDecodeContainerServiceProperties covers the decoding rules and the
// two AWS-specific ingress refusals.
func TestDecodeContainerServiceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		allowed []string
		wantErr error
		wantMsg string
	}{
		{name: "a digest-pinned private service", props: containerProps(nil)},
		{
			name: "public with a domain",
			props: containerProps(func(p map[string]any) {
				p["public"] = true
				p["domain"] = "api.acme.example"
			}),
		},
		{
			// The RFC 017 §2.3.1 refusal. ACM cannot certify an ALB's own
			// name, so the alternative would be plain HTTP on a public
			// endpoint — a silent downgrade from what §2.3 promises.
			name:    "public without a domain",
			props:   containerProps(func(p map[string]any) { p["public"] = true }),
			wantErr: ErrPublicRequiresDomain,
		},
		{
			// A hostname on a service nothing outside can reach is a
			// property the user asked for and will not get.
			name:    "a domain without public",
			props:   containerProps(func(p map[string]any) { p["domain"] = "api.acme.example" }),
			wantErr: container.ErrDomainWithoutPublic,
		},
		{
			name: "a malformed domain",
			props: containerProps(func(p map[string]any) {
				p["public"] = true
				p["domain"] = "not a hostname"
			}),
			wantMsg: "property validation failed",
		},
		{
			name:    "a tag with no allowlist",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:2.1" }),
			wantErr: container.ErrImageMutable,
		},
		{
			name:    "a tag from an allow-listed registry",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:2.1" }),
			allowed: []string{"ghcr.io"},
		},
		{
			name:    "latest",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:latest" }),
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrImageLatest,
		},
		{
			// Legal on AWS, unlike Cloud Run: an ECS service with a
			// desired count of zero is deployed and running nothing, which
			// is exactly what the schema means by it.
			name:  "zero replicas",
			props: containerProps(func(p map[string]any) { p["replicas"] = 0 }),
		},
		{
			name:    "an unmapped size",
			props:   containerProps(func(p map[string]any) { p["size"] = "enormous" }),
			wantMsg: "property validation failed",
		},
		{
			name:    "an unknown property",
			props:   containerProps(func(p map[string]any) { p["cpu_architecture"] = "arm64" }),
			wantMsg: "unknown or malformed property",
		},
		{
			name:    "a credential-shaped property",
			props:   containerProps(func(p map[string]any) { p["password"] = "hunter2" }),
			wantMsg: "looks like a credential",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeContainerServiceProperties(tt.props, tt.allowed)

			if tt.wantErr == nil && tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("decodeContainerServiceProperties() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decodeContainerServiceProperties() = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}

// TestDeclareContainerServiceRefusesANetworkWithoutAPublicTier: a network
// created before RFC 017 §2.7, or altered out of band, cannot host an
// internet-facing balancer. The error names CloudSDD's layout, which the
// ALB API's own message does not.
func TestDeclareContainerServiceRefusesANetworkWithoutAPublicTier(t *testing.T) {
	props, err := decodeContainerServiceProperties(containerProps(func(p map[string]any) {
		p["public"] = true
		p["domain"] = "api.acme.example"
	}), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}

	net := testNetwork()
	net.publicSubnetIDs = []string{"subnet-pub-a"} // one zone, not two

	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, containerResource(nil), net, *props, nil)
		return err
	}, pulumi.WithMocks("cloudsdd-aws", "test", mockMonitor{rec: &recorder{}}))

	if !errors.Is(err, ErrPublicSubnetsMissing) {
		t.Fatalf("declareContainerService() = %v, want ErrPublicSubnetsMissing", err)
	}
}

// TestValidateContainerServiceAcceptsAWellFormedOne is the regression test
// for the defect RFC 017 §1 records: a container_service used to validate,
// compile a schedule, render a plan, and then fail with "unsupported
// resource type".
func TestValidateContainerServiceAcceptsAWellFormedOne(t *testing.T) {
	p := &AWSProvider{stateDir: t.TempDir(), passphrase: "test"}

	if err := p.Validate(context.Background(), containerResource(nil), spec.Policies{}); err != nil {
		t.Fatalf("Validate() = %v, want a well-formed container_service to be accepted", err)
	}
}

// TestValidateContainerServiceRejectsZones: Fargate places tasks itself
// across the subnets it is given, so a pinned zone is a placement the user
// asked for and will not get.
func TestValidateContainerServiceRejectsZones(t *testing.T) {
	p := &AWSProvider{stateDir: t.TempDir(), passphrase: "test"}

	r := containerResource(func(r *spec.Resource) {
		r.Scope.Zones = []string{"eu-central-1b"}
	})

	if err := p.Validate(context.Background(), r, spec.Policies{}); !errors.Is(err, ErrZonesNotSupported) {
		t.Fatalf("Validate() = %v, want ErrZonesNotSupported", err)
	}
}

// declaredScheduledContainer runs a program declaring a container service
// and its power schedule.
func declaredScheduledContainer(t *testing.T, replicas int) []recordedResource {
	t.Helper()

	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() = %v", err)
	}
	props, err := decodeContainerServiceProperties(containerProps(func(p map[string]any) {
		p["replicas"] = replicas
	}), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}

	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, containerResource(nil), testNetwork(), *props, rules)
		return err
	})
}

// TestDeclareContainerSchedule covers RFC 017 §2.5 on AWS: "off" is zero
// replicas, not a stopped task.
//
// A container service has no power state, so unlike a database or a VM
// both rules call the same API — ecs:UpdateService — and differ only in
// the desired count they send. That is the assertion worth having: a
// schedule that called a start/stop API here would be calling one that
// does not exist for this resource type.
func TestDeclareContainerSchedule(t *testing.T) {
	recorded := declaredScheduledContainer(t, 3)

	schedules := resourcesOfType(recorded, scheduleToken)
	if len(schedules) != 2 {
		t.Fatalf("declared %d schedules, want 2 (one start, one stop)", len(schedules))
	}

	counts := map[float64]bool{}
	for _, s := range schedules {
		target := s.Inputs["target"].ObjectValue()
		if got := target["arn"].StringValue(); got != updateServiceTarget {
			t.Errorf("schedule target = %q, want %q — ECS has no start/stop API", got, updateServiceTarget)
		}

		var payload struct {
			Cluster      string
			Service      string
			DesiredCount float64
		}
		if err := json.Unmarshal([]byte(target["input"].StringValue()), &payload); err != nil {
			t.Fatalf("schedule input is not valid JSON: %v", err)
		}
		if payload.Service == "" || payload.Cluster == "" {
			t.Errorf("schedule input names no service or cluster: %+v", payload)
		}
		counts[payload.DesiredCount] = true
	}

	// One rule restores the requested replicas, the other takes them to
	// zero. Both sending the same count would be a schedule that runs and
	// changes nothing.
	if !counts[3] {
		t.Errorf("no schedule restores the requested 3 replicas; got counts %v", counts)
	}
	if !counts[0] {
		t.Errorf("no schedule scales to zero; got counts %v", counts)
	}
}

// TestDeclareContainerScheduleGrantsOnlyUpdateService: the schedule's role
// may scale this one service and do nothing else.
func TestDeclareContainerScheduleGrantsOnlyUpdateService(t *testing.T) {
	recorded := declaredScheduledContainer(t, 1)

	policies := resourcesOfType(recorded, iamRolePolicyToken)
	if len(policies) != 1 {
		t.Fatalf("declared %d role policies, want 1", len(policies))
	}

	var document struct {
		Statement []struct {
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(policies[0].Inputs["policy"].StringValue()), &document); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}
	if len(document.Statement) != 1 {
		t.Fatalf("permission policy has %d statements, want 1", len(document.Statement))
	}
	if got := document.Statement[0].Action; len(got) != 1 || got[0] != "ecs:UpdateService" {
		t.Errorf("granted actions = %v, want exactly [ecs:UpdateService]", got)
	}
	for _, arn := range document.Statement[0].Resource {
		if arn == "*" {
			t.Error("the schedule role is granted on *, want the one service")
		}
		if !strings.Contains(arn, ":service/") {
			t.Errorf("granted on %q, want the container service's own ARN", arn)
		}
	}
}

// TestDeclareContainerServiceWithoutScheduleDeclaresNoScheduleResources:
// an unscheduled service must carry no scheduling machinery at all, not
// a disabled one.
func TestDeclareContainerServiceWithoutScheduleDeclaresNoScheduleResources(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if hasResource(recorded, scheduleToken) {
		t.Error("a schedule was declared for an unscheduled service")
	}
}

// mountingContainerResource is a service that mounts volumes its scope
// already holds.
//
// It carries the scope's environment rather than the empty one every other
// fixture here uses, because the creation token both halves derive is built
// from it: a service in another environment looks for another filesystem,
// which is the whole of what "the volume belongs to the scope" means.
func mountingContainerResource(volumes []spec.Volume) spec.Resource {
	return containerResource(func(r *spec.Resource) {
		r.Scope.Environment = testScope().Environment
		r.Volumes = volumes
	})
}

func declaredMountingContainer(t *testing.T, volumes []spec.Volume) []recordedResource {
	t.Helper()

	props, err := decodeContainerServiceProperties(containerProps(nil), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, mountingContainerResource(volumes), testNetwork(), *props, nil)
		return err
	})
}

// TestDeclareContainerServiceMountsItsVolumes covers the AWS row of RFC
// 020 §2.8's mount table.
//
// Two volumes rather than one, because every assertion here is about
// *which* filesystem a mount reaches. A single volume would pass just as
// well against an implementation that looked the first one up and wired
// every mount to it — which is the shape of the failure §1 describes, two
// names resolving to one share.
func TestDeclareContainerServiceMountsItsVolumes(t *testing.T) {
	volumes := []spec.Volume{
		{Name: "uploads", MountPath: "/var/lib/uploads"},
		// A size the AWS row of §2.7 ignores: EFS capacity is elastic, so
		// declaring one here must change nothing about the mount.
		{Name: "cache", MountPath: "/var/cache/app", SizeGB: 100},
	}
	recorded := declaredMountingContainer(t, volumes)

	// Lookup, not create. A filesystem declared here would be a second one
	// beside the scope's, and the two services meant to share it would each
	// see an empty directory — the silent wrong answer RFC 020 §1 exists to
	// prevent.
	for _, token := range []string{
		fileSystemToken, accessPointToken, mountTargetToken, efsBackupPolicyToken,
	} {
		if n := len(resourcesOfType(recorded, token)); n != 0 {
			t.Errorf("the resource stack declared %d %q; the scope's network stack owns them", n, token)
		}
	}

	lookedUp := map[string]bool{}
	for _, l := range resourcesOfType(recorded, getFileSystemToken) {
		lookedUp[l.Inputs["creationToken"].StringValue()] = true
	}
	if len(lookedUp) != len(volumes) {
		t.Fatalf("looked up %d distinct filesystems, want one per volume (%d)", len(lookedUp), len(volumes))
	}

	// Each volume is reached by the token the network stack set. A
	// filesystem sought under any other token is one that does not exist.
	fileSystemOf := make(map[string]string, len(volumes))
	for _, v := range volumes {
		token := fileSystemTokenFor(testScope(), v.Name)
		if !lookedUp[token] {
			t.Fatalf("volume %q was not looked up by its creation token %q; tokens sought = %v",
				v.Name, token, lookedUp)
		}
		fileSystemOf[v.Name] = testFileSystemID(token)
	}

	assertTaskVolumes(t, recorded, fileSystemOf)
	assertMountPoints(t, recorded, volumes)
	assertTaskRoleMounts(t, recorded, fileSystemOf)
}

// assertTaskVolumes covers the task definition's half of the mount: each
// volume names its own filesystem, and reaches it encrypted and through
// the access point rather than as root over the whole share.
func assertTaskVolumes(t *testing.T, recorded []recordedResource, fileSystemOf map[string]string) {
	t.Helper()

	task := findResource(t, recorded, ecsTaskDefinitionToken)
	declared := task.Inputs["volumes"].ArrayValue()
	if len(declared) != len(fileSystemOf) {
		t.Fatalf("the task definition declares %d volumes, want %d", len(declared), len(fileSystemOf))
	}

	for _, entry := range declared {
		volume := entry.ObjectValue()
		name := volume["name"].StringValue()
		wantFileSystem, ok := fileSystemOf[name]
		if !ok {
			t.Fatalf("the task definition declares a volume %q the resource never asked for", name)
		}

		config := volume["efsVolumeConfiguration"].ObjectValue()
		if got := config["fileSystemId"].StringValue(); got != wantFileSystem {
			t.Errorf("volume %q mounts filesystem %q, want %q — the one the scope holds under its creation token",
				name, got, wantFileSystem)
		}
		// Not decoration: the file system policy of §2.6 denies anything
		// arriving over an unencrypted connection, so a mount without this
		// is one the filesystem itself refuses.
		if got := config["transitEncryption"].StringValue(); got != "ENABLED" {
			t.Errorf("volume %q: transitEncryption = %q, want ENABLED; the file system policy denies plaintext",
				name, got)
		}

		auth := config["authorizationConfig"].ObjectValue()
		want := testAccessPointID(wantFileSystem)
		if got := auth["accessPointId"].StringValue(); got != want {
			t.Errorf("volume %q: accessPointId = %q, want %q; without it the task reaches the whole "+
				"filesystem as root, whatever the image runs as", name, got, want)
		}
		// IAM authorization is what makes the task role's grant apply at
		// all. Without it the mount is authorized by the network alone.
		if got := auth["iam"].StringValue(); got != "ENABLED" {
			t.Errorf("volume %q: iam = %q, want ENABLED", name, got)
		}

		// AWS admits a root directory beside an access point only when it
		// is "/", and the access point already pins /<volume>.
		if root, ok := config["rootDirectory"]; ok && root.IsString() && root.StringValue() != "/" {
			t.Errorf("volume %q: rootDirectory = %q beside an access point; AWS admits only %q or nothing",
				name, root.StringValue(), "/")
		}
	}
}

// assertMountPoints covers the container's half: a volume the task
// definition declares but the container never mounts is a filesystem
// nothing can reach, which is a deployment that reports success and serves
// an empty directory.
func assertMountPoints(t *testing.T, recorded []recordedResource, volumes []spec.Volume) {
	t.Helper()

	task := findResource(t, recorded, ecsTaskDefinitionToken)

	var definitions []struct {
		MountPoints []struct {
			SourceVolume  string `json:"sourceVolume"`
			ContainerPath string `json:"containerPath"`
		} `json:"mountPoints"`
	}
	if err := json.Unmarshal([]byte(task.Inputs["containerDefinitions"].StringValue()), &definitions); err != nil {
		t.Fatalf("container definitions are not valid JSON: %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("declared %d container definitions, want 1", len(definitions))
	}

	paths := map[string]string{}
	for _, mp := range definitions[0].MountPoints {
		paths[mp.SourceVolume] = mp.ContainerPath
	}
	if len(paths) != len(volumes) {
		t.Fatalf("the container declares %d mount points, want one per volume (%d)", len(paths), len(volumes))
	}
	for _, v := range volumes {
		if got := paths[v.Name]; got != v.MountPath {
			t.Errorf("volume %q appears at %q inside the container, want the requested %q",
				v.Name, got, v.MountPath)
		}
	}
}

// assertTaskRoleMounts covers the identity half of RFC 020 §2.6: the task
// role receives its first permission ever — RFC 017 §2.6 declared it empty
// on purpose — and it names the filesystems this service mounts and
// nothing else.
func assertTaskRoleMounts(t *testing.T, recorded []recordedResource, fileSystemOf map[string]string) {
	t.Helper()

	policies := resourcesOfType(recorded, iamRolePolicyToken)
	if len(policies) != 1 {
		t.Fatalf("declared %d inline role policies, want 1 (the mount grant)", len(policies))
	}
	policy := policies[0]

	// On the task role, which is what the container runs as. The execution
	// role belongs to the ECS agent, and a grant there would let the agent
	// mount while the container still could not.
	if role := policy.Inputs["role"].StringValue(); !strings.Contains(role, "task-role") {
		t.Errorf("the mount grant is attached to %q, want the task role", role)
	}

	var document struct {
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(policy.Inputs["policy"].StringValue()), &document); err != nil {
		t.Fatalf("the mount policy is not valid JSON: %v", err)
	}
	if len(document.Statement) != 1 {
		t.Fatalf("the mount policy has %d statements, want 1", len(document.Statement))
	}
	statement := document.Statement[0]

	if statement.Effect != "Allow" {
		t.Errorf("the mount statement has effect %q, want Allow", statement.Effect)
	}
	// Exactly two actions, which is also how ClientRootAccess is refused:
	// it is the one permission that would make the access point's non-root
	// user pointless, and an allowlist rejects it without naming it.
	wanted := map[string]bool{
		"elasticfilesystem:ClientMount": true,
		"elasticfilesystem:ClientWrite": true,
	}
	if len(statement.Action) != len(wanted) {
		t.Errorf("granted actions = %v, want exactly ClientMount and ClientWrite", statement.Action)
	}
	for _, action := range statement.Action {
		if !wanted[action] {
			t.Errorf("granted %q; ClientMount and ClientWrite are all a mount needs", action)
		}
	}

	arns := map[string]bool{}
	for _, id := range fileSystemOf {
		arns[testFileSystemARN(id)] = true
	}
	if len(statement.Resource) != len(arns) {
		t.Errorf("granted on %v, want one ARN per mounted filesystem (%d)", statement.Resource, len(arns))
	}
	for _, arn := range statement.Resource {
		if arn == "*" {
			t.Error("the task role is granted on *, want the filesystems this service mounts")
		}
		if !arns[arn] {
			t.Errorf("granted on %q, which is not a filesystem this service mounts", arn)
		}
	}
}

// TestDeclareContainerServiceWithoutVolumesGrantsTheTaskRoleNothing keeps
// RFC 017 §2.6 true for every service that mounts nothing: the task role's
// first permission arrives with a volume, and only with one.
func TestDeclareContainerServiceWithoutVolumesGrantsTheTaskRoleNothing(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if n := len(resourcesOfType(recorded, iamRolePolicyToken)); n != 0 {
		t.Errorf("declared %d inline role policies for a service that mounts nothing, want none", n)
	}
	for _, token := range []string{getFileSystemToken, getAccessPointsToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was invoked for a service with no volumes", token)
		}
	}
	if volumes := findResource(t, recorded, ecsTaskDefinitionToken).Inputs["volumes"]; volumes.IsArray() {
		if n := len(volumes.ArrayValue()); n != 0 {
			t.Errorf("the task definition declares %d volumes, want none", n)
		}
	}
}

// TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave covers
// the lookup-not-create discipline of RFC 020 §2.8.
//
// The Engine provisions a scope's network stack before any resource in it,
// so a filesystem absent here was removed out of band rather than lost to
// a race. Creating a replacement would be §1's silent wrong answer — a
// second, empty share where a shared one was meant to be — so the provider
// refuses and names what it could not find.
func TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave(t *testing.T) {
	tests := []struct {
		name    string
		efs     efsScope
		wantMsg string
	}{
		{
			name:    "no filesystem answers to the creation token",
			efs:     efsScope{missingToken: fileSystemTokenFor(testScope(), "uploads")},
			wantMsg: "uploads",
		},
		{
			// The filesystem is there and the identity a task would reach
			// it through is not. Mounting anyway would build a task
			// definition that fails at task start, long after the plan was
			// approved.
			name:    "the filesystem carries no access point",
			efs:     efsScope{withoutAccessPoints: true},
			wantMsg: "access point",
		},
	}

	props, err := decodeContainerServiceProperties(containerProps(nil), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	r := mountingContainerResource([]spec.Volume{{Name: "uploads", MountPath: "/var/lib/uploads"}})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pulumi.RunErr(func(ctx *pulumi.Context) error {
				_, err := declareContainerService(ctx, r, testNetwork(), *props, nil)
				return err
			}, pulumi.WithMocks("cloudsdd-aws", "test", mockMonitor{rec: &recorder{}, efs: tt.efs}))

			if !errors.Is(err, ErrFilesystemMissing) {
				t.Fatalf("declareContainerService() = %v, want ErrFilesystemMissing", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantMsg)
			}
		})
	}
}
