package spec

import (
	"strings"
	"testing"
)

// FuzzParse esercita Parse contro input JSON malformati o ostili
// (RFC 001 §3 "Fuzzing"). L'obiettivo non è che Parse accetti l'input, ma
// che non vada mai in panic e restituisca sempre un errore esplicito per
// input non validi.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{"sdd_version": "1.0", "intent": "deploy", "resources": [{"id": "a", "type": "relational_database", "provider": "agnostic", "properties": {}}]}`,
		`{}`,
		`null`,
		`[]`,
		`{"sdd_version": "1.0"`,
		`{"resources": {"id": "a"}}`,
		`{"sdd_version": 1.0, "intent": "deploy", "resources": []}`,
		``,
		`{"__proto__": {"polluted": true}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", input, r)
			}
		}()
		_, _ = Parse(strings.NewReader(input))
	})
}
