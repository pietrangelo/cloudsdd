// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudsdd/internal/config"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

func TestWriteTempAndRename(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (dir, target string)
		wantErr string
	}{
		{
			name: "writes and installs atomically",
			setup: func(t *testing.T) (string, string) {
				d := t.TempDir()
				return d, filepath.Join(d, "ledger.json")
			},
		},
		{
			name: "overwrites an existing target",
			setup: func(t *testing.T) (string, string) {
				d := t.TempDir()
				target := filepath.Join(d, "ledger.json")
				if err := os.WriteFile(target, []byte("stale"), 0o600); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return d, target
			},
		},
		{
			name: "reports a missing directory",
			setup: func(t *testing.T) (string, string) {
				d := filepath.Join(t.TempDir(), "does-not-exist")
				return d, filepath.Join(d, "ledger.json")
			},
			wantErr: "failed to create temp ledger",
		},
		{
			// The rename is the step that makes the write atomic, so its
			// failure is the one that most needs to surface: a silent
			// failure here leaves the ledger at its previous content
			// while the caller believes the record landed.
			name: "reports a failed rename",
			setup: func(t *testing.T) (string, string) {
				d := t.TempDir()
				target := filepath.Join(d, "ledger.json")
				// A directory cannot be replaced by a rename from a file.
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return d, target
			},
			wantErr: "state:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, target := tt.setup(t)
			payload := []byte(`{"resources":[]}`)

			err := writeTempAndRename(dir, target, payload)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("writeTempAndRename() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("writeTempAndRename() unexpected error: %v", err)
			}

			got, readErr := os.ReadFile(target) // #nosec G304 -- test-controlled path
			if readErr != nil {
				t.Fatalf("target was not installed: %v", readErr)
			}
			if string(got) != string(payload) {
				t.Errorf("target content = %q, want %q", got, payload)
			}

			info, statErr := os.Stat(target)
			if statErr != nil {
				t.Fatalf("stat: %v", statErr)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("target permissions = %o, want 0600", perm)
			}

			// The temp file must not survive a successful rename.
			entries, dirErr := os.ReadDir(dir)
			if dirErr != nil {
				t.Fatalf("ReadDir: %v", dirErr)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".ledger-") {
					t.Errorf("temp file %q survived the rename", e.Name())
				}
			}
		})
	}
}

// TestReadLedgerReportsHomeFailure covers the error path out of dir() and
// ledgerPath(): an unusable CloudSDD home must surface, not be papered
// over with an empty ledger.
func TestReadLedgerReportsHomeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	// A regular file where the home directory should be: MkdirAll fails.
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, filepath.Join(blocker, "cloudsdd"))

	if _, err := ReadLedger(); err == nil {
		t.Fatal("ReadLedger() error = nil, want the unusable home directory to be reported")
	}
}

func TestRecordDeploymentReportsHomeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, filepath.Join(blocker, "cloudsdd"))

	r := resource("app-db", "", "dev", "eu-central-1")
	err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")})
	if err == nil {
		t.Fatal("RecordDeployment() error = nil, want the unusable home directory to be reported")
	}
}

func TestRecordDestructionReportsHomeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, filepath.Join(blocker, "cloudsdd"))

	r := resource("app-db", "", "dev", "eu-central-1")
	err := RecordDestruction([]spec.Resource{r}, []provider.Result{destroyedIn("app-db", "eu-central-1")})
	if err == nil {
		t.Fatal("RecordDestruction() error = nil, want the unusable home directory to be reported")
	}
}

// TestRecordDestructionReportsCorruptLedger covers the read-failure path
// inside the record functions.
func TestRecordDestructionReportsCorruptLedger(t *testing.T) {
	home := withTempHome(t)

	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := resource("app-db", "", "dev", "eu-central-1")
	err := RecordDestruction([]spec.Resource{r}, []provider.Result{destroyedIn("app-db", "eu-central-1")})
	if err == nil {
		t.Fatal("RecordDestruction() error = nil, want the parse failure to be reported")
	}
}

func TestRecordDeploymentReportsCorruptLedger(t *testing.T) {
	home := withTempHome(t)

	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	r := resource("app-db", "", "dev", "eu-central-1")
	err := RecordDeployment([]spec.Resource{r}, []provider.Result{appliedIn("app-db", "eu-central-1")})
	if err == nil {
		t.Fatal("RecordDeployment() error = nil, want the parse failure to be reported")
	}
}

// TestReadLedgerReportsUnreadableFile covers the branch between "no
// ledger yet", which is normal and yields an empty one, and "there is a
// ledger and it cannot be read", which is not. Collapsing the two would
// make an unreadable ledger look like a first run and hand the translator
// an empty view of the world.
func TestReadLedgerReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte("[]"), 0o000); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := readLedger()
	if err == nil {
		t.Fatal("readLedger() error = nil, want the read failure to be reported")
	}
	if !strings.Contains(err.Error(), "failed to read ledger") {
		t.Errorf("error = %q, want it to name the read failure", err)
	}
}

// TestLockFileReportsNonContentionFailure separates the two ways
// OpenFile can fail. An existing lock means somebody else holds it and is
// worth waiting on; anything else means the lock can never be taken, and
// spinning until the timeout would report contention that does not exist.
func TestLockFileReportsNonContentionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	home := filepath.Join(parent, "cloudsdd")
	if err := os.Mkdir(home, 0o500); err != nil { // read+execute, no write
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, home)

	unlock, err := lockFile()
	if err == nil {
		unlock()
		t.Fatal("lockFile() error = nil, want the unwritable directory to be reported")
	}
	if errors.Is(err, ErrLedgerLocked) {
		t.Errorf("error = %v, want a failure to acquire rather than contention", err)
	}
	if !strings.Contains(err.Error(), "failed to acquire ledger lock") {
		t.Errorf("error = %q, want it to name the acquisition failure", err)
	}
}

// TestWriteLedgerReportsHomeFailure covers writeLedger's own error path
// out of ledgerPath. RecordDeployment reaches writeLedger only after a
// successful read, so a home that breaks between the two is not otherwise
// exercised.
func TestWriteLedgerReportsHomeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, filepath.Join(blocker, "cloudsdd"))

	if err := writeLedger(&Ledger{}); err == nil {
		t.Fatal("writeLedger() error = nil, want the unusable home directory to be reported")
	}
}

// TestReadLedgerNormalisesNullResources covers the nil-slice branch. A
// ledger whose resources marshalled as JSON null — which pre-RFC-011
// files can be — must come back as an empty slice, so callers ranging
// over it and appending to it behave identically to a fresh ledger.
func TestReadLedgerNormalisesNullResources(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	if err := os.WriteFile(filepath.Join(home, "ledger.json"),
		[]byte(`{"resources":null}`), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	l, err := readLedger()
	if err != nil {
		t.Fatalf("readLedger() error = %v", err)
	}
	if l.Resources == nil {
		t.Error("Resources = nil, want an empty slice")
	}
	if len(l.Resources) != 0 {
		t.Errorf("Resources = %v, want empty", l.Resources)
	}
}

// TestReadLedgerReportsHomeFailureDirectly reaches readLedger's own error
// path. The exported ReadLedger takes the lock first, so a broken home
// fails there and this branch is never entered through it.
func TestReadLedgerReportsHomeFailureDirectly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, filepath.Join(blocker, "cloudsdd"))

	if _, err := readLedger(); err == nil {
		t.Fatal("readLedger() error = nil, want the unusable home directory to be reported")
	}
}

// TestWriteLedgerNeverMarshalsNull covers RFC 011 §1.1G4 at the writeLedger
// level: a nil slice must reach the file as [], because a ledger
// containing `null` is one a later read has to special-case forever.
func TestWriteLedgerNeverMarshalsNull(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	if err := writeLedger(&Ledger{Resources: nil}); err != nil {
		t.Fatalf("writeLedger() error = %v", err)
	}

	b, err := os.ReadFile(filepath.Join(home, "ledger.json"))
	if err != nil {
		t.Fatalf("ledger was not written: %v", err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("ledger contains null: %s", b)
	}
	if !strings.Contains(string(b), `"resources": []`) {
		t.Errorf("ledger = %s, want an empty array", b)
	}
}

// TestRecordDestructionWithNoDestroyedResultsIsNoop covers the early
// return. It matters beyond coverage: a destroy that failed for every
// resource must not take the lock or rewrite the ledger, or a run that
// changed nothing would still touch the file every other process
// synchronises on.
func TestRecordDestructionWithNoDestroyedResultsIsNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)

	r := resource("app-db", "", "dev", "eu-central-1")
	err := RecordDestruction([]spec.Resource{r}, []provider.Result{
		{ResourceID: "app-db", Region: "eu-central-1", Status: provider.StatusFailed},
	})
	if err != nil {
		t.Fatalf("RecordDestruction() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, "ledger.json")); !os.IsNotExist(err) {
		t.Errorf("ledger was written for a destroy that destroyed nothing (stat err = %v)", err)
	}
}
