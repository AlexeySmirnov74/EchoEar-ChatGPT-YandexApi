package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/os/gcfg"
	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

type fakePersistentProvider struct {
	input     chan struct{}
	cancelled chan struct{}
	callbacks RealtimeCallbacks
}

func (f *fakePersistentProvider) AppendAudio([]int16) error {
	select {
	case f.input <- struct{}{}:
	default:
	}
	return nil
}
func (*fakePersistentProvider) CommitAudio() error { return nil }
func (f *fakePersistentProvider) CancelResponse() error {
	select {
	case f.cancelled <- struct{}{}:
	default:
	}
	return nil
}
func (*fakePersistentProvider) Close() {}

func fakeDeviceServer(t *testing.T) (*websocket.Conn, *fakePersistentProvider) {
	t.Helper()
	t.Setenv("STACKCHAN_DATA_DIR", t.TempDir())
	previous := g.Cfg().GetAdapter()
	t.Cleanup(func() { g.Cfg().SetAdapter(previous) })
	adapter, err := gcfg.NewAdapterContent(`{"ai":{"ha_enabled":false,"conversation_idle_seconds":1,"audio_prebuffer_ms":0}}`)
	if err != nil {
		t.Fatal(err)
	}
	g.Cfg().SetAdapter(adapter)
	fake := &fakePersistentProvider{input: make(chan struct{}, 20), cancelled: make(chan struct{}, 20)}
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleWSWithDial(w, r, func(_ context.Context, _ string, _ *haWSClient, cb RealtimeCallbacks) (RealtimeSession, error) {
			fake.callbacks = cb
			close(ready)
			return fake, nil
		})
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	<-ready
	return conn, fake
}

func deviceJSON(t *testing.T, c *websocket.Conn, msg map[string]any) {
	t.Helper()
	if err := c.WriteJSON(msg); err != nil {
		t.Fatal(err)
	}
}
func deviceReadJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	var msg map[string]any
	if err := c.ReadJSON(&msg); err != nil {
		t.Fatal(err)
	}
	return msg
}
func deviceAudio(t *testing.T, c *websocket.Conn) {
	t.Helper()
	e, err := opus.NewEncoder(deviceSampleRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, deviceFrameSamples)
	for i := range pcm {
		if i%2 == 0 {
			pcm[i] = 5000
		} else {
			pcm[i] = -5000
		}
	}
	buf := make([]byte, 4096)
	n, err := e.Encode(pcm, buf)
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte{0, 0, byte(n >> 8), byte(n)}, buf[:n]...)
	if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		t.Fatal(err)
	}
}
func persistentHello(t *testing.T, c *websocket.Conn) {
	t.Helper()
	deviceJSON(t, c, map[string]any{"type": "hello", "features": map[string]any{"persistent_idle": true}})
	msg := deviceReadJSON(t, c)
	features, _ := msg["features"].(map[string]any)
	if msg["type"] != "hello" || features["persistent_idle"] != true {
		t.Fatal(msg)
	}
}

func TestLegacyDeviceStillReceivesUntaggedAudio(t *testing.T) {
	c, f := fakeDeviceServer(t)
	deviceJSON(t, c, map[string]any{"type": "hello"})
	hello := deviceReadJSON(t, c)
	if _, ok := hello["features"]; ok {
		t.Fatal("legacy hello advertised persistent mode", hello)
	}
	deviceJSON(t, c, map[string]any{"type": "listen", "state": "start", "mode": "auto"})
	deviceAudio(t, c)
	select {
	case <-f.input:
	case <-time.After(time.Second):
		t.Fatal("legacy microphone audio missing")
	}
	f.callbacks.OnStart()
	f.callbacks.OnAudio(make([]int16, serverFrameSamples))
	f.callbacks.OnStop()
	// Legacy mode sends its original llm emotion before the TTS start.
	msg := deviceReadJSON(t, c)
	if msg["type"] != "llm" {
		t.Fatal(msg)
	}
	msg = deviceReadJSON(t, c)
	if msg["type"] != "tts" || msg["state"] != "start" {
		t.Fatal(msg)
	}
	if _, ok := msg["listen_id"]; ok {
		t.Fatal("legacy TTS unexpectedly tagged")
	}
	typ, _, err := c.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage {
		t.Fatal(typ, err)
	}
	msg = deviceReadJSON(t, c)
	if msg["type"] != "tts" || msg["state"] != "stop" {
		t.Fatal(msg)
	}
}

func TestPersistentListenIDJSONIsNumeric(t *testing.T) {
	var msg map[string]any
	if err := json.Unmarshal([]byte(`{"listen_id":4294967295}`), &msg); err != nil {
		t.Fatal(err)
	}
	if id, err := listenIDFrom(msg); err != nil || id != ^uint32(0) {
		t.Fatal(id, err)
	}
}
