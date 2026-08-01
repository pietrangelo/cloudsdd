// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"cloudsdd/internal/spec"

	"github.com/sashabaranov/go-openai"
)

// OpenAITranslator implements Translator using OpenAI's chat completions.
type OpenAITranslator struct {
	client *openai.Client
	model  string
}

func NewOpenAITranslator(model string) (*OpenAITranslator, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY environment variable is required")
	}
	if model == "" {
		model = DefaultOpenAIModel
	}

	cfg := openai.DefaultConfig(apiKey)
	cfg.HTTPClient = &http.Client{Timeout: requestTimeout}
	// Honoured for OpenAI-compatible gateways, and what makes this
	// translator exercisable against an httptest server (RFC 011 §5).
	if base := os.Getenv("OPENAI_BASE_URL"); base != "" {
		cfg.BaseURL = base
	}

	return &OpenAITranslator{
		client: openai.NewClientWithConfig(cfg),
		model:  model,
	}, nil
}

func (t *OpenAITranslator) Translate(ctx context.Context, prompt string, contextJSON string) (spec.Specification, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := t.client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: t.model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: withLedgerContext(systemPrompt, contextJSON),
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
			ResponseFormat: &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONObject,
			},
		},
	)
	if err != nil {
		return spec.Specification{}, fmt.Errorf("openai api error: %w", err)
	}

	// A completion with no choices is possible (e.g. a content filter
	// stop) and indexing Choices[0] unconditionally panicked on it
	// (RFC 011 §1.1B2).
	if len(resp.Choices) == 0 {
		return spec.Specification{}, errors.New("openai: response contained no choices")
	}

	return parseSpecification("openai", resp.Choices[0].Message.Content)
}
