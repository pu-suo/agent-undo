//go:build unix

package receipt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockDir takes an exclusive lock on dir/.lock so that two recorders on this
// machine can't write the same receipts. The lock is released when the file
// is closed or the process exits.
func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("receipts directory %s is in use by another recorder", dir)
		}
		return nil, fmt.Errorf("lock receipts directory: %w", err)
	}
	return f, nil
}
