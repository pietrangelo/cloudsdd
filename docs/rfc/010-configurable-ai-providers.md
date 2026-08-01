# RFC 010: Configurable AI Providers (OpenAI & Local Models)

## 1. Context and Problem Statement
Currently, `cloudsdd` is hardcoded to use Anthropic's Claude 3.5 Sonnet. While this provides excellent accuracy, many enterprise users have strict data privacy policies that forbid sending infrastructure architectures to third-party APIs. To maximize adoption and flexibility, the CLI must allow users to configure their own AI backend, including completely offline, local models.

## 2. Proposed Architecture

We will introduce a configuration system and expand our `Translator` interface implementations.

### 2.1 Global Configuration (`~/.cloudsdd/config.yaml`)
We will introduce a global configuration file that the user can edit to control CLI behavior. 
Example structure:
```yaml
ai:
  provider: "ollama" # Options: anthropic, openai, ollama
  model: "llama3"    # E.g., claude-opus-5, gpt-4o, llama3
```
If the config file does not exist, the CLI will auto-generate it with Anthropic as the default to preserve backward compatibility.

### 2.2 New Translator Implementations
We already have the `Translator` interface in `internal/nlp/translator.go`. We will add two new implementations:

1. **`OpenAITranslator` (`internal/nlp/openai.go`)**:
   - Uses the official `github.com/sashabaranov/go-openai` SDK.
   - Relies on the `OPENAI_API_KEY` environment variable.
   - Uses OpenAI's `response_format = { "type": "json_object" }` to guarantee JSON output.

2. **`OllamaTranslator` (`internal/nlp/ollama.go`)**:
   - Interacts with a locally running Ollama daemon (usually `http://localhost:11434`).
   - Requires no API keys.
   - Uses Ollama's `format: "json"` parameter to force the local model to adhere strictly to JSON, minimizing hallucinations from smaller models like Llama 3.

### 2.3 CLI Integration
- Create `internal/config/config.go` to manage reading, parsing (via `gopkg.in/yaml.v3`), and writing the `config.yaml` file.
- Update the initialization logic in `cmd/cloudsdd/deploy.go` and `destroy.go` to read the config and instantiate the correct `Translator` based on `config.AI.Provider`.

## 3. Security Considerations
- **Privacy Guarantee**: By configuring `provider: ollama`, users are mathematically guaranteed that their infrastructure prompts never leave their local machine, ensuring absolute compliance with data sovereignty laws (GDPR, HIPAA, etc.).
- **API Keys**: We will continue to read API keys strictly from environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) rather than storing them in the plaintext `config.yaml`.

---

**Amendment (RFC 011 §1.1B3, 2026-08-01):** the model example above originally
read `claude-3-5-sonnet-20241022`, which was retired on 2025-10-28. That value
was also the implementation's hardcoded default, so a fresh install wrote it to
`~/.cloudsdd/config.yaml` and every translation failed with a 404. Defaults are
now resolved per provider in `internal/nlp` (`DefaultAnthropicModel`,
`DefaultOpenAIModel`, `DefaultOllamaModel`) rather than from a single constant,
so a config pinning `provider: ollama` without a model no longer inherits an
Anthropic model name.
