package nodes

import (
	"sync"
	"sync/atomic"
)

type stickyNodeSnapshot struct {
	pool map[string]struct{}
}

type StickyNodePool struct { //nolint:govet
	mu   sync.RWMutex
	pool map[string]bool
	view atomic.Pointer[stickyNodeSnapshot]
}

var globalStickyPool = NewStickyNodePool() //nolint:gochecknoglobals

func GetStickyPool() *StickyNodePool {
	return globalStickyPool
}

func NewStickyNodePool() *StickyNodePool {
	pool := &StickyNodePool{ //nolint:exhaustruct
		pool: make(map[string]bool),
	}
	pool.view.Store(&stickyNodeSnapshot{})
	return pool
}

func (p *StickyNodePool) Add(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pool[uri] {
		return
	}
	p.pool[uri] = true
	p.publishSnapshotLocked()
}

func (p *StickyNodePool) Evict(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pool[uri] {
		return
	}
	delete(p.pool, uri)
	p.publishSnapshotLocked()
}

func (p *StickyNodePool) IsSticky(uri string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, exists := p.pool[uri]
	return exists
}

func (p *StickyNodePool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pool)
}

func (p *StickyNodePool) StaleCount() int {
	return 0
}

func (p *StickyNodePool) List() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uris := make([]string, 0, len(p.pool))
	for uri := range p.pool {
		uris = append(uris, uri)
	}
	return uris
}

func (p *StickyNodePool) snapshot() map[string]struct{} {
	snapshot := p.view.Load()
	if snapshot == nil {
		return nil
	}
	return snapshot.pool
}

func (p *StickyNodePool) publishSnapshotLocked() {
	if len(p.pool) == 0 {
		p.view.Store(&stickyNodeSnapshot{})
		return
	}
	out := make(map[string]struct{}, len(p.pool))
	for uri := range p.pool {
		out[uri] = struct{}{}
	}
	p.view.Store(&stickyNodeSnapshot{pool: out})
}
