// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testAPIKey is recognisable on purpose: any error that echoes it is a
// credential leak (RFC 021 §4.7).
const testAPIKey = "ts-test-key-must-never-leak"

// jevAgainst starts an httptest server, points the client at it through
// CLOUDSDD_TYPESAFE_ENDPOINT, and returns a Jev for the given model.
func jevAgainst(t *testing.T, model string, handler http.HandlerFunc) *Jev {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDSDD_TYPESAFE_ENDPOINT", srv.URL)
	t.Setenv("TYPESAFE_API_KEY", testAPIKey)

	j, err := NewJev(model)
	if err != nil {
		t.Fatalf("NewJev(%q) error: %v", model, err)
	}
	return j
}

// respond writes a fixed body with a status, as the vendor would.
func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// sentRequest is the request body as the vendor reads it. It is decoded
// strictly, so a field the client adds without the RFC saying so fails.
type sentRequest struct {
	Model     string                  `json:"model"`
	State     json.RawMessage         `json:"state"`
	Questions map[string]sentQuestion `json:"questions"`
}

type sentQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

func TestNewJevRequiresTheAPIKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")

	if _, err := NewJev(""); err == nil {
		t.Fatal("NewJev() error = nil with TYPESAFE_API_KEY unset; a client that can never authenticate must not be built")
	}
}

// TestJevSendsTheRequestTheRFCDescribes asserts the wire field by field
// (RFC 021 §1.1): a silently renamed field would reach the vendor as an
// unknown one and every call would abstain without anyone noticing.
func TestJevSendsTheRequestTheRFCDescribes(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantModel string
	}{
		{name: "unpinned model defaults to jev-latest", model: "", wantModel: DefaultJevModel},
		{name: "pinned model is sent verbatim", model: "jev-1.13.0", wantModel: "jev-1.13.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				got     sentRequest
				method  string
				path    string
				auth    string
				ctype   string
				decoded error
			)
			j := jevAgainst(t, tt.model, func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
				dec := json.NewDecoder(r.Body)
				dec.DisallowUnknownFields()
				decoded = dec.Decode(&got)
				respond(http.StatusOK, answer(goodChoice, goodScore, goodNoul))(w, r)
			})

			state := map[string]string{"request": "remove the staging bucket"}
			if _, err := j.Ask(context.Background(), state, asked); err != nil {
				t.Fatalf("Ask() error: %v", err)
			}

			if decoded != nil {
				t.Fatalf("request body is not the RFC's shape: %v", decoded)
			}
			if method != http.MethodPost || path != "/v1/systemone" {
				t.Errorf("request = %s %s, want POST /v1/systemone", method, path)
			}
			if auth != "Bearer "+testAPIKey {
				t.Errorf("Authorization = %q, want the bearer key from TYPESAFE_API_KEY", auth)
			}
			if ctype != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ctype)
			}
			if got.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tt.wantModel)
			}
			assertJSON(t, "state", got.State, `{"request":"remove the staging bucket"}`)

			if len(got.Questions) != len(asked) {
				t.Fatalf("sent %d questions, want %d: %+v", len(got.Questions), len(asked), got.Questions)
			}
			wantCriteria := map[string]string{
				"action":      `{"deploy":"Create resources.","destroy":"Remove resources."}`,
				"risk":        `["none","routine","notable","serious","severe"]`,
				"unrequested": ``,
			}
			for id, q := range asked {
				sent := got.Questions[id]
				if sent.Type != string(q.Type) {
					t.Errorf("question %q type = %q, want %q", id, sent.Type, q.Type)
				}
				if sent.Instructions != q.Instructions {
					t.Errorf("question %q instructions = %q, want %q", id, sent.Instructions, q.Instructions)
				}
				assertJSON(t, "question "+id+" criteria", sent.Criteria, wantCriteria[id])
			}
		})
	}
}

// assertJSON compares two JSON values semantically; an empty want means
// the field must be absent from the wire.
func assertJSON(t *testing.T, what string, got json.RawMessage, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want it absent", what, got)
		}
		return
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Errorf("%s = %s, not JSON: %v", what, got, err)
		return
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want for %s: %v", what, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Errorf("%s = %s, want %s", what, gb, wb)
	}
}

// TestJevAnswersEachPrimitive is the happy path for each primitive, asked
// alone, so a client that only handles one shape fails the others.
func TestJevAnswersEachPrimitive(t *testing.T) {
	tests := []struct {
		id   string
		body string
		want Answer
	}{
		{id: "action", body: goodChoice, want: Answer{Type: Choice, Choice: "deploy", Confidence: 0.85}},
		{id: "risk", body: goodScore, want: Answer{Type: Score, Score: 2.6, Confidence: 0.7}},
		{id: "unrequested", body: goodNoul, want: Answer{Type: Noul, Noul: 0.25}},
	}

	for _, tt := range tests {
		t.Run(string(tt.want.Type), func(t *testing.T) {
			body := `{"model":"jev-1.13.0","answers":{"` + tt.id + `":` + tt.body + `}}`
			j := jevAgainst(t, "", respond(http.StatusOK, body))

			got, err := j.Ask(context.Background(), "state", map[string]Question{tt.id: asked[tt.id]})
			if err != nil {
				t.Fatalf("Ask() error: %v", err)
			}
			if got.Model != "jev-1.13.0" {
				t.Errorf("Model = %q, want the resolved version the vendor reported", got.Model)
			}
			if len(got.Answers) != 1 || got.Answers[tt.id] != tt.want {
				t.Errorf("Answers = %+v, want only %q: %+v", got.Answers, tt.id, tt.want)
			}
		})
	}
}

// TestJevMapsVendorErrorsToSentinels pins §1.1's four statuses. 429 and
// 529 are retried exactly once (§2.5), so a persistent one is two calls;
// 401 and 422 cannot improve on a retry, so they are one. The server
// echoes the key in its body to prove the error never repeats it.
func TestJevMapsVendorErrorsToSentinels(t *testing.T) {
	tests := []struct {
		status   int
		want     error
		attempts int32
	}{
		{status: http.StatusUnauthorized, want: ErrUnauthorized, attempts: 1},
		{status: http.StatusUnprocessableEntity, want: ErrRejected, attempts: 1},
		{status: http.StatusTooManyRequests, want: ErrRateLimited, attempts: 2},
		{status: 529, want: ErrOverloaded, attempts: 2},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status)+"/"+tt.want.Error(), func(t *testing.T) {
			var calls atomic.Int32
			j := jevAgainst(t, "", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				respond(tt.status, `{"error":"bad key `+testAPIKey+`"}`)(w, r)
			})

			got, err := j.Ask(context.Background(), "state", asked)

			if !errors.Is(err, tt.want) {
				t.Fatalf("Ask() error = %v, want %v", err, tt.want)
			}
			if got.Answers != nil {
				t.Errorf("Ask() returned answers alongside an error: %+v", got.Answers)
			}
			if n := calls.Load(); n != tt.attempts {
				t.Errorf("server saw %d calls, want %d", n, tt.attempts)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Errorf("error %q echoes the API key", err)
			}
		})
	}
}

// TestJevRetriesOnceThenSucceeds: a transient 429 is the vendor asking for
// backoff, not a failure (§2.5).
func TestJevRetriesOnceThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	j := jevAgainst(t, "", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			respond(http.StatusTooManyRequests, `{}`)(w, r)
			return
		}
		respond(http.StatusOK, answer(goodChoice, goodScore, goodNoul))(w, r)
	})

	got, err := j.Ask(context.Background(), "state", asked)
	if err != nil {
		t.Fatalf("Ask() error = %v, want the retried answer", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server saw %d calls, want 2", n)
	}
	if got.Answers["action"].Choice != "deploy" {
		t.Errorf("Answers = %+v, want the second response's", got.Answers)
	}
}

// TestJevRejectsHostileBodies covers §4.3's transport half: the body is
// bounded before it is read, and a body that ends early is malformed.
func TestJevRejectsHostileBodies(t *testing.T) {
	// A complete, valid response whose model string alone exceeds the cap:
	// only a bounded read rejects it.
	oversized := `{"model":"` + strings.Repeat("a", maxResponseBytes) + `","answers":{` +
		`"action":` + goodChoice + `,"risk":` + goodScore + `,"unrequested":` + goodNoul + `}}`
	full := answer(goodChoice, goodScore, goodNoul)

	tests := []struct {
		name string
		body string
	}{
		{name: "body exceeding maxResponseBytes", body: oversized},
		{name: "truncated body", body: full[:len(full)/2]},
		{name: "empty body", body: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := jevAgainst(t, "", respond(http.StatusOK, tt.body))

			got, err := j.Ask(context.Background(), "state", asked)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Ask() error = %v, want %v", err, ErrMalformed)
			}
			if got.Answers != nil {
				t.Errorf("Ask() returned answers alongside an error: %+v", got.Answers)
			}
		})
	}
}

// TestJevHonoursItsOwnDeadline pins §2.5: the adviser gets three seconds
// even when the caller gives it forever, because a slow answer has already
// lost its reason to exist.
func TestJevHonoursItsOwnDeadline(t *testing.T) {
	if requestTimeout != 3*time.Second {
		t.Fatalf("requestTimeout = %v, want the 3s of RFC 021 §2.5", requestTimeout)
	}

	release := make(chan struct{})
	j := jevAgainst(t, "", func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := j.Ask(context.Background(), "state", asked)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Ask() error = %v, want %v", err, context.DeadlineExceeded)
		}
		if elapsed := time.Since(start); elapsed > requestTimeout+time.Second {
			t.Errorf("Ask() returned after %v, want within %v", elapsed, requestTimeout)
		}
	case <-time.After(requestTimeout + 5*time.Second):
		t.Fatal("Ask() did not return; the request is not bounded by requestTimeout")
	}
}

// TestJevHonoursTheCallersDeadline: a caller with less patience than the
// adviser wins.
func TestJevHonoursTheCallersDeadline(t *testing.T) {
	release := make(chan struct{})
	j := jevAgainst(t, "", func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := j.Ask(ctx, "state", asked)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ask() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Ask() returned after %v, want it to stop at the caller's 100ms", elapsed)
	}
}

// TestNewJevDefaultsToTheVendor pins the production endpoint: with no
// override, the path is appended to the vendor's base URL.
func TestNewJevDefaultsToTheVendor(t *testing.T) {
	t.Setenv("CLOUDSDD_TYPESAFE_ENDPOINT", "")
	t.Setenv("TYPESAFE_API_KEY", testAPIKey)

	j, err := NewJev("")
	if err != nil {
		t.Fatalf("NewJev() error: %v", err)
	}
	if want := "https://api.typesafe.ai/v1/systemone"; j.endpoint != want {
		t.Errorf("endpoint = %q, want %q", j.endpoint, want)
	}
}

// TestJevFailsWithoutAnswers covers every other way an Ask can fail: each
// is an error, never a decision, never a retry, and never the key.
func TestJevFailsWithoutAnswers(t *testing.T) {
	tests := []struct {
		name     string
		state    any
		handler  http.HandlerFunc
		attempts int32
	}{
		{
			name:     "status the vendor does not document",
			state:    "state",
			handler:  respond(http.StatusInternalServerError, `{"error":"`+testAPIKey+`"}`),
			attempts: 1,
		},
		{
			// A well-formed decision under a failure status is still a
			// failure: the status is read before the body.
			name:     "valid decision under an undocumented status",
			state:    "state",
			handler:  respond(http.StatusServiceUnavailable, answer(goodChoice, goodScore, goodNoul)),
			attempts: 1,
		},
		{
			// Following it would carry the bearer key to another host.
			name:  "redirect is not followed",
			state: "state",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			},
			attempts: 1,
		},
		{
			name:     "state that cannot be marshalled is never sent",
			state:    make(chan int),
			handler:  respond(http.StatusOK, answer(goodChoice, goodScore, goodNoul)),
			attempts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			j := jevAgainst(t, "", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tt.handler(w, r)
			})

			got, err := j.Ask(context.Background(), tt.state, asked)
			if err == nil {
				t.Fatalf("Ask() error = nil, Decision = %+v", got)
			}
			if got.Answers != nil {
				t.Errorf("Ask() returned answers alongside an error: %+v", got.Answers)
			}
			if n := calls.Load(); n != tt.attempts {
				t.Errorf("server saw %d calls, want %d", n, tt.attempts)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Errorf("error %q echoes the API key", err)
			}
		})
	}
}

// TestJevRejectsAnUnusableEndpoint: a malformed override is an error on
// the first Ask, not a panic and not a request somewhere else.
func TestJevRejectsAnUnusableEndpoint(t *testing.T) {
	t.Setenv("CLOUDSDD_TYPESAFE_ENDPOINT", "::not a url")
	t.Setenv("TYPESAFE_API_KEY", testAPIKey)

	j, err := NewJev("")
	if err != nil {
		t.Fatalf("NewJev() error: %v", err)
	}
	if _, err := j.Ask(context.Background(), "state", asked); err == nil {
		t.Fatal("Ask() error = nil against an endpoint that is not a URL")
	}
}

// TestJevBackoffYieldsToTheCallersDeadline: the retry waits inside the
// caller's budget, never past it.
func TestJevBackoffYieldsToTheCallersDeadline(t *testing.T) {
	var calls atomic.Int32
	j := jevAgainst(t, "", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		respond(http.StatusTooManyRequests, `{}`)(w, r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), retryBackoff/5)
	defer cancel()

	start := time.Now()
	if _, err := j.Ask(ctx, "state", asked); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Ask() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server saw %d calls, want 1: the retry outlived the caller", n)
	}
	if elapsed := time.Since(start); elapsed >= retryBackoff {
		t.Errorf("Ask() returned after %v, want it to stop at the caller's %v, not sit out the %v backoff", elapsed, retryBackoff/5, retryBackoff)
	}
}
