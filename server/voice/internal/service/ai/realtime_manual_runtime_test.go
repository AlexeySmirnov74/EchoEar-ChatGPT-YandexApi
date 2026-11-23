package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func fakeManualRuntime(t *testing.T, scenario func(*websocket.Conn)) *manualRealtimeConnection {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var update map[string]any
		if c.ReadJSON(&update) != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"audio": map[string]any{"input": map[string]any{"turn_detection": nil}}}})
		scenario(c)
	}))
	t.Cleanup(server.Close)
	s, err := manualDialReady(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), "fake", "prompt", "voice", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func fakeRead(t *testing.T, c *websocket.Conn, typ string) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var v map[string]any
	if err := c.ReadJSON(&v); err != nil {
		t.Errorf("read %s: %v", typ, err)
		return nil
	}
	if v["type"] != typ {
		t.Errorf("got %v want %s", v["type"], typ)
	}
	return v
}

func TestManualRuntimeActualClientToolContinuation(t *testing.T) {
	scenarioDone := make(chan struct{})
	s := fakeManualRuntime(t, func(c *websocket.Conn) {
		defer close(scenarioDone)
		appendEvent := fakeRead(t, c, "input_audio_buffer.append")
		audio, _ := appendEvent["audio"].(string)
		pcm, err := base64.StdEncoding.DecodeString(audio)
		if err != nil || len(pcm) == 0 || len(pcm)%2 != 0 {
			t.Error("invalid PCM append")
			return
		}
		_ = fakeRead(t, c, "input_audio_buffer.commit")
		_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item1"})
		first := fakeRead(t, c, "response.create")
		response := first["response"].(map[string]any)
		meta := response["metadata"].(map[string]any)
		if meta["listen_id"] != "7" {
			t.Error("listen id must be string")
		}
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp1", "metadata": meta}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_item.done", "response_id": "resp1", "item": map[string]any{"type": "function_call", "status": "completed", "call_id": "call1", "name": "set_speaker_volume", "arguments": "{\"volume\":40}"}})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp1", "status": "completed"}})
		_ = fakeRead(t, c, "conversation.item.create")
		next := fakeRead(t, c, "response.create")
		nextMeta := next["response"].(map[string]any)["metadata"].(map[string]any)
		if nextMeta["listen_id"] != "7" || nextMeta["nonce"] == meta["nonce"] {
			t.Error("bad continuation metadata")
		}
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp2", "metadata": nextMeta}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "resp2", "delta": base64.StdEncoding.EncodeToString([]byte{1, 0, 2, 0})})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.done", "response_id": "resp2"})
		_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp2", "status": "completed"}})
		_, _, _ = c.ReadMessage()
	})
	if err := s.startManualRuntime(9, 10); err != nil {
		t.Fatal(err)
	}
	r := s.runtime
	if err := r.Begin(7); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(7, make([]int16, deviceFrameSamples)); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(7); err != nil {
		t.Fatal(err)
	}
	get := func(kind, response string, final bool) {
		t.Helper()
		select {
		case e := <-r.events:
			if e.Kind != kind || e.Generation != 9 || e.ListenID != 7 || e.ResponseID != response || e.Final != final {
				t.Fatalf("unexpected event %+v", e)
			}
			if kind == "tool" && (e.CallID != "call1" || e.ToolName != "set_speaker_volume" || e.Args["volume"] != float64(40)) {
				t.Fatal("bad tool", e)
			}
			if kind == "audio" && (len(e.PCM) != 2 || e.PCM[0] != 1 || e.PCM[1] != 2) {
				t.Fatal("bad PCM", e.PCM)
			}
		case <-r.failures:
			t.Fatal("runtime failed")
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for %s", kind)
		}
	}
	get("tool", "resp1", false)
	get("done", "resp1", false)
	if err := r.ToolResult(7, "resp1", "call1", `{"success":true}`); err != nil {
		t.Fatal(err)
	}
	get("audio", "resp2", false)
	get("audio_done", "resp2", false)
	get("done", "resp2", true)
	if err := r.FinishPlayback(7, "resp2", func(id uint32) error {
		if id != 7 {
			t.Fatal(id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Begin(8); err != nil {
		t.Fatal("provider not reusable", err)
	}
	s.Close()
	select {
	case <-scenarioDone:
	case <-time.After(time.Second):
		t.Fatal("fake server did not stop")
	}
}

func TestManualRuntimeMissingCommitAckCloses(t *testing.T) {
	s := fakeManualRuntime(t, func(c *websocket.Conn) {
		_ = fakeRead(t, c, "input_audio_buffer.append")
		_ = fakeRead(t, c, "input_audio_buffer.commit")
		_, _, _ = c.ReadMessage()
	})
	if err := s.startManualRuntime(5, 8); err != nil {
		t.Fatal(err)
	}
	r := s.runtime
	r.mu.Lock()
	r.ackTimeout = 80 * time.Millisecond
	r.mu.Unlock()
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(1, []int16{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(1); err != nil {
		t.Fatal(err)
	}
	select {
	case gen := <-r.failures:
		if gen != 5 {
			t.Fatal(gen)
		}
	case <-time.After(time.Second):
		t.Fatal("missing commit ACK did not close")
	}
}

func TestParseManualResponseRejectsUntrustedMetadata(t *testing.T) {
	for _, raw := range []string{`{"response":{"id":"r","metadata":{"listen_id":1,"nonce":"n"}}}`, `{"response":{"id":"r","metadata":{"listen_id":"01","nonce":"n"}}}`, `{"response":{"id":"r","metadata":{"listen_id":"4294967296","nonce":"n"}}}`} {
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := parseManualResponse(v); err == nil {
			t.Fatal("accepted invalid metadata", raw)
		}
	}
}

func TestManualRuntimeQueueFullFailsWithoutBlocking(t *testing.T) {
	s := fakeManualRuntime(t, func(c *websocket.Conn) {
		_ = fakeRead(t, c, "input_audio_buffer.append")
		_ = fakeRead(t, c, "input_audio_buffer.commit")
		_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item1"})
		request := fakeRead(t, c, "response.create")
		metadata := request["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp1", "metadata": metadata}})
		for i := 0; i < 2; i++ {
			_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "resp1", "delta": "AAA="})
		}
		_, _, _ = c.ReadMessage()
	})
	if err := s.startManualRuntime(3, 1); err != nil {
		t.Fatal(err)
	}
	r := s.runtime
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(1, []int16{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(1); err != nil {
		t.Fatal(err)
	}
	select {
	case gen := <-r.failures:
		if gen != 3 {
			t.Fatal(gen)
		}
	case <-time.After(time.Second):
		t.Fatal("full event queue blocked runtime")
	}
	if err := r.Begin(2); err == nil {
		t.Fatal("closed provider accepted new turn")
	}
}

func TestManualRuntimeRejectsOldResponseAudio(t *testing.T) {
	s := fakeManualRuntime(t, func(c *websocket.Conn) {
		_ = fakeRead(t, c, "input_audio_buffer.append")
		_ = fakeRead(t, c, "input_audio_buffer.commit")
		_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item1"})
		request := fakeRead(t, c, "response.create")
		metadata := request["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp1", "metadata": metadata}})
		_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": "stale", "delta": "AAA="})
		_, _, _ = c.ReadMessage()
	})
	if err := s.startManualRuntime(4, 2); err != nil {
		t.Fatal(err)
	}
	r := s.runtime
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	if err := r.Append(1, []int16{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(1); err != nil {
		t.Fatal(err)
	}
	select {
	case gen := <-r.failures:
		if gen != 4 {
			t.Fatal(gen)
		}
	case <-time.After(time.Second):
		t.Fatal("stale audio did not close")
	}
	select {
	case e := <-r.events:
		t.Fatal("stale audio delivered", e)
	default:
	}
}
