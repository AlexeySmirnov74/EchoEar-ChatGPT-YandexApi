package ai

import (
	"errors"
	"math"
	"sync"
)

// persistentControl is connection-local. listen_id distinguishes replies and
// standby timers from a previous press when messages cross on the two peers.
// It does not itself read audio, send network data, or manage an API session.
type persistentControl struct {
	mu       sync.Mutex
	enabled  bool
	active   bool
	listenID uint32
}

func (p *persistentControl) negotiate(hello map[string]any) bool {
	features, _ := hello["features"].(map[string]any)
	enabled, _ := features["persistent_idle"].(bool)
	p.mu.Lock()
	p.enabled = enabled
	p.active = false
	p.listenID = 0
	p.mu.Unlock()
	return enabled
}

func listenIDFrom(msg map[string]any) (uint32, error) {
	value, ok := msg["listen_id"].(float64)
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 1 || value > math.MaxUint32 || value != math.Trunc(value) {
		return 0, errors.New("invalid listen_id")
	}
	return uint32(value), nil
}

func (p *persistentControl) start(id uint32) error {
	if id == 0 {
		return errors.New("invalid listen_id")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return errors.New("persistent idle not negotiated")
	}
	// Every start, including an automatic rearm, requires a fresh ID.
	// Serial-number comparison
	// permits uint32 wrap while rejecting a delayed older generation.
	if id == p.listenID {
		return errors.New("listen_id already used")
	}
	if p.listenID != 0 && id != p.listenID && int32(id-p.listenID) <= 0 {
		return errors.New("stale listen_id")
	}
	p.listenID, p.active = id, true
	return nil
}

// end is a compare-and-swap: an old idle timer or pause never cancels a new
// press even when it fires after the new session has started.
func (p *persistentControl) end(id uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || !p.active || id != p.listenID {
		return false
	}
	p.active = false
	return true
}

func (p *persistentControl) current() (id uint32, active bool, enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listenID, p.active, p.enabled
}

func (p *persistentControl) permitsOutput(id uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled && p.active && id == p.listenID
}
