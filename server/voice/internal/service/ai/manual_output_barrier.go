package ai

import "errors"

// manualOutputBarrier is driven by the same serialized actor that handles
// provider events and device sends. It requires both response.done and a
// physically drained *server queue*, in either order, before stop is sent. A
// caller must not arm it for a response with an unresolved tool continuation,
// and must report drained only after output_audio.done plus actual queue drain;
// a transiently empty queue is not end of output.
// It makes no claim about physical device playback after network delivery.
type manualOutputBarrier struct {
	responseID string
	done       bool
	drained    bool
	finished   bool
}

var errManualOutputBarrier = errors.New("invalid output barrier event; close provider")

func (b *manualOutputBarrier) responseDone(id string) error {
	if id == "" || id != b.responseID || b.done || b.finished {
		return errManualOutputBarrier
	}
	b.done = true
	return nil
}
func (b *manualOutputBarrier) outputDrained(id string) error {
	if id == "" || id != b.responseID || b.drained || b.finished {
		return errManualOutputBarrier
	}
	b.drained = true
	return nil
}
func (b *manualOutputBarrier) finish(sendStop func() error) (bool, error) {
	if b.finished {
		return false, errManualOutputBarrier
	}
	if !b.done || !b.drained {
		return false, nil
	}
	b.finished = true
	if err := sendStop(); err != nil {
		return false, err
	}
	return true, nil
}
