// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package config loads the user's local CloudSDD configuration
// (RFC 010): today, which AI provider and model to use for translation.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"cloudsdd/internal/nlp"
)

// DefaultProvider is the AI provider used when the config file does not
// pin one.
const DefaultProvider = "anthropic"

type AIConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// DefaultsConfig carries machine-wide defaults applied when a
// Specification does not state a preference of its own.
type DefaultsConfig struct {
	// Provider breaks a tie for resources declaring "agnostic", after
	// the Specification's own policies.provider_preference and before
	// CloudSDD refuses to choose (RFC 014 §2.4).
	//
	// Absent by default: an installation that has never said which cloud
	// it prefers should be told to choose, not guessed at.
	Provider string `yaml:"provider,omitempty"`
}

type Config struct {
	AI       AIConfig       `yaml:"ai"`
	Defaults DefaultsConfig `yaml:"defaults,omitempty"`
}

// ErrInvalidDefaultProvider indicates a defaults.provider naming
// something that is not a concrete cloud.
var ErrInvalidDefaultProvider = errors.New("config: defaults.provider must be one of aws, gcp, azure")

// validate checks the fields that have a closed set of values. The AI
// provider is left to the translator factory, which already rejects an
// unknown one with a better message.
func (c *Config) validate() error {
	switch c.Defaults.Provider {
	case "", "aws", "gcp", "azure":
		return nil
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidDefaultProvider, c.Defaults.Provider)
	}
}

// HomeEnv overrides the directory CloudSDD keeps its local state in. It
// exists so the config and ledger can be exercised against a temp dir
// instead of the invoking user's real home, which is what made both
// packages untestable before RFC 011 §5.
const HomeEnv = "CLOUDSDD_HOME"

// Dir returns the CloudSDD configuration directory, creating it with
// owner-only permissions if absent.
func Dir() (string, error) {
	dir := os.Getenv(HomeEnv)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: failed to resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".cloudsdd")
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil { // #nosec G703 -- dir is operator-controlled (env var), not Specification input
		return "", fmt.Errorf("config: failed to create config dir %q: %w", dir, err)
	}
	return dir, nil
}

// LoadConfig reads ~/.cloudsdd/config.yaml, writing a default file on
// first run.
//
// Every error here is now propagated. Previously the marshal and write of
// the default config were both discarded (RFC 011 §1.1F3), so a config
// directory that could not be written failed silently and the user was
// given no clue why their settings never persisted.
func LoadConfig() (*Config, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(dir, "config.yaml")

	// #nosec G304 -- configPath is derived from the operator-controlled
	// CloudSDD home directory, never from Specification or model input.
	b, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: failed to read %q: %w", configPath, err)
		}
		return writeDefaultConfig(configPath)
	}

	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: failed to parse %q: %w", configPath, err)
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func writeDefaultConfig(path string) (*Config, error) {
	c := &Config{}
	c.applyDefaults()

	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("config: failed to marshal default config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("config: failed to write default config %q: %w", path, err)
	}
	return c, nil
}

// applyDefaults fills in any unset field. The model default is resolved
// per provider rather than from a single constant: a config pinning
// provider "ollama" without a model must not inherit an Anthropic model
// name.
func (c *Config) applyDefaults() {
	if c.AI.Provider == "" {
		c.AI.Provider = DefaultProvider
	}
	if c.AI.Model == "" {
		c.AI.Model = DefaultModelFor(c.AI.Provider)
	}
}

// DefaultModelFor returns the default model for an AI provider, or the
// empty string for an unknown one (the translator factory rejects it).
func DefaultModelFor(provider string) string {
	switch provider {
	case "anthropic":
		return nlp.DefaultAnthropicModel
	case "openai":
		return nlp.DefaultOpenAIModel
	case "ollama":
		return nlp.DefaultOllamaModel
	default:
		return ""
	}
}
