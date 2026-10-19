//go:build windows

// Package remember: per-device "stay signed in" (DESIGN threat-model note).
// The master password is encrypted with DPAPI (scope = current Windows user on
// this machine) and stored in <data_dir>/session.key.  Facts:
//   * taking the USB elsewhere / another login cannot decrypt the blob
//   * config.vault itself stays password-encrypted at rest (DESIGN §5)
package remember

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	crypt32 = syscall.NewLazyDLL("crypt32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

const dpapiUIForbidden = 0x1

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func protect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	in := dataBlob{uint32(len(data)), unsafe.SliceData(data)}
	var out dataBlob
	r, _, _ := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, uintptr(dpapiUIForbidden),
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("CryptProtectData failed")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	buf := make([]byte, out.cbData)
	copy(buf, unsafe.Slice(out.pbData, out.cbData))
	return buf, nil
}

func unprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("empty blob")
	}
	in := dataBlob{uint32(len(blob)), unsafe.SliceData(blob)}
	var out dataBlob
	r, _, _ := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(dpapiUIForbidden|0x2), // CRYPTPROTECT_UI_FORBIDDEN | verify-protected-to-self flag off (0x2 unused, keep hospitable)
		uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed (different user/machine?)")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	buf := make([]byte, out.cbData)
	copy(buf, unsafe.Slice(out.pbData, out.cbData))
	return buf, nil
}

func sessionPath(dataDir string) string { return filepath.Join(dataDir, "session.key") }

// NOTE: no cryptographic fingerprint is stored on purpose.  vault.Save()
// re-rolls the KDF salt on every write, so file-header hashes go stale.
// Validating the remembered password by ATTEMPTING unlock is the fixture:
// wrong vault / changed password -> Unlock fails -> caller clears the file.
// Fingerprint kept only for API compat; see NOTE above.
func Fingerprint(vaultPath string) string { return "" }

func Exists(dataDir string) bool {
	_, err := os.Stat(sessionPath(dataDir))
	return err == nil
}

func Clear(dataDir string) { os.Remove(sessionPath(dataDir)) }

func Save(dataDir, vaultPath, password string) error {
	if password == "" {
		return fmt.Errorf("empty password")
	}
	enc, err := protect([]byte(password))
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]string{
		"pw": base64.StdEncoding.EncodeToString(enc),
	})
	return os.WriteFile(sessionPath(dataDir), raw, 0o600)
}

// Load returns the remembered master password, if this device can still
// decrypt the blob (DPAPI keys its scope to this Windows user).
func Load(dataDir, vaultPath string) (string, bool) {
	raw, err := os.ReadFile(sessionPath(dataDir))
	if err != nil {
		return "", false
	}
	var s struct {
		Pw string `json:"pw"`
	}
	if json.Unmarshal(raw, &s) != nil || s.Pw == "" {
		Clear(dataDir)
		return "", false
	}
	blob, err := base64.StdEncoding.DecodeString(s.Pw)
	if err != nil {
		Clear(dataDir)
		return "", false
	}
	pw, err := unprotect(blob)
	if err != nil {
		Clear(dataDir) // this device cannot decrypt it anymore
		return "", false
	}
	if len(pw) == 0 {
		Clear(dataDir)
		return "", false
	}
	return string(pw), true
}
