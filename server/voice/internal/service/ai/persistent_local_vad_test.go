package ai

import (
	"errors"
	"testing"
)

func localFrame(level int16) []int16 {
	frame := make([]int16, deviceFrameSamples)
	for i := range frame {
		if i%2 == 0 {
			frame[i] = level
		} else {
			frame[i] = -level
		}
	}
	return frame
}

func TestLocalTurnBufferKeepsBeginningAndWaitsForSilence(t *testing.T) {
	v, err := newLocalTurnBuffer(400, 300, 600, 300, 30000)
	if err != nil {
		t.Fatal(err)
	}
	// Quiet onset is retained in pre-roll even though it doesn't trigger VAD.
	for i := 0; i < 5; i++ {
		if out, e := v.feed(localFrame(200)); e != nil || out != nil {
			t.Fatal("unexpected quiet output", e)
		}
	}
	for i := 0; i < 5; i++ {
		if out, e := v.feed(localFrame(800)); e != nil || out != nil {
			t.Fatal("unexpected speech output", e)
		}
	}
	// A 360 ms pause does not finish the utterance.
	for i := 0; i < 6; i++ {
		if out, e := v.feed(localFrame(0)); e != nil || out != nil {
			t.Fatal("short pause ended turn", e)
		}
	}
	if out, e := v.feed(localFrame(800)); e != nil || out != nil {
		t.Fatal("unexpected output", e)
	}
	for i := 0; i < 9; i++ {
		if out, e := v.feed(localFrame(0)); e != nil || out != nil {
			t.Fatal("ended early", e)
		}
	}
	out, err := v.feed(localFrame(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != (5+5+6+1+10)*deviceFrameSamples {
		t.Fatalf("unexpected samples %d", len(out))
	}
	if out[0] != 200 {
		t.Fatal("quiet onset was not retained")
	}
}

func TestLocalTurnBufferBoundsSilenceNoiseAndLongTurns(t *testing.T) {
	v, err := newLocalTurnBuffer(400, 300, 600, 300, 1200)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if out, e := v.feed(localFrame(0)); e != nil || out != nil {
			t.Fatal(e)
		}
	}
	if len(v.recent) > v.preRoll || len(v.turn) != 0 {
		t.Fatal("idle audio buffer grew")
	}
	if out, e := v.feed(localFrame(800)); e != nil || out != nil {
		t.Fatal(e)
	}
	for i := 0; i < 10; i++ {
		if out, e := v.feed(localFrame(0)); e != nil || out != nil {
			t.Fatal("noise burst committed", e)
		}
	}
	if v.active || len(v.turn) != 0 {
		t.Fatal("noise burst not discarded")
	}
	for i := 0; i < 20; i++ {
		if out, e := v.feed(localFrame(800)); e != nil || out != nil {
			t.Fatal(e)
		}
	}
	if out, e := v.feed(localFrame(800)); out != nil || !errors.Is(e, errLocalTurnTooLong) {
		t.Fatal("long utterance not rejected", e)
	}
	if v.active || len(v.turn) != 0 {
		t.Fatal("overflow was not cleared")
	}
}

func TestLocalTurnBufferRejectsInvalidSettings(t *testing.T) {
	for _, vals := range [][5]int{{0, 300, 600, 300, 30000}, {400, 0, 600, 300, 30000}, {400, 300, 600, 300, 900000}, {400, 300, 600, 1200, 1200}} {
		if _, err := newLocalTurnBuffer(vals[0], vals[1], vals[2], vals[3], vals[4]); err == nil {
			t.Fatalf("accepted invalid settings %v", vals)
		}
	}
}
