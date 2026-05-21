package auth

import (
	"context"
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

const (
	codexRefreshGateConcurrency = 2
	codexRefreshMinInterval     = 3 * time.Second
	codexRefreshJitterWindow    = 2 * time.Minute
)

type codexRefreshGate struct {
	jobs        chan string
	concurrency int
	minInterval time.Duration
	mu          sync.Mutex
	nextSlot    time.Time
	pending     map[string]struct{}
}

func newCodexRefreshGate(concurrency int, minInterval time.Duration) *codexRefreshGate {
	if concurrency <= 0 {
		concurrency = codexRefreshGateConcurrency
	}
	if minInterval <= 0 {
		minInterval = codexRefreshMinInterval
	}
	return &codexRefreshGate{
		jobs:        make(chan string, 1024),
		concurrency: concurrency,
		minInterval: minInterval,
		pending:     make(map[string]struct{}),
	}
}

func (g *codexRefreshGate) has(authID string) bool {
	if g == nil || strings.TrimSpace(authID) == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.pending[authID]
	return ok
}

func (g *codexRefreshGate) submit(ctx context.Context, authID string) bool {
	if g == nil || strings.TrimSpace(authID) == "" {
		return false
	}
	g.mu.Lock()
	if _, exists := g.pending[authID]; exists {
		g.mu.Unlock()
		return false
	}
	g.pending[authID] = struct{}{}
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.forget(authID)
		return false
	case g.jobs <- authID:
		return true
	}
}

func (g *codexRefreshGate) forget(authID string) {
	if g == nil || strings.TrimSpace(authID) == "" {
		return
	}
	g.mu.Lock()
	delete(g.pending, authID)
	g.mu.Unlock()
}

func (g *codexRefreshGate) run(ctx context.Context, refresh func(context.Context, string), reschedule func(string)) {
	if g == nil || refresh == nil {
		return
	}
	workers := g.concurrency
	if workers <= 0 {
		workers = codexRefreshGateConcurrency
	}
	for i := 0; i < workers; i++ {
		go g.worker(ctx, refresh, reschedule)
	}
}

func (g *codexRefreshGate) worker(ctx context.Context, refresh func(context.Context, string), reschedule func(string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case authID := <-g.jobs:
			if strings.TrimSpace(authID) == "" {
				continue
			}
			if !g.waitForSlot(ctx) {
				g.forget(authID)
				return
			}
			refresh(ctx, authID)
			g.forget(authID)
			if reschedule != nil {
				reschedule(authID)
			}
		}
	}
}

func (g *codexRefreshGate) waitForSlot(ctx context.Context) bool {
	if g == nil {
		return true
	}
	now := time.Now()
	g.mu.Lock()
	startAt := now
	if g.nextSlot.After(startAt) {
		startAt = g.nextSlot
	}
	g.nextSlot = startAt.Add(g.minInterval)
	g.mu.Unlock()

	if wait := time.Until(startAt); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return true
}

func codexRefreshJitter(authID string) time.Duration {
	if codexRefreshJitterWindow <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.TrimSpace(authID)))
	return time.Duration(h.Sum64() % uint64(codexRefreshJitterWindow))
}
