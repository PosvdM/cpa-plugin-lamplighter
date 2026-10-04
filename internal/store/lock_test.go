package store

import (
	"path/filepath"
	"testing"
)

func TestTryLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "instance.lock")
	first, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := TryLock(path); err != nil || ok {
		t.Fatalf("second lock while held: ok=%v err=%v", ok, err)
	}
	first.Release()
	again, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v", ok, err)
	}
	again.Release()
}
