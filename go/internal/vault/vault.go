// Package vault: encrypted config for USB bridge (DESIGN §5).
// Reads BOTH python-prototype backends (argon2/aesgcm, scrypt/hmac) and
// WRITES the argon2 backend.  Binary format identical to the prototype:
//
//	magic 4B "BVLT" | version 1B | kdf 1B | salt 16B | body...
//	body(argon2): nonce12 + AES-GCM ct (aad="BVLT")
//	body(scrypt): nonce16 + ct + hmac32   (fallback used by python on cryptography <43)
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/scrypt"
)

const (
	magic     = "BVLT"
	version   = 1
	kdfArgon2 = 0x01
	kdfScrypt = 0x02
)

// ------------------------------------------------------------- data model

type Channel struct {
	ID             string   `json:"id"`
	Protocol       string   `json:"protocol"`
	BaseURL        string   `json:"base_url"`
	APIKey         string   `json:"api_key"`
	Models         []string `json:"models"`
	Weight         int      `json:"weight,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	Enabled        bool     `json:"enabled"` // python seed import may omit -> default true (see UnmarshalJSON)
	AuthHeader     string   `json:"auth_header,omitempty"` // bearer | x-api-key
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
}

// UnmarshalJSON applies python-side defaults for missing optional keys
// (prototype's add_channel defaults enabled=true; seed import may omit it).
func (c *Channel) UnmarshalJSON(b []byte) error {
	type alias Channel
	aux := struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		c.Enabled = true
	} else {
		c.Enabled = *aux.Enabled
	}
	return nil
}

type DownstreamKey struct {
	Key          string  `json:"key"`
	Name         string  `json:"name"`
	Enabled      bool    `json:"enabled"` // default true when omitted
	RateLimitRPM int     `json:"rate_limit_rpm,omitempty"`
	TTLHours     float64 `json:"ttl_hours,omitempty"` // 0 = never
	Created      float64 `json:"created"`
}

// UnmarshalJSON defaults Enabled=true when the key is absent (seed import).
func (k *DownstreamKey) UnmarshalJSON(b []byte) error {
	type alias DownstreamKey
	aux := struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{alias: (*alias)(k)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if aux.Enabled == nil {
		k.Enabled = true
	} else {
		k.Enabled = *aux.Enabled
	}
	return nil
}

type CircuitPolicy struct {
	ErrRate    float64 `json:"err_rate_threshold"`
	MinSamples int     `json:"min_samples"`
	CooldownS  float64 `json:"cooldown_s"`
}

type Policy struct {
	Strategy          string        `json:"strategy"`
	RetryBudget       int           `json:"retry_budget"`
	TimeoutFirstByteS float64       `json:"timeout_first_byte_s"`
	TimeoutStreamIdle float64       `json:"timeout_stream_idle_s"`
	Circuit           CircuitPolicy `json:"circuit"`
	BindLan           bool          `json:"bind_lan"` // 0.0.0.0 for LAN/phone access (restart to apply)
}

type Payload struct {
	Port           int              `json:"port"`
	Policy         Policy           `json:"policy"`
	Channels       []*Channel       `json:"channels"`
	DownstreamKeys []*DownstreamKey `json:"downstream_keys"`
	AdminToken     string           `json:"admin_token"`
}

func DefaultPolicy() Policy {
	return Policy{
		Strategy:          "failover",
		RetryBudget:       2,
		TimeoutFirstByteS: 20,
		TimeoutStreamIdle: 120,
		Circuit:           CircuitPolicy{0.5, 10, 300},
	}
}

// ------------------------------------------------------------- crypto

func randKey(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	const cs = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	for i, v := range b {
		out[i] = cs[int(v)%len(cs)]
	}
	return string(out)
}

func NewDownstreamKey() string { return "sk-usb-" + randKey(32) }
func NewAdminToken() string    { return "brg-" + randKey(20) }

func deriveArgon2(password, salt []byte) []byte {
	return argon2.IDKey(password, salt, 3, 64*1024, 1, 32) // t=3, m=64MiB, p=1
}

// keystreamXor mirrors the python hashlib sha256-CTR fallback.
func keystreamXor(key, nonce, data []byte) []byte {
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += 32 {
		end := i + 32
		if end > len(data) {
			end = len(data)
		}
		var blk [8]byte
		ctr := uint64(i / 32)
		blk[0] = byte(ctr)
		blk[1] = byte(ctr >> 8)
		blk[2] = byte(ctr >> 16)
		blk[3] = byte(ctr >> 24)
		blk[4] = byte(ctr >> 32)
		blk[5] = byte(ctr >> 40)
		blk[6] = byte(ctr >> 48)
		blk[7] = byte(ctr >> 56)
		h := sha256.New()
		h.Write(key)
		h.Write(nonce)
		h.Write(blk[:])
		ks := h.Sum(nil)
		for j := i; j < end; j++ {
			out[j] = data[j] ^ ks[j-i]
		}
	}
	return out
}

func encryptArgon2(encKey, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	ct := gcm.Seal(nil, nonce, plaintext, []byte(magic))
	return append(nonce, ct...), nil
}

func decryptArgon2(encKey, body []byte) ([]byte, error) {
	if len(body) < 12 {
		return nil, errors.New("argon2 body too short")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, body[:12], body[12:], []byte(magic))
}

var ErrBadPassword = errors.New("wrong master password / corrupted vault")

func decryptBlob(password, blob []byte) ([]byte, error) {
	if len(blob) < 4+1+1+16 || string(blob[:4]) != magic {
		return nil, errors.New("not a bridge vault file")
	}
	ver, kdf := blob[4], blob[5]
	if ver != version {
		return nil, fmt.Errorf("unsupported vault version %d", ver)
	}
	salt := blob[6:22]
	body := blob[22:]
	switch kdf {
	case kdfArgon2:
		enc := deriveArgon2(password, salt)
		return decryptArgon2(enc, body)
	case kdfScrypt:
		df, err := scrypt.Key(password, salt, 1<<15, 8, 1, 64)
		if err != nil {
			return nil, err
		}
		enc, macKey := df[:32], df[32:64]
		if len(body) < 16+32 {
			return nil, ErrBadPassword
		}
		nonce, ct, mac := body[:16], body[16:len(body)-32], body[len(body)-32:]
		h := hmac.New(sha256.New, macKey)
		h.Write(ct)
		if subtle.ConstantTimeCompare(h.Sum(nil), mac) != 1 {
			return nil, ErrBadPassword
		}
		return keystreamXor(enc, nonce, ct), nil
	default:
		return nil, fmt.Errorf("unknown kdf %d", kdf)
	}
}

// ------------------------------------------------------------- vault file

type Vault struct {
	Path string
	Data *Payload
	pw   []byte // master password, held while running (Go hardening TODO: wipe)
}

func Path(dataDir string) string { return filepath.Join(dataDir, "config.vault") }
func Exists(dataDir string) bool { _, err := os.Stat(Path(dataDir)); return err == nil }

func DefaultPayload() *Payload {
	return &Payload{Port: 8989, Policy: DefaultPolicy(), AdminToken: NewAdminToken()}
}

// Load reads and decrypts; supports both backends.
func Load(dataDir, password string) (*Vault, error) {
	blob, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return nil, err
	}
	raw, err := decryptBlob([]byte(password), blob)
	if err != nil {
		if errors.Is(err, ErrBadPassword) {
			return nil, ErrBadPassword
		}
		return nil, ErrBadPassword // decrypt/parse failure == wrong password
	}
	p := &Payload{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, ErrBadPassword
	}
	if p.Policy.TimeoutFirstByteS == 0 {
		p.Policy = DefaultPolicy()
	}
	return &Vault{Path: Path(dataDir), Data: p, pw: []byte(password)}, nil
}

// Create builds a brand new vault with one primary downstream key.
func Create(dataDir, password string, seed *Payload) (*Vault, string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, "", err
	}
	var p *Payload
	if seed != nil {
		p = seed
	} else {
		p = DefaultPayload()
	}
	p.Policy = DefaultPolicy()
	dk := &DownstreamKey{
		Key: NewDownstreamKey(), Name: "primary", Enabled: true,
		Created: nowF(),
	}
	p.DownstreamKeys = append([]*DownstreamKey{dk}, p.DownstreamKeys...)
	if p.AdminToken == "" {
		p.AdminToken = NewAdminToken()
	}
	if p.Port == 0 {
		p.Port = 8989
	}
	v := &Vault{Path: Path(dataDir), Data: p, pw: []byte(password)}
	if err := v.Save(); err != nil {
		return nil, "", err
	}
	return v, dk.Key, nil
}

func (v *Vault) Save() error {
	raw, err := json.Marshal(v.Data)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	enc := deriveArgon2(v.pw, salt)
	body, err := encryptArgon2(enc, raw)
	if err != nil {
		return err
	}
	head := append([]byte(magic), byte(version), kdfArgon2)
	out := append(append(head, salt...), body...)
	// atomic write
	tmp := v.Path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.Path)
}

// ------------------------------------------------------------- helpers

func nowF() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ChangePassword re-encrypts the vault under a new master password.
func (v *Vault) ChangePassword(newPw string) error {
	old := v.pw
	v.pw = []byte(newPw)
	if err := v.Save(); err != nil {
		v.pw = old // rollback on failure
		return err
	}
	return nil
}

func (v *Vault) AddChannel(ch *Channel) *Channel {
	if ch.ID == "" {
		ch.ID = "ch-" + randKey(4)
	}
	if ch.Models == nil {
		ch.Models = []string{"*"}
	}
	ch.Enabled = true
	switch ch.Protocol {
	case "openai":
		ch.AuthHeader = "bearer"
	case "anthropic":
		ch.AuthHeader = "x-api-key"
	}
	v.Data.Channels = append(v.Data.Channels, ch)
	v.Save()
	return ch
}

func (v *Vault) AddDownstreamKey(name string, ttlHours float64) *DownstreamKey {
	k := &DownstreamKey{
		Key: NewDownstreamKey(), Name: name, Enabled: true,
		TTLHours: ttlHours, Created: nowF(),
	}
	v.Data.DownstreamKeys = append(v.Data.DownstreamKeys, k)
	v.Save()
	return k
}

func (v *Vault) CheckDownstreamKey(key string) *DownstreamKey {
	for _, dk := range v.Data.DownstreamKeys {
		if dk.Key != key || !dk.Enabled {
			continue
		}
		if dk.TTLHours > 0 && time.Now().Unix() > int64(dk.Created+dk.TTLHours*3600) {
			return nil // expired
		}
		return dk
	}
	return nil
}
