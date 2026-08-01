# RFC 006: Natural Language CLI Entry Point

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 005](005-account-environment-region-scoping.md)

## 1. Context and Problem Statement
The CloudSDD project currently consists of a robust internal engine and AWS provider logic (`internal/engine`, `internal/provider`, `internal/spec`). However, there is no entry point.

The mandatory core goal is that the user must use **natural language** to describe the cloud resources they want to deploy. The system must act as an interface for powerful IaC tools (like Pulumi/AWS CDK) while enforcing state-of-the-art implementations based on cloud provider specifications. 

## 2. Proposed Architecture

We will build the CLI using `github.com/spf13/cobra`.

### 2.1 CLI Structure
The primary interaction model will be driven by natural language prompts.
*   `cloudsdd deploy "a highly available postgres database and a public s3 bucket"`
*   `cloudsdd destroy "the postgres database"`

### 2.2 The NLP-to-Engine Pipeline
When the user runs a command:
1. **Interpretation:** The CLI sends the natural language prompt, along with the strict SDD JSON Schema rules, to an LLM provider (e.g., OpenAI, Anthropic, or Gemini API).
2. **Translation:** The LLM returns a structured JSON payload conforming exactly to our `spec.Specification`.
3. **Validation & State-of-the-Art Defaults:** The `engine` parses the JSON. Crucially, the user doesn't need to specify complex security rules in their prompt; the underlying providers (e.g., AWS) will automatically enforce state-of-the-art configurations (e.g., forcing encryption at rest, blocking public access by default unless explicitly requested, configuring proper IAM boundaries).
4. **Execution:** The engine computes the diff (via Pulumi) and presents a clean, powerful preview to the user before applying it.

### 2.3 User Experience
The CLI must strike a balance: incredibly simple to invoke (just natural language), but exposing the professional power of Pulumi/CDK underneath. When a deployment is running, the CLI will output detailed, color-coded progress and infrastructure diffs.

## 3. Impacted JSON Schema
No changes to the SDD JSON Schema (`internal/spec/spec.go`) are required. The LLM will be instructed to adhere strictly to the existing `v1.0` schema.

## 4. Security Considerations
*   **Prompt Injection / Hostile Input:** The CLI will treat the LLM output as potentially hostile. It relies on the strict domain validation in `spec.ParseAndValidate` to prevent Mass Assignment and ensure only valid resources are passed to the engine.
*   **Credential Handling:** The CLI uses the cloud providers' default credential chains locally. Credentials are never logged or exposed.
*   **API Keys:** The CLI will require an API key for the LLM provider (e.g., `OPENAI_API_KEY` or `GEMINI_API_KEY`), which must be read securely from the environment.

## 5. Implementation Steps
1. Add `github.com/spf13/cobra` dependency.
2. Create `cmd/cloudsdd/main.go` and the root command.
3. Implement the `deploy` and `destroy` subcommands accepting string arguments.
4. Create an `internal/nlp` package to handle the LLM integration and translation.
5. Wire the NLP output to the existing `engine.DefaultEngine` and output the IaC diffs/results.
