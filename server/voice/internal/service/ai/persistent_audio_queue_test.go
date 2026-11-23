package ai

import (
	"errors"
	"testing"
	"time"
)

func TestAudioQueueRejectsOldFramesAndStops(t *testing.T) {
	q := newPersistentAudioQueue(3)
	if err := q.start(1); err != nil {
		t.Fatal(err)
	}
	if err := q.push(1, []byte{1}, false); err != nil {
		t.Fatal(err)
	}
	if err := q.push(1, nil, true); err != nil {
		t.Fatal(err)
	}
	if !q.stop(1) {
		t.Fatal("pause failed")
	}
	if err := q.start(2); err != nil {
		t.Fatal(err)
	}
	if err := q.push(1, []byte{9}, false); !errors.Is(err, errStaleAudioGeneration) {
		t.Fatal(err)
	}
	if err := q.withGeneration(1, func() error { return nil }); !errors.Is(err, errStaleAudioGeneration) {
		t.Fatal(err)
	}
	if sent, err := q.sendNext(func(persistentAudioFrame) error { return nil }); sent || err != nil {
		t.Fatal("stale output survived", sent, err)
	}
	if err := q.push(2, []byte{2}, false); err != nil {
		t.Fatal(err)
	}
	if sent, err := q.sendNext(func(f persistentAudioFrame) error {
		if f.listenID != 2 || len(f.data) != 1 || f.data[0] != 2 {
			t.Fatal(f)
		}
		return nil
	}); !sent || err != nil {
		t.Fatal(sent, err)
	}
}

func TestAudioQueueFullIsExplicit(t *testing.T) {
	q := newPersistentAudioQueue(1)
	_ = q.start(1)
	frame := []byte{42}
	if err := q.push(1, frame, false); err != nil {
		t.Fatal(err)
	}
	frame[0] = 0
	if err := q.push(1, nil, true); !errors.Is(err, errPersistentAudioQueueFull) {
		t.Fatal("overflow was not reported", err)
	}
	if sent, err := q.sendNext(func(f persistentAudioFrame) error {
		if f.data[0] != 42 {
			t.Fatal("mutable input changed queued audio")
		}
		return nil
	}); !sent || err != nil {
		t.Fatal(err)
	}
}

func TestAudioQueueSerializesOldWriteBeforeNewGeneration(t *testing.T) {
	q := newPersistentAudioQueue(2)
	_ = q.start(1)
	_ = q.push(1, []byte{1}, false)
	insideWrite := make(chan struct{})
	releaseWrite := make(chan struct{})
	order := make(chan string, 2)
	go func() {
		_, _ = q.sendNext(func(f persistentAudioFrame) error {
			close(insideWrite)
			<-releaseWrite
			order <- "old frame written"
			return nil
		})
	}()
	<-insideWrite
	go func() { _ = q.start(2); order <- "new generation" }()
	close(releaseWrite)
	for _, want := range []string{"old frame written", "new generation"} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("deadlock")
		}
	}
}
