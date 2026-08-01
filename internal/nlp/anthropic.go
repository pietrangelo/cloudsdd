// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"context"
	"errors"
	"fmt"
	"os"

	"cloudsdd/internal/spec"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicTranslator implements Translator using Anthropic's Claude models.
type AnthropicTranslator struct {
	client *anthropic.Client
	model  string
}

// NewAnthropicTranslator creates a new translator. It requires ANTHROPIC_API_KEY
// to be set in the environment.
func NewAnthropicTranslator(model string) (*AnthropicTranslator, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, errors.New("ANTHROPIC_API_KEY environment variable is required")
	}

	if model == "" {
		model = DefaultAnthropicModel
	}

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithRequestTimeout(requestTimeout),
	}
	// Honoured for local gateways and proxies, and what makes this
	// translator exercisable against an httptest server (RFC 011 §5).
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}

	client := anthropic.NewClient(opts...)

	return &AnthropicTranslator{client: &client, model: model}, nil
}

// Translate sends the natural language prompt to Claude and parses the resulting JSON.
func (t *AnthropicTranslator) Translate(ctx context.Context, prompt string, contextJSON string) (spec.Specification, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	message, err := t.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(t.model),
		MaxTokens: 8192,
		System: []anthropic.TextBlockParam{
			{Text: withLedgerContext(systemPrompt, contextJSON)},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return spec.Specification{}, fmt.Errorf("anthropic api error: %w", err)
	}

	// Safety classifiers can decline a request. That arrives as a
	// successful response carrying stop_reason "refusal", not as an
	// error, so it must be checked before reading content.
	if message.StopReason == anthropic.StopReasonRefusal {
		return spec.Specification{}, errors.New("anthropic: request was declined by the model's safety classifiers")
	}

	var jsonText string
	for _, block := range message.Content {
		if block.Text != "" {
			jsonText = block.Text
			break
		}
	}
	if jsonText == "" {
		return spec.Specification{}, errors.New("anthropic: no text block found in response")
	}

	return parseSpecification("anthropic", jsonText)
}
