// Package audit: usage log on the USB folder (DESIGN §4.3) -- JSONL per day,
// plus an in-memory ring of recent requests for the panel.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Record struct {
	TS          float64 `json:"ts"`
	Model       string  `json:"model"`
	ChannelID   string  `json:"channel_id"`
	Status      int     `json:"status"`
	FirstByteMS float64 `json:"first_byte_ms"`
	PromptToks  int     `json:"prompt_tokens"`
	CompToks    int     `json:"completion_tokens"`
	Error       string  `json:"error"`
}

type Audit struct {
	mu       sync.Mutex
	dir      string
	day      string   // "20060102"
	f        *os.File // today's file
	ring     []Record // last 200, newest first
	ringMu   sync.Mutex
}

func New(dataDir string) *Audit {
	dir := filepath.Join(dataDir, "audit")
	os.MkdirAll(dir, 0o755)
	return &Audit{dir: dir}
}

func today() string { return time.Now().Format("20060102") }

func (a *Audit) rotx() error {
	t := today()
	if a.day == t && a.f != nil {
		return nil
	}
	if a.f != nil {
		a.f.Close()
	}
	f, err := os.OpenFile(filepath.Join(a.dir, "log-"+t+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	a.f, a.day = f, t
	return nil
}

func (a *Audit) Record(r Record) {
	r.TS = float64(time.Now().UnixNano()) / 1e9
	a.mu.Lock()
	if err := a.rotx(); err == nil {
		b, _ := json.Marshal(r)
		a.f.Write(append(b, '\n'))
	}
	a.mu.Unlock()

	a.ringMu.Lock()
	a.ring = append([]Record{r}, a.ring...)
	if len(a.ring) > 200 {
		a.ring = a.ring[:200]
	}
	a.ringMu.Unlock()
}

func (a *Audit) Recent() []Record {
	a.ringMu.Lock()
	defer a.ringMu.Unlock()
	out := make([]Record, len(a.ring))
	copy(out, a.ring)
	return out
}

// Summary aggregates today's JSONL on disk.  Fields mirror the python panel.
func (a *Audit) Summary() map[string]any {
	a.mu.Lock()
	day := a.day
	a.mu.Unlock()
	if day == "" {
		day = today()
	}
	f, err := os.Open(filepath.Join(a.dir, "log-"+day+".jsonl"))
	if err != nil {
		return zeroSummary("")
	}
	defer f.Close()

	var reqs int
	var pin, pout, errs int
	type chAgg struct{ n, tokens int }
	chs := map[string]*chAgg{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		reqs++
		pin += r.PromptToks
		pout += r.CompToks
		if r.Status >= 400 || r.Status == 0 {
			errs++
		}
		agg := chs[r.ChannelID]
		if agg == nil {
			agg = &chAgg{}
			chs[r.ChannelID] = agg
		}
		agg.n++
		agg.tokens += r.PromptToks + r.CompToks
	}

	type row struct {
		Channel string `json:"channel"`
		Req     int    `json:"requests"`
		Tokens  int    `json:"tokens"`
	}
	var rows []row
	keys := make([]string, 0, len(chs))
	for k := range chs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rows = append(rows, row{Channel: k, Req: chs[k].n, Tokens: chs[k].tokens})
	}
	out := zeroSummary(day)
	out["requests_24h"] = reqs
	out["tokens_24h"] = map[string]int{"prompt": pin, "completion": pout}
	out["errors_24h"] = errs
	out["by_channel"] = rows
	return out
}

func zeroSummary(day string) map[string]any {
	return map[string]any{
		"requests_24h": 0,
		"tokens_24h":   map[string]int{"prompt": 0, "completion": 0},
		"errors_24h":   0,
		"by_channel":   []any{},
		"day":          day,
	}
}

func (a *Audit) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
		a.f = nil
	}
}
