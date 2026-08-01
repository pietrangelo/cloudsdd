// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"
	"strings"
	"time"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/authorization"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/automation"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Azure has no equivalent of an EventBridge universal target or a Cloud
// Scheduler HTTP job: something has to call the ARM API. An Automation
// Account with a system-assigned identity and a runbook is the standard,
// least-surprising answer, and automation.Schedule supports StartTime and
// ExpiryTime, so exception windows work here as they do on AWS (RFC 012
// §4.3).
//
// It is the only provider that needs a deployed code artifact, which is
// why the runbook below is a fixed constant in this repository. Nothing
// from the Specification is ever interpolated into it: the action and the
// target are passed as runbook parameters, never as script text.
const powerRunbook = `param(
    [Parameter(Mandatory = $true)][string] $resourceid,
    [Parameter(Mandatory = $true)][ValidateSet('start', 'stop', 'deallocate')][string] $action,
    [Parameter(Mandatory = $true)][string] $apiversion
)

$ErrorActionPreference = 'Stop'

Disable-AzContextAutosave -Scope Process | Out-Null
Connect-AzAccount -Identity | Out-Null

$path = '{0}/{1}?api-version={2}' -f $resourceid, $action, $apiversion
$response = Invoke-AzRestMethod -Method POST -Path $path

if ($response.StatusCode -ge 400) {
    throw "ARM returned $($response.StatusCode): $($response.Content)"
}
`

// ARM API versions for the two Flexible Server providers. Pinned rather
// than tracking "latest": a schedule that silently starts calling a new
// API version is a schedule that can silently stop working.
const (
	postgresAPIVersion       = "2022-12-01"
	mysqlAPIVersion          = "2023-06-30"
	virtualMachineAPIVersion = "2023-09-01"
)

const (
	runbookName        = "cloudsdd-power"
	automationSKU      = "Basic"
	schedulerRoleTitle = "CloudSDD power schedule"

	// scheduleLeadTime is how far ahead of now a schedule's first
	// occurrence must fall. Azure rejects a start time less than five
	// minutes out; the extra margin absorbs a slow deployment.
	scheduleLeadTime = 15 * time.Minute
)

// timeNow is a seam so the first-occurrence calculation can be driven
// from a fixed clock in tests.
var timeNow = time.Now

// resourceSchedule resolves and compiles the power schedule governing r.
func resourceSchedule(r spec.Resource, policies spec.Policies) ([]schedule.Rule, error) {
	sch := spec.EffectiveSchedule(r, policies)
	if !sch.IsEnabled() {
		return nil, nil
	}
	if !r.Type.SupportsSchedule() {
		if r.Schedule != nil {
			return nil, fmt.Errorf("azure: resource %q: %w", r.ID, ErrResourceNotSchedulable)
		}
		return nil, nil
	}

	rules, err := schedule.Compile(sch)
	if err != nil {
		return nil, fmt.Errorf("azure: resource %q: %w", r.ID, err)
	}
	return rules, nil
}

// databaseServer is what a declared Flexible Server exposes to the
// resources built on top of it.
type databaseServer struct {
	engine        string
	id            pulumi.IDOutput
	resourceGroup *core.ResourceGroup
}

// apiVersion returns the ARM API version for this server's engine.
func (s databaseServer) apiVersion() (string, error) {
	switch strings.ToLower(s.engine) {
	case "postgres":
		return postgresAPIVersion, nil
	case "mysql":
		return mysqlAPIVersion, nil
	default:
		return "", fmt.Errorf("azure: no ARM API version known for engine %q", s.engine)
	}
}

// powerActions returns the RBAC actions the schedule's identity needs.
func (s databaseServer) powerActions() ([]string, error) {
	var provider string
	switch strings.ToLower(s.engine) {
	case "postgres":
		provider = "Microsoft.DBforPostgreSQL"
	case "mysql":
		provider = "Microsoft.DBforMySQL"
	default:
		return nil, fmt.Errorf("azure: no RBAC actions known for engine %q", s.engine)
	}
	return []string{
		provider + "/flexibleServers/read",
		provider + "/flexibleServers/start/action",
		provider + "/flexibleServers/stop/action",
	}, nil
}

// powerTarget describes an ARM resource a schedule powers on and off, so
// the Automation machinery is written once and each resource type
// contributes only what actually differs: which API version to call, which
// RBAC actions that needs, and which verb stops it.
type powerTarget struct {
	id            pulumi.IDOutput
	resourceGroup *core.ResourceGroup
	apiVersion    string
	actions       []string

	// stopAction is the ARM verb a schedule's stop rule invokes.
	//
	// It is a field rather than the constant "stop" because on a virtual
	// machine `stop` leaves the VM allocated and **still billing for
	// compute**; only `deallocate` releases the hardware. A scheduling
	// feature whose whole purpose is cost reduction, wired to the wrong
	// verb, would run correctly and save nothing (RFC 013 §2.5).
	stopAction string
}

// powerTarget describes the Flexible Server for the scheduler.
func (s databaseServer) powerTarget() (powerTarget, error) {
	apiVersion, err := s.apiVersion()
	if err != nil {
		return powerTarget{}, err
	}
	actions, err := s.powerActions()
	if err != nil {
		return powerTarget{}, err
	}
	return powerTarget{
		id:            s.id,
		resourceGroup: s.resourceGroup,
		apiVersion:    apiVersion,
		actions:       actions,
		stopAction:    "stop",
	}, nil
}

// declareDatabaseSchedule registers the Automation Account, runbook, role
// and schedules that power a Flexible Server on and off.
func declareDatabaseSchedule(
	ctx *pulumi.Context,
	resourceID string,
	server databaseServer,
	rules []schedule.Rule,
) error {
	if len(rules) == 0 {
		return nil
	}
	target, err := server.powerTarget()
	if err != nil {
		return err
	}
	return declarePowerSchedule(ctx, resourceID, target, rules)
}

// declarePowerSchedule registers the Automation Account, runbook, role and
// schedules that power an ARM resource on and off.
func declarePowerSchedule(
	ctx *pulumi.Context,
	resourceID string,
	server powerTarget,
	rules []schedule.Rule,
) error {
	if len(rules) == 0 {
		return nil
	}

	apiVersion := server.apiVersion
	actions := server.actions

	account, err := automation.NewAccount(ctx, resourceID+"-automation", &automation.AccountArgs{
		ResourceGroupName: server.resourceGroup.Name,
		Location:          server.resourceGroup.Location,
		SkuName:           pulumi.String(automationSKU),
		Identity: &automation.AccountIdentityArgs{
			Type: pulumi.String("SystemAssigned"),
		},
		// The account exists only to run one runbook against one server;
		// it has no reason to be reachable from the internet or to accept
		// key-based authentication.
		PublicNetworkAccessEnabled: pulumi.Bool(false),
		LocalAuthenticationEnabled: pulumi.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare automation account for %q: %w", resourceID, err)
	}

	// Scoped to this server alone, with three actions: read, start, stop.
	// Azure's built-in Contributor role would also work and would be far
	// too much (RFC 012 §7).
	roleDefinition, err := authorization.NewRoleDefinition(ctx, resourceID+"-power-role", &authorization.RoleDefinitionArgs{
		Name:        pulumi.String(fmt.Sprintf("%s (%s)", schedulerRoleTitle, resourceID)),
		Description: pulumi.String(fmt.Sprintf("Starts and stops %s on a schedule", resourceID)),
		Scope:       server.id.ToStringOutput(),
		Permissions: authorization.RoleDefinitionPermissionArray{
			authorization.RoleDefinitionPermissionArgs{Actions: pulumi.ToStringArray(actions)},
		},
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare power role for %q: %w", resourceID, err)
	}

	principalID := account.Identity.PrincipalId().Elem()
	if _, err := authorization.NewAssignment(ctx, resourceID+"-power-assignment", &authorization.AssignmentArgs{
		Scope:            server.id.ToStringOutput(),
		RoleDefinitionId: roleDefinition.RoleDefinitionResourceId,
		PrincipalId:      principalID,
	}); err != nil {
		return fmt.Errorf("azure: failed to assign the power role for %q: %w", resourceID, err)
	}

	runbook, err := automation.NewRunBook(ctx, resourceID+"-runbook", &automation.RunBookArgs{
		Name:                  pulumi.String(runbookName),
		AutomationAccountName: account.Name,
		ResourceGroupName:     server.resourceGroup.Name,
		Location:              server.resourceGroup.Location,
		RunbookType:           pulumi.String("PowerShell"),
		LogProgress:           pulumi.Bool(false),
		LogVerbose:            pulumi.Bool(false),
		Content:               pulumi.String(powerRunbook),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare power runbook for %q: %w", resourceID, err)
	}

	now := timeNow()
	for _, rule := range rules {
		if err := declareScheduleRule(ctx, resourceID, rule, now, server, account, runbook, apiVersion); err != nil {
			return err
		}
	}
	return nil
}

// declareScheduleRule registers one compiled Rule as an Automation
// schedule plus the job schedule that binds it to the runbook.
func declareScheduleRule(
	ctx *pulumi.Context,
	resourceID string,
	rule schedule.Rule,
	now time.Time,
	server powerTarget,
	account *automation.Account,
	runbook *automation.RunBook,
	apiVersion string,
) error {
	start, ok, err := firstOccurrence(rule, now)
	if err != nil {
		return err
	}
	if !ok {
		// The rule's window has already closed. Declaring it would be
		// rejected by Azure, and it has nothing left to do.
		return nil
	}

	name := resourceID + "-" + rule.Name

	args := &automation.ScheduleArgs{
		Name:                  pulumi.String(name),
		AutomationAccountName: account.Name,
		ResourceGroupName:     server.resourceGroup.Name,
		Description:           pulumi.String(fmt.Sprintf("CloudSDD %s for %s", rule.Action, resourceID)),
		Timezone:              pulumi.String(rule.Timezone),
		StartTime:             pulumi.String(start.Format(time.RFC3339)),
	}
	if len(rule.Days) > 0 {
		args.Frequency = pulumi.String("Week")
		args.WeekDays = pulumi.ToStringArray(weekDayNames(rule.Days))
	} else {
		args.Frequency = pulumi.String("Day")
	}
	if rule.ValidTo != nil {
		args.ExpiryTime = pulumi.String(rule.ValidTo.Format(time.RFC3339))
	}

	// StartTime only anchors the first occurrence; the recurrence is what
	// matters. Without this, every apply would recompute it against the
	// current clock and show a spurious diff.
	sched, err := automation.NewSchedule(ctx, name, args, pulumi.IgnoreChanges([]string{"startTime"}))
	if err != nil {
		return fmt.Errorf("azure: failed to declare schedule %q: %w", name, err)
	}

	// Runbook parameter names are normalized to lowercase by Azure
	// Automation, so they are written that way here too.
	if _, err := automation.NewJobSchedule(ctx, name+"-job", &automation.JobScheduleArgs{
		AutomationAccountName: account.Name,
		ResourceGroupName:     server.resourceGroup.Name,
		RunbookName:           runbook.Name,
		ScheduleName:          sched.Name,
		Parameters: pulumi.StringMap{
			"resourceid": server.id.ToStringOutput(),
			"action":     pulumi.String(armAction(rule.Action, server.stopAction)),
			"apiversion": pulumi.String(apiVersion),
		},
	}); err != nil {
		return fmt.Errorf("azure: failed to bind schedule %q to the runbook: %w", name, err)
	}
	return nil
}

// firstOccurrence computes the first time a Rule fires, at least
// scheduleLeadTime from now and within the rule's validity interval.
//
// Azure rejects a start time in the past, and unlike EventBridge's cron
// its schedules are anchored rather than purely recurrent, so the anchor
// has to be computed. The second return value is false when the rule's
// window has already closed and there is nothing left to schedule.
func firstOccurrence(rule schedule.Rule, now time.Time) (time.Time, bool, error) {
	loc, err := time.LoadLocation(rule.Timezone)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("azure: schedule rule %q has an unresolvable timezone %q: %w", rule.Name, rule.Timezone, err)
	}
	if rule.Hour < 0 || rule.Hour > 23 || rule.Min < 0 || rule.Min > 59 {
		return time.Time{}, false, fmt.Errorf("azure: schedule rule %q has an invalid time %02d:%02d", rule.Name, rule.Hour, rule.Min)
	}

	earliest := now.Add(scheduleLeadTime)
	if rule.ValidFrom != nil && rule.ValidFrom.After(earliest) {
		earliest = *rule.ValidFrom
	}

	allowed := map[time.Weekday]bool{}
	for _, d := range rule.Days {
		wd, ok := d.ToTime()
		if !ok {
			return time.Time{}, false, fmt.Errorf("azure: schedule rule %q has an unknown weekday %q", rule.Name, d)
		}
		allowed[wd] = true
	}

	// A week of candidates is always enough: the rule fires at a fixed
	// time of day on at least one weekday.
	day := earliest.In(loc)
	for i := 0; i < 8; i++ {
		candidate := time.Date(day.Year(), day.Month(), day.Day(), rule.Hour, rule.Min, 0, 0, loc)
		if !candidate.Before(earliest) && (len(allowed) == 0 || allowed[candidate.Weekday()]) {
			if rule.ValidTo != nil && !candidate.Before(*rule.ValidTo) {
				return time.Time{}, false, nil
			}
			return candidate, true, nil
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, false, fmt.Errorf("azure: schedule rule %q has no occurrence within a week", rule.Name)
}

// weekDayNames renders weekdays the way the Automation API spells them.
func weekDayNames(days []schedule.Weekday) []string {
	out := make([]string, 0, len(days))
	for _, d := range days {
		wd, ok := d.ToTime()
		if !ok {
			continue
		}
		out = append(out, wd.String())
	}
	return out
}

// armAction maps a compiled rule's action onto the ARM verb for this
// target. Start is universally "start"; stop is not (see powerTarget).
func armAction(action schedule.Action, stopAction string) string {
	if action == schedule.ActionStart {
		return "start"
	}
	return stopAction
}
