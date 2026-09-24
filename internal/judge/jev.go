// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultJevModel is sent when the config pins no version (RFC 021 §7.4).
const DefaultJevModel = "jev-latest"

// defaultJevEndpoint is TypeSafe's API base URL. CLOUDSDD_TYPESAFE_ENDPOINT
// overrides the base only: the path belongs to the client, so it cannot
// drift through configuration (RFC 011 §5).
const (
	defaultJevEndpoint = "https://api.typesafe.ai"
	systemOnePath      = "/v1/systemone"
)

// requestTimeout bounds a whole Ask, retry included. The call it stands
// for takes ~0.114 s; an answer later than this has lost its reason to
// exist (RFC 021 §2.5).
const requestTimeout = 3 * time.Second

// retryBackoff is the pause before the single retry of a 429 or 529, the
// vendor's prescribed backoff fitted inside requestTimeout.
const retryBackoff = 250 * time.Millisecond

// maxResponseBytes caps how much of a response is read. A decision is a
// few hundred bytes; the body is untrusted and bounded before it is read
// (RFC 021 §4.3).
const maxResponseBytes = 64 << 10 // 64 KiB

// The transport sentinels of RFC 021 §1.1: one vendor's statuses, kept
// here rather than beside the vocabulary in judge.go.
var (
	ErrUnauthorized = errors.New("judge: jev rejected the API key")
	ErrRejected     = errors.New("judge: jev rejected the request as malformed")
	ErrRateLimited  = errors.New("judge: jev rate limit reached")
	ErrOverloaded   = errors.New("judge: jev overloaded")
)

// statusErrors maps each status the vendor documents to its sentinel.
var statusErrors = map[int]error{
	http.StatusUnauthorized:        ErrUnauthorized,
	http.StatusUnprocessableEntity: ErrRejected,
	http.StatusTooManyRequests:     ErrRateLimited,
	529:                            ErrOverloaded,
}

// Jev is a Judge backed by TypeSafe's System One endpoint.
type Jev struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

var _ Judge = (*Jev)(nil)

// NewJev builds a client for the given model. It requires TYPESAFE_API_KEY:
// a client that can never authenticate must not be built.
func NewJev(model string) (*Jev, error) {
	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		return nil, errors.New("TYPESAFE_API_KEY environment variable is required")
	}
	if model == "" {
		model = DefaultJevModel
	}
	endpoint := os.Getenv("CLOUDSDD_TYPESAFE_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultJevEndpoint
	}

	return &Jev{
		endpoint: endpoint + systemOnePath,
		apiKey:   apiKey,
		model:    model,
		// No Client.Timeout: Ask's context bounds the whole call, retry
		// and backoff included, which a per-attempt timeout cannot.
		client: &http.Client{
			// The bearer key is for one host. A redirect is answered
			// as a status, never followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// wireRequest and wireQuestion are the request body, built field by field
// from the caller's questions: never a marshal of an internal type.
type wireRequest struct {
	Model     string                  `json:"model"`
	State     any                     `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireQuestion struct {
	Type         Primitive `json:"type"`
	Instructions string    `json:"instructions"`
	Criteria     any       `json:"criteria,omitempty"`
}

// Ask sends the questions about state and returns the validated decision.
// A 429 or 529 is retried once; everything else is final.
func (j *Jev) Ask(ctx context.Context, state any, questions map[string]Question) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, err := json.Marshal(wireRequest{Model: j.model, State: state, Questions: project(questions)})
	if err != nil {
		return Decision{}, fmt.Errorf("judge: failed to marshal request: %w", err)
	}

	decision, err := j.post(ctx, body, questions)
	if !errors.Is(err, ErrRateLimited) && !errors.Is(err, ErrOverloaded) {
		return decision, err
	}
	if err := pause(ctx, retryBackoff); err != nil {
		return Decision{}, err
	}
	return j.post(ctx, body, questions)
}

// project picks the one criteria shape each primitive uses (RFC 021 §8).
func project(questions map[string]Question) map[string]wireQuestion {
	wire := make(map[string]wireQuestion, len(questions))
	for id, q := range questions {
		w := wireQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case Choice:
			w.Criteria = q.Options
		case Score:
			w.Criteria = q.Levels
		}
		wire[id] = w
	}
	return wire
}

// post makes one attempt. The response body is never echoed into an
// error: it is untrusted and may repeat what was sent (RFC 021 §4.7).
func (j *Jev) post(ctx context.Context, body []byte, asked map[string]Question) (Decision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("judge: failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := j.client.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("judge: jev unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if sentinel, known := statusErrors[resp.StatusCode]; known {
			return Decision{}, sentinel
		}
		return Decision{}, fmt.Errorf("judge: jev returned status %d", resp.StatusCode)
	}
	return DecodeDecision(io.LimitReader(resp.Body, maxResponseBytes), asked)
}

// pause waits for d unless ctx ends first.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
