package ai

import "errors"

// streamLocalVAD is an experimental opt-in speech gate. Its caller sends each
// returned chunk to Realtime in order, commits only when commit=true and waits
// for committed ACK, and on clear=true must block new remote writes until a
// clear ACK. The caller is responsible for deadline/reconnect and any bounded
// buffering while waiting for that ACK. Never persist the PCM. RMS is a simple
// energy gate and must be measured on the actual device before use.
type streamLocalVAD struct {
	threshold                                 int64
	minSpeech, silence, preRoll, maxTurn      int
	recent                                    []int16
	active                                    bool
	speechSamples, quietSamples, totalSamples int
}

type streamVADEvent struct {
	pcm    []int16
	commit bool
	clear  bool
}

func newStreamLocalVAD(threshold, minSpeechMS, silenceMS, preRollMS, maxTurnMS int) (*streamLocalVAD, error) {
	if threshold <= 0 || threshold > 32767 || minSpeechMS <= 0 || silenceMS <= 0 || preRollMS < 0 || maxTurnMS > 60000 || maxTurnMS <= preRollMS || minSpeechMS > maxTurnMS || silenceMS > maxTurnMS-minSpeechMS {
		return nil, errors.New("invalid streaming VAD settings")
	}
	samples := func(ms int) int { return ms * deviceSampleRate / 1000 }
	return &streamLocalVAD{threshold: int64(threshold), minSpeech: samples(minSpeechMS), silence: samples(silenceMS), preRoll: samples(preRollMS), maxTurn: samples(maxTurnMS)}, nil
}

// feed must be serialized with clear/commit handling. PCM in the returned
// event is detached from the caller's buffer.
func (v *streamLocalVAD) feed(pcm []int16) (streamVADEvent, error) {
	if len(pcm) == 0 {
		return streamVADEvent{}, nil
	}
	var energy int64
	for _, s := range pcm {
		energy += int64(s) * int64(s)
	}
	speech := energy/int64(len(pcm)) > v.threshold*v.threshold
	if !v.active && !speech {
		v.addRecent(pcm)
		return streamVADEvent{}, nil
	}
	var out []int16
	if !v.active {
		v.active = true
		out = append(out, v.recent...)
		v.recent = nil
	}
	out = append(out, pcm...)
	if v.totalSamples+len(out) > v.maxTurn {
		v.reset()
		return streamVADEvent{}, errLocalTurnTooLong
	}
	v.totalSamples += len(out)
	if speech {
		v.speechSamples += len(pcm)
		v.quietSamples = 0
	} else {
		v.quietSamples += len(pcm)
	}
	event := streamVADEvent{pcm: out}
	if v.quietSamples >= v.silence {
		if v.speechSamples >= v.minSpeech {
			event.commit = true
		} else {
			event.clear = true
		}
		v.reset()
	}
	return event, nil
}

func (v *streamLocalVAD) addRecent(pcm []int16) {
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

func (v *streamLocalVAD) reset() {
	v.recent = nil
	v.active = false
	v.speechSamples = 0
	v.quietSamples = 0
	v.totalSamples = 0
}
