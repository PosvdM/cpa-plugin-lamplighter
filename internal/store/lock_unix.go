//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an exclusive lock on a file in the data directory. It keeps a newly
// loaded plugin version from running next to an older one that CPA has not
// stopped yet.
type Lock struct {
	file *os.File
}

// TryLock takes the lock without waiting. It returns ok=false when another
// instance holds it.
func TryLock(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{file: file}, true, nil
}

// Release frees the lock.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	l.file.Close()
	l.file = nil
}
