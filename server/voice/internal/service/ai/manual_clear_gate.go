package ai

import (
	"errors"
	"time"
)

// manualClearGate retains a strictly bounded amount of PCM while exactly one
// input_audio_buffer.clear acknowledgement is pending. A single device actor
// must serialize begin, hold, and acknowledge with its VAD and provider writes.
// expiry must also be enforced by an independent timer when no frames arrive.
type manualClearGate struct {
	limit    int
	deadline time.Time
	frames   [][]int16
	samples  int
	pending  bool
}

var errManualClearGate = errors.New("clear acknowledgement expired or PCM limit exceeded; close provider")

func newManualClearGate(maxSamples int) (*manualClearGate, error) {
	if maxSamples <= 0 || maxSamples > deviceSampleRate*5 {
		return nil, errManualClearGate
	}
	return &manualClearGate{limit: maxSamples}, nil
}

func (q *manualClearGate) begin(now time.Time, timeout time.Duration) error {
	if q.pending || timeout <= 0 {
		return errManualClearGate
	}
	q.pending = true
	q.deadline = now.Add(timeout)
	q.frames = nil
	q.samples = 0
	return nil
}

func (q *manualClearGate) hold(now time.Time, pcm []int16) error {
	if !q.pending || !now.Before(q.deadline) || len(pcm) > q.limit-q.samples {
		return errManualClearGate
	}
	if len(pcm) == 0 {
		return nil
	}
	q.frames = append(q.frames, append([]int16(nil), pcm...))
	q.samples += len(pcm)
	return nil
}

func (q *manualClearGate) acknowledge(now time.Time) ([][]int16, error) {
	if !q.pending || !now.Before(q.deadline) {
		return nil, errManualClearGate
	}
	frames := q.frames
	q.pending = false
	q.frames = nil
	q.samples = 0
	return frames, nil
}
