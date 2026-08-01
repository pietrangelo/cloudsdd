// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	iamRoleToken       = "aws:iam/role:Role"
	iamRolePolicyToken = "aws:iam/rolePolicy:RolePolicy"
	scheduleToken      = "aws:scheduler/schedule:Schedule"

	testAccountID   = "123456789012"
	testInstanceARN = "arn:aws:rds:eu-central-1:" + testAccountID + ":db:app-db"
)

func workWeekSchedule() *schedule.Schedule {
	return &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
}

// declareScheduledDatabase runs a program declaring a database and its
// power schedule, and returns everything the monitor recorded.
func declareScheduledDatabase(t *testing.T, sch *schedule.Schedule) []recordedResource {
	t.Helper()

	rules, err := schedule.Compile(sch)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return runProgram(t, func(ctx *pulumi.Context) error {
		instance, err := declareRelationalDatabase(ctx, "app-db",
			relationalDatabaseProperties{Engine: "postgres", Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", instance, rules)
	})
}

func resourcesOfType(recorded []recordedResource, typeToken string) []recordedResource {
	var out []recordedResource
	for _, r := range recorded {
		if r.Type == typeToken {
			out = append(out, r)
		}
	}
	return out
}

func TestDeclareDatabaseScheduleCreatesOneSchedulePerRule(t *testing.T) {
	recorded := declareScheduledDatabase(t, workWeekSchedule())

	schedules := resourcesOfType(recorded, scheduleToken)
	if len(schedules) != 2 {
		t.Fatalf("declared %d schedules, want 2 (one start, one stop)", len(schedules))
	}

	byName := map[string]resource.PropertyMap{}
	for _, s := range schedules {
		byName[s.Inputs["name"].StringValue()] = s.Inputs
	}

	start, ok := byName["app-db-start-base-0"]
	if !ok {
		t.Fatalf("no start schedule declared; got %v", keys(byName))
	}
	stop, ok := byName["app-db-stop-base-0"]
	if !ok {
		t.Fatalf("no stop schedule declared; got %v", keys(byName))
	}

	if got := start.Mappable()["scheduleExpression"]; got != "cron(0 8 ? * MON,TUE,WED,THU,FRI *)" {
		t.Errorf("start expression = %v, want the work-week cron", got)
	}
	if got := stop.Mappable()["scheduleExpression"]; got != "cron(0 19 ? * MON,TUE,WED,THU,FRI *)" {
		t.Errorf("stop expression = %v, want the work-week cron", got)
	}

	for name, inputs := range byName {
		m := inputs.Mappable()
		// The zone name is passed through, never a precomputed offset:
		// otherwise the schedule is an hour wrong for half the year.
		if got := m["scheduleExpressionTimezone"]; got != "Europe/Rome" {
			t.Errorf("%s timezone = %v, want Europe/Rome", name, got)
		}
		// A start-of-workday action must be exact, not smeared.
		window, _ := m["flexibleTimeWindow"].(map[string]any)
		if window == nil || window["mode"] != "OFF" {
			t.Errorf("%s flexibleTimeWindow = %v, want mode OFF", name, m["flexibleTimeWindow"])
		}
		if got := m["state"]; got != "ENABLED" {
			t.Errorf("%s state = %v, want ENABLED", name, got)
		}

		target, _ := m["target"].(map[string]any)
		if target == nil {
			t.Fatalf("%s has no target", name)
		}
		wantARN := stopDBInstanceTarget
		if strings.Contains(name, "start") {
			wantARN = startDBInstanceTarget
		}
		if got := target["arn"]; got != wantARN {
			t.Errorf("%s target arn = %v, want %s", name, got, wantARN)
		}
		// The universal target carries the RDS API's own payload, so no
		// Lambda has to exist to translate one.
		var payload map[string]string
		input, _ := target["input"].(string)
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			t.Fatalf("%s input is not JSON: %q", name, input)
		}
		if payload["DbInstanceIdentifier"] != "app-db" {
			t.Errorf("%s targets %q, want the declared instance", name, payload["DbInstanceIdentifier"])
		}
	}
}

func TestDeclareDatabaseScheduleGrantsLeastPrivilege(t *testing.T) {
	recorded := declareScheduledDatabase(t, workWeekSchedule())

	policy := findResource(t, recorded, iamRolePolicyToken)
	var doc iamPolicyDocument
	if err := json.Unmarshal([]byte(policy.Inputs["policy"].StringValue()), &doc); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}

	if len(doc.Statement) != 1 {
		t.Fatalf("permission policy has %d statements, want 1", len(doc.Statement))
	}
	stmt := doc.Statement[0]

	wantActions := map[string]bool{"rds:StartDBInstance": true, "rds:StopDBInstance": true}
	if len(stmt.Action) != len(wantActions) {
		t.Fatalf("actions = %v, want exactly %v", stmt.Action, wantActions)
	}
	for _, a := range stmt.Action {
		if !wantActions[a] {
			t.Errorf("unexpected action %q; the role may only start and stop", a)
		}
		if strings.Contains(a, "*") {
			t.Errorf("action %q contains a wildcard", a)
		}
	}

	if len(stmt.Resource) != 1 || stmt.Resource[0] != testInstanceARN {
		t.Errorf("resources = %v, want exactly the instance ARN %q", stmt.Resource, testInstanceARN)
	}
}

func TestDeclareDatabaseScheduleGuardsAgainstTheConfusedDeputy(t *testing.T) {
	// scheduler.amazonaws.com will assume this role on behalf of whoever
	// asks unless the source is constrained.
	recorded := declareScheduledDatabase(t, workWeekSchedule())

	role := findResource(t, recorded, iamRoleToken)
	var doc iamPolicyDocument
	if err := json.Unmarshal([]byte(role.Inputs["assumeRolePolicy"].StringValue()), &doc); err != nil {
		t.Fatalf("trust policy is not valid JSON: %v", err)
	}

	if len(doc.Statement) != 1 {
		t.Fatalf("trust policy has %d statements, want 1", len(doc.Statement))
	}
	stmt := doc.Statement[0]

	if stmt.Principal == nil || stmt.Principal.Service != schedulerServicePrincipal {
		t.Fatalf("principal = %+v, want the scheduler service principal", stmt.Principal)
	}
	if stmt.Principal.AWS != "" {
		t.Errorf("trust policy also names an AWS principal %q, want only the service", stmt.Principal.AWS)
	}

	account := stmt.Condition["StringEquals"]["aws:SourceAccount"]
	if account != testAccountID {
		t.Errorf("aws:SourceAccount = %v, want %s", account, testAccountID)
	}
	sourceARN, _ := stmt.Condition["ArnLike"]["aws:SourceArn"].(string)
	wantPrefix := "arn:aws:scheduler:eu-central-1:" + testAccountID + ":schedule/default/app-db-"
	if !strings.HasPrefix(sourceARN, wantPrefix) {
		t.Errorf("aws:SourceArn = %q, want it scoped to %q", sourceARN, wantPrefix)
	}
	if !strings.HasSuffix(sourceARN, "*") {
		t.Errorf("aws:SourceArn = %q, want a prefix match on this resource's schedules", sourceARN)
	}
}

func TestDeclareDatabaseScheduleBoundsWindowRules(t *testing.T) {
	sch := workWeekSchedule()
	sch.Exceptions = []schedule.Window{
		{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff, Reason: "company shutdown"},
	}

	recorded := declareScheduledDatabase(t, sch)
	byName := map[string]map[string]any{}
	for _, s := range resourcesOfType(recorded, scheduleToken) {
		byName[s.Inputs["name"].StringValue()] = s.Inputs.Mappable()
	}

	window, ok := byName["app-db-stop-window-0"]
	if !ok {
		t.Fatalf("no window schedule declared; got %v", keysOfAny(byName))
	}

	// Every day for the whole shutdown: AWS restarts an RDS instance
	// stopped for more than seven days, so a single stop would leave it
	// running for the second week (RFC 012 §2.2 step 5).
	if got := window["scheduleExpression"]; got != "cron(0 0 * * ? *)" {
		t.Errorf("window expression = %v, want a daily stop", got)
	}
	// Backdated by a second: EventBridge begins invoking *after*
	// StartDate, and the rule fires exactly on the boundary.
	if got := window["startDate"]; got != "2026-12-23T22:59:59Z" {
		t.Errorf("window startDate = %v, want the boundary backdated by a second", got)
	}
	if got := window["endDate"]; got != "2027-01-06T23:00:00Z" {
		t.Errorf("window endDate = %v, want midnight after the final day, in UTC", got)
	}

	// The base rhythm is segmented around it rather than competing.
	base0, ok := byName["app-db-stop-base-0"]
	if !ok {
		t.Fatal("the first base segment is missing")
	}
	if got := base0["endDate"]; got != "2026-12-23T23:00:00Z" {
		t.Errorf("first segment endDate = %v, want the window's opening", got)
	}
	if base0["startDate"] != nil {
		t.Errorf("first segment startDate = %v, want it unbounded", base0["startDate"])
	}
	if _, ok := byName["app-db-stop-base-1"]; !ok {
		t.Error("the base rhythm does not resume after the window")
	}
}

func TestDeclareDatabaseScheduleDeclaresNothingWhenUnscheduled(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		instance, err := declareRelationalDatabase(ctx, "app-db",
			relationalDatabaseProperties{Engine: "postgres", Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", instance, nil)
	})

	if hasResource(recorded, scheduleToken) {
		t.Error("a schedule was declared for an unscheduled resource")
	}
	if hasResource(recorded, iamRoleToken) {
		t.Error("a scheduler role was declared for an unscheduled resource")
	}
}

func TestCronExpression(t *testing.T) {
	tests := []struct {
		name string
		rule schedule.Rule
		want string
	}{
		{
			name: "work week",
			rule: schedule.Rule{Hour: 8, Days: []schedule.Weekday{schedule.Mon, schedule.Fri}},
			want: "cron(0 8 ? * MON,FRI *)",
		},
		{
			// Exactly one of day-of-month and day-of-week must be "?" in
			// EventBridge's dialect, which is why this cannot be shared
			// with the five-field unix cron the other providers use.
			name: "every day",
			rule: schedule.Rule{Hour: 0, Min: 0},
			want: "cron(0 0 * * ? *)",
		},
		{
			name: "minutes are preserved",
			rule: schedule.Rule{Hour: 19, Min: 30, Days: []schedule.Weekday{schedule.Sun}},
			want: "cron(30 19 ? * SUN *)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cronExpression(tt.rule)
			if err != nil {
				t.Fatalf("cronExpression() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("cronExpression() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCronExpressionRejectsOutOfRangeTimes(t *testing.T) {
	// Compile cannot produce these, but a malformed cron expression would
	// reach the target account rather than failing here.
	for _, rule := range []schedule.Rule{
		{Name: "bad-hour", Hour: 24},
		{Name: "negative-hour", Hour: -1},
		{Name: "bad-minute", Min: 60},
		{Name: "negative-minute", Min: -1},
	} {
		t.Run(rule.Name, func(t *testing.T) {
			if _, err := cronExpression(rule); err == nil {
				t.Errorf("cronExpression(%+v) = nil error, want a rejection", rule)
			}
		})
	}
}

func TestSchedulePrefixFitsTheNameLimit(t *testing.T) {
	longID := strings.Repeat("a", 63)
	rules := []schedule.Rule{{Name: "start-window-11"}, {Name: "stop-base-0"}}

	prefix := schedulePrefix(longID, rules)
	for _, r := range rules {
		if got := len(prefix + r.Name); got > maxScheduleNameLength {
			t.Errorf("name %q is %d characters, want at most %d", prefix+r.Name, got, maxScheduleNameLength)
		}
	}
	if !strings.HasSuffix(prefix, "-") {
		t.Errorf("prefix = %q, want it to end with the separator", prefix)
	}
}

func TestParseARN(t *testing.T) {
	tests := []struct {
		name        string
		arn         string
		wantAccount string
		wantRegion  string
		wantErr     bool
	}{
		{
			name:        "rds instance",
			arn:         testInstanceARN,
			wantAccount: testAccountID,
			wantRegion:  "eu-central-1",
		},
		{
			name:        "china partition",
			arn:         "arn:aws-cn:rds:cn-north-1:210987654321:db:app-db",
			wantAccount: "210987654321",
			wantRegion:  "cn-north-1",
		},
		{name: "empty", arn: "", wantErr: true},
		{name: "not an arn", arn: "app-db", wantErr: true},
		{name: "too few segments", arn: "arn:aws:rds:eu-central-1:123456789012", wantErr: true},
		{name: "missing region", arn: "arn:aws:rds::123456789012:db:app-db", wantErr: true},
		{name: "missing account", arn: "arn:aws:rds:eu-central-1::db:app-db", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, region, account, err := parseARN(tt.arn)
			if tt.wantErr {
				if !errors.Is(err, ErrMalformedARN) {
					t.Fatalf("parseARN() error = %v, want %v", err, ErrMalformedARN)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseARN() error = %v", err)
			}
			if region != tt.wantRegion || account != tt.wantAccount {
				t.Errorf("parseARN() = (%q, %q), want (%q, %q)", region, account, tt.wantRegion, tt.wantAccount)
			}
		})
	}
}

func TestBuildSchedulerTrustPolicyRejectsAMalformedARN(t *testing.T) {
	if _, err := buildSchedulerTrustPolicy("not-an-arn", "app-db-"); !errors.Is(err, ErrMalformedARN) {
		t.Fatalf("buildSchedulerTrustPolicy() error = %v, want %v", err, ErrMalformedARN)
	}
}

func TestResourceSchedule(t *testing.T) {
	db := spec.Resource{
		ID:         "app-db",
		Type:       spec.ResourceTypeRelationalDatabase,
		Provider:   spec.ProviderAWS,
		Properties: map[string]any{"engine": "postgres", "version": "15"},
	}
	bucket := spec.Resource{
		ID:         "assets",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderAWS,
		Properties: map[string]any{"bucket_name": "assets"},
	}

	tests := []struct {
		name      string
		resource  spec.Resource
		policies  spec.Policies
		wantRules int
		wantErr   error
	}{
		{name: "no schedule", resource: db},
		{
			name:      "inherited schedule",
			resource:  db,
			policies:  spec.Policies{Schedule: workWeekSchedule()},
			wantRules: 2,
		},
		{
			name: "resource opts out",
			resource: func() spec.Resource {
				r := db
				r.Schedule = &schedule.Schedule{Enabled: boolPtr(false)}
				return r
			}(),
			policies: spec.Policies{Schedule: workWeekSchedule()},
		},
		{
			// Without this, no Specification could hold both a bucket and
			// a database.
			name:     "inherited schedule over a bucket is skipped",
			resource: bucket,
			policies: spec.Policies{Schedule: workWeekSchedule()},
		},
		{
			name: "explicit schedule over a bucket is refused",
			resource: func() spec.Resource {
				r := bucket
				r.Schedule = workWeekSchedule()
				return r
			}(),
			wantErr: ErrResourceNotSchedulable,
		},
		{
			name:     "uncompilable schedule",
			resource: db,
			policies: spec.Policies{Schedule: &schedule.Schedule{Enabled: boolPtr(true), Timezone: "Europe/Rome"}},
			wantErr:  schedule.ErrStartRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := resourceSchedule(tt.resource, tt.policies)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resourceSchedule() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resourceSchedule() error = %v", err)
			}
			if len(rules) != tt.wantRules {
				t.Errorf("resourceSchedule() = %d rules, want %d", len(rules), tt.wantRules)
			}
		})
	}
}

func keys(m map[string]resource.PropertyMap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfAny(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
