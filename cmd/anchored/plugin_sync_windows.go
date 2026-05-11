//go:build windows

package main

// tryAcquireSyncLock is a permissive noop on Windows: syscall.Flock is not
// available, and properly wiring LockFileEx via golang.org/x/sys/windows would
// be heavier than the protection is worth for a desktop CLI tool. The
// concurrency window we're guarding (two Claude Code instances doing
// SessionStart at the same exact moment AND landing on a stale plugin cache)
// is rare and the worst-case failure is one redundant `git pull` + an idempotent
// RemoveAll — no data loss. If a Windows user runs into trouble we can promote
// this to a real LockFileEx implementation.
func tryAcquireSyncLock() (release func(), ok bool) {
	return func() {}, true
}
