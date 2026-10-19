// Package probe: channel health probe (start-probe, rev-A spec + admin test).
package probe

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"usbridge/internal/vault"
	"usbridge/internal/xfmt"
)

type Result struct {
	ID      string  `json:"id"`
	OK      bool    `json:"ok"`
	Status  int     `json:"status"`
	FirstMS float64 `json:"first_byte_ms"`
	Err     string  `json:"error"`
}

var (
	mu        sync.Mutex
	lastN     int
	lastAlive int
)

// Channel sends a tiny max_tokens=16 request through the channel.
func Channel(ch *vault.Channel, timeoutFirst float64) Result {
	t0 := time.Now()
	model := "*"
	if len(ch.Models) > 0 {
		model = ch.Models[0]
	}
	res := func() Result {
		body := map[string]any{
			"model":      model,
			"max_tokens": 16,
			"messages":   []map[string]any{{"role": "user", "content": "bridge connectivity test"}},
		}
		reqOut, err := xfmt.Build(ch, body)
		if err != nil {
			return Result{ID: ch.ID, Err: err.Error()}
		}
		hreq, err := http.NewRequest("POST", reqOut.URL, strings.NewReader(string(reqOut.Body)))
		if err != nil {
			return Result{ID: ch.ID, Err: err.Error()}
		}
		for k, v := range reqOut.Headers {
			hreq.Header.Set(k, v)
		}
		client := &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: time.Duration(timeoutFirst * float64(time.Second)),
		}}
		resp, err := client.Do(hreq)
		if err != nil {
			return Result{ID: ch.ID, Err: err.Error()}
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Result{
			ID:      ch.ID,
			OK:      resp.StatusCode < 400,
			Status:  resp.StatusCode,
			FirstMS: float64(time.Since(t0).Microseconds()) / 1000.0,
		}
	}()
	res.ID = ch.ID
	return res
}

// SetAggregate stores the summary of the last full probe round.
func SetAggregate(n, alive int) {
	mu.Lock()
	lastN, lastAlive = n, alive
	mu.Unlock()
}

// LastResult returns (total, alive) of the last probe round.
func LastResult() (int, int) {
	mu.Lock()
	defer mu.Unlock()
	return lastN, lastAlive
}
