//go:build !unix

package store

// Lock is a no-op outside Unix. The plugin is only built for Linux and macOS;
// this file lets the pure Go packages compile and run their tests on Windows.
type Lock struct{}

// TryLock always succeeds.
func TryLock(path string) (*Lock, bool, error) {
	return &Lock{}, true, nil
}

// Release does nothing.
func (l *Lock) Release() {}
