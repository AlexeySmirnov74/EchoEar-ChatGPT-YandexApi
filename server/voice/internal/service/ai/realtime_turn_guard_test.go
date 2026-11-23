package ai

import (
	"errors"
	"testing"
	"time"
)

func activeGuard(t *testing.T, id uint32) *realtimeTurnGuard {
	t.Helper()
	g := &realtimeTurnGuard{}
	if err := g.begin(id); err != nil {
		t.Fatal(err)
	}
	if err := g.input(id, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.speechStarted("item1"); err != nil {
		t.Fatal(err)
	}
	if err := g.speechStopped("item1"); err != nil {
		t.Fatal(err)
	}
	if err := g.committed("item1"); err != nil {
		t.Fatal(err)
	}
	if err := g.request(id, "nonce1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.created(id, "nonce1", "resp1"); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestTurnGuardClearStillClosesAfterAudio(t *testing.T) {
	g := activeGuard(t, 1)
	if err := g.authorizeOutput("resp1", func(id uint32) error {
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
	if err := g.pause(1, func(cancel string) error {
		if cancel != "" {
			t.Fatal(cancel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.cleared(); err != nil {
		t.Fatal(err)
	}
	if err := g.begin(2); err == nil {
		t.Fatal("clear ACK cannot fence delayed VAD events")
	}
}

func TestTurnGuardNoAudioAllowsNextListen(t *testing.T) {
	g := &realtimeTurnGuard{}
	if err := g.begin(1); err != nil {
		t.Fatal(err)
	}
	if err := g.pause(1, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.cleared(); err != nil {
		t.Fatal(err)
	}
	if err := g.begin(2); err != nil {
		t.Fatal(err)
	}
}

func TestTurnGuardCancelRequiresBothBarriers(t *testing.T) {
	for _, clearFirst := range []bool{false, true} {
		g := activeGuard(t, 1)
		if err := g.pause(1, func(cancel string) error {
			if cancel != "resp1" {
				t.Fatal(cancel)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if clearFirst {
			if err := g.cleared(); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := g.done("resp1"); err != nil {
				t.Fatal(err)
			}
		}
		if clearFirst {
			if err := g.done("resp1"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := g.cleared(); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.begin(2); err == nil {
			t.Fatal("partially generated response left stale context")
		}
	}
	for _, operation := range []func(*realtimeTurnGuard) error{
		func(g *realtimeTurnGuard) error { return g.begin(2) },
		func(g *realtimeTurnGuard) error {
			return g.input(1, func() error { t.Fatal("idle input sent"); return nil })
		},
	} {
		g := activeGuard(t, 1)
		_ = g.pause(1, func(string) error { return nil })
		if !errors.Is(operation(g), errRealtimeGuard) {
			t.Fatal("unconfirmed barrier allowed")
		}
	}
}

func TestTurnGuardRejectsStaleAndAmbiguousEvents(t *testing.T) {
	for name, operation := range map[string]func(*realtimeTurnGuard) error{
		"unknown audio": func(g *realtimeTurnGuard) error {
			return g.authorizeOutput("old", func(uint32) error { t.Fatal("sent old audio"); return nil })
		},
		"unknown done":     func(g *realtimeTurnGuard) error { return g.done("old") },
		"unexpected clear": func(g *realtimeTurnGuard) error { return g.cleared() },
		"stale tool": func(g *realtimeTurnGuard) error {
			return g.authorizeTool("old", "call", func(uint32) error { t.Fatal("sent stale tool"); return nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := activeGuard(t, 1)
			if !errors.Is(operation(g), errRealtimeGuard) {
				t.Fatal("accepted invalid event")
			}
			if err := g.input(1, func() error { t.Fatal("sent after failure"); return nil }); err == nil {
				t.Fatal("not closed")
			}
		})
	}
	g := &realtimeTurnGuard{}
	_ = g.begin(1)
	_ = g.speechStarted("one")
	if err := g.speechStopped("other"); err == nil {
		t.Fatal("item mismatch accepted")
	}
	g = &realtimeTurnGuard{}
	_ = g.begin(1)
	_ = g.speechStarted("one")
	_ = g.speechStopped("one")
	_ = g.committed("one")
	_ = g.request(1, "nonce", func() error { return nil })
	if err := g.created(1, "bad", "response"); err == nil {
		t.Fatal("metadata mismatch accepted")
	}
	g = &realtimeTurnGuard{}
	_ = g.begin(1)
	_ = g.speechStarted("one")
	_ = g.speechStopped("one")
	_ = g.committed("one")
	_ = g.request(1, "nonce", func() error { return nil })
	if err := g.pause(1, func(string) error { t.Fatal("ambiguous cancellation sent"); return nil }); err == nil {
		t.Fatal("unidentified response accepted")
	}
}

func TestTurnGuardToolAndPause(t *testing.T) {
	g := activeGuard(t, 1)
	if err := g.authorizeTool("resp1", "call1", func(id uint32) error {
		if id != 1 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.pause(1, func(string) error { t.Fatal("pending tool allowed clear"); return nil }); err == nil {
		t.Fatal("pending tool not fenced")
	}
	g = activeGuard(t, 1)
	_ = g.authorizeTool("resp1", "call1", func(uint32) error { return nil })
	if err := g.toolDone(1, "call1"); err != nil {
		t.Fatal(err)
	}
	if err := g.done("resp1"); err != nil {
		t.Fatal(err)
	}
}

func TestTurnGuardSerializesOutputBeforePause(t *testing.T) {
	g := activeGuard(t, 1)
	inside := make(chan struct{})
	release := make(chan struct{})
	order := make(chan string, 2)
	go func() {
		_ = g.authorizeOutput("resp1", func(uint32) error { close(inside); <-release; order <- "output"; return nil })
	}()
	<-inside
	go func() { _ = g.pause(1, func(string) error { order <- "pause"; return nil }) }()
	close(release)
	for _, want := range []string{"output", "pause"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatal(got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("deadlock")
		}
	}
	if err := g.authorizeOutput("resp1", func(uint32) error { t.Fatal("late output sent"); return nil }); err == nil {
		t.Fatal("late output accepted")
	}
}
