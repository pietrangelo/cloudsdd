// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	databaseInstanceToken = "gcp:sql/databaseInstance:DatabaseInstance"
	serviceAccountToken   = "gcp:serviceaccount/account:Account"
	customRoleToken       = "gcp:projects/iAMCustomRole:IAMCustomRole"
	iamMemberToken        = "gcp:projects/iAMMember:IAMMember"
	schedulerJobToken     = "gcp:cloudscheduler/job:Job"

	testProjectID = "cloudsdd-test"
)

func boolPtr(b bool) *bool { return &b }

func workWeekSchedule() *schedule.Schedule {
	return &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}
}

func declareScheduledDatabase(t *testing.T, sch *schedule.Schedule) []recordedResource {
	t.Helper()

	rules, err := schedule.Compile(sch)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return runProgram(t, func(ctx *pulumi.Context) error {
		instance, err := declareRelationalDatabase(ctx, "app-db", "europe-west1", testNetworkName,
			relationalDatabaseProperties{Engine: "postgres", Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", "europe-west1", instance, rules)
	})
}

// hasResource reports whether a resource of the given type was declared.
func hasResource(recorded []recordedResource, typeToken string) bool {
	return len(resourcesOfType(recorded, typeToken)) > 0
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

func TestDeclareDatabaseScheduleCreatesOneJobPerRule(t *testing.T) {
	recorded := declareScheduledDatabase(t, workWeekSchedule())

	jobs := resourcesOfType(recorded, schedulerJobToken)
	if len(jobs) != 2 {
		t.Fatalf("declared %d jobs, want 2 (one start, one stop)", len(jobs))
	}

	byName := map[string]map[string]any{}
	for _, j := range jobs {
		byName[j.Inputs["name"].StringValue()] = j.Inputs.Mappable()
	}

	start, ok := byName["app-db-start-base-0"]
	if !ok {
		t.Fatalf("no start job declared; got %v", keysOfAny(byName))
	}
	stop, ok := byName["app-db-stop-base-0"]
	if !ok {
		t.Fatalf("no stop job declared; got %v", keysOfAny(byName))
	}

	// Five-field unix cron, weekdays numeric with Sunday 0.
	if got := start["schedule"]; got != "0 8 * * 1,2,3,4,5" {
		t.Errorf("start schedule = %v, want the work-week cron", got)
	}
	if got := stop["schedule"]; got != "0 19 * * 1,2,3,4,5" {
		t.Errorf("stop schedule = %v, want the work-week cron", got)
	}

	for name, job := range byName {
		if got := job["timeZone"]; got != "Europe/Rome" {
			t.Errorf("%s timeZone = %v, want Europe/Rome", name, got)
		}
		if got := job["region"]; got != "europe-west1" {
			t.Errorf("%s region = %v, want the resource's region", name, got)
		}

		target, _ := job["httpTarget"].(map[string]any)
		if target == nil {
			t.Fatalf("%s has no httpTarget", name)
		}
		if got := target["httpMethod"]; got != "PATCH" {
			t.Errorf("%s httpMethod = %v, want PATCH", name, got)
		}
		wantURI := "https://sqladmin.googleapis.com/v1/projects/" + testProjectID + "/instances/app-db"
		if got := target["uri"]; got != wantURI {
			t.Errorf("%s uri = %v, want %s", name, got, wantURI)
		}

		// The job authenticates as the dedicated service account, never
		// as the default Compute Engine identity.
		oauth, _ := target["oauthToken"].(map[string]any)
		if oauth == nil {
			t.Fatalf("%s has no oauthToken; the call would be unauthenticated", name)
		}
		email, _ := oauth["serviceAccountEmail"].(string)
		if !strings.HasSuffix(email, "@"+testProjectID+".iam.gserviceaccount.com") {
			t.Errorf("%s serviceAccountEmail = %q, want the declared service account", name, email)
		}

		wantPolicy := activationPolicyOff
		if strings.Contains(name, "start") {
			wantPolicy = activationPolicyOn
		}
		if got := decodeActivationPolicy(t, target["body"]); got != wantPolicy {
			t.Errorf("%s activationPolicy = %q, want %q", name, got, wantPolicy)
		}
	}
}

// decodeActivationPolicy unwraps the base64-encoded Cloud SQL patch body.
func decodeActivationPolicy(t *testing.T, body any) string {
	t.Helper()

	encoded, _ := body.(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("body is not base64: %q", encoded)
	}
	var payload struct {
		Settings struct {
			ActivationPolicy string `json:"activationPolicy"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("body is not JSON: %q", raw)
	}
	return payload.Settings.ActivationPolicy
}

func TestDeclareDatabaseScheduleGrantsLeastPrivilege(t *testing.T) {
	recorded := declareScheduledDatabase(t, workWeekSchedule())

	role := findResource(t, recorded, customRoleToken)
	perms := role.Inputs["permissions"].ArrayValue()
	if len(perms) != len(schedulerPermissions) {
		t.Fatalf("custom role has %d permissions, want %d", len(perms), len(schedulerPermissions))
	}
	for _, p := range perms {
		got := p.StringValue()
		if got != "cloudsql.instances.get" && got != "cloudsql.instances.update" {
			t.Errorf("unexpected permission %q; the role may only read and update the instance", got)
		}
	}

	// Cloud SQL has no resource-level IAM, so the binding is necessarily
	// project-wide — which is exactly why it must not be
	// roles/cloudsql.admin.
	member := findResource(t, recorded, iamMemberToken)
	if got := member.Inputs["role"].StringValue(); !strings.Contains(got, "/roles/cloudsddPowerSchedule_") {
		t.Errorf("binding role = %q, want the custom role rather than a predefined one", got)
	}
	if got := member.Inputs["member"].StringValue(); !strings.HasPrefix(got, "serviceAccount:") {
		t.Errorf("binding member = %q, want the scheduler service account", got)
	}
	if got := member.Inputs["project"].StringValue(); got != testProjectID {
		t.Errorf("binding project = %q, want %q", got, testProjectID)
	}
}

func TestDeclareDatabaseScheduleDeclaresNothingWhenUnscheduled(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		instance, err := declareRelationalDatabase(ctx, "app-db", "europe-west1", testNetworkName,
			relationalDatabaseProperties{Engine: "postgres", Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", "europe-west1", instance, nil)
	})

	for _, token := range []string{schedulerJobToken, serviceAccountToken, customRoleToken, iamMemberToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for an unscheduled resource", token)
		}
	}
}

func TestResourceScheduleRejectsExceptionWindows(t *testing.T) {
	// Refusing is the only honest option: a Cloud Scheduler job has no
	// validity period, and a silently dropped window would leave an
	// environment running through a shutdown the user believed they had
	// scheduled (RFC 012 §4.2).
	sch := workWeekSchedule()
	sch.Exceptions = []schedule.Window{
		{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff, Reason: "company shutdown"},
	}

	r := spec.Resource{
		ID:         "app-db",
		Type:       spec.ResourceTypeRelationalDatabase,
		Provider:   spec.ProviderGCP,
		Properties: map[string]any{"engine": "postgres", "version": "15"},
	}

	_, err := resourceSchedule(r, spec.Policies{Schedule: sch})
	if !errors.Is(err, ErrScheduleExceptionsUnsupported) {
		t.Fatalf("resourceSchedule() error = %v, want %v", err, ErrScheduleExceptionsUnsupported)
	}
	if !strings.Contains(err.Error(), "AWS or Azure") {
		t.Errorf("error = %q, want it to say where exception windows do work", err)
	}
}

func TestResourceSchedule(t *testing.T) {
	db := spec.Resource{
		ID:         "app-db",
		Type:       spec.ResourceTypeRelationalDatabase,
		Provider:   spec.ProviderGCP,
		Properties: map[string]any{"engine": "postgres", "version": "15"},
	}
	bucket := spec.Resource{
		ID:         "assets",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderGCP,
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
			name:      "base rhythm is supported",
			resource:  db,
			policies:  spec.Policies{Schedule: workWeekSchedule()},
			wantRules: 2,
		},
		{
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

func TestUnixCronExpression(t *testing.T) {
	tests := []struct {
		name string
		rule schedule.Rule
		want string
	}{
		{
			name: "work week",
			rule: schedule.Rule{Hour: 8, Days: []schedule.Weekday{schedule.Mon, schedule.Fri}},
			want: "0 8 * * 1,5",
		},
		{
			// Sunday is 0 in unix cron, not 7.
			name: "sunday",
			rule: schedule.Rule{Hour: 19, Min: 30, Days: []schedule.Weekday{schedule.Sun}},
			want: "30 19 * * 0",
		},
		{
			name: "every day",
			rule: schedule.Rule{},
			want: "0 0 * * *",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unixCronExpression(tt.rule)
			if err != nil {
				t.Fatalf("unixCronExpression() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("unixCronExpression() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnixCronExpressionRejectsBadRules(t *testing.T) {
	for _, rule := range []schedule.Rule{
		{Name: "bad-hour", Hour: 24},
		{Name: "bad-minute", Min: -1},
		{Name: "bad-weekday", Days: []schedule.Weekday{"caturday"}},
	} {
		t.Run(rule.Name, func(t *testing.T) {
			if _, err := unixCronExpression(rule); err == nil {
				t.Errorf("unixCronExpression(%+v) = nil error, want a rejection", rule)
			}
		})
	}
}

func TestServiceAccountID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple", in: "app-db", want: "app-db-sched"},
		{name: "underscores become hyphens", in: "app_db", want: "app-db-sched"},
		{name: "uppercase is lowered", in: "AppDB", want: "appdb-sched"},
		{name: "must start with a letter", in: "9db", want: "r9db-sched"},
		{name: "long ids are truncated", in: strings.Repeat("a", 63), want: strings.Repeat("a", 24) + "-sched"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceAccountID(tt.in)
			if got != tt.want {
				t.Errorf("serviceAccountID(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) < 6 || len(got) > 30 {
				t.Errorf("serviceAccountID(%q) = %q, %d characters; Google requires 6 to 30", tt.in, got, len(got))
			}
			if got[0] < 'a' || got[0] > 'z' {
				t.Errorf("serviceAccountID(%q) = %q, want it to start with a letter", tt.in, got)
			}
		})
	}
}

func TestCustomRoleIDHasNoHyphens(t *testing.T) {
	// Resource ids admit hyphens; role ids explicitly do not.
	for _, in := range []string{"app-db", "app_db", strings.Repeat("a-b", 21)} {
		t.Run(in, func(t *testing.T) {
			got := customRoleID(in)
			if strings.Contains(got, "-") {
				t.Errorf("customRoleID(%q) = %q, want no hyphens", in, got)
			}
			if len(got) > 64 {
				t.Errorf("customRoleID(%q) = %q, %d characters; the limit is 64", in, got, len(got))
			}
		})
	}
}

func keysOfAny(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
