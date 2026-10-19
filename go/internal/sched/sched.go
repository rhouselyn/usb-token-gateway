// Package sched: channel scheduling (DESIGN 搂7) -- strategies, health window,
// circuit breaker, 429-aware cooldown.
package sched

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"usbridge/internal/vault"
)

const window = 300.0 // seconds; 5-minute sliding window

type sample struct {
	ts      float64
	ok      bool
	firstMS float64
}

type Health struct {
	mu           sync.Mutex
	window       []sample
	minSamples   int
	errRate      float64
	cooldownUntil time.Time
	halfOpen     bool
	retryAfter   time.Time // 429 earliest-retry hint
}

func newHealth(minSamples int, errRate float64) *Health {
	return &Health{minSamples: minSamples, errRate: errRate}
}

func (h *Health) record(ok bool, firstMS float64) {
	now := nowF()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.window = append(h.window, sample{now, ok, firstMS})
	for len(h.window) > 50 {
		h.window = h.window[1:]
	}
	if ok {
		h.cooldownUntil = time.Time{}
		h.halfOpen = false
		return
	}
	n := 0
	bad := 0
	for _, s := range h.window {
		if now-s.ts > window {
			continue
		}
		n++
		if !s.ok {
			bad++
		}
	}
	if n >= h.minSamples && float64(bad)/float64(n) >= h.errRate && !h.halfOpen {
		h.cooldownUntil = time.Now().Add(cooldownDur)
		h.halfOpen = true
	}
}

func (h *Health) record429(retryAfterS float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	d := time.Duration(retryAfterS * float64(time.Second))
	if d <= 0 {
		d = 30 * time.Second
	}
	h.retryAfter = now.Add(d)
	if now.After(h.cooldownUntil) || d > h.cooldownUntil.Sub(now) {
		h.cooldownUntil = h.retryAfter
	}
}

func (h *Health) isOpen() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cooldownUntil.After(time.Now()) {
		return true
	}
	h.cooldownUntil = time.Time{}
	return false
}

func (h *Health) errorRate() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := nowF()
	n, bad := 0, 0
	for _, s := range h.window {
		if now-s.ts > window {
			continue
		}
		n++
		if !s.ok {
			bad++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(bad) / float64(n)
}

var cooldownDur = 5 * time.Minute

func nowF() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// ------------------------------------------------------------------ scheduler

type Scheduler struct {
	mu          sync.Mutex
	channels    []*vault.Channel
	health      map[string]*Health
	strategy    string
	retryBudget int
	rr          int64
}

func New(channels []*vault.Channel, p vault.Policy) *Scheduler {
	s := &Scheduler{
		channels:    channels,
		health:      map[string]*Health{},
		strategy:    p.Strategy,
		retryBudget: p.RetryBudget,
	}
	for _, ch := range channels {
		s.health[ch.ID] = newHealth(p.Circuit.MinSamples, p.Circuit.ErrRate)
	}
	return s
}

func (s *Scheduler) SetCooldownDefault(d float64) {
	if d > 0 {
		cooldownDur = time.Duration(d * float64(time.Second))
	}
}

func matching(patterns []string, model string) bool {
	for _, pat := range patterns {
		if pat == model || (len(pat) > 0 && pat[len(pat)-1] == '*' && len(model) >= len(pat)-1 && pat[:len(pat)-1] == model[:len(pat)-1]) {
			return true
		}
		if len(pat) > 1 && pat[0] == '*' && len(model) >= len(pat)-1 && pat[1:] == model[len(model)-(len(pat)-1):] {
			return true
		}
	}
	return false
}

// Candidates returns the ordered healthy candidate list for a model.
func (s *Scheduler) Candidates(model string) []*vault.Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fits []*vault.Channel
	for _, ch := range s.channels {
		if !ch.Enabled || (s.health[ch.ID] != nil && s.health[ch.ID].isOpen()) {
			continue
		}
		if matching(ch.Models, model) {
			fits = append(fits, ch)
		}
	}
	switch s.strategy {
	case "round_robin":
		n := int(atomic.AddInt64(&s.rr, 1))
		k := n % len(fits)
		return append(append([]*vault.Channel{}, fits[k:]...), fits[:k]...)
	case "weighted":
		return sortWeighted(fits)
	case "least_errors":
		out := append([]*vault.Channel{}, fits...)
		for i := 0; i < len(out); i++ {
			for j := i + 1; j < len(out); j++ {
				a, b := s.health[out[i].ID], s.health[out[j].ID]
				if a.errorRate() > b.errorRate() ||
					(a.errorRate() == b.errorRate() && out[i].Priority > out[j].Priority) {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	default: // failover: priority ASC
		out := append([]*vault.Channel{}, fits...)
		for i := 0; i < len(out); i++ {
			for j := i + 1; j < len(out); j++ {
				if out[i].Priority > out[j].Priority {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}
}

func sortWeighted(chs []*vault.Channel) []*vault.Channel {
	out := append([]*vault.Channel{}, chs...)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i].Weight < out[j].Weight {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (s *Scheduler) Get(id string) *vault.Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.channels {
		if ch.ID == id {
			return ch
		}
	}
	return nil
}

func (s *Scheduler) All() []*vault.Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]*vault.Channel{}, s.channels...)
	return out
}

// Order sorts channels by the CURRENT strategy (failover: priority ASC,
// weighted: weight DESC, least_errors: lowest error rate first).
func (s *Scheduler) Order(chs []*vault.Channel) []*vault.Channel {
	s.mu.Lock()
	strat := s.strategy
	health := map[string]*Health{}
	for id, h := range s.health {
		health[id] = h
	}
	s.mu.Unlock()
	out := append([]*vault.Channel{}, chs...)
	switch strat {
	case "least_errors":
		sort.SliceStable(out, func(i, j int) bool {
			a, b := health[out[i].ID], health[out[j].ID]
			ra, rb := 0.0, 0.0
			if a != nil {
				ra = a.errorRate()
			}
			if b != nil {
				rb = b.errorRate()
			}
			if ra != rb {
				return ra < rb
			}
			return out[i].Priority < out[j].Priority
		})
	case "weighted":
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Weight != out[j].Weight {
				return out[i].Weight > out[j].Weight
			}
			return out[i].Priority < out[j].Priority
		})
	default: // failover & round_robin base order: priority ASC
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].Priority < out[j].Priority
		})
	}
	return out
}

func (s *Scheduler) Enabled(id string) bool {
	ch := s.Get(id)
	return ch != nil && ch.Enabled
}

func (s *Scheduler) IsOpen(id string) bool {
	s.mu.Lock()
	h, ok := s.health[id]
	s.mu.Unlock()
	return ok && h.isOpen()
}

func (s *Scheduler) Refresh(channels []*vault.Channel, p vault.Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels = channels
	s.strategy = p.Strategy
	s.retryBudget = p.RetryBudget
	for _, ch := range channels {
		if _, ok := s.health[ch.ID]; !ok {
			s.health[ch.ID] = newHealth(p.Circuit.MinSamples, p.Circuit.ErrRate)
		}
	}
}

func (s *Scheduler) Budget() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retryBudget
}

// Report records an attempt outcome.
func (s *Scheduler) Report(id string, ok bool, firstMS float64, status int, retryAfterS float64) {
	s.mu.Lock()
	h := s.health[id]
	s.mu.Unlock()
	if h == nil {
		return
	}
	if status == 429 {
		h.record429(retryAfterS)
		return
	}
	h.record(ok, firstMS)
}
