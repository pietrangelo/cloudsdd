// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package state

import (
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
