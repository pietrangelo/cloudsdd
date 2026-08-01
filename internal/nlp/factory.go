// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"fmt"
)

// NewTranslator is a factory function that instantiates the correct AI Provider
// based on the configuration file settings.
func NewTranslator(provider, model string) (Translator, error) {
	switch provider {
	case "anthropic":
		return NewAnthropicTranslator(model)
	case "openai":
		return NewOpenAITranslator(model)
	case "ollama":
		return NewOllamaTranslator(model)
	default:
		return nil, fmt.Errorf("unsupported AI provider: %q", provider)
	}
}
