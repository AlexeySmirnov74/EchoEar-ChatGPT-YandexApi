package ai

import (
	"errors"
	"testing"
	"time"
)

func TestDeviceProtocolLegacyAndHeartbeat(t *testing.T) {
	p := newPersistentDeviceProtocol(4)
	if p.hello(map[string]any{}) != nil {
		t.Fatal("legacy hello acknowledged feature")
	}
	if p.heartbeat(func(map[string]any) error { t.Fatal("legacy heartbeat"); return nil }) == nil {
		t.Fatal("legacy heartbeat accepted")
	}
	if p.hello(map[string]any{"features": map[string]any{"persistent_idle": true}}) != nil {
		t.Fatal("duplicate hello changed negotiation")
	}
	p = newPersistentDeviceProtocol(4)
	if p.hello(map[string]any{"features": map[string]any{"persistent_idle": true}})["persistent_idle"] != true {
		t.Fatal("feature not acknowledged")
	}
	if err := p.heartbeat(func(msg map[string]any) error {
		if msg["type"] != "heartbeat" {
			t.Fatal(msg)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceProtocolStaleMessages(t *testing.T) {
	p := newPersistentDeviceProtocol(4)
	p.hello(map[string]any{"features": map[string]any{"persistent_idle": true}})
	msg := func(id float64) map[string]any { return map[string]any{"listen_id": id} }
	if err := p.start(msg(1)); err != nil {
		t.Fatal(err)
	}
	if err := p.start(msg(1)); err == nil {
		t.Fatal("repeated start")
	}
	if err := p.input(1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.push(1, []byte{1}, false); err != nil {
		t.Fatal(err)
	}
	if err := p.start(msg(2)); !errors.Is(err, errProviderGenerationAmbiguous) {
		t.Fatal("used provider accepted new ID", err)
	}
	if err := p.replaceProvider(2); err != nil {
		t.Fatal(err)
	}
	if err := p.start(msg(2)); err != nil {
		t.Fatal(err)
	}
	if p.hello(map[string]any{"features": map[string]any{"persistent_idle": true}}) != nil {
		t.Fatal("duplicate hello accepted")
	}
	if err := p.start(msg(1)); err == nil {
		t.Fatal("duplicate hello reset generation")
	}
	if p.pause(msg(1)) {
		t.Fatal("stale pause accepted")
	}
	if sent, err := p.standby(1, func(map[string]any) error { t.Fatal("stale standby sent"); return nil }); sent || err != nil {
		t.Fatal(sent, err)
	}
	if err := p.tts(1, "start", func(map[string]any) error { t.Fatal("stale tts sent"); return nil }); !errors.Is(err, errStaleAudioGeneration) {
		t.Fatal(err)
	}
	if err := p.push(1, []byte{1}, false); !errors.Is(err, errStaleAudioGeneration) {
		t.Fatal(err)
	}
	if sent, err := p.sendNext(func(persistentAudioFrame) error { t.Fatal("old frame survived"); return nil }); sent || err != nil {
		t.Fatal(sent, err)
	}
	if err := p.input(2, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.ttsFor(2, 2, "start", func(m map[string]any) error {
		if m["listen_id"] != uint32(2) {
			t.Fatal(m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sent, err := p.standby(2, func(m map[string]any) error {
		if m["listen_id"] != uint32(2) || m["type"] != "standby" {
			t.Fatal(m)
		}
		return nil
	}); !sent || err != nil {
		t.Fatal(sent, err)
	}
	if err := p.push(2, []byte{2}, false); !errors.Is(err, errStaleAudioGeneration) {
		t.Fatal(err)
	}
}

func TestDeviceProtocolTransitionWaitsForOutput(t *testing.T) {
	p := newPersistentDeviceProtocol(2)
	p.hello(map[string]any{"features": map[string]any{"persistent_idle": true}})
	if err := p.start(map[string]any{"listen_id": float64(1)}); err != nil {
		t.Fatal(err)
	}
	if err := p.input(1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.push(1, []byte{1}, false); err != nil {
		t.Fatal(err)
	}
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan string, 2)
	go func() {
		_, _ = p.sendNext(func(persistentAudioFrame) error { close(inside); <-release; done <- "frame"; return nil })
	}()
	<-inside
	go func() { _ = p.replaceProvider(2); _ = p.start(map[string]any{"listen_id": float64(2)}); done <- "new" }()
	close(release)
	for _, want := range []string{"frame", "new"} {
		select {
		case got := <-done:
			if got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("deadlock")
		}
	}
}
