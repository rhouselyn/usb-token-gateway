// Package bridge: shared gateway state (DESIGN §4) -- locked/disunlocked
// lifecycle, scheduler + audit wiring, downstream auth.
package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"usbridge/internal/audit"
	"usbridge/internal/remember"
	"usbridge/internal/sched"
	"usbridge/internal/vault"
)

var ErrLocked = errors.New("bridge is locked; open /admin to unlock")

type Bridge struct {
	mu           sync.RWMutex
	dataDir      string
	portOverride int
	vault        *vault.Vault
	sched        *sched.Scheduler
	auditDB      *audit.Audit
	admin        string // admin token, valid only while unlocked
	port         int
	started      float64
}

func (b *Bridge) Port() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.port
}

func (b *Bridge) SetPort(p int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.port = p
}

func (b *Bridge) Started() float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.started
}

func (b *Bridge) DataDir() string { return b.dataDir }

// SavePortMarker writes dataDir/port.txt (bare bind port, not secret).
func (b *Bridge) SavePortMarker(p int) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return os.WriteFile(filepath.Join(b.dataDir, "port.txt"), []byte(strconv.Itoa(p)), 0o644)
}

func (b *Bridge) State() map[string]any {
	return map[string]any{
		"locked":     b.Locked(),
		"has_vault":  vault.Exists(b.dataDir),
		"port":       b.port,
		"started":    b.started,
		"bind_lan":   b.Policy().BindLan,
		"remembered": remember.Exists(b.dataDir),
	}
}

func nowF() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func New(dataDir string, portOverride int) *Bridge {
	// bind-port resolution order: --port > port.txt marker > 8989.  The vault
	// port HIDES inside the encrypted payload and cannot be read while locked
	// (DESIGN §5: everything sensitive is ciphertext); the marker file holds
	// only a bare port number -- not confidential by the threat model.
	p := 0
	if portOverride > 0 {
		p = portOverride
	} else {
		p = peekPortMarker(dataDir)
	}
	if p <= 0 || p > 65535 {
		p = 8989
	}
	return &Bridge{
		dataDir:      dataDir,
		auditDB:      audit.New(dataDir),
		port:         p,
		portOverride: portOverride,
		started:      nowF(),
	}
}

func peekPortMarker(dataDir string) int {
	raw, err := os.ReadFile(filepath.Join(dataDir, "port.txt"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// ------------------------------------------------------------- lifecycle

func (b *Bridge) Locked() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.vault == nil
}

// Unlock an existing or freshly, *create* (first run) the vault.
func (b *Bridge) Unlock(password string) (adminToken string, created bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var v *vault.Vault
	created = !vault.Exists(b.dataDir)
	if created {
		v, _, err = vault.Create(b.dataDir, password, nil)
	} else {
		v, err = vault.Load(b.dataDir, password)
	}
	if err != nil {
		return "", created, err
	}
	b.vault = v
	b.admin = v.Data.AdminToken
	// NOTE: b.port (set at New from --port / port.txt / 8989) is what is
	// actually bound; the vault's stored port is informational only.  Use the
	// panel 「保存端口」 to change it -- it writes the port.txt marker and
	// applies on next launch.
	b.auditDB = audit.New(b.dataDir)
	s := sched.New(v.Data.Channels, v.Data.Policy)
	s.SetCooldownDefault(v.Data.Policy.Circuit.CooldownS)
	b.sched = s
	return b.admin, created, nil
}

func (b *Bridge) Lock() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.vault = nil
	b.sched = nil
	b.admin = ""
	// NOTE (DESIGN §5.3): full memory wipe of derived keys is a Go hardening TODO.
}

// ------------------------------------------------------------- accessors

func (b *Bridge) Vault() *vault.Vault {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.vault
}

func (b *Bridge) Sched() *sched.Scheduler {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sched
}

func (b *Bridge) Audit() *audit.Audit { return b.auditDB }

func (b *Bridge) Policy() vault.Policy {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.vault == nil {
		return vault.DefaultPolicy()
	}
	return b.vault.Data.Policy
}

// ------------------------------------------------------------- misc

// Save persists config and refreshes the scheduler snapshot.
func (b *Bridge) Save() error {
	b.mu.RLock()
	v := b.vault
	b.mu.RUnlock()
	if v == nil {
		return ErrLocked
	}
	if err := v.Save(); err != nil {
		return err
	}
	s := sched.New(v.Data.Channels, v.Data.Policy)
	s.SetCooldownDefault(v.Data.Policy.Circuit.CooldownS)
	b.mu.Lock()
	b.sched = s
	b.mu.Unlock()
	return nil
}

// ------------------------------------------------------------- downstream auth

func (b *Bridge) CheckDownstream(key string) *vault.DownstreamKey {
	v := b.Vault()
	if v == nil {
		return nil
	}
	return v.CheckDownstreamKey(key)
}

// AdminOK checks the admin token; requires unlocked state.
func (b *Bridge) AdminOK(token string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.admin != "" && token == b.admin
}

// AdminToken exposes the current session admin token ("" while locked).
// Callers MUST additionally enforce a same-machine context -- see the
// /admin/token endpoint, which refuses non-loopback peers.
func (b *Bridge) AdminToken() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.admin
}

// ------------------------------------------------------------- misc

// Save persists config and refreshes the scheduler snapshot.
