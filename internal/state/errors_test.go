// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cloudsdd/internal/config"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// TestLockTimeoutIsReported covers the contended path: a lock held by a
// live process must fail loudly rather than silently proceeding and
// losing the other writer's entries.
func TestLockTimeoutIsReported(t *testing.T) {
	home := withTempHome(t)

	// A fresh lock, so it is not treated as stale.
	lock := filepath.Join(home, "ledger.lock")
	if err := os.WriteFile(lock, []byte("1\n"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Remove(lock) })

	// Shorten the wait so the test does not sit for the full timeout.
	origTimeout := lockTimeout
	t.Cleanup(func() { lockTimeout = origTimeout })
	lockTimeout = 200 * time.Millisecond

	_, err := ReadLedger()
	if err == nil {
		t.Fatal("ReadLedger() error = nil, want a lock timeout")
	}
	if !errors.Is(err, ErrLedgerLocked) {
		t.Errorf("error = %v, want it to wrap ErrLedgerLocked", err)
	}
}

// TestWriteFailureIsReported covers RFC 011 §1.1F3 at the ledger layer:
// a write that cannot complete must surface, not be discarded.
func TestWriteFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	home := filepath.Join(parent, "cloudsdd")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, home)

	// Seed a ledger, then make the directory read-only so the atomic
	// write cannot create its temp file.
	r := resource("app-db", "", "dev", "eu-central-1")
	if err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(home, 0o700) })

	other := resource("other", "", "dev", "eu-central-1")
	err := RecordDeployment([]spec.Resource{other}, []provider.Result{appliedIn("other", "eu-central-1")})
	if err == nil {
		t.Fatal("RecordDeployment() error = nil, want the failed write to be reported")
	}
}

func TestRecordDestructionOnMissingLedgerIsSafe(t *testing.T) {
	withTempHome(t)

	r := resource("never-deployed", "", "dev", "eu-central-1")
	err := RecordDestruction(
		[]spec.Resource{r},
		[]provider.Result{destroyedIn("never-deployed", "eu-central-1")},
	)
	if err != nil {
		t.Fatalf("RecordDestruction() on an absent ledger error: %v", err)
	}
}

// TestMatchResultsIgnoresUnknownResources: a Result naming a resource
// that is not in the Specification must be skipped rather than panicking
// or inventing an entry.
func TestMatchResultsIgnoresUnknownResources(t *testing.T) {
	resources := []spec.Resource{resource("known", "", "dev", "eu-central-1")}
	results := []provider.Result{
		appliedIn("known", "eu-central-1"),
		appliedIn("ghost", "eu-central-1"),
	}

	got := matchResults(resources, results, provider.StatusApplied)
	if len(got) != 1 {
		t.Fatalf("matchResults() returned %d resources, want only the known one", len(got))
	}
	if got[0].ID != "known" {
		t.Errorf("matchResults() returned %q, want %q", got[0].ID, "known")
	}
}
