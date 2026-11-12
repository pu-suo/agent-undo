//go:build !unix

package receipt

import (
	"os"
	"path/filepath"
)

// lockDir only creates the lock file: file locking is implemented for Unix
// systems, where the recorder is meant to run.
func lockDir(dir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
}
