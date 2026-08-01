// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package nlp provides the translation layer from Natural Language to SDD Specifications.
package nlp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloudsdd/internal/spec"
)

// Default models per provider. These are referenced by internal/config
// when the user's config file does not pin one.
//
// RFC 011 §1.1B3: the previous default, claude-3-5-sonnet-20241022, was
// retired on 2025-10-28. A fresh install wrote it to ~/.cloudsdd/config.yaml
// and every deploy/destroy failed with a 404 — the tool did not work out of
// the box.
const (
	DefaultAnthropicModel = "claude-opus-5"
	DefaultOpenAIModel    = "gpt-4o"
	DefaultOllamaModel    = "llama3"
)

// requestTimeout bounds a single translation call. Without it the CLI can
// hang indefinitely on an unresponsive endpoint (RFC 011 §1.1F1).
const requestTimeout = 2 * time.Minute

// maxResponseBytes caps how much of a translator response is read. The
// response is untrusted input and is parsed into memory, so it needs a
// ceiling (RFC 011 §1.1F2).
const maxResponseBytes = 4 << 20 // 4 MiB

// maxErrorOutputBytes caps how much raw model output is echoed into an
// error message. The output derives from a prompt containing the ledger,
// so an unbounded echo can spill infrastructure detail into logs (RFC 011
// §4).
const maxErrorOutputBytes = 2048

// Translator defines the contract for converting natural language into a structured spec.
type Translator interface {
	// Translate converts a user's natural language prompt into a strict Specification.
	Translate(ctx context.Context, prompt string, contextJSON string) (spec.Specification, error)
}

// stripMarkdownFence removes a ```json ... ``` wrapper that models
// sometimes emit despite being told not to.
func stripMarkdownFence(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "```json"):
		s = strings.TrimSuffix(strings.TrimPrefix(s, "```json"), "```")
	case strings.HasPrefix(s, "```"):
		s = strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```")
	}
	return strings.TrimSpace(s)
}

// parseSpecification cleans up and validates a model's raw output through
// the strict internal/spec parser, which is the trust boundary for
// everything a Translator produces.
func parseSpecification(provider, raw string) (spec.Specification, error) {
	jsonText := stripMarkdownFence(raw)
	if jsonText == "" {
		return spec.Specification{}, fmt.Errorf("%s: empty response", provider)
	}

	s, err := spec.ParseAndValidate(strings.NewReader(jsonText))
	if err != nil {
		return spec.Specification{}, fmt.Errorf("%s: failed to parse or validate translated JSON: %w (output: %s)",
			provider, err, truncateForError(jsonText))
	}
	return *s, nil
}

func truncateForError(s string) string {
	if len(s) <= maxErrorOutputBytes {
		return s
	}
	return s[:maxErrorOutputBytes] + "... [truncated]"
}
