// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer starts an httptest server and points the relevant SDK at
// it via its base-URL environment variable.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// --- Ollama ------------------------------------------------------------

func ollamaAgainst(t *testing.T, handler http.HandlerFunc) *OllamaTranslator {
	t.Helper()
	srv := newTestServer(t, handler)
	t.Setenv("CLOUDSDD_OLLAMA_ENDPOINT", srv.URL)

	tr, err := NewOllamaTranslator("")
	if err != nil {
		t.Fatalf("NewOllamaTranslator() error: %v", err)
	}
	return tr
}

func TestOllamaTranslate(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "valid specification",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(ollamaResponse{Response: validSpecJSON})
			},
		},
		{
			name: "markdown fenced response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(ollamaResponse{Response: "```json\n" + validSpecJSON + "\n```"})
			},
		},
		{
			name: "empty response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(ollamaResponse{Response: ""})
			},
			wantErr: "empty response",
		},
		{
			name: "non-json response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(ollamaResponse{Response: "I cannot do that"})
			},
			wantErr: "failed to parse or validate",
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "model not found")
			},
			wantErr: "status 500",
		},
		{
			name: "malformed envelope",
			handler: func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, "{not json")
			},
			wantErr: "failed to decode response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := ollamaAgainst(t, tt.handler)

			got, err := tr.Translate(context.Background(), "a bucket", "")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Translate() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Translate() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Translate() unexpected error: %v", err)
			}
			if len(got.Resources) != 1 {
				t.Fatalf("Translate() returned %d resources, want 1", len(got.Resources))
			}
		})
	}
}

// TestOllamaSendsSharedPrompt verifies the request actually carries the
// full shared system prompt — the gap RFC 011 §1.1H1 identified.
func TestOllamaSendsSharedPrompt(t *testing.T) {
	var captured ollamaRequest

	tr := ollamaAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		json.NewEncoder(w).Encode(ollamaResponse{Response: validSpecJSON})
	})

	if _, err := tr.Translate(context.Background(), "a bucket", `{"resources":[{"id":"existing"}]}`); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	if !strings.Contains(captured.System, "relational_database") {
		t.Error("request system prompt does not document the resource types")
	}
	if !strings.Contains(captured.System, "bucket_name") {
		t.Error("request system prompt does not document the per-type properties")
	}
	if !strings.Contains(captured.System, "existing") {
		t.Error("request system prompt does not carry the ledger context")
	}
	if !strings.Contains(captured.System, "UNTRUSTED DATA") {
		t.Error("request system prompt does not label the ledger as untrusted")
	}
	if captured.Prompt != "a bucket" {
		t.Errorf("request prompt = %q, want the user's prompt", captured.Prompt)
	}
	if captured.Stream {
		t.Error("request set stream=true; the translator expects a single response")
	}
}

// TestOllamaBoundsResponseSize covers RFC 011 §1.1F2: the decode was
// previously unbounded.
func TestOllamaBoundsResponseSize(t *testing.T) {
	tr := ollamaAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		// Far more than maxResponseBytes, never terminated as valid JSON.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"response":"`)
		chunk := strings.Repeat("a", 64*1024)
		for written := 0; written < maxResponseBytes+(1<<20); written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := tr.Translate(context.Background(), "a bucket", "")
		if err == nil {
			t.Error("Translate() error = nil, want a decode failure on the truncated body")
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Translate() did not return; the response size is not bounded")
	}
}

// TestOllamaRespectsContextCancellation covers RFC 011 §1.1F1: the old
// client had no timeout and the CLI passed a context with no deadline.
func TestOllamaRespectsContextCancellation(t *testing.T) {
	release := make(chan struct{})
	tr := ollamaAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := tr.Translate(ctx, "a bucket", "")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Translate() error = nil, want a cancellation error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Translate() ignored the cancelled context")
	}
}

// --- OpenAI ------------------------------------------------------------

func openaiAgainst(t *testing.T, handler http.HandlerFunc) *OpenAITranslator {
	t.Helper()
	srv := newTestServer(t, handler)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	tr, err := NewOpenAITranslator("")
	if err != nil {
		t.Fatalf("NewOpenAITranslator() error: %v", err)
	}
	return tr
}

func openAIResponse(content string) string {
	return fmt.Sprintf(`{
      "id": "chatcmpl-test",
      "object": "chat.completion",
      "model": "gpt-4o",
      "choices": [{"index": 0, "message": {"role": "assistant", "content": %s}, "finish_reason": "stop"}]
    }`, mustJSONString(content))
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestOpenAITranslate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  int
		wantErr string
	}{
		{name: "valid specification", body: openAIResponse(validSpecJSON)},
		{name: "markdown fenced", body: openAIResponse("```json\n" + validSpecJSON + "\n```")},
		{
			// RFC 011 §1.1B2: Choices[0] was indexed unconditionally, so
			// a filtered completion panicked.
			name:    "no choices does not panic",
			body:    `{"id":"x","object":"chat.completion","model":"gpt-4o","choices":[]}`,
			wantErr: "no choices",
		},
		{
			name:    "empty content",
			body:    openAIResponse(""),
			wantErr: "empty response",
		},
		{
			name:    "server error",
			body:    `{"error":{"message":"boom","type":"server_error"}}`,
			status:  http.StatusInternalServerError,
			wantErr: "openai api error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := openaiAgainst(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				io.WriteString(w, tt.body)
			})

			got, err := tr.Translate(context.Background(), "a bucket", "")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Translate() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Translate() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Translate() unexpected error: %v", err)
			}
			if len(got.Resources) != 1 {
				t.Fatalf("Translate() returned %d resources, want 1", len(got.Resources))
			}
		})
	}
}

// TestOpenAISendsSharedPrompt is the other half of the RFC 011 §1.1H1
// fix: this translator previously sent a degraded prompt with no
// resource-type list.
func TestOpenAISendsSharedPrompt(t *testing.T) {
	type chatRequest struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	var captured chatRequest

	tr := openaiAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openAIResponse(validSpecJSON))
	})

	if _, err := tr.Translate(context.Background(), "a bucket", ""); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	if len(captured.Messages) != 2 {
		t.Fatalf("request carried %d messages, want 2 (system + user)", len(captured.Messages))
	}
	system := captured.Messages[0].Content
	if !strings.Contains(system, "relational_database") {
		t.Error("system prompt does not document the resource types")
	}
	if !strings.Contains(system, "bucket_name") {
		t.Error("system prompt does not document the per-type properties")
	}
	if captured.Messages[1].Content != "a bucket" {
		t.Errorf("user message = %q, want the user's prompt", captured.Messages[1].Content)
	}
}

// --- Anthropic ---------------------------------------------------------

func anthropicAgainst(t *testing.T, handler http.HandlerFunc) *AnthropicTranslator {
	t.Helper()
	srv := newTestServer(t, handler)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	tr, err := NewAnthropicTranslator("")
	if err != nil {
		t.Fatalf("NewAnthropicTranslator() error: %v", err)
	}
	return tr
}

func anthropicResponse(text, stopReason string) string {
	return fmt.Sprintf(`{
      "id": "msg_test",
      "type": "message",
      "role": "assistant",
      "model": "claude-opus-5",
      "content": [{"type": "text", "text": %s}],
      "stop_reason": %s,
      "usage": {"input_tokens": 1, "output_tokens": 1}
    }`, mustJSONString(text), mustJSONString(stopReason))
}

func TestAnthropicTranslate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  int
		wantErr string
	}{
		{name: "valid specification", body: anthropicResponse(validSpecJSON, "end_turn")},
		{name: "markdown fenced", body: anthropicResponse("```json\n"+validSpecJSON+"\n```", "end_turn")},
		{
			// Safety classifiers decline with HTTP 200, so this must be
			// checked before reading content.
			name:    "refusal is surfaced",
			body:    `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":0}}`,
			wantErr: "declined by the model's safety classifiers",
		},
		{
			name:    "no text block",
			body:    `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`,
			wantErr: "no text block",
		},
		{
			name:    "non-json content",
			body:    anthropicResponse("I cannot help with that", "end_turn"),
			wantErr: "failed to parse or validate",
		},
		{
			name:    "server error",
			body:    `{"type":"error","error":{"type":"api_error","message":"boom"}}`,
			status:  http.StatusInternalServerError,
			wantErr: "anthropic api error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := anthropicAgainst(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				io.WriteString(w, tt.body)
			})

			got, err := tr.Translate(context.Background(), "a bucket", "")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Translate() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Translate() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Translate() unexpected error: %v", err)
			}
			if len(got.Resources) != 1 {
				t.Fatalf("Translate() returned %d resources, want 1", len(got.Resources))
			}
		})
	}
}

func TestAnthropicSendsSharedPrompt(t *testing.T) {
	type messageRequest struct {
		Model  string `json:"model"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	var captured messageRequest

	tr := anthropicAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicResponse(validSpecJSON, "end_turn"))
	})

	if _, err := tr.Translate(context.Background(), "a bucket", ""); err != nil {
		t.Fatalf("Translate() error: %v", err)
	}

	if captured.Model != DefaultAnthropicModel {
		t.Errorf("request model = %q, want %q", captured.Model, DefaultAnthropicModel)
	}
	if len(captured.System) == 0 {
		t.Fatal("request carried no system prompt")
	}
	if !strings.Contains(captured.System[0].Text, "relational_database") {
		t.Error("system prompt does not document the resource types")
	}
}

// TestAllTranslatorsShareOneSystemPrompt is the cross-provider parity
// check: RFC 011 §1.1H1 existed because the three prompts had drifted.
func TestAllTranslatorsShareOneSystemPrompt(t *testing.T) {
	var ollamaSystem, openaiSystem, anthropicSystem string

	ollamaTr := ollamaAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		var req ollamaRequest
		json.NewDecoder(r.Body).Decode(&req)
		ollamaSystem = req.System
		json.NewEncoder(w).Encode(ollamaResponse{Response: validSpecJSON})
	})
	if _, err := ollamaTr.Translate(context.Background(), "x", ""); err != nil {
		t.Fatalf("ollama Translate() error: %v", err)
	}

	openaiTr := openaiAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			openaiSystem = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, openAIResponse(validSpecJSON))
	})
	if _, err := openaiTr.Translate(context.Background(), "x", ""); err != nil {
		t.Fatalf("openai Translate() error: %v", err)
	}

	anthropicTr := anthropicAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.System) > 0 {
			anthropicSystem = req.System[0].Text
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicResponse(validSpecJSON, "end_turn"))
	})
	if _, err := anthropicTr.Translate(context.Background(), "x", ""); err != nil {
		t.Fatalf("anthropic Translate() error: %v", err)
	}

	if ollamaSystem != systemPrompt {
		t.Error("ollama sent a system prompt that differs from the shared one")
	}
	if openaiSystem != systemPrompt {
		t.Error("openai sent a system prompt that differs from the shared one")
	}
	if anthropicSystem != systemPrompt {
		t.Error("anthropic sent a system prompt that differs from the shared one")
	}
}
