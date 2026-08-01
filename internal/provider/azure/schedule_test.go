// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	automationAccountToken  = "azure:automation/account:Account"
	automationScheduleToken = "azure:automation/schedule:Schedule"
	jobScheduleToken        = "azure:automation/jobSchedule:JobSchedule"
	runbookToken            = "azure:automation/runBook:RunBook"
	roleDefinitionToken     = "azure:authorization/roleDefinition:RoleDefinition"
	assignmentToken         = "azure:authorization/assignment:Assignment"

	testPrincipalID = "00000000-0000-0000-0000-000000000001"
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

// freezeClock pins the provider's clock so the computed first occurrence
// of every schedule is deterministic. Monday 2026-08-03, 09:00 Rome time.
func freezeClock(t *testing.T) {
	t.Helper()
	orig := timeNow
	t.Cleanup(func() { timeNow = orig })

	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	frozen := time.Date(2026, 8, 3, 9, 0, 0, 0, loc)
	timeNow = func() time.Time { return frozen }
}

func declareScheduledDatabase(t *testing.T, engine string, sch *schedule.Schedule) []recordedResource {
	t.Helper()

	rules, err := schedule.Compile(sch)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	return runProgram(t, func(ctx *pulumi.Context) error {
		server, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(),
			relationalDatabaseProperties{Engine: engine, Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", server, rules)
	})
}

// TestScheduledDatabaseKeepsCanNotDeleteLock covers RFC 015 §2.1's
// composition with RFC 012. Deletion protection and a power schedule have
// to coexist, and the lock level is what decides whether they do: Azure's
// stronger ReadOnly lock forbids modification, which would leave the
// runbook unable to stop or start the server it is scheduled against. The
// only symptom of that would be an unchanged bill, so it is worth a test
// rather than a comment.
func TestScheduledDatabaseKeepsCanNotDeleteLock(t *testing.T) {
	freezeClock(t)

	recorded := declareScheduledDatabase(t, "postgres", &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	})

	if !hasResource(recorded, automationScheduleToken) {
		t.Fatal("no automation schedule was declared for a scheduled database")
	}

	lock := findResource(t, recorded, "azure:management/lock:Lock")
	if got := lock.Inputs["lockLevel"].StringValue(); got != lockLevelCanNotDelete {
		t.Errorf("lockLevel = %q, want %q — ReadOnly would break the runbook", got, lockLevelCanNotDelete)
	}
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
	freezeClock(t)
	recorded := declareScheduledDatabase(t, "postgres", workWeekSchedule())

	schedules := resourcesOfType(recorded, automationScheduleToken)
	if len(schedules) != 2 {
		t.Fatalf("declared %d schedules, want 2 (one start, one stop)", len(schedules))
	}

	byName := map[string]map[string]any{}
	for _, s := range schedules {
		byName[s.Inputs["name"].StringValue()] = s.Inputs.Mappable()
	}

	start, ok := byName["app-db-start-base-0"]
	if !ok {
		t.Fatalf("no start schedule declared; got %v", keysOfAny(byName))
	}

	if got := start["frequency"]; got != "Week" {
		t.Errorf("frequency = %v, want Week for a weekday rule", got)
	}
	days, _ := start["weekDays"].([]any)
	if len(days) != 5 {
		t.Fatalf("weekDays = %v, want the five working days", start["weekDays"])
	}
	if days[0] != "Monday" || days[4] != "Friday" {
		t.Errorf("weekDays = %v, want Monday through Friday spelled the Automation way", days)
	}
	// The zone name is passed through, never a precomputed offset.
	if got := start["timezone"]; got != "Europe/Rome" {
		t.Errorf("timezone = %v, want Europe/Rome", got)
	}
	// Frozen clock is Monday 09:00, past the day's 08:00 start, so the
	// first occurrence is the following morning.
	if got := start["startTime"]; got != "2026-08-04T08:00:00+02:00" {
		t.Errorf("startTime = %v, want the next occurrence after the lead time", got)
	}
	if _, bounded := start["expiryTime"]; bounded {
		t.Errorf("startTime carries an expiryTime = %v, want none without exception windows", start["expiryTime"])
	}

	// Every schedule is bound to the runbook with the action as a
	// parameter, so nothing about the action is baked into the script.
	jobs := resourcesOfType(recorded, jobScheduleToken)
	if len(jobs) != 2 {
		t.Fatalf("declared %d job schedules, want one per schedule", len(jobs))
	}
	actions := map[string]bool{}
	for _, j := range jobs {
		params := j.Inputs["parameters"].ObjectValue()
		actions[params["action"].StringValue()] = true
		if got := params["apiversion"].StringValue(); got != postgresAPIVersion {
			t.Errorf("apiversion = %q, want the pinned PostgreSQL version %q", got, postgresAPIVersion)
		}
		if params["resourceid"].StringValue() == "" {
			t.Error("resourceid parameter is empty; the runbook would have no target")
		}
	}
	if !actions["start"] || !actions["stop"] {
		t.Errorf("job schedule actions = %v, want both start and stop", actions)
	}
}

func TestDeclareDatabaseScheduleGrantsLeastPrivilege(t *testing.T) {
	freezeClock(t)

	tests := []struct {
		engine       string
		wantProvider string
		wantAPI      string
	}{
		{engine: "postgres", wantProvider: "Microsoft.DBforPostgreSQL", wantAPI: postgresAPIVersion},
		{engine: "mysql", wantProvider: "Microsoft.DBforMySQL", wantAPI: mysqlAPIVersion},
	}

	for _, tt := range tests {
		t.Run(tt.engine, func(t *testing.T) {
			recorded := declareScheduledDatabase(t, tt.engine, workWeekSchedule())

			role := findResource(t, recorded, roleDefinitionToken)
			perms := role.Inputs["permissions"].ArrayValue()
			if len(perms) != 1 {
				t.Fatalf("role has %d permission blocks, want 1", len(perms))
			}
			actions := perms[0].ObjectValue()["actions"].ArrayValue()
			if len(actions) != 3 {
				t.Fatalf("role grants %d actions, want read, start and stop only", len(actions))
			}
			for _, a := range actions {
				got := a.StringValue()
				if !strings.HasPrefix(got, tt.wantProvider+"/flexibleServers/") {
					t.Errorf("action %q is not scoped to %s flexible servers", got, tt.wantProvider)
				}
				if strings.Contains(got, "*") {
					t.Errorf("action %q contains a wildcard", got)
				}
			}

			// Scoped to the one server, not the resource group or the
			// subscription.
			scope := role.Inputs["scope"].StringValue()
			if scope != "app-db-server-id" {
				t.Errorf("role scope = %q, want the server alone", scope)
			}
			assignment := findResource(t, recorded, assignmentToken)
			if got := assignment.Inputs["scope"].StringValue(); got != scope {
				t.Errorf("assignment scope = %q, want the role's scope %q", got, scope)
			}
			if got := assignment.Inputs["principalId"].StringValue(); got != testPrincipalID {
				t.Errorf("assignment principalId = %q, want the account's managed identity", got)
			}

			// The runbook is a fixed constant; only its parameters vary.
			runbook := findResource(t, recorded, runbookToken)
			if got := runbook.Inputs["content"].StringValue(); got != powerRunbook {
				t.Error("the runbook content differs from the reviewed constant")
			}
			job := findResource(t, recorded, jobScheduleToken)
			if got := job.Inputs["parameters"].ObjectValue()["apiversion"].StringValue(); got != tt.wantAPI {
				t.Errorf("apiversion = %q, want %q", got, tt.wantAPI)
			}
		})
	}
}

func TestDeclareDatabaseScheduleLocksDownTheAutomationAccount(t *testing.T) {
	freezeClock(t)
	recorded := declareScheduledDatabase(t, "postgres", workWeekSchedule())

	account := findResource(t, recorded, automationAccountToken)
	if account.Inputs["publicNetworkAccessEnabled"].BoolValue() {
		t.Error("the automation account is reachable from the internet; it runs one runbook against one server")
	}
	if account.Inputs["localAuthenticationEnabled"].BoolValue() {
		t.Error("key-based authentication is enabled; the account uses its managed identity")
	}
	if got := account.Inputs["identity"].ObjectValue()["type"].StringValue(); got != "SystemAssigned" {
		t.Errorf("identity type = %q, want SystemAssigned", got)
	}
}

func TestDeclareDatabaseScheduleBoundsWindowRules(t *testing.T) {
	freezeClock(t)

	sch := workWeekSchedule()
	sch.Exceptions = []schedule.Window{
		{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff, Reason: "company shutdown"},
	}

	recorded := declareScheduledDatabase(t, "postgres", sch)
	byName := map[string]map[string]any{}
	for _, s := range resourcesOfType(recorded, automationScheduleToken) {
		byName[s.Inputs["name"].StringValue()] = s.Inputs.Mappable()
	}

	window, ok := byName["app-db-stop-window-0"]
	if !ok {
		t.Fatalf("no window schedule declared; got %v", keysOfAny(byName))
	}
	// Daily for the whole shutdown, because a stopped instance can be
	// restarted by the platform (RFC 012 §2.2 step 5).
	if got := window["frequency"]; got != "Day" {
		t.Errorf("window frequency = %v, want Day", got)
	}
	if got := window["startTime"]; got != "2026-12-24T00:00:00+01:00" {
		t.Errorf("window startTime = %v, want the window's first midnight", got)
	}
	if got := window["expiryTime"]; got != "2027-01-07T00:00:00+01:00" {
		t.Errorf("window expiryTime = %v, want midnight after the final day", got)
	}

	base0, ok := byName["app-db-stop-base-0"]
	if !ok {
		t.Fatal("the first base segment is missing")
	}
	if got := base0["expiryTime"]; got != "2026-12-24T00:00:00+01:00" {
		t.Errorf("first segment expiryTime = %v, want the window's opening", got)
	}
	if _, ok := byName["app-db-stop-base-1"]; !ok {
		t.Error("the base rhythm does not resume after the window")
	}
}

func TestDeclareDatabaseScheduleSkipsExpiredWindows(t *testing.T) {
	freezeClock(t)

	// A window that closed before the deployment. Azure rejects a start
	// time in the past, and the rule has nothing left to do.
	sch := workWeekSchedule()
	sch.Exceptions = []schedule.Window{
		{From: "2020-12-24", To: "2021-01-06", Mode: schedule.ModeAlwaysOff},
	}

	recorded := declareScheduledDatabase(t, "postgres", sch)
	for _, s := range resourcesOfType(recorded, automationScheduleToken) {
		if s.Inputs["name"].StringValue() == "app-db-stop-window-0" {
			t.Error("a schedule was declared for a window that has already closed")
		}
	}
}

func TestDeclareDatabaseScheduleDeclaresNothingWhenUnscheduled(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		server, err := declareRelationalDatabase(ctx, "app-db", "westeurope", testScope(),
			relationalDatabaseProperties{Engine: "postgres", Version: "15"})
		if err != nil {
			return err
		}
		return declareDatabaseSchedule(ctx, "app-db", server, nil)
	})

	for _, token := range []string{automationAccountToken, automationScheduleToken, runbookToken, roleDefinitionToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for an unscheduled resource", token)
		}
	}
}

func TestFirstOccurrence(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	// Monday 2026-08-03, 09:00 Rome.
	now := time.Date(2026, 8, 3, 9, 0, 0, 0, loc)

	windowStart := time.Date(2026, 12, 24, 0, 0, 0, 0, loc)
	windowEnd := time.Date(2027, 1, 7, 0, 0, 0, 0, loc)
	pastEnd := time.Date(2021, 1, 7, 0, 0, 0, 0, loc)

	tests := []struct {
		name    string
		rule    schedule.Rule
		want    string
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "later today",
			rule:   schedule.Rule{Timezone: "Europe/Rome", Hour: 19, Days: []schedule.Weekday{schedule.Mon}},
			want:   "2026-08-03T19:00:00+02:00",
			wantOK: true,
		},
		{
			// 08:00 has already passed on the frozen Monday.
			name:   "already passed today",
			rule:   schedule.Rule{Timezone: "Europe/Rome", Hour: 8, Days: []schedule.Weekday{schedule.Mon, schedule.Tue}},
			want:   "2026-08-04T08:00:00+02:00",
			wantOK: true,
		},
		{
			name:   "next matching weekday",
			rule:   schedule.Rule{Timezone: "Europe/Rome", Hour: 8, Days: []schedule.Weekday{schedule.Sat}},
			want:   "2026-08-08T08:00:00+02:00",
			wantOK: true,
		},
		{
			// The lead time matters: a schedule starting in two minutes
			// is rejected by Azure.
			name:   "inside the lead time",
			rule:   schedule.Rule{Timezone: "Europe/Rome", Hour: 9, Min: 5, Days: []schedule.Weekday{schedule.Mon}},
			want:   "2026-08-10T09:05:00+02:00",
			wantOK: true,
		},
		{
			name: "bounded window in the future",
			rule: schedule.Rule{
				Timezone:  "Europe/Rome",
				ValidFrom: &windowStart,
				ValidTo:   &windowEnd,
			},
			want:   "2026-12-24T00:00:00+01:00",
			wantOK: true,
		},
		{
			name: "window already closed",
			rule: schedule.Rule{
				Timezone: "Europe/Rome",
				ValidTo:  &pastEnd,
			},
			wantOK: false,
		},
		{
			name:    "unresolvable timezone",
			rule:    schedule.Rule{Timezone: "Middle/Earth"},
			wantErr: true,
		},
		{
			name:    "invalid time",
			rule:    schedule.Rule{Timezone: "Europe/Rome", Hour: 24},
			wantErr: true,
		},
		{
			name:    "unknown weekday",
			rule:    schedule.Rule{Timezone: "Europe/Rome", Days: []schedule.Weekday{"caturday"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := firstOccurrence(tt.rule, now)
			if tt.wantErr {
				if err == nil {
					t.Fatal("firstOccurrence() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("firstOccurrence() error = %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("firstOccurrence() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.Format(time.RFC3339) != tt.want {
				t.Errorf("firstOccurrence() = %s, want %s", got.Format(time.RFC3339), tt.want)
			}
		})
	}
}

func TestResourceSchedule(t *testing.T) {
	db := spec.Resource{
		ID:         "app-db",
		Type:       spec.ResourceTypeRelationalDatabase,
		Provider:   spec.ProviderAzure,
		Properties: map[string]any{"engine": "postgres", "version": "15"},
	}
	bucket := spec.Resource{
		ID:         "assets",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderAzure,
		Properties: map[string]any{"bucket_name": "assets"},
	}

	withWindows := workWeekSchedule()
	withWindows.Exceptions = []schedule.Window{
		{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff},
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
			name:      "base rhythm",
			resource:  db,
			policies:  spec.Policies{Schedule: workWeekSchedule()},
			wantRules: 2,
		},
		{
			// Unlike GCP, Automation schedules carry an expiry time, so
			// exception windows are supported here.
			name:      "exception windows are supported",
			resource:  db,
			policies:  spec.Policies{Schedule: withWindows},
			wantRules: 5,
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

func TestDatabaseServerRejectsUnknownEngines(t *testing.T) {
	// Unreachable through decode, but a future engine added to the oneof
	// tag must not silently reach the ARM API with the wrong provider
	// namespace or an empty API version.
	s := databaseServer{engine: "cassandra"}
	if _, err := s.apiVersion(); err == nil {
		t.Error("apiVersion() = nil error for an unknown engine")
	}
	if _, err := s.powerActions(); err == nil {
		t.Error("powerActions() = nil error for an unknown engine")
	}
}

func TestWeekDayNames(t *testing.T) {
	got := weekDayNames([]schedule.Weekday{schedule.Mon, schedule.Sun, "caturday"})
	if len(got) != 2 || got[0] != "Monday" || got[1] != "Sunday" {
		t.Errorf("weekDayNames() = %v, want [Monday Sunday]", got)
	}
}

func keysOfAny(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
