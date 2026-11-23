package ai

import (
	"errors"
	"testing"
	"time"
)

func TestClearGatePreservesOrderAndCopies(t *testing.T) {
	q, _ := newManualClearGate(4)
	now := time.Now()
	if err := q.begin(now, time.Second); err != nil {
		t.Fatal(err)
	}
	a := []int16{1, 2}
	_ = q.hold(now, a)
	a[0] = 99
	_ = q.hold(now, []int16{3})
	frames, err := q.acknowledge(now.Add(time.Millisecond))
	if err != nil || len(frames) != 2 || frames[0][0] != 1 || frames[1][0] != 3 {
		t.Fatal(frames, err)
	}
}
func TestClearGateFailsClosedOnTimeoutOverflowAndDuplicate(t *testing.T) {
	now := time.Now()
	q, _ := newManualClearGate(2)
	_ = q.begin(now, time.Second)
	if err := q.hold(now, []int16{1, 2, 3}); !errors.Is(err, errManualClearGate) {
		t.Fatal(err)
	}
	if _, err := q.acknowledge(now.Add(time.Second)); !errors.Is(err, errManualClearGate) {
		t.Fatal(err)
	}
	if err := q.begin(now, time.Second); !errors.Is(err, errManualClearGate) {
		t.Fatal(err)
	}
}
func TestClearGateIgnoresEmptyFrames(t *testing.T) {
	q, _ := newManualClearGate(1)
	now := time.Now()
	_ = q.begin(now, time.Second)
	for i := 0; i < 1000; i++ {
		if err := q.hold(now, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.frames) != 0 {
		t.Fatal("empty frames grew queue")
	}
}
