// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package judge

import (
	"errors"
	"strings"
	"testing"
)

// asked is the question set every decoding case is validated against: one
// question of each primitive, so a single table covers all three answer
// shapes and every cross-question rule.
var asked = map[string]Question{
	"action": {
		Type:         Choice,
		Instructions: "Which operation is this?",
		Options: map[string]string{
			"deploy":  "Create resources.",
			"destroy": "Remove resources.",
		},
	},
	"risk": {
		Type:         Score,
		Instructions: "Rate the risk.",
		Levels:       []string{"none", "routine", "notable", "serious", "severe"},
	},
	"unrequested": {
		Type:         Noul,
		Instructions: "Does this remove something unrequested?",
	},
}

// answer renders a response body whose three answers are the given JSON
// fragments, so each table row varies exactly one answer.
func answer(action, risk, unrequested string) string {
	return `{"model":"jev-1.13.0","answers":{` +
		`"action":` + action + `,` +
		`"risk":` + risk + `,` +
		`"unrequested":` + unrequested +
		`},"usage":{"input_tokens":392,"output_tokens":65}}`
}

const (
	goodChoice = `{"type":"choice","choice":"deploy","probabilities":{"deploy":0.9,"destroy":0.1},"confidence":0.85}`
	goodScore  = `{"type":"score","score":2.6,"legend":"notable","probabilities":[0,0.1,0.3,0.5,0.1],"confidence":0.7}`
	goodNoul   = `{"type":"noul","noul":0.25}`
)

func TestDecodeDecisionAcceptsAWellFormedAnswerOfEachPrimitive(t *testing.T) {
	got, err := DecodeDecision(strings.NewReader(answer(goodChoice, goodScore, goodNoul)), asked)
	if err != nil {
		t.Fatalf("DecodeDecision rejected a well-formed response: %v", err)
	}

	want := Decision{
		Model: "jev-1.13.0",
		Answers: map[string]Answer{
			"action":      {Type: Choice, Choice: "deploy", Confidence: 0.85},
			"risk":        {Type: Score, Score: 2.6, Confidence: 0.7},
			"unrequested": {Type: Noul, Noul: 0.25},
		},
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if len(got.Answers) != len(want.Answers) {
		t.Fatalf("got %d answers, want %d: %+v", len(got.Answers), len(want.Answers), got.Answers)
	}
	for id, w := range want.Answers {
		if g := got.Answers[id]; g != w {
			t.Errorf("answer %q = %+v, want %+v", id, g, w)
		}
	}
}

// TestDecodeDecisionAcceptsTheBoundaries pins that the valid ranges are
// closed: a probability of exactly 0 or 1 and a score on either end of the
// rubric are answers, not errors.
func TestDecodeDecisionAcceptsTheBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"noul of zero", answer(goodChoice, goodScore, `{"type":"noul","noul":0}`)},
		{"noul of one", answer(goodChoice, goodScore, `{"type":"noul","noul":1}`)},
		{"score at the first level", answer(goodChoice, `{"type":"score","score":0,"confidence":1}`, goodNoul)},
		{"score at the last level", answer(goodChoice, `{"type":"score","score":4,"confidence":0}`, goodNoul)},
		{"choice of certainty", answer(`{"type":"choice","choice":"destroy","probabilities":{"destroy":1},"confidence":1}`, goodScore, goodNoul)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeDecision(strings.NewReader(tc.body), asked); err != nil {
				t.Errorf("DecodeDecision rejected a boundary value: %v", err)
			}
		})
	}
}

// TestDecodeDecisionRejectsRatherThanClamps is RFC 021 §4.3: every
// malformed answer is refused with its sentinel. A clamp would turn a
// malformed 7.0 into a halt-worthy 4.0, and an adviser that can be made to
// halt arbitrarily is a denial-of-service on the operator.
func TestDecodeDecisionRejectsRatherThanClamps(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		// Probabilities outside [0,1].
		{"noul above one", answer(goodChoice, goodScore, `{"type":"noul","noul":1.7}`), ErrProbabilityOutOfRange},
		{"noul below zero", answer(goodChoice, goodScore, `{"type":"noul","noul":-0.01}`), ErrProbabilityOutOfRange},
		{"noul just above one", answer(goodChoice, goodScore, `{"type":"noul","noul":1.0000001}`), ErrProbabilityOutOfRange},
		{"choice confidence above one", answer(`{"type":"choice","choice":"deploy","confidence":1.5}`, goodScore, goodNoul), ErrProbabilityOutOfRange},
		{"score confidence below zero", answer(goodChoice, `{"type":"score","score":1,"confidence":-0.2}`, goodNoul), ErrProbabilityOutOfRange},
		{"option probability above one", answer(`{"type":"choice","choice":"deploy","probabilities":{"deploy":2},"confidence":0.9}`, goodScore, goodNoul), ErrProbabilityOutOfRange},
		{"level probability below zero", answer(goodChoice, `{"type":"score","score":1,"probabilities":[-1],"confidence":0.9}`, goodNoul), ErrProbabilityOutOfRange},

		// Scores outside the rubric's index range [0, len(Levels)-1].
		{"score of seven", answer(goodChoice, `{"type":"score","score":7.0,"confidence":0.9}`, goodNoul), ErrScoreOutOfRange},
		{"score just past the last level", answer(goodChoice, `{"type":"score","score":4.0001,"confidence":0.9}`, goodNoul), ErrScoreOutOfRange},
		{"score of minus one", answer(goodChoice, `{"type":"score","score":-1,"confidence":0.9}`, goodNoul), ErrScoreOutOfRange},

		// A choice naming an option that was never sent.
		{"choice of an unsent option", answer(`{"type":"choice","choice":"approve","confidence":0.99}`, goodScore, goodNoul), ErrUnknownOption},
		{"choice of the empty key", answer(`{"type":"choice","choice":"","confidence":0.99}`, goodScore, goodNoul), ErrUnknownOption},
		{"choice differing only in case", answer(`{"type":"choice","choice":"Deploy","confidence":0.99}`, goodScore, goodNoul), ErrUnknownOption},
		{"probability for an unsent option", answer(`{"type":"choice","choice":"deploy","probabilities":{"approve":0.5},"confidence":0.9}`, goodScore, goodNoul), ErrUnknownOption},

		// An answer to a question that was never asked.
		{"answer to an unasked question", `{"model":"jev-1.13.0","answers":{` +
			`"action":` + goodChoice + `,"risk":` + goodScore + `,"unrequested":` + goodNoul +
			`,"approve_everything":{"type":"noul","noul":1}}}`, ErrUnaskedQuestion},

		// A missing answer to a question that was asked.
		{"missing answer", `{"model":"jev-1.13.0","answers":{"action":` + goodChoice + `,"risk":` + goodScore + `}}`, ErrMissingAnswer},
		{"null answer", answer(goodChoice, goodScore, `null`), ErrMissingAnswer},
		{"no answers object", `{"model":"jev-1.13.0"}`, ErrMissingAnswer},

		// An answer of the wrong primitive for its question.
		{"noul given for a choice", answer(`{"type":"noul","noul":0.9}`, goodScore, goodNoul), ErrTypeMismatch},
		{"choice given for a noul", answer(goodChoice, goodScore, goodChoice), ErrTypeMismatch},
		{"untyped answer", answer(goodChoice, goodScore, `{"noul":0.5}`), ErrTypeMismatch},

		// An answer with its value absent: a zero would be a valid, and
		// wrong, answer delivered as a success.
		{"noul without its value", answer(goodChoice, goodScore, `{"type":"noul"}`), ErrMalformed},
		{"score without its value", answer(goodChoice, `{"type":"score","confidence":0.9}`, goodNoul), ErrMalformed},
		{"choice without its confidence", answer(`{"type":"choice","choice":"deploy"}`, goodScore, goodNoul), ErrMalformed},
		{"score without its confidence", answer(goodChoice, `{"type":"score","score":1}`, goodNoul), ErrMalformed},

		// Structurally malformed bodies.
		{"unknown top-level field", `{"model":"jev-1.13.0","approved":true,"answers":{"action":` + goodChoice + `,"risk":` + goodScore + `,"unrequested":` + goodNoul + `}}`, ErrMalformed},
		{"unknown answer field", answer(goodChoice, goodScore, `{"type":"noul","noul":0.1,"override":true}`), ErrMalformed},
		{"string where a number belongs", answer(goodChoice, goodScore, `{"type":"noul","noul":"0.1"}`), ErrMalformed},
		{"trailing document", answer(goodChoice, goodScore, goodNoul) + `{}`, ErrMalformed},
		{"truncated body", answer(goodChoice, goodScore, goodNoul)[:40], ErrMalformed},
		{"empty body", ``, ErrMalformed},
		{"null body", `null`, ErrMalformed},
		{"array body", `[]`, ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeDecision(strings.NewReader(tc.body), asked)
			if !errors.Is(err, tc.want) {
				t.Fatalf("DecodeDecision error = %v, want %v", err, tc.want)
			}
			if got.Answers != nil {
				t.Errorf("a rejected response still yielded answers: %+v", got.Answers)
			}
		})
	}
}

// FuzzDecodeDecision exercises the decoder against hostile bytes (RFC 021
// §4.3). The goal is not acceptance but containment: no panic, and anything
// accepted satisfies every rule the table above pins.
func FuzzDecodeDecision(f *testing.F) {
	seeds := []string{
		answer(goodChoice, goodScore, goodNoul),
		answer(goodChoice, goodScore, `{"type":"noul","noul":1.7}`),
		answer(goodChoice, `{"type":"score","score":7,"confidence":0.9}`, goodNoul),
		answer(`{"type":"choice","choice":"approve","confidence":0.99}`, goodScore, goodNoul),
		`{"model":"jev","answers":{}}`,
		`{"answers":{"action":null}}`,
		`{"answers":{"x":{"type":"noul","noul":1e308}}}`,
		`{"__proto__":{"polluted":true}}`,
		`{`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		got, err := DecodeDecision(strings.NewReader(input), asked)
		if err != nil {
			return
		}
		if len(got.Answers) != len(asked) {
			t.Fatalf("accepted %d answers for %d questions: %q", len(got.Answers), len(asked), input)
		}
		for id, a := range got.Answers {
			q, ok := asked[id]
			if !ok {
				t.Fatalf("accepted an answer to unasked question %q: %q", id, input)
			}
			if a.Type != q.Type {
				t.Fatalf("accepted a %s answer for %s question %q: %q", a.Type, q.Type, id, input)
			}
			if a.Confidence < 0 || a.Confidence > 1 || a.Noul < 0 || a.Noul > 1 {
				t.Fatalf("accepted a probability outside [0,1] for %q: %+v", id, a)
			}
			if q.Type == Score && (a.Score < 0 || a.Score > float64(len(q.Levels)-1)) {
				t.Fatalf("accepted a score outside the rubric for %q: %+v", id, a)
			}
			if _, sent := q.Options[a.Choice]; q.Type == Choice && !sent {
				t.Fatalf("accepted unsent option %q for %q", a.Choice, id)
			}
		}
	})
}
