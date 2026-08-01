// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import "os"

// writeFile creates an empty regular file, used to block a path that a
// test then asks the provider to create a directory under.
func writeFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}
