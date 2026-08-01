// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/scheduler"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Power scheduling on AWS is built on EventBridge Scheduler's *universal
// targets*, which call an AWS API directly through an execution role
// (RFC 012 §4.1).
//
// The alternative — a Lambda that calls the RDS API — would mean shipping,
// versioning and patching a code artifact for what is fundamentally
// configuration. The universal target keeps the whole feature declarative.
const (
	// schedulerServicePrincipal is the service that assumes the execution
	// role when a schedule fires.
	schedulerServicePrincipal = "scheduler.amazonaws.com"

	// startDBInstanceTarget and stopDBInstanceTarget are the universal
	// target ARNs for the two RDS operations. The empty region and
	// account fields are part of the format: a universal target names a
	// service API, not a resource.
	startDBInstanceTarget = "arn:aws:scheduler:::aws-sdk:rds:startDBInstance"
	stopDBInstanceTarget  = "arn:aws:scheduler:::aws-sdk:rds:stopDBInstance"

	// defaultScheduleGroup is the schedule group new schedules land in.
	// It appears in the ARN the trust policy constrains.
	defaultScheduleGroup = "default"

	// maxScheduleNameLength is the EventBridge Scheduler limit.
	maxScheduleNameLength = 64
)

// scheduleRetryAttempts bounds retries of a failed invocation.
//
// The AWS default is 185. Most failures here are permanent — asking RDS
// to stop an instance that is already stopped is an error, not a blip —
// so retrying that many times only generates noise. A few attempts still
// absorb a genuine transient fault.
const scheduleRetryAttempts = 3

// resourceSchedule resolves and compiles the power schedule governing r.
// It returns no rules, and no error, when the resource is not scheduled.
func resourceSchedule(r spec.Resource, policies spec.Policies) ([]schedule.Rule, error) {
	sch := spec.EffectiveSchedule(r, policies)
	if !sch.IsEnabled() {
		return nil, nil
	}
	if !r.Type.SupportsSchedule() {
		// An inherited schedule over a bucket is inapplicable rather than
		// wrong; the Engine already made that distinction and only an
		// explicit one reaches here as an error (RFC 012 §3).
		if r.Schedule != nil {
			return nil, fmt.Errorf("aws: resource %q: %w", r.ID, ErrResourceNotSchedulable)
		}
		return nil, nil
	}

	rules, err := schedule.Compile(sch)
	if err != nil {
		return nil, fmt.Errorf("aws: resource %q: %w", r.ID, err)
	}
	return rules, nil
}

// declareDatabaseSchedule registers the EventBridge schedules that power
// an RDS instance on and off, plus the least-privilege role they assume.
//
// It is declared inside the same Pulumi program as the instance, so the
// schedules share its stack identity and the existing Destroy path tears
// them down with no extra work.
func declareDatabaseSchedule(
	ctx *pulumi.Context,
	resourceID string,
	instance *rds.Instance,
	rules []schedule.Rule,
	opts ...pulumi.ResourceOption,
) error {
	if len(rules) == 0 {
		return nil
	}

	prefix := schedulePrefix(resourceID, rules)

	// The account and region are read out of the instance ARN rather than
	// through an aws:getCallerIdentity invoke: the schedules necessarily
	// live in the same account and region as the instance they manage, so
	// the ARN already carries the answer, and the program stays free of
	// invokes that would have to be threaded through assumed-credential
	// providers.
	trustPolicy := instance.Arn.ApplyT(func(arn string) (string, error) {
		return buildSchedulerTrustPolicy(arn, prefix)
	}).(pulumi.StringOutput)

	role, err := iam.NewRole(ctx, resourceID+"-scheduler-role", &iam.RoleArgs{
		AssumeRolePolicy: trustPolicy,
		Description:      pulumi.String(fmt.Sprintf("CloudSDD power schedule for %s", resourceID)),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare scheduler role for resource %q: %w", resourceID, err)
	}

	permissionPolicy := instance.Arn.ApplyT(buildSchedulerPermissionPolicy).(pulumi.StringOutput)

	if _, err := iam.NewRolePolicy(ctx, resourceID+"-scheduler-permissions", &iam.RolePolicyArgs{
		Role:   role.ID(),
		Policy: permissionPolicy,
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare scheduler policy for resource %q: %w", resourceID, err)
	}

	for _, rule := range rules {
		if err := declareScheduleRule(ctx, prefix, rule, instance, role, opts...); err != nil {
			return err
		}
	}
	return nil
}

// declareScheduleRule registers one compiled Rule as an EventBridge
// schedule.
func declareScheduleRule(
	ctx *pulumi.Context,
	prefix string,
	rule schedule.Rule,
	instance *rds.Instance,
	role *iam.Role,
	opts ...pulumi.ResourceOption,
) error {
	expression, err := cronExpression(rule)
	if err != nil {
		return err
	}

	targetARN := stopDBInstanceTarget
	if rule.Action == schedule.ActionStart {
		targetARN = startDBInstanceTarget
	}

	// The universal target takes the RDS API's own request payload.
	input := instance.Identifier.ApplyT(func(identifier string) (string, error) {
		payload, err := json.Marshal(map[string]string{"DbInstanceIdentifier": identifier})
		if err != nil {
			return "", fmt.Errorf("aws: failed to build schedule input: %w", err)
		}
		return string(payload), nil
	}).(pulumi.StringOutput)

	args := &scheduler.ScheduleArgs{
		Name:                       pulumi.String(prefix + rule.Name),
		GroupName:                  pulumi.String(defaultScheduleGroup),
		ScheduleExpression:         pulumi.String(expression),
		ScheduleExpressionTimezone: pulumi.String(rule.Timezone),
		State:                      pulumi.String("ENABLED"),
		// A start-of-workday action must happen at the time it says, not
		// smeared across a window.
		FlexibleTimeWindow: &scheduler.ScheduleFlexibleTimeWindowArgs{
			Mode: pulumi.String("OFF"),
		},
		Target: &scheduler.ScheduleTargetArgs{
			Arn:     pulumi.String(targetARN),
			RoleArn: role.Arn,
			Input:   input,
			RetryPolicy: &scheduler.ScheduleTargetRetryPolicyArgs{
				MaximumRetryAttempts: pulumi.Int(scheduleRetryAttempts),
			},
		},
	}

	if rule.ValidFrom != nil {
		args.StartDate = pulumi.String(scheduleStartDate(*rule.ValidFrom))
	}
	if rule.ValidTo != nil {
		args.EndDate = pulumi.String(rule.ValidTo.UTC().Format(time.RFC3339))
	}

	if _, err := scheduler.NewSchedule(ctx, prefix+rule.Name, args, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare schedule %q: %w", rule.Name, err)
	}
	return nil
}

// scheduleStartDate renders a rule's lower bound for the API.
//
// EventBridge documents StartDate as the date "after which the schedule
// can begin invoking its target", so an occurrence falling exactly on the
// boundary may be skipped. Exception windows open at local midnight and
// their rules fire at local midnight, which is precisely that case, so
// the bound is backdated by a second. The second belongs to the segment
// before it, in which nothing is scheduled to fire.
func scheduleStartDate(from time.Time) string {
	return from.Add(-time.Second).UTC().Format(time.RFC3339)
}

// cronExpression renders a Rule in EventBridge's six-field cron dialect:
// cron(minutes hours day-of-month month day-of-week year).
//
// Exactly one of day-of-month and day-of-week must be "?", which is why
// this cannot be shared with the five-field unix cron the other providers
// use.
func cronExpression(rule schedule.Rule) (string, error) {
	if rule.Hour < 0 || rule.Hour > 23 || rule.Min < 0 || rule.Min > 59 {
		return "", fmt.Errorf("aws: schedule rule %q has an invalid time %02d:%02d", rule.Name, rule.Hour, rule.Min)
	}

	dayOfMonth, dayOfWeek := "*", "?"
	if len(rule.Days) > 0 {
		days := make([]string, 0, len(rule.Days))
		for _, d := range rule.Days {
			days = append(days, strings.ToUpper(string(d)))
		}
		dayOfMonth, dayOfWeek = "?", strings.Join(days, ",")
	}

	return fmt.Sprintf("cron(%d %d %s * %s *)", rule.Min, rule.Hour, dayOfMonth, dayOfWeek), nil
}

// schedulePrefix builds the common prefix of every schedule name for a
// resource, truncating the resource ID so the longest generated name
// still fits EventBridge's 64-character limit.
//
// The prefix is also what the trust policy's aws:SourceArn condition
// matches, so the two must be derived from the same value.
func schedulePrefix(resourceID string, rules []schedule.Rule) string {
	longest := 0
	for _, r := range rules {
		if len(r.Name) > longest {
			longest = len(r.Name)
		}
	}

	// One character for the separator.
	budget := maxScheduleNameLength - longest - 1
	if budget < 1 {
		budget = 1
	}
	if len(resourceID) > budget {
		resourceID = resourceID[:budget]
	}
	return resourceID + "-"
}

// buildSchedulerTrustPolicy builds the execution role's trust policy.
//
// scheduler.amazonaws.com is an AWS service principal that will assume
// this role on behalf of whoever asks unless constrained, so the
// aws:SourceAccount and aws:SourceArn conditions are mandatory rather
// than optional hardening (RFC 012 §7, the confused deputy). The ARN is
// necessarily a prefix match: conditioning on a concrete schedule ARN
// would be circular, since the schedules reference this role.
func buildSchedulerTrustPolicy(instanceARN, namePrefix string) (string, error) {
	partition, region, account, err := parseARN(instanceARN)
	if err != nil {
		return "", err
	}

	sourceARN := fmt.Sprintf("arn:%s:scheduler:%s:%s:schedule/%s/%s*",
		partition, region, account, defaultScheduleGroup, namePrefix)

	doc := iamPolicyDocument{
		Version: "2012-10-17",
		Statement: []iamStatement{{
			Effect:    "Allow",
			Principal: &iamPrincipal{Service: schedulerServicePrincipal},
			Action:    []string{"sts:AssumeRole"},
			Condition: map[string]map[string]any{
				"StringEquals": {"aws:SourceAccount": account},
				"ArnLike":      {"aws:SourceArn": sourceARN},
			},
		}},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("aws: failed to build scheduler trust policy: %w", err)
	}
	return string(b), nil
}

// buildSchedulerPermissionPolicy builds the inline policy the schedules
// act through: two actions, one resource, no wildcards. A scheduling
// feature that provisioned a broadly-privileged role would be a worse
// trade than the money it saves.
func buildSchedulerPermissionPolicy(instanceARN string) (string, error) {
	doc := iamPolicyDocument{
		Version: "2012-10-17",
		Statement: []iamStatement{{
			Effect:   "Allow",
			Action:   []string{"rds:StartDBInstance", "rds:StopDBInstance"},
			Resource: []string{instanceARN},
		}},
	}

	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("aws: failed to build scheduler permission policy: %w", err)
	}
	return string(b), nil
}

// parseARN extracts the partition, region and account from an ARN.
func parseARN(arn string) (partition, region, account string, err error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" {
		return "", "", "", fmt.Errorf("aws: %w: %q", ErrMalformedARN, arn)
	}
	if parts[1] == "" || parts[3] == "" || parts[4] == "" {
		return "", "", "", fmt.Errorf("aws: %w: %q", ErrMalformedARN, arn)
	}
	return parts[1], parts[3], parts[4], nil
}
