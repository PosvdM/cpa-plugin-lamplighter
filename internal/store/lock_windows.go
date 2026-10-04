package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const errorSharingViolation syscall.Errno = 32

// Lock is an exclusive lock on a file in the data directory. It keeps a newly
// loaded plugin version from running next to an older one that CPA has not
// stopped yet. The file is opened without sharing, so other opens fail until
// the holder closes it or its process exits.
type Lock struct {
	handle syscall.Handle
}

// TryLock takes the lock without waiting. It returns ok=false when another
// instance holds it.
func TryLock(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{handle: handle}, true, nil
}

// Release frees the lock.
func (l *Lock) Release() {
	if l == nil || l.handle == syscall.InvalidHandle {
		return
	}
	syscall.CloseHandle(l.handle)
	l.handle = syscall.InvalidHandle
}
