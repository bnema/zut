//go:build !unix

package daemon

import "os"

// fileOwner is unavailable on this platform; ownership checks are skipped.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
