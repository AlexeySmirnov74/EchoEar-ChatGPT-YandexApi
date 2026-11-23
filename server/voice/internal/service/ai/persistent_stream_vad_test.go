package ai

import (
	"errors"
	"testing"
)

func pcmFrame(value int16) []int16 {
	p := make([]int16, deviceFrameSamples)
	for i := range p {
		p[i] = value
	}
	return p
}

func TestStreamVADPrerollAndImmediateFirstFrame(t *testing.T) {
	v, err := newStreamLocalVAD(400, 120, 120, 120, 2000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		e, err := v.feed(pcmFrame(100))
		if err != nil || len(e.pcm) != 0 {
			t.Fatal(e, err)
		}
	}
	e, err := v.feed(pcmFrame(3000))
	if err != nil || len(e.pcm) != 3*deviceFrameSamples || e.pcm[0] != 100 || e.pcm[len(e.pcm)-1] != 3000 || e.commit || e.clear {
		t.Fatal("pre-roll/first frame", len(e.pcm), err)
	}
	e, err = v.feed(pcmFrame(3000))
	if err != nil || len(e.pcm) != deviceFrameSamples {
		t.Fatal(err)
	}
	e, err = v.feed(pcmFrame(0))
	if err != nil || e.commit {
		t.Fatal(e, err)
	}
	e, err = v.feed(pcmFrame(0))
	if err != nil || !e.commit || e.clear {
		t.Fatal(e, err)
	}
}

func TestStreamVADShortBurstClearsAndOverflowFails(t *testing.T) {
	v, _ := newStreamLocalVAD(400, 120, 120, 60, 360)
	_, _ = v.feed(pcmFrame(3000))
	_, _ = v.feed(pcmFrame(0))
	e, err := v.feed(pcmFrame(0))
	if err != nil || !e.clear || e.commit {
		t.Fatal(e, err)
	}
	for i := 0; i < 6; i++ {
		if _, err := v.feed(pcmFrame(3000)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.feed(pcmFrame(3000)); !errors.Is(err, errLocalTurnTooLong) {
		t.Fatal("long utterance was not rejected", err)
	}
}

func TestStreamVADMemoryBoundAndDetachedPCM(t *testing.T) {
	v, _ := newStreamLocalVAD(400, 60, 120, 120, 1200)
	for i := 0; i < 100; i++ {
		_, _ = v.feed(pcmFrame(1))
		if len(v.recent) > v.preRoll {
			t.Fatal("pre-roll unbounded")
		}
	}
	p := pcmFrame(1000)
	e, err := v.feed(p)
	if err != nil {
		t.Fatal(err)
	}
	p[0] = 0
	if e.pcm[len(e.pcm)-len(p)] != 1000 {
		t.Fatal("caller mutated output")
	}
}
