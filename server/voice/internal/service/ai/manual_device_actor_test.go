package ai

import (
	"context"
	"encoding/base64"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

func TestManualDeviceActorWithRealManualClientAndFakeProvider(t *testing.T) {
	providerDone := make(chan struct{})
	manual := fakeManualRuntime(t, func(c *websocket.Conn) {
		defer close(providerDone)
		for {
			var msg map[string]any
			if c.ReadJSON(&msg) != nil {
				return
			}
			if msg["type"] == "input_audio_buffer.commit" {
				break
			}
			if msg["type"] != "input_audio_buffer.append" {
				t.Error("unexpected input event", msg["type"])
				return
			}
		}
		_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item1"})
		first := fakeRead(t, c, "response.create")
		meta := first["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp1", "metadata": meta}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_item.done", "response_id": "resp1", "item": map[string]any{"type": "function_call", "status": "completed", "call_id": "call1", "name": "set_speaker_volume", "arguments": "{\"volume\":40}"}})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp1", "status": "completed"}})
		_ = fakeRead(t, c, "conversation.item.create")
		next := fakeRead(t, c, "response.create")
		meta2 := next["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp2", "metadata": meta2}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "resp2", "delta": base64.StdEncoding.EncodeToString(make([]byte, serverFrameSamples*2))})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.done", "response_id": "resp2"})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp2", "status": "completed"}})
		for {
			var msg map[string]any
			if c.ReadJSON(&msg) != nil {
				return
			}
			if msg["type"] == "input_audio_buffer.commit" {
				break
			}
			if msg["type"] != "input_audio_buffer.append" {
				t.Error("unexpected follow-up event", msg["type"])
				return
			}
		}
		_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item2"})
		third := fakeRead(t, c, "response.create")
		meta3 := third["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp3", "metadata": meta3}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_item.done", "response_id": "resp3", "item": map[string]any{"type": "function_call", "status": "completed", "call_id": "call2", "name": "set_speaker_volume", "arguments": "{\"volume\":50}"}})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp3", "status": "completed"}})
		_ = fakeRead(t, c, "conversation.item.create")
		fourth := fakeRead(t, c, "response.create")
		meta4 := fourth["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp4", "metadata": meta4}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "resp4", "delta": base64.StdEncoding.EncodeToString(make([]byte, serverFrameSamples*2))})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.done", "response_id": "resp4"})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp4", "status": "completed"}})
		_, _, _ = c.ReadMessage()
	})
	actorDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(actorDone)
		handleExperimentalManualWS(w, r, func(_ context.Context, _ string) (*manualRealtimeConnection, error) { return manual, nil })
	}))
	defer server.Close()
	device, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	deviceJSON(t, device, map[string]any{"type": "hello", "features": map[string]any{"persistent_idle": true}})
	hello := deviceReadJSON(t, device)
	sessionID, _ := hello["session_id"].(string)
	if sessionID == "" {
		t.Fatal(hello)
	}
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "mode": "auto", "listen_id": 1})
	sendSpeech := func() {
		t.Helper()
		enc, e := opus.NewEncoder(deviceSampleRate, 1, opus.AppVoIP)
		if e != nil {
			t.Fatal(e)
		}
		for frame := 0; frame < 22; frame++ {
			pcm := make([]int16, deviceFrameSamples)
			if frame < 4 {
				for i := range pcm {
					pcm[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))
				}
			}
			buf := make([]byte, 4096)
			n, e := enc.Encode(pcm, buf)
			if e != nil {
				t.Fatal(e)
			}
			if e = device.WriteMessage(websocket.BinaryMessage, append([]byte{0, 0, byte(n >> 8), byte(n)}, buf[:n]...)); e != nil {
				t.Fatal(e)
			}
		}
	}
	sendSpeech()
	mcp := deviceReadJSON(t, device)
	if mcp["type"] != "mcp" || mcp["session_id"] != sessionID {
		t.Fatal(mcp)
	}
	payload := mcp["payload"].(map[string]any)
	deviceJSON(t, device, map[string]any{"type": "mcp", "session_id": sessionID, "payload": map[string]any{"jsonrpc": "2.0", "id": payload["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "true"}}}}})
	start := deviceReadJSON(t, device)
	if start["type"] != "tts" || start["state"] != "start" || start["listen_id"] != float64(1) {
		t.Fatal(start)
	}
	typ, packet, err := device.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || len(packet) < 5 {
		t.Fatal(typ, err)
	}
	if _, err := decodeFramesToPCM([][]byte{packet[4:]}); err != nil {
		t.Fatal(err)
	}
	stop := deviceReadJSON(t, device)
	if stop["type"] != "tts" || stop["state"] != "stop" || stop["listen_id"] != float64(1) {
		t.Fatal(stop)
	}
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "mode": "auto", "listen_id": 2})
	sendSpeech()
	mcp2 := deviceReadJSON(t, device)
	if mcp2["type"] != "mcp" {
		t.Fatal(mcp2)
	}
	payload2 := mcp2["payload"].(map[string]any)
	if payload2["id"] == payload["id"] {
		t.Fatal("MCP request ID was reused")
	}
	// A late, unsuccessful reply to the first request must not terminate the second.
	deviceJSON(t, device, map[string]any{"type": "mcp", "session_id": sessionID, "payload": map[string]any{"jsonrpc": "2.0", "id": payload["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "false"}}}}})
	deviceJSON(t, device, map[string]any{"type": "mcp", "session_id": sessionID, "payload": map[string]any{"jsonrpc": "2.0", "id": payload2["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "true"}}}}})
	start = deviceReadJSON(t, device)
	if start["type"] != "tts" || start["state"] != "start" || start["listen_id"] != float64(2) {
		t.Fatal(start)
	}
	typ, packet, err = device.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || len(packet) < 5 {
		t.Fatal(typ, err)
	}
	stop = deviceReadJSON(t, device)
	if stop["type"] != "tts" || stop["state"] != "stop" || stop["listen_id"] != float64(2) {
		t.Fatal(stop)
	}
	device.Close()
	select {
	case <-actorDone:
	case <-time.After(time.Second):
		t.Fatal("actor leaked")
	}
	select {
	case <-providerDone:
	case <-time.After(time.Second):
		t.Fatal("provider leaked")
	}
}

func TestManualDeviceActorPauseFailsClosed(t *testing.T) {
	providerDone := make(chan struct{})
	manual := fakeManualRuntime(t, func(c *websocket.Conn) { defer close(providerDone); _, _, _ = c.ReadMessage() })
	actorDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(actorDone)
		handleExperimentalManualWS(w, r, func(context.Context, string) (*manualRealtimeConnection, error) { return manual, nil })
	}))
	defer server.Close()
	device, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	deviceJSON(t, device, map[string]any{"type": "hello", "features": map[string]any{"persistent_idle": true}})
	_ = deviceReadJSON(t, device)
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "listen_id": 1})
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "pause", "listen_id": 1})
	standby := deviceReadJSON(t, device)
	if standby["type"] != "standby" || standby["listen_id"] != float64(1) {
		t.Fatal(standby)
	}
	select {
	case <-actorDone:
		t.Fatal("pause closed the device socket")
	default:
	}
	device.Close()
	select {
	case <-actorDone:
	case <-time.After(time.Second):
		t.Fatal("actor leaked")
	}
	select {
	case <-providerDone:
	case <-time.After(time.Second):
		t.Fatal("provider leaked")
	}
}
