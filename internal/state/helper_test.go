// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package state

import "time"

// timeNowMinus returns a timestamp d in the past, used to age a lockfile
// past the stale window.
func timeNowMinus(d time.Duration) time.Time {
	return time.Now().Add(-d)
}
