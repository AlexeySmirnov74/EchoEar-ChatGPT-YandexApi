package ai

import (
	"errors"
	"sync"
)

// persistentDeviceProtocol serializes provider generations, device generations,
// and device output. A provider is bound to at most one listen generation:
// without upstream response IDs it is unsafe to reassign a used provider.
// All methods that change the generation or write output take the same lock.
// Callers must set a finite WebSocket write deadline before invoking send.
type persistentDeviceProtocol struct {
	mu                 sync.Mutex
	control            persistentControl
	queue              *persistentAudioQueue
	helloReceived      bool
	providerGeneration uint64
	providerListenID   uint32
}

func newPersistentDeviceProtocol(queueLimit int) *persistentDeviceProtocol {
	return &persistentDeviceProtocol{queue: newPersistentAudioQueue(queueLimit), providerGeneration: 1}
}

var errProviderGenerationAmbiguous = errors.New("used provider cannot be associated with a new listen generation")

// replaceProvider is the atomic merge point for a provider rotator. The caller
// supplies a fresh, fully ready provider and installs its callbacks before
// calling this; the old provider must be closed after this method returns.
// The production handler never invokes it until rotation has been implemented.
func (p *persistentDeviceProtocol) replaceProvider(generation uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation == 0 || generation <= p.providerGeneration {
		return errors.New("provider generation must increase")
	}
	id, _, _ := p.control.current()
	if id != 0 {
		p.queue.stop(id)
	}
	p.providerGeneration, p.providerListenID = generation, 0
	return nil
}

func (p *persistentDeviceProtocol) enabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _, enabled := p.control.current()
	return enabled
}

func (p *persistentDeviceProtocol) current() (uint32, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.control.current()
}

func (p *persistentDeviceProtocol) queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue.mu.Lock()
	defer p.queue.mu.Unlock()
	return len(p.queue.frames)
}

// input serializes audio forwarding with pause, standby, and replacement.
func (p *persistentDeviceProtocol) input(generation uint64, send func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, active, enabled := p.control.current()
	if !enabled || !active || generation != p.providerGeneration {
		return errStaleAudioGeneration
	}
	if p.providerListenID != 0 && p.providerListenID != id {
		return errProviderGenerationAmbiguous
	}
	p.providerListenID = id
	return send()
}

func (p *persistentDeviceProtocol) outputID(generation uint64) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, active, enabled := p.control.current()
	if !enabled || !active || generation != p.providerGeneration || p.providerListenID != id {
		return 0
	}
	return id
}

func (p *persistentDeviceProtocol) hello(msg map[string]any) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A second hello cannot reset the serial number of this connection.
	if p.helloReceived {
		return nil
	}
	p.helloReceived = true
	enabled := p.control.negotiate(msg)
	if !enabled {
		return nil
	}
	return map[string]any{"persistent_idle": true}
}

func (p *persistentDeviceProtocol) start(msg map[string]any) error {
	return p.startWith(msg, nil)
}

func (p *persistentDeviceProtocol) startWith(msg map[string]any, onStart func()) error {
	id, err := listenIDFrom(msg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.providerListenID != 0 && p.providerListenID != id {
		return errProviderGenerationAmbiguous
	}
	if err := p.control.start(id); err != nil {
		return err
	}
	if err := p.queue.start(id); err != nil {
		return err
	}
	if onStart != nil {
		onStart()
	}
	return nil
}

func (p *persistentDeviceProtocol) pause(msg map[string]any) bool {
	id, err := listenIDFrom(msg)
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.control.end(id) {
		return false
	}
	p.queue.stop(id)
	return true
}

// standby sends only if the timer's generation is still current. It clears
// queued output before emitting the state transition under the same lock.
func (p *persistentDeviceProtocol) standby(id uint32, send func(map[string]any) error) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.control.end(id) {
		return false, nil
	}
	p.queue.stop(id)
	return true, send(map[string]any{"type": "standby", "listen_id": id})
}

func (p *persistentDeviceProtocol) heartbeat(send func(map[string]any) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _, enabled := p.control.current()
	if !enabled {
		return errors.New("persistent idle not negotiated")
	}
	return send(map[string]any{"type": "heartbeat"})
}

func (p *persistentDeviceProtocol) push(id uint32, frame []byte, end bool) error {
	return p.pushFor(1, id, frame, end)
}

func (p *persistentDeviceProtocol) pushFor(generation uint64, id uint32, frame []byte, end bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.providerGeneration || p.providerListenID != id {
		return errStaleAudioGeneration
	}
	return p.queue.push(id, frame, end)
}

func (p *persistentDeviceProtocol) tts(id uint32, state string, send func(map[string]any) error) error {
	return p.ttsFor(1, id, state, send)
}

func (p *persistentDeviceProtocol) ttsFor(generation uint64, id uint32, state string, send func(map[string]any) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.providerGeneration || p.providerListenID != id {
		return errStaleAudioGeneration
	}
	return p.queue.withGeneration(id, func() error {
		return send(map[string]any{"type": "tts", "state": state, "listen_id": id})
	})
}

func (p *persistentDeviceProtocol) sendNext(send func(persistentAudioFrame) error) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queue.sendNext(send)
}
