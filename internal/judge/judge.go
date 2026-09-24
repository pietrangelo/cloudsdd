// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package judge is the vocabulary of a System One model (RFC 021 §2.2):
// closed questions, typed and bounded answers, and the strict decoder that
// turns an untrusted response into them. It knows what a noul is; it does
// not know what a VPC is, and nothing in this file speaks HTTP.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// Primitive is the shape of a question and of its answer (RFC 021 §1.1).
type Primitive string

const (
	// Choice picks one key from the options the question sent.
	Choice Primitive = "choice"
	// Score rates on an ordered rubric; the score indexes its levels.
	Score Primitive = "score"
	// Noul is the probability, in [0,1], that a claim holds.
	Noul Primitive = "noul"
)

// Question is one closed question. Options and Levels are the two shapes
// the wire's single `criteria` field takes: a Choice reads Options, a
// Score reads Levels, and a Noul reads neither (RFC 021 §8).
type Question struct {
	Type         Primitive
	Instructions string
	Options      map[string]string
	Levels       []string
}

// Answer is one validated answer. Only the field its Type names carries
// meaning; Confidence is zero for a Noul, which has none.
type Answer struct {
	Type       Primitive
	Choice     string
	Score      float64
	Noul       float64
	Confidence float64
}

// Decision is a response whose every answer passed validation against the
// questions that were asked: one answer per question, and no others.
type Decision struct {
	Model   string
	Answers map[string]Answer
}

// Judge answers closed questions about a state with typed, bounded
// answers. It is the System One counterpart to nlp.Translator: the
// translator writes a document, the judge picks from a list.
type Judge interface {
	Ask(ctx context.Context, state any, questions map[string]Question) (Decision, error)
}

// The sentinels of RFC 021 §4.3. Each malformed answer is rejected with
// one of them and never clamped: a clamp would turn a malformed 7.0 into a
// halt-worthy 4.0.
var (
	ErrMalformed             = errors.New("judge: malformed response")
	ErrProbabilityOutOfRange = errors.New("judge: probability outside [0,1]")
	ErrScoreOutOfRange       = errors.New("judge: score outside the rubric")
	ErrUnknownOption         = errors.New("judge: option that was never sent")
	ErrUnaskedQuestion       = errors.New("judge: answer to a question that was never asked")
	ErrMissingAnswer         = errors.New("judge: question left unanswered")
	ErrTypeMismatch          = errors.New("judge: answer of the wrong primitive")
)

// wireDecision is the response body as the vendor sends it. Answers stay
// raw until each is matched to its question, because only the question
// says which strict shape its answer must take.
type wireDecision struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// DecodeDecision reads one response body and validates it against the
// questions that were asked. Anything short of a complete, well-formed,
// in-range answer to every asked question is an error, and an error never
// comes with answers.
func DecodeDecision(r io.Reader, asked map[string]Question) (Decision, error) {
	var body *wireDecision
	if err := decodeStrict(r, &body); err != nil {
		return Decision{}, err
	}
	if body == nil {
		return Decision{}, fmt.Errorf("%w: null body", ErrMalformed)
	}

	for _, id := range slices.Sorted(maps.Keys(body.Answers)) {
		if _, ok := asked[id]; !ok {
			return Decision{}, fmt.Errorf("%w: %q", ErrUnaskedQuestion, id)
		}
	}

	answers := make(map[string]Answer, len(asked))
	for _, id := range slices.Sorted(maps.Keys(asked)) {
		answer, err := decodeAnswer(body.Answers[id], asked[id])
		if err != nil {
			return Decision{}, fmt.Errorf("answer %q: %w", id, err)
		}
		answers[id] = answer
	}
	return Decision{Model: body.Model, Answers: answers}, nil
}

// decodeAnswer matches one raw answer to its question: present, of the
// question's primitive, and then decoded strictly into that primitive's
// shape — so a field belonging to another primitive is an unknown field.
func decodeAnswer(raw json.RawMessage, q Question) (Answer, error) {
	if raw == nil || string(raw) == "null" {
		return Answer{}, ErrMissingAnswer
	}

	var head struct {
		Type Primitive `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Answer{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if head.Type != q.Type {
		return Answer{}, fmt.Errorf("%w: got %q, asked %q", ErrTypeMismatch, head.Type, q.Type)
	}

	switch q.Type {
	case Choice:
		return decodeChoice(raw, q.Options)
	case Score:
		return decodeScore(raw, q.Levels)
	case Noul:
		return decodeNoul(raw)
	default:
		return Answer{}, fmt.Errorf("%w: question of unknown primitive %q", ErrTypeMismatch, q.Type)
	}
}

func decodeChoice(raw json.RawMessage, options map[string]string) (Answer, error) {
	var w struct {
		Type          Primitive          `json:"type"`
		Choice        *string            `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    *float64           `json:"confidence"`
	}
	if err := decodeStrict(bytes.NewReader(raw), &w); err != nil {
		return Answer{}, err
	}
	if w.Choice == nil || w.Confidence == nil {
		return Answer{}, fmt.Errorf("%w: choice answer without its choice or confidence", ErrMalformed)
	}
	if _, sent := options[*w.Choice]; !sent {
		return Answer{}, fmt.Errorf("%w: %q", ErrUnknownOption, *w.Choice)
	}
	for _, key := range slices.Sorted(maps.Keys(w.Probabilities)) {
		if _, sent := options[key]; !sent {
			return Answer{}, fmt.Errorf("%w: probability for %q", ErrUnknownOption, key)
		}
		if err := probability(w.Probabilities[key]); err != nil {
			return Answer{}, err
		}
	}
	if err := probability(*w.Confidence); err != nil {
		return Answer{}, err
	}
	return Answer{Type: Choice, Choice: *w.Choice, Confidence: *w.Confidence}, nil
}

func decodeScore(raw json.RawMessage, levels []string) (Answer, error) {
	var w struct {
		Type          Primitive `json:"type"`
		Score         *float64  `json:"score"`
		Legend        *string   `json:"legend"`
		Probabilities []float64 `json:"probabilities"`
		Confidence    *float64  `json:"confidence"`
	}
	if err := decodeStrict(bytes.NewReader(raw), &w); err != nil {
		return Answer{}, err
	}
	if w.Score == nil || w.Confidence == nil {
		return Answer{}, fmt.Errorf("%w: score answer without its score or confidence", ErrMalformed)
	}
	if last := float64(len(levels) - 1); *w.Score < 0 || *w.Score > last {
		return Answer{}, fmt.Errorf("%w: %v not in [0, %v]", ErrScoreOutOfRange, *w.Score, last)
	}
	for _, p := range w.Probabilities {
		if err := probability(p); err != nil {
			return Answer{}, err
		}
	}
	if err := probability(*w.Confidence); err != nil {
		return Answer{}, err
	}
	return Answer{Type: Score, Score: *w.Score, Confidence: *w.Confidence}, nil
}

func decodeNoul(raw json.RawMessage) (Answer, error) {
	var w struct {
		Type Primitive `json:"type"`
		Noul *float64  `json:"noul"`
	}
	if err := decodeStrict(bytes.NewReader(raw), &w); err != nil {
		return Answer{}, err
	}
	if w.Noul == nil {
		return Answer{}, fmt.Errorf("%w: noul answer without its noul", ErrMalformed)
	}
	if err := probability(*w.Noul); err != nil {
		return Answer{}, err
	}
	return Answer{Type: Noul, Noul: *w.Noul}, nil
}

// probability admits the closed interval [0,1] and nothing else.
func probability(p float64) error {
	if p < 0 || p > 1 {
		return fmt.Errorf("%w: %v", ErrProbabilityOutOfRange, p)
	}
	return nil
}

// decodeStrict decodes exactly one JSON value from r into v, rejecting
// unknown fields and anything after the value.
func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the response", ErrMalformed)
	}
	return nil
}
