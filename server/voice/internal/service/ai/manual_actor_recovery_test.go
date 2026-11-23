package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeActorManager struct {
	mu       sync.Mutex
	gen      uint64
	id       uint32
	states   chan manualProviderState
	events   chan manualRealtimeEvent
	failures chan uint64
	started  chan uint32
	committed chan struct{}
}

func (m *fakeActorManager) Begin(id uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen == 0 {
		return errManualGuard
	}
	m.id = id
	m.started <- id
	return nil
}
func (*fakeActorManager) Append(uint32, []int16) error                    { return nil }
func (m *fakeActorManager) Commit(uint32) error {
	if m.committed != nil { m.committed <- struct{}{} }
	return nil
}
func (*fakeActorManager) Clear(uint32) error                              { return nil }
func (*fakeActorManager) ToolResult(uint32, string, string, string) error { return nil }
func (*fakeActorManager) NotifyImageResult(uint32, string) error { return nil }
func (m *fakeActorManager) CancelImageNotice(id uint32) error {m.events<-manualRealtimeEvent{Kind:"notice_cancelled",Generation:1,ListenID:id,ResponseID:"notice"};return nil}
func (*fakeActorManager) FinishImageNoticeCancel(id uint32,_ string,stop func(uint32)error,_ bool)error{return stop(id)}
func (m *fakeActorManager) FinishPlayback(id uint32, _ string, send func(uint32) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != m.id || m.gen == 0 {
		return errManualGuard
	}
	return send(id)
}
func (m *fakeActorManager) DeviceWrite(gen uint64, id uint32, _ string, send func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen != m.gen || id != m.id || gen == 0 {
		return errManualGuard
	}
	return send()
}
func (*fakeActorManager) Standby(uint32) error                 { return nil }
func (*fakeActorManager) EndTurnAndWait(context.Context) error { return nil }
func (m *fakeActorManager) Invalidate(gen uint64) {
	m.mu.Lock()
	if m.gen == gen {
		m.gen = 0
	}
	m.mu.Unlock()
}
func (m *fakeActorManager) GenerationEvents() <-chan manualProviderState { return m.states }
func (m *fakeActorManager) Events() <-chan manualRealtimeEvent           { return m.events }
func (m *fakeActorManager) Failures() <-chan uint64                      { return m.failures }
func (m *fakeActorManager) ready(gen uint64) {
	m.mu.Lock()
	m.gen = gen
	m.mu.Unlock()
	m.states <- manualProviderState{Generation: gen, Ready: true}
}
func (m *fakeActorManager) lost(gen uint64) {
	m.mu.Lock()
	if m.gen == gen {
		m.gen = 0
	}
	m.mu.Unlock()
	m.states <- manualProviderState{Generation: gen}
}

func TestManualActorKeepsDeviceSocketAcrossProviderRecovery(t *testing.T) {
	m := &fakeActorManager{states: make(chan manualProviderState), events: make(chan manualRealtimeEvent, 8), failures: make(chan uint64, 1), started: make(chan uint32, 3)}
	actorDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(actorDone)
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		runExperimentalManualActor(r.Context(), c, m, "session")
	}))
	defer server.Close()
	device, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	m.ready(1)
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "listen_id": 1})
	select {
	case id := <-m.started:
		if id != 1 {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("first start")
	}
	m.lost(1)
	standby := deviceReadJSON(t, device)
	if standby["type"] != "standby" || standby["listen_id"] != float64(1) {
		t.Fatal(standby)
	}
	m.ready(2)
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "listen_id": 2})
	select {
	case id := <-m.started:
		if id != 2 {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("second start")
	}
	// A delayed old-generation event must not affect the new stream.
	m.events <- manualRealtimeEvent{Kind: "audio", Generation: 1, ListenID: 2, ResponseID: "old", PCM: make([]int16, serverFrameSamples)}
	m.events <- manualRealtimeEvent{Kind: "audio", Generation: 2, ListenID: 2, ResponseID: "new", PCM: make([]int16, serverFrameSamples)}
	m.events <- manualRealtimeEvent{Kind: "audio_done", Generation: 2, ListenID: 2, ResponseID: "new"}
	m.events <- manualRealtimeEvent{Kind: "done", Generation: 2, ListenID: 2, ResponseID: "new", Final: true}
	start := deviceReadJSON(t, device)
	if start["type"] != "tts" || start["state"] != "start" || start["listen_id"] != float64(2) {
		t.Fatal(start)
	}
	typ, _, err := device.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage {
		t.Fatal(typ, err)
	}
	stop := deviceReadJSON(t, device)
	if stop["type"] != "tts" || stop["state"] != "stop" || stop["listen_id"] != float64(2) {
		t.Fatal(stop)
	}
	device.Close()
	select {
	case <-actorDone:
	case <-time.After(time.Second):
		t.Fatal("device actor leaked")
	}
}
