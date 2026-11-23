package ai

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestPersistentCapabilityIsOptIn(t *testing.T) {
	var p persistentControl
	if p.negotiate(map[string]any{"type": "hello"}) {
		t.Fatal("legacy device opted in")
	}
	if p.start(1) == nil {
		t.Fatal("legacy device started persistent session")
	}
	if !p.negotiate(map[string]any{"features": map[string]any{"persistent_idle": true}}) {
		t.Fatal("feature not negotiated")
	}
}

func TestPersistentStartPauseAndStaleStandby(t *testing.T) {
	var p persistentControl
	p.negotiate(map[string]any{"features": map[string]any{"persistent_idle": true}})
	var msg map[string]any
	if err := json.Unmarshal([]byte(`{"type":"listen","state":"start","listen_id":123}`), &msg); err != nil {
		t.Fatal(err)
	}
	id, err := listenIDFrom(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.start(id); err != nil {
		t.Fatal(err)
	}
	if !p.permitsOutput(id) || !p.end(id) {
		t.Fatal("pause did not end active generation")
	}
	if p.permitsOutput(id) || p.start(id) == nil {
		t.Fatal("paused output or old ID was accepted")
	}
	if err := p.start(124); err != nil {
		t.Fatal(err)
	}
	if p.end(id) || p.permitsOutput(id) {
		t.Fatal("stale standby or audio affected new session")
	}
	if p.start(id) == nil {
		t.Fatal("stale start replaced new session")
	}
	if !p.permitsOutput(124) {
		t.Fatal("new session was interrupted")
	}
}

func TestPersistentIDWrap(t *testing.T) {
	var p persistentControl
	p.negotiate(map[string]any{"features": map[string]any{"persistent_idle": true}})
	if err := p.start(^uint32(0)); err != nil {
		t.Fatal(err)
	}
	if err := p.start(1); err != nil {
		t.Fatal("valid wrap rejected", err)
	}
	if err := p.start(^uint32(0)); err == nil {
		t.Fatal("pre-wrap ID accepted after wrap")
	}
}

func TestPersistentRejectsMalformedIDs(t *testing.T) {
	for _, value := range []any{nil, "1", float64(-1), float64(0), 1.2, float64(4294967296)} {
		if _, err := listenIDFrom(map[string]any{"listen_id": value}); err == nil {
			t.Fatalf("accepted %v", value)
		}
	}
}

func TestPersistentConcurrentStaleTimerDoesNotEndNewPress(t *testing.T) {
	for i := 0; i < 1000; i++ {
		var p persistentControl
		p.negotiate(map[string]any{"features": map[string]any{"persistent_idle": true}})
		if err := p.start(1); err != nil {
			t.Fatal(err)
		}
		startNext := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); <-startNext; p.end(1) }()
		if err := p.start(2); err != nil {
			t.Fatal(err)
		}
		close(startNext)
		wg.Wait()
		if id, active, _ := p.current(); id != 2 || !active {
			t.Fatal("old timer cancelled new press")
		}
	}
}
