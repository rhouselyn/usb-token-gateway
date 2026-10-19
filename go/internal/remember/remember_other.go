//go:build !windows

// Package remember: no-op on non-Windows until an OS-native secure store
// (Keychain / libsecret) is wired up.
package remember

func Fingerprint(string) string          { return "" }
func Exists(string) bool                 { return false }
func Clear(string)                       {}
func Save(string, string, string) error  { return nil }
func Load(string, string) (string, bool) { return "", false }
