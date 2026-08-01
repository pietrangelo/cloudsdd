// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudsdd/internal/nlp"
)

func TestLoadConfigWritesSecureDefaultOnFirstRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnv, home)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}

	if cfg.AI.Provider != DefaultProvider {
		t.Errorf("Provider = %q, want %q", cfg.AI.Provider, DefaultProvider)
	}
	// RFC 011 §1.1B3: the previous default was a model retired on
	// 2025-10-28, so a fresh install 404'd on every translation.
	if cfg.AI.Model != nlp.DefaultAnthropicModel {
		t.Errorf("Model = %q, want %q", cfg.AI.Model, nlp.DefaultAnthropicModel)
	}

	path := filepath.Join(home, "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("default config was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.yaml permissions = %o, want 0600", perm)
	}

	// The written file must round-trip to the same values.
	reloaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() on the written file: %v", err)
	}
	if *reloaded != *cfg {
		t.Errorf("reloaded config = %+v, want %+v", *reloaded, *cfg)
	}
}

func TestLoadConfigDefaultModelIsNotRetired(t *testing.T) {
	// Guards against a regression to any dated Claude 3.x snapshot: all
	// of those are retired and return 404.
	if strings.Contains(nlp.DefaultAnthropicModel, "claude-3") {
		t.Errorf("DefaultAnthropicModel = %q, which is a retired model family", nlp.DefaultAnthropicModel)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name         string
		file         string
		wantProvider string
		wantModel    string
		wantErr      string
	}{
		{
			name:         "fully specified",
			file:         "ai:\n  provider: openai\n  model: gpt-4o-mini\n",
			wantProvider: "openai",
			wantModel:    "gpt-4o-mini",
		},
		{
			// The default model must follow the provider, not fall back
			// to an Anthropic model name.
			name:         "provider without model",
			file:         "ai:\n  provider: ollama\n",
			wantProvider: "ollama",
			wantModel:    nlp.DefaultOllamaModel,
		},
		{
			name:         "model without provider",
			file:         "ai:\n  model: some-model\n",
			wantProvider: DefaultProvider,
			wantModel:    "some-model",
		},
		{
			name:         "empty file",
			file:         "",
			wantProvider: DefaultProvider,
			wantModel:    nlp.DefaultAnthropicModel,
		},
		{
			name:         "unknown provider keeps user value with no model default",
			file:         "ai:\n  provider: mystery\n",
			wantProvider: "mystery",
			wantModel:    "",
		},
		{
			name:    "malformed yaml",
			file:    "ai:\n\tprovider: [unclosed\n",
			wantErr: "failed to parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(HomeEnv, home)

			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(tt.file), 0o600); err != nil {
				t.Fatalf("setup: %v", err)
			}

			cfg, err := LoadConfig()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadConfig() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("LoadConfig() unexpected error: %v", err)
			}
			if cfg.AI.Provider != tt.wantProvider {
				t.Errorf("Provider = %q, want %q", cfg.AI.Provider, tt.wantProvider)
			}
			if cfg.AI.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", cfg.AI.Model, tt.wantModel)
			}
		})
	}
}

// TestLoadConfigReportsUnwritableDir covers RFC 011 §1.1F3: the marshal
// and write of the default config were both discarded, so a config
// directory that could not be written failed silently.
func TestLoadConfigReportsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	parent := t.TempDir()
	home := filepath.Join(parent, "cloudsdd")
	if err := os.Mkdir(home, 0o500); err != nil { // read+execute, no write
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(HomeEnv, home)

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want the failed default-config write to be reported")
	}
}

func TestLoadConfigReportsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	home := t.TempDir()
	t.Setenv(HomeEnv, home)

	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("ai:\n"), 0o000); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want the read failure to be reported")
	}
}

func TestDirCreatesWithOwnerOnlyPermissions(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", "cloudsdd")
	t.Setenv(HomeEnv, home)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error: %v", err)
	}
	if dir != filepath.Clean(home) {
		t.Errorf("Dir() = %q, want %q", dir, home)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Dir() did not create the directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory permissions = %o, want 0700", perm)
	}
}

func TestDefaultModelFor(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"anthropic", nlp.DefaultAnthropicModel},
		{"openai", nlp.DefaultOpenAIModel},
		{"ollama", nlp.DefaultOllamaModel},
		{"unknown", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := DefaultModelFor(tt.provider); got != tt.want {
				t.Errorf("DefaultModelFor(%q) = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

// TestDirDefaultsToHomeDirectory covers the branch taken when
// CLOUDSDD_HOME is unset, which is the path every real invocation takes
// and the one the env override exists to avoid in tests.
func TestDirDefaultsToHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv(HomeEnv, "")
	t.Setenv("HOME", home)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() error: %v", err)
	}

	want := filepath.Join(home, ".cloudsdd")
	if dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Dir() did not create the directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory permissions = %o, want 0700", perm)
	}
}

// TestDirReportsUncreatableDirectory covers the MkdirAll failure, which is
// distinct from the unwritable-directory case already tested: there the
// directory exists and cannot be written, here it cannot be created at
// all. Both must surface rather than leave LoadConfig reporting a missing
// config file.
func TestDirReportsUncreatableDirectory(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// A path under a regular file: MkdirAll cannot create it.
	t.Setenv(HomeEnv, filepath.Join(blocker, "cloudsdd"))

	if _, err := Dir(); err == nil {
		t.Fatal("Dir() error = nil, want the failed directory creation to be reported")
	} else if !strings.Contains(err.Error(), "failed to create config dir") {
		t.Errorf("error = %q, want it to name the creation failure", err)
	}
}
