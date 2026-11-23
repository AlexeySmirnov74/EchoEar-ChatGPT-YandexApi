package ai

import "errors"

// localTurnBuffer is an experimental, separately configured VAD path. It is
// intentionally not connected to HandleWS. It holds only bounded in-memory
// audio, supplies the frames before the first detected speech as pre-roll, and
// never sends silence or partial turns to a provider. Quality must be measured
// on the real microphone before enabling it.
type localTurnBuffer struct {
	threshold                            int64
	minSpeech, silence, preRoll, maxTurn int
	recent, turn                         []int16
	speechSamples, quietSamples          int
	active                               bool
}

var errLocalTurnTooLong = errors.New("local turn exceeds maximum duration")

func newLocalTurnBuffer(threshold, minSpeechMS, silenceMS, preRollMS, maxTurnMS int) (*localTurnBuffer, error) {
	if threshold <= 0 || threshold > 32767 || minSpeechMS <= 0 || silenceMS <= 0 || preRollMS < 0 || maxTurnMS > 60000 || maxTurnMS <= preRollMS || minSpeechMS > maxTurnMS || silenceMS > maxTurnMS-minSpeechMS {
		return nil, errors.New("invalid local VAD settings")
	}
	samples := func(ms int) int { return ms * deviceSampleRate / 1000 }
	return &localTurnBuffer{threshold: int64(threshold), minSpeech: samples(minSpeechMS), silence: samples(silenceMS), preRoll: samples(preRollMS), maxTurn: samples(maxTurnMS)}, nil
}

// feed is called in order by one session actor with decoded 16 kHz PCM. A
// non-nil result is one bounded complete utterance for explicit commit. While
// no speech has been detected, only the most recent pre-roll is retained. The
// overflow error intentionally does not commit a truncated utterance.
func (v *localTurnBuffer) feed(pcm []int16) ([]int16, error) {
	if len(pcm) == 0 {
		return nil, nil
	}
	var energy int64
	for _, s := range pcm {
		energy += int64(s) * int64(s)
	}
	speech := energy/int64(len(pcm)) > v.threshold*v.threshold
	if !v.active {
		if !speech {
			v.addRecent(pcm)
			return nil, nil
		}
		v.active = true
		v.turn = append(v.turn, v.recent...)
		v.recent = nil
	}
	if len(v.turn)+len(pcm) > v.maxTurn {
		v.reset()
		return nil, errLocalTurnTooLong
	}
	v.turn = append(v.turn, pcm...)
	if speech {
		v.speechSamples += len(pcm)
		v.quietSamples = 0
	} else {
		v.quietSamples += len(pcm)
	}
	if v.quietSamples < v.silence {
		return nil, nil
	}
	if v.speechSamples < v.minSpeech {
		v.reset()
		return nil, nil
	}
	result := v.turn
	v.turn = nil
	v.reset()
	return result, nil
}

func (v *localTurnBuffer) addRecent(pcm []int16) {
	if v.preRoll == 0 {
		return
	}
	if len(pcm) >= v.preRoll {
		v.recent = append(v.recent[:0], pcm[len(pcm)-v.preRoll:]...)
		return
	}
	excess := len(v.recent) + len(pcm) - v.preRoll
	if excess > 0 {
		copy(v.recent, v.recent[excess:])
		v.recent = v.recent[:len(v.recent)-excess]
	}
	v.recent = append(v.recent, pcm...)
}

func (v *localTurnBuffer) reset() {
	v.recent = nil
	v.turn = nil
	v.speechSamples = 0
	v.quietSamples = 0
	v.active = false
}
