// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/cloudscheduler"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/sql"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Power scheduling on GCP is a Cloud Scheduler job calling the Cloud SQL
// Admin API directly, authenticated with an OAuth token minted for a
// dedicated service account (RFC 012 §4.2). Like the AWS implementation,
// and unlike Azure's, it needs no deployed code artifact.
const (
	// sqlAdminEndpoint is the Cloud SQL Admin API instance resource.
	sqlAdminEndpoint = "https://sqladmin.googleapis.com/v1/projects/%s/instances/%s"

	// activationPolicyOn and activationPolicyOff are how Cloud SQL
	// expresses a powered-on and a powered-off instance.
	activationPolicyOn  = "ALWAYS"
	activationPolicyOff = "NEVER"

	// schedulerRoleTitle names the custom role in the console.
	schedulerRoleTitle = "CloudSDD power schedule"
)

// schedulerPermissions is what the scheduler's service account may do.
//
// Cloud SQL does not support resource-level IAM bindings, so the binding
// is necessarily project-wide; a custom role with these two permissions
// is therefore the tightest grant available, and materially tighter than
// the roles/cloudsql.admin the obvious implementation would reach for
// (RFC 012 §7).
var schedulerPermissions = []string{
	"cloudsql.instances.get",
	"cloudsql.instances.update",
}

// resourceSchedule resolves and compiles the power schedule governing r,
// rejecting anything GCP cannot express.
func resourceSchedule(r spec.Resource, policies spec.Policies) ([]schedule.Rule, error) {
	sch := spec.EffectiveSchedule(r, policies)
	if !sch.IsEnabled() {
		return nil, nil
	}
	if !r.Type.SupportsSchedule() {
		if r.Schedule != nil {
			return nil, fmt.Errorf("gcp: resource %q: %w", r.ID, ErrResourceNotSchedulable)
		}
		return nil, nil
	}

	rules, err := schedule.Compile(sch)
	if err != nil {
		return nil, fmt.Errorf("gcp: resource %q: %w", r.ID, err)
	}

	// Neither scheduling mechanism on GCP can bound a rule to a date
	// range, so an exception window cannot be expressed. Reporting that
	// here is the whole point: silently dropping the window would leave an
	// environment running through a shutdown the user believed they had
	// scheduled.
	for _, rule := range rules {
		if rule.Bounded() {
			return nil, fmt.Errorf("gcp: resource %q: %w: %s; remove schedule.exceptions or deploy this resource on AWS or Azure",
				r.ID, ErrScheduleExceptionsUnsupported, exceptionLimitReason(r.Type))
		}
	}
	return rules, nil
}

// exceptionLimitReason explains why a resource type cannot carry exception
// windows on GCP. The two reasons are genuinely different, and an error
// that named the wrong one would send the reader looking in the wrong
// place (RFC 013 §2.5).
func exceptionLimitReason(t spec.ResourceType) string {
	if t == spec.ResourceTypeComputeInstance {
		return "an instance accepts one schedule policy, with a single validity interval"
	}
	return "a Cloud Scheduler job has no validity period"
}

// declareDatabaseSchedule registers the Cloud Scheduler jobs that power a
// Cloud SQL instance on and off, along with the service account and
// custom role they act through.
func declareDatabaseSchedule(
	ctx *pulumi.Context,
	resourceID, region string,
	instance *sql.DatabaseInstance,
	rules []schedule.Rule,
) error {
	if len(rules) == 0 {
		return nil
	}

	account, err := serviceaccount.NewAccount(ctx, resourceID+"-scheduler-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(serviceAccountID(resourceID)),
		DisplayName: pulumi.String(fmt.Sprintf("CloudSDD power schedule for %s", resourceID)),
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare scheduler service account for %q: %w", resourceID, err)
	}

	role, err := projects.NewIAMCustomRole(ctx, resourceID+"-scheduler-role", &projects.IAMCustomRoleArgs{
		RoleId:      pulumi.String(customRoleID(resourceID)),
		Title:       pulumi.String(schedulerRoleTitle),
		Description: pulumi.String(fmt.Sprintf("Starts and stops %s on a schedule", resourceID)),
		Permissions: pulumi.ToStringArray(schedulerPermissions),
		Project:     instance.Project,
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare scheduler role for %q: %w", resourceID, err)
	}

	if _, err := projects.NewIAMMember(ctx, resourceID+"-scheduler-binding", &projects.IAMMemberArgs{
		Project: instance.Project,
		Role:    role.Name,
		Member:  account.Email.ApplyT(func(email string) string { return "serviceAccount:" + email }).(pulumi.StringOutput),
	}); err != nil {
		return fmt.Errorf("gcp: failed to bind scheduler role for %q: %w", resourceID, err)
	}

	uri := pulumi.All(instance.Project, instance.Name).ApplyT(func(v []any) string {
		return fmt.Sprintf(sqlAdminEndpoint, v[0].(string), v[1].(string))
	}).(pulumi.StringOutput)

	for _, rule := range rules {
		if err := declareScheduleJob(ctx, resourceID, region, rule, uri, account); err != nil {
			return err
		}
	}
	return nil
}

// declareScheduleJob registers one compiled Rule as a Cloud Scheduler job.
func declareScheduleJob(
	ctx *pulumi.Context,
	resourceID, region string,
	rule schedule.Rule,
	uri pulumi.StringOutput,
	account *serviceaccount.Account,
) error {
	expression, err := unixCronExpression(rule)
	if err != nil {
		return err
	}

	body, err := activationPolicyBody(rule.Action)
	if err != nil {
		return err
	}

	name := resourceID + "-" + rule.Name
	if _, err := cloudscheduler.NewJob(ctx, name, &cloudscheduler.JobArgs{
		Name:        pulumi.String(name),
		Region:      pulumi.String(region),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s for %s", rule.Action, resourceID)),
		Schedule:    pulumi.String(expression),
		TimeZone:    pulumi.String(rule.Timezone),
		HttpTarget: &cloudscheduler.JobHttpTargetArgs{
			Uri:        uri,
			HttpMethod: pulumi.String("PATCH"),
			Body:       pulumi.String(body),
			Headers:    pulumi.StringMap{"Content-Type": pulumi.String("application/json")},
			OauthToken: &cloudscheduler.JobHttpTargetOauthTokenArgs{
				ServiceAccountEmail: account.Email,
			},
		},
	}); err != nil {
		return fmt.Errorf("gcp: failed to declare schedule job %q: %w", name, err)
	}
	return nil
}

// activationPolicyBody builds the Cloud SQL patch payload, base64-encoded
// as the Cloud Scheduler API requires.
func activationPolicyBody(action schedule.Action) (string, error) {
	policy := activationPolicyOff
	if action == schedule.ActionStart {
		policy = activationPolicyOn
	}

	payload, err := json.Marshal(map[string]any{
		"settings": map[string]string{"activationPolicy": policy},
	})
	if err != nil {
		return "", fmt.Errorf("gcp: failed to build schedule body: %w", err)
	}
	return base64.StdEncoding.EncodeToString(payload), nil
}

// unixCronExpression renders a Rule in the five-field unix cron dialect
// Cloud Scheduler uses: minute hour day-of-month month day-of-week.
//
// Weekdays are emitted numerically (Sunday 0) rather than by name: the
// numeric form is the one every unix-cron implementation agrees on.
func unixCronExpression(rule schedule.Rule) (string, error) {
	if rule.Hour < 0 || rule.Hour > 23 || rule.Min < 0 || rule.Min > 59 {
		return "", fmt.Errorf("gcp: schedule rule %q has an invalid time %02d:%02d", rule.Name, rule.Hour, rule.Min)
	}

	dayOfWeek := "*"
	if len(rule.Days) > 0 {
		days := make([]string, 0, len(rule.Days))
		for _, d := range rule.Days {
			wd, ok := d.ToTime()
			if !ok {
				return "", fmt.Errorf("gcp: schedule rule %q has an unknown weekday %q", rule.Name, d)
			}
			days = append(days, fmt.Sprintf("%d", int(wd)))
		}
		dayOfWeek = strings.Join(days, ",")
	}

	return fmt.Sprintf("%d %d * * %s", rule.Min, rule.Hour, dayOfWeek), nil
}

// serviceAccountID derives a Google-acceptable account id from a resource
// id: 6 to 30 characters, lowercase letters, digits and hyphens, starting
// with a letter.
func serviceAccountID(resourceID string) string {
	const (
		suffix    = "-sched"
		maxLength = 30
	)

	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, resourceID)

	// Must start with a letter, and a trailing hyphen before the suffix
	// would produce a double separator.
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" || sanitized[0] < 'a' || sanitized[0] > 'z' {
		sanitized = "r" + sanitized
	}
	if budget := maxLength - len(suffix); len(sanitized) > budget {
		sanitized = strings.TrimRight(sanitized[:budget], "-")
	}
	return sanitized + suffix
}

// customRoleID derives a role id from a resource id. Role ids admit
// letters, digits, underscores and dots, but explicitly not hyphens,
// which resource ids do allow.
func customRoleID(resourceID string) string {
	const (
		prefix    = "cloudsddPowerSchedule_"
		maxLength = 64
	)

	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, resourceID)

	if budget := maxLength - len(prefix); len(sanitized) > budget {
		sanitized = sanitized[:budget]
	}
	return prefix + sanitized
}
