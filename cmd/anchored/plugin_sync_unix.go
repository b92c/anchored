//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// tryAcquireSyncLock attempts a non-blocking exclusive flock on
// ~/.anchored/plugin_sync.lock. Returns (releaseFn, true) on success and
// (nop, false) when the lock is already held by another anchored process,
// or when the home dir is unwritable. Used by applyPluginAutoUpdate to
// prevent two SessionStart firings from racing on the cache directory.
func tryAcquireSyncLock() (release func(), ok bool) {
	path := syncLockPath()
	if path == "" {
		return func() {}, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}
