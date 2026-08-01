// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"cloudsdd/internal/spec"
)

// defaultOllamaEndpoint is the local Ollama generate endpoint. Overridable
// via CLOUDSDD_OLLAMA_ENDPOINT, which also makes the translator testable
// against an httptest server.
const defaultOllamaEndpoint = "http://localhost:11434/api/generate"

// OllamaTranslator implements Translator against a local Ollama instance.
type OllamaTranslator struct {
	endpoint string
	model    string
	client   *http.Client
}

func NewOllamaTranslator(model string) (*OllamaTranslator, error) {
	if model == "" {
		model = DefaultOllamaModel
	}

	endpoint := os.Getenv("CLOUDSDD_OLLAMA_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultOllamaEndpoint
	}

	return &OllamaTranslator{
		endpoint: endpoint,
		model:    model,
		// http.DefaultClient has no timeout, and the CLI passes a
		// context.Background() with no deadline: together they made a
		// hung Ollama block the CLI forever (RFC 011 §1.1F1).
		client: &http.Client{Timeout: requestTimeout},
	}, nil
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	System string `json:"system"`
	Stream bool   `json:"stream"`
	Format string `json:"format"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

func (t *OllamaTranslator) Translate(ctx context.Context, prompt string, contextJSON string) (spec.Specification, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	reqBody := ollamaRequest{
		Model:  t.model,
		Prompt: prompt,
		System: withLedgerContext(systemPrompt, contextJSON),
		Stream: false,
		Format: "json", // Enforces strict JSON generation locally
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return spec.Specification{}, fmt.Errorf("ollama: failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(b))
	if err != nil {
		return spec.Specification{}, fmt.Errorf("ollama: failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return spec.Specification{}, fmt.Errorf("ollama connection error (is ollama running?): %w", err)
	}
	defer resp.Body.Close()

	// The response is untrusted input: bound it before reading it into
	// memory (RFC 011 §1.1F2).
	body := io.LimitReader(resp.Body, maxResponseBytes)

	if resp.StatusCode != http.StatusOK {
		msg, err := io.ReadAll(body)
		if err != nil {
			return spec.Specification{}, fmt.Errorf("ollama error: status %d (response unreadable: %w)", resp.StatusCode, err)
		}
		return spec.Specification{}, fmt.Errorf("ollama error: status %d: %s", resp.StatusCode, truncateForError(string(msg)))
	}

	var resData ollamaResponse
	if err := json.NewDecoder(body).Decode(&resData); err != nil {
		return spec.Specification{}, fmt.Errorf("ollama: failed to decode response: %w", err)
	}

	return parseSpecification("ollama", resData.Response)
}
