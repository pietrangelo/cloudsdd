// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloudsdd/internal/config"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)
	return home
}

func resource(id, account, environment, region string) spec.Resource {
	return spec.Resource{
		ID:       id,
		Type:     spec.ResourceTypeObjectStorage,
		Provider: spec.ProviderAWS,
		Account:  account,
		Scope:    spec.Scope{Environment: environment, Region: region},
		Properties: map[string]any{
			"bucket_name": id,
		},
	}
}

func appliedIn(id, region string) provider.Result {
	return provider.Result{ResourceID: id, Region: region, Status: provider.StatusApplied}
}

func destroyedIn(id, region string) provider.Result {
	return provider.Result{ResourceID: id, Region: region, Status: provider.StatusDestroyed}
}

func TestRecordDeploymentAndRead(t *testing.T) {
	withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources, want 1", len(l.Resources))
	}
	if l.Resources[0].ID != "app-db" {
		t.Errorf("resource ID = %q, want %q", l.Resources[0].ID, "app-db")
	}
	if l.LastUpdated.IsZero() {
		t.Error("LastUpdated was not set")
	}
}

// TestScopeKeyedIdentity is the regression test for RFC 011 §1.1G1: the
// ledger matched on Resource.ID alone, so the same logical resource
// deployed to two environments collapsed into one entry.
func TestScopeKeyedIdentity(t *testing.T) {
	withTempHome(t)

	dev := resource("app-db", "", "dev", "eu-central-1")
	prod := resource("app-db", "", "prod", "eu-central-1")

	if err := RecordDeployment(
		[]spec.Resource{dev},
		[]provider.Result{appliedIn("app-db", "eu-central-1")},
	); err != nil {
		t.Fatalf("RecordDeployment(dev) error: %v", err)
	}
	if err := RecordDeployment(
		[]spec.Resource{prod},
		[]provider.Result{appliedIn("app-db", "eu-central-1")},
	); err != nil {
		t.Fatalf("RecordDeployment(prod) error: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 2 {
		t.Fatalf("ledger has %d resources, want 2 (dev and prod are distinct)", len(l.Resources))
	}

	// Destroying dev must leave prod intact.
	if err := RecordDestruction(
		[]spec.Resource{dev},
		[]provider.Result{destroyedIn("app-db", "eu-central-1")},
	); err != nil {
		t.Fatalf("RecordDestruction(dev) error: %v", err)
	}

	l, err = ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources after destroying dev, want 1", len(l.Resources))
	}
	if l.Resources[0].Scope.Environment != "prod" {
		t.Errorf("surviving resource environment = %q, want %q", l.Resources[0].Scope.Environment, "prod")
	}
}

func TestScopeKeyedIdentityAcrossDimensions(t *testing.T) {
	tests := []struct {
		name string
		a, b spec.Resource
	}{
		{
			name: "different account",
			a:    resource("db", "staging", "", "eu-central-1"),
			b:    resource("db", "production", "", "eu-central-1"),
		},
		{
			name: "different environment",
			a:    resource("db", "", "dev", "eu-central-1"),
			b:    resource("db", "", "prod", "eu-central-1"),
		},
		{
			name: "different region",
			a:    resource("db", "", "dev", "eu-central-1"),
			b:    resource("db", "", "dev", "us-east-1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempHome(t)

			if err := RecordDeployment([]spec.Resource{tt.a}, []provider.Result{appliedIn("db", tt.a.Scope.Region)}); err != nil {
				t.Fatalf("RecordDeployment(a) error: %v", err)
			}
			if err := RecordDeployment([]spec.Resource{tt.b}, []provider.Result{appliedIn("db", tt.b.Scope.Region)}); err != nil {
				t.Fatalf("RecordDeployment(b) error: %v", err)
			}

			l, err := ReadLedger()
			if err != nil {
				t.Fatalf("ReadLedger() error: %v", err)
			}
			if len(l.Resources) != 2 {
				t.Fatalf("ledger has %d resources, want 2 — %s must not collapse", len(l.Resources), tt.name)
			}
		})
	}
}

func TestRecordDeploymentUpsertsSameScope(t *testing.T) {
	withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	for i := 0; i < 3; i++ {
		if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
			t.Fatalf("RecordDeployment() error: %v", err)
		}
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources, want 1 (repeated deploys upsert)", len(l.Resources))
	}
}

// TestRecordsOnlySuccessfulResults is the regression test for RFC 011
// §1.1G2: the CLI passed the whole Specification, so a partial failure
// either recorded resources that were never created or — because it
// returned early — recorded none at all.
func TestRecordsOnlySuccessfulResults(t *testing.T) {
	withTempHome(t)

	resources := []spec.Resource{
		resource("created", "", "dev", "eu-central-1"),
		resource("failed", "", "dev", "eu-central-1"),
	}
	results := []provider.Result{
		appliedIn("created", "eu-central-1"),
		{ResourceID: "failed", Region: "eu-central-1", Status: provider.StatusFailed},
	}

	if err := RecordDeployment(resources, results); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources, want only the applied one", len(l.Resources))
	}
	if l.Resources[0].ID != "created" {
		t.Errorf("recorded %q, want the resource that actually applied", l.Resources[0].ID)
	}
}

func TestRecordDeploymentFansOutMultiRegion(t *testing.T) {
	withTempHome(t)

	r := resource("bucket", "", "dev", "")
	r.Scope.Regions = []string{"eu-central-1", "us-east-1"}

	results := []provider.Result{
		appliedIn("bucket", "eu-central-1"),
		appliedIn("bucket", "us-east-1"),
	}
	if err := RecordDeployment([]spec.Resource{r}, results); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 2 {
		t.Fatalf("ledger has %d resources, want one row per region actually applied", len(l.Resources))
	}
	for _, entry := range l.Resources {
		if entry.Scope.Region == "" {
			t.Error("ledger entry has no region; each row must be pinned to one region")
		}
		if entry.Scope.Regions != nil {
			t.Error("ledger entry retained the plural Regions form; it records what exists, not the request shape")
		}
	}
}

func TestRecordWithNoSuccessfulResultsIsNoop(t *testing.T) {
	withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	results := []provider.Result{{ResourceID: "app-db", Region: "eu-central-1", Status: provider.StatusFailed}}

	if err := RecordDeployment([]spec.Resource{r}, results); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	// Nothing succeeded, so no ledger file should have been created.
	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 0 {
		t.Errorf("ledger has %d resources, want 0", len(l.Resources))
	}
}

// TestEmptiedLedgerMarshalsAsArray covers RFC 011 §1.1G4.
func TestEmptiedLedgerMarshalsAsArray(t *testing.T) {
	home := withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}
	if err := RecordDestruction([]spec.Resource{r}, []provider.Result{destroyedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDestruction() error: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(home, "ledger.json"))
	if err != nil {
		t.Fatalf("reading ledger: %v", err)
	}
	if strings.Contains(string(raw), `"resources": null`) {
		t.Errorf("emptied ledger marshalled resources as null, want []:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"resources": []`) {
		t.Errorf("emptied ledger does not contain an empty array:\n%s", raw)
	}
}

func TestReadLedgerOnMissingFile(t *testing.T) {
	withTempHome(t)

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if l.Resources == nil {
		t.Error("Resources = nil, want an empty non-nil slice")
	}
	if len(l.Resources) != 0 {
		t.Errorf("Resources has %d entries, want 0", len(l.Resources))
	}
}

func TestReadLedgerOnCorruptFile(t *testing.T) {
	home := withTempHome(t)

	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := ReadLedger(); err == nil {
		t.Fatal("ReadLedger() error = nil, want a parse error")
	}
}

// TestLegacyLedgerMigrates covers the compatibility path in RFC 011 §2.7:
// a pre-RFC-011 ledger has no scope fields, which read as the empty
// string — exactly their previous meaning.
func TestLegacyLedgerMigrates(t *testing.T) {
	home := withTempHome(t)

	legacy := `{
  "last_updated": "2026-07-01T00:00:00Z",
  "resources": [
    {"id": "old-bucket", "type": "object_storage", "provider": "aws", "properties": {"bucket_name": "old-bucket"}}
  ]
}`
	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources, want 1", len(l.Resources))
	}
	got := l.Resources[0]
	if got.ID != "old-bucket" || got.Account != "" || got.Scope.Environment != "" || got.Scope.Region != "" {
		t.Errorf("legacy entry = %+v, want the unscoped resource preserved", got)
	}
}

func TestLedgerFileIsOwnerOnly(t *testing.T) {
	home := withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	info, err := os.Stat(filepath.Join(home, "ledger.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger.json permissions = %o, want 0600", perm)
	}
}

func TestWriteIsAtomicLeavingNoTempFiles(t *testing.T) {
	home := withTempHome(t)

	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ledger-") {
			t.Errorf("temp file %q was left behind after an atomic write", e.Name())
		}
	}
}

// TestConcurrentRecordsDoNotLoseEntries exercises the locking RFC 009
// §2.3 specified and RFC 011 §2.7 finally delivered. With only the
// process-local mutex this still passes in-process, but the lockfile is
// what makes it hold across separate cloudsdd invocations.
func TestConcurrentRecordsDoNotLoseEntries(t *testing.T) {
	withTempHome(t)

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "res-" + string(rune('a'+i))
			errs[i] = RecordDeployment(
				[]spec.Resource{resource(id, "", "dev", "eu-central-1")},
				[]provider.Result{appliedIn(id, "eu-central-1")},
			)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("RecordDeployment(%d) error: %v", i, err)
		}
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != n {
		t.Errorf("ledger has %d resources, want %d — concurrent writes lost entries", len(l.Resources), n)
	}
}

func TestStaleLockIsReclaimed(t *testing.T) {
	home := withTempHome(t)

	// A lock left behind by a dead process, aged past the stale window.
	lock := filepath.Join(home, "ledger.lock")
	if err := os.WriteFile(lock, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	stale := timeNowMinus(lockStaleAfter * 2)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() did not reclaim the stale lock: %v", err)
	}
}

func TestLedgerRoundTripsThroughJSON(t *testing.T) {
	withTempHome(t)

	r := resource("app-db", "acct", "prod", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("RecordDeployment() error: %v", err)
	}

	l, err := ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}

	// The ledger is injected into the AI prompt, so it must serialize.
	if _, err := json.Marshal(l); err != nil {
		t.Fatalf("ledger does not marshal: %v", err)
	}

	got := l.Resources[0]
	if got.Account != "acct" || got.Scope.Environment != "prod" || got.Scope.Region != "eu-central-1" {
		t.Errorf("scope not preserved through the round trip: %+v", got)
	}
}
