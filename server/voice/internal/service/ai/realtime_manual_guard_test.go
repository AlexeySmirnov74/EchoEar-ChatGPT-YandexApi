package ai

import (
	"errors"
	"testing"
	"time"
)

func manualReady(t *testing.T, id uint32) *manualTurnGuard {
	t.Helper()
	g := &manualTurnGuard{}
	if err := g.beginNext(id); err != nil {
		t.Fatal(err)
	}
	if err := g.appendAudio(id, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.commit(id, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.committed("item1"); err != nil {
		t.Fatal(err)
	}
	if err := g.requestResponse(id, "nonce1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.responseCreated(id, "nonce1", "resp1"); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestManualNormalTurnKeepsProvider(t *testing.T) {
	g := manualReady(t, 1)
	if err := g.output("resp1", func(id uint32) error {
		if id != 1 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.done("resp1"); err != nil {
		t.Fatal(err)
	}
	if err := g.played("resp1"); err != nil {
		t.Fatal(err)
	}
	if err := g.beginNext(2); err != nil {
		t.Fatal(err)
	}
	if err := g.appendAudio(2, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.commit(2, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.committed("item2"); err != nil {
		t.Fatal(err)
	}
	if err := g.requestResponse(2, "nonce2", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.responseCreated(2, "nonce2", "resp2"); err != nil {
		t.Fatal(err)
	}
	if err := g.output("resp2", func(id uint32) error {
		if id != 2 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManualToolResponseDoneBeforeResult(t *testing.T) {
	g := manualReady(t, 1)
	if err := g.tool("resp1", "call1", func(id uint32) error {
		if id != 1 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.done("resp1"); err != nil {
		t.Fatal(err)
	}
	if err := g.toolResult(1, "call1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.requestResponse(1, "nonce2", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.responseCreated(1, "nonce2", "resp2"); err != nil {
		t.Fatal(err)
	}
	if err := g.output("resp2", func(id uint32) error {
		if id != 1 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.done("resp2"); err != nil {
		t.Fatal(err)
	}
	if err := g.played("resp2"); err != nil {
		t.Fatal(err)
	}
	if err := g.beginNext(2); err != nil {
		t.Fatal(err)
	}
}

func TestManualToolResultBeforeResponseDone(t *testing.T) {
	g := manualReady(t, 1)
	_ = g.tool("resp1", "call", func(uint32) error { return nil })
	if err := g.toolResult(1, "call", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.done("resp1"); err != nil {
		t.Fatal(err)
	}
	if err := g.requestResponse(1, "next", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestManualRejectsUncertainTransitions(t *testing.T) {
	tests := map[string]func(*manualTurnGuard) error{
		"unknown response": func(g *manualTurnGuard) error {
			return g.output("other", func(uint32) error { t.Fatal("old output"); return nil })
		},
		"unknown tool": func(g *manualTurnGuard) error {
			return g.tool("other", "call", func(uint32) error { t.Fatal("old tool"); return nil })
		},
		"early next":       func(g *manualTurnGuard) error { return g.beginNext(2) },
		"uncommitted item": func(g *manualTurnGuard) error { return g.committed("other") },
		"late audio": func(g *manualTurnGuard) error {
			return g.appendAudio(1, func() error { t.Fatal("audio after commit"); return nil })
		},
		"interrupt": func(g *manualTurnGuard) error { return g.interrupt() },
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			g := manualReady(t, 1)
			if !errors.Is(fn(g), errManualGuard) {
				t.Fatal("ambiguous transition accepted")
			}
		})
	}
	g := manualReady(t, 1)
	_ = g.done("resp1")
	_ = g.played("resp1")
	_ = g.beginNext(2)
	_ = g.appendAudio(2, func() error { return nil })
	_ = g.commit(2, func() error { return nil })
	if err := g.committed("item1"); err == nil {
		t.Fatal("old commit item accepted")
	}
}

func TestManualAppendFinishesBeforeCommit(t *testing.T) {
	g := &manualTurnGuard{}
	_ = g.beginNext(1)
	inside := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 2)
	go func() { _ = g.appendAudio(1, func() error { close(inside); <-release; order <- "audio"; return nil }) }()
	<-inside
	go func() { _ = g.commit(1, func() error { order <- "commit"; return nil }) }()
	close(release)
	for _, want := range []string{"audio", "commit"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatal(got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("deadlock")
		}
	}
	if err := g.appendAudio(1, func() error { t.Fatal("audio sent after commit"); return nil }); err == nil {
		t.Fatal("late append accepted")
	}
}

func TestManualClearBurstRequiresSingleAck(t *testing.T) {
	g := &manualTurnGuard{}
	_ = g.beginNext(1)
	_ = g.appendAudio(1, func() error { return nil })
	if err := g.clearShortBurst(1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.cleared(); err != nil {
		t.Fatal(err)
	}
	if err := g.appendAudio(1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.commit(1, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	g = &manualTurnGuard{}
	_ = g.beginNext(1)
	_ = g.appendAudio(1, func() error { return nil })
	_ = g.clearShortBurst(1, func() error { return nil })
	if err := g.appendAudio(1, func() error { t.Fatal("audio sent before clear ack"); return nil }); err == nil {
		t.Fatal("sent during clear")
	}
	g = &manualTurnGuard{}
	if err := g.cleared(); err == nil {
		t.Fatal("unexpected clear ack accepted")
	}
}

func TestManualStopAndNextStartAreSerialized(t *testing.T) {
	g := manualReady(t, 1)
	_ = g.done("resp1")
	inside := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 2)
	go func() {
		_ = g.finishPlayback("resp1", func(uint32) error { close(inside); <-release; order <- "stop"; return nil })
	}()
	<-inside
	go func() {
		if err := g.beginNext(2); err != nil {
			t.Errorf("start: %v", err)
		}
		order <- "start"
	}()
	close(release)
	for _, want := range []string{"stop", "start"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatal(got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("deadlock")
		}
	}
}
