// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	return dir
}

func TestDefaultProviderIsAbsentUnlessStated(t *testing.T) {
	// An installation that has never said which cloud it prefers should
	// be told to choose, not guessed at (RFC 014 §2.4).
	writeConfig(t, "ai:\n  provider: anthropic\n")

	c, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if c.Defaults.Provider != "" {
		t.Errorf("defaults.provider = %q, want it unset", c.Defaults.Provider)
	}
}

func TestDefaultProviderIsParsed(t *testing.T) {
	for _, want := range []string{"aws", "gcp", "azure"} {
		t.Run(want, func(t *testing.T) {
			writeConfig(t, "ai:\n  provider: anthropic\ndefaults:\n  provider: "+want+"\n")

			c, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if c.Defaults.Provider != want {
				t.Errorf("defaults.provider = %q, want %q", c.Defaults.Provider, want)
			}
		})
	}
}

func TestDefaultProviderRejectsUnknownValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "not a cloud", value: "oracle"},
		{
			// A default of "agnostic" would be circular: it is the value
			// resolution exists to replace.
			name:  "agnostic is circular",
			value: "agnostic",
		},
		{name: "wrong case", value: "AWS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeConfig(t, "ai:\n  provider: anthropic\ndefaults:\n  provider: "+tt.value+"\n")

			_, err := LoadConfig()
			if !errors.Is(err, ErrInvalidDefaultProvider) {
				t.Fatalf("LoadConfig() error = %v, want %v", err, ErrInvalidDefaultProvider)
			}
		})
	}
}

func TestDefaultConfigOmitsTheProviderDefault(t *testing.T) {
	// The file written on first run must not pin a cloud the user never
	// chose.
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)

	if _, err := LoadConfig(); err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	// #nosec G304 -- a path this test just created.
	b, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("failed to read the written config: %v", err)
	}
	for _, cloud := range []string{"aws", "gcp", "azure"} {
		if strings.Contains(string(b), "provider: "+cloud) {
			t.Errorf("the default config pins %s:\n%s", cloud, b)
		}
	}
}
