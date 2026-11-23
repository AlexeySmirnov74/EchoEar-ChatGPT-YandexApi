package ai

import (
	"errors"
	"sync"
)

var errStaleAudioGeneration = errors.New("audio belongs to an inactive listen generation")
var errPersistentAudioQueueFull = errors.New("persistent audio queue full")

type persistentAudioFrame struct {
	listenID uint32
	data     []byte
	end      bool
}

// persistentAudioQueue associates every output frame and terminal marker with
// the request that produced it. Starting or ending an interaction discards
// only pending output for the old generation, including its TTS stop marker.
// A caller must surface an overflow as an error, not pretend speech completed.
type persistentAudioQueue struct {
	mu       sync.Mutex
	limit    int
	activeID uint32
	frames   []persistentAudioFrame
}

func newPersistentAudioQueue(limit int) *persistentAudioQueue {
	return &persistentAudioQueue{limit: limit}
}

func (q *persistentAudioQueue) start(id uint32) error {
	if id == 0 {
		return errStaleAudioGeneration
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.activeID = id
	q.frames = nil
	return nil
}

func (q *persistentAudioQueue) stop(id uint32) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == 0 || id != q.activeID {
		return false
	}
	q.activeID = 0
	q.frames = nil
	return true
}

func (q *persistentAudioQueue) push(id uint32, data []byte, end bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == 0 || id != q.activeID {
		return errStaleAudioGeneration
	}
	if len(q.frames) >= q.limit {
		return errPersistentAudioQueueFull
	}
	q.frames = append(q.frames, persistentAudioFrame{listenID: id, data: append([]byte(nil), data...), end: end})
	return nil
}

// sendNext holds the generation lock through the WebSocket write. Thus a new
// generation cannot start between dequeuing an old frame and writing it.
// The caller must set a finite write deadline before invoking this method.
func (q *persistentAudioQueue) sendNext(send func(persistentAudioFrame) error) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 || q.activeID == 0 {
		return false, nil
	}
	frame := q.frames[0]
	q.frames[0] = persistentAudioFrame{}
	q.frames = q.frames[1:]
	if frame.listenID != q.activeID {
		return false, errStaleAudioGeneration
	}
	if err := send(frame); err != nil {
		return false, err
	}
	return true, nil
}

// withGeneration serializes an associated TTS start/stop control with writes.
func (q *persistentAudioQueue) withGeneration(id uint32, send func() error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == 0 || id != q.activeID {
		return errStaleAudioGeneration
	}
	return send()
}
