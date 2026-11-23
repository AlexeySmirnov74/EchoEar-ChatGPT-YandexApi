package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSpeakerMCPCallsAndMatching(t *testing.T) {
	requests := make(chan map[string]any, 2)
	b := newDeviceMCPBridge(func(v map[string]any) error { requests <- v; return nil }, "session-1")
	done := make(chan string, 1)
	go func() {
		result, err := b.call(context.Background(), "set_speaker_volume", map[string]any{"volume": float64(35)})
		if err != nil {
			done <- "error: " + err.Error()
		} else {
			done <- result
		}
	}()
	msg := <-requests
	p := msg["payload"].(map[string]any)
	args := p["params"].(map[string]any)
	if msg["type"] != "mcp" || msg["session_id"] != "session-1" || p["method"] != "tools/call" || args["name"] != "self.audio_speaker.set_volume" || args["arguments"].(map[string]any)["volume"] != 35 {
		t.Fatal("wrong command", msg)
	}
	id := float64(p["id"].(int))
	b.handleReply(map[string]any{"payload": map[string]any{"jsonrpc": "2.0", "id": id + 1, "result": true}})
	select {
	case <-done:
		t.Fatal("accepted mismatched id")
	default:
	}
	b.handleReply(map[string]any{"payload": map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "true"}}}}})
	select {
	case result := <-done:
		if !strings.Contains(result, "true") {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("no matching result")
	}
}

func TestSpeakerMCPValidationErrorsAndCancellation(t *testing.T) {
	called := false
	b := newDeviceMCPBridge(func(v map[string]any) error { called = true; return nil }, "s")
	for _, args := range []map[string]any{{}, {"volume": -1.0}, {"volume": 101.0}, {"volume": 5.5}, {"volume": "50"}, {"volume": 50.0, "other": true}} {
		if _, err := b.call(context.Background(), "set_speaker_volume", args); err == nil {
			t.Fatalf("accepted %#v", args)
		}
	}
	if called {
		t.Fatal("invalid request reached device")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.call(ctx, "get_speaker_volume", map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if len(b.pending) != 0 {
		t.Fatal("pending request leaked")
	}
}

func TestSpeakerMCPErrorReply(t *testing.T) {
	requests := make(chan map[string]any, 1)
	b := newDeviceMCPBridge(func(v map[string]any) error { requests <- v; return nil }, "s")
	done := make(chan error, 1)
	go func() { _, err := b.call(context.Background(), "get_speaker_volume", map[string]any{}); done <- err }()
	p := (<-requests)["payload"].(map[string]any)
	b.handleReply(map[string]any{"payload": map[string]any{"jsonrpc": "2.0", "id": float64(p["id"].(int)), "error": map[string]any{"message": "unknown tool"}}})
	if err := <-done; err == nil {
		t.Fatal("device rejection was treated as success")
	}
}

func TestSpeakerRealtimeToolsHaveValidNamesAndSchemas(t *testing.T) {
	tools := speakerRealtimeTools()
	if len(tools) != 7 {
		t.Fatal("expected seven tools")
	}
	for _, tool := range tools {
		name := tool["name"].(string)
		if !isSpeakerTool(name) || strings.Contains(name, ".") {
			t.Fatal("invalid OpenAI function name", name)
		}
		if _, err := json.Marshal(tool); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSpeakerMCPStatusAndJSONWire(t *testing.T) {
	requests := make(chan []byte, 1)
	b := newDeviceMCPBridge(func(v map[string]any) error {
		data, err := json.Marshal(v)
		if err == nil {
			requests <- data
		}
		return err
	}, "s")
	done := make(chan string, 1)
	go func() {
		v, err := b.call(context.Background(), "get_speaker_volume", map[string]any{})
		if err != nil {
			done <- err.Error()
		} else {
			done <- v
		}
	}()
	var request map[string]any
	if err := json.Unmarshal(<-requests, &request); err != nil {
		t.Fatal(err)
	}
	p := request["payload"].(map[string]any)
	if p["params"].(map[string]any)["name"] != "self.get_device_status" {
		t.Fatal(request)
	}
	reply := map[string]any{"type": "mcp", "payload": map[string]any{"jsonrpc": "2.0", "id": p["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"audio_speaker\":{\"volume\":55},\"network\":{\"ssid\":\"private\"}}"}}}}}
	encoded, _ := json.Marshal(reply)
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	b.handleReply(decoded)
	if got := <-done; !strings.Contains(got, "55") || strings.Contains(got, "private") {
		t.Fatal(got)
	}
}

func TestRealtimeVADValuesAndSessionConfig(t *testing.T) {
	for _, tc := range []struct {
		threshold, silence string
		wantThreshold      float64
		wantSilence        int
	}{
		{"0.5", "300", 0.5, 300}, {"0.35", "600", 0.35, 600}, {"NaN", "999999", 0.5, 300}, {"-0.1", "bad", 0.5, 300}, {"1.1", "50", 0.5, 300},
	} {
		threshold, silence := realtimeVADValues(tc.threshold, tc.silence)
		if threshold != tc.wantThreshold || silence != tc.wantSilence {
			t.Fatalf("%s/%s got %v/%d", tc.threshold, tc.silence, threshold, silence)
		}
		msg := openAIRealtimeSessionUpdateWithVAD("", "alloy", nil, threshold, silence)
		session := msg["session"].(map[string]any)
		audio := session["audio"].(map[string]any)
		input := audio["input"].(map[string]any)
		vad := input["turn_detection"].(map[string]any)
		if vad["threshold"] != tc.wantThreshold || vad["silence_duration_ms"] != tc.wantSilence {
			t.Fatal(vad)
		}
	}
}

func TestSpeakerRealtimeToolCallWaitsForDeviceAndResponseCompletion(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			serverConn <- c
		}
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	model := <-serverConn
	defer model.Close()
	deviceRequests := make(chan map[string]any, 1)
	bridge := newDeviceMCPBridge(func(m map[string]any) error { deviceRequests <- m; return nil }, "test-session")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &openaiRealtimeSession{conn: client, logCtx: ctx, cb: RealtimeCallbacks{OnDeviceToolCall: bridge.call}}
	go s.readLoop(ctx)
	if err := model.WriteJSON(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "name": "set_speaker_volume", "call_id": "call-123", "arguments": `{"volume":42}`}}); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	select {
	case req = <-deviceRequests:
	case <-time.After(time.Second):
		t.Fatal("device command not sent")
	}
	payload := req["payload"].(map[string]any)
	bridge.handleReply(map[string]any{"payload": map[string]any{"jsonrpc": "2.0", "id": float64(payload["id"].(int)), "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "true"}}}}})
	var result map[string]any
	if err := model.ReadJSON(&result); err != nil {
		t.Fatal(err)
	}
	item, ok := result["item"].(map[string]any)
	if result["type"] != "conversation.item.create" || !ok || item["call_id"] != "call-123" || !strings.Contains(item["output"].(string), "true") {
		t.Fatal(result)
	}
	got := make(chan map[string]any, 1)
	go func() {
		var event map[string]any
		if model.ReadJSON(&event) == nil {
			got <- event
		}
	}()
	select {
	case <-got:
		t.Fatal("response requested before previous response completed")
	case <-time.After(50 * time.Millisecond):
	}
	if err := model.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp-1", "status": "completed"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-got:
		if event["type"] != "response.create" {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("model was not asked to continue")
	}
}

func TestSpeakerMCPThroughDeviceWebsocket(t *testing.T) {
	serverSession := make(chan *wsSession, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s := &wsSession{conn: conn, sessionID: "test-device-session"}
		s.mcp = newDeviceMCPBridge(func(v map[string]any) error { return s.sendJSON(v) }, s.sessionID)
		serverSession <- s
		s.run(ctx)
	}))
	defer server.Close()
	device, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	s := <-serverSession
	finished := make(chan error, 1)
	go func() {
		_, err := s.mcp.call(ctx, "set_speaker_volume", map[string]any{"volume": float64(25)})
		finished <- err
	}()
	var request map[string]any
	if err := device.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	payload := request["payload"].(map[string]any)
	if request["session_id"] != "test-device-session" || payload["params"].(map[string]any)["name"] != "self.audio_speaker.set_volume" {
		t.Fatal(request)
	}
	reply := map[string]any{"type": "mcp", "payload": map[string]any{"jsonrpc": "2.0", "id": payload["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "true"}}, "isError": false}}}
	if err := device.WriteJSON(reply); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("device response was not delivered")
	}
}
