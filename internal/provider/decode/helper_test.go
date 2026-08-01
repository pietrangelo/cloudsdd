// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package decode

import "encoding/json"

// unmarshalLenient parses raw into out, used by the fuzz target to turn a
// fuzzed string into the map[string]any shape Properties consumes.
func unmarshalLenient(raw string, out *map[string]any) error {
	return json.Unmarshal([]byte(raw), out)
}
