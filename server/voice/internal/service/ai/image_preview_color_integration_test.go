package ai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Exercise the real show_image actor path, not just the conversion helper.
// The mock search service uses the documented little-endian contract; the
// device WebSocket must receive big-endian display bytes and their checksum.
func TestImageSearchSendsDisplayByteOrderToDevice(t *testing.T) {
	input := bytes.Repeat([]byte{0x00, 0xf8, 0xe0, 0x07, 0x1f, 0x00}, imageFrameBytes/6)
	expected := bytes.Repeat([]byte{0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f}, imageFrameBytes/6)
	m := &imageActorManager{fakeActorManager: &fakeActorManager{
		states: make(chan manualProviderState, 4), events: make(chan manualRealtimeEvent, 16),
		failures: make(chan uint64, 1), started: make(chan uint32, 4),
	}, results: make(chan string, 2)}
	queried := make(chan string, 1)
	fetch := imageSearchFetcher(func(_ context.Context, query string) ([]byte, error) {
		queried <- query
		return bytes.Clone(input), nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		ctx := context.WithValue(r.Context(), imagePreviewContextKey{}, true)
		ctx = context.WithValue(ctx, imageSearchFetcherKey{}, fetch)
		runExperimentalManualActor(ctx, c, m, "session")
	}))
	defer server.Close()
	d, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m.ready(1)
	deviceJSON(t, d, map[string]any{"type": "listen", "state": "start", "listen_id": 1})
	select {
	case <-m.started:
	case <-time.After(time.Second):
		t.Fatal("no listen")
	}
	m.events <- manualRealtimeEvent{Kind: "tool", Generation: 1, ListenID: 1, ResponseID: "resp", CallID: "call", ToolName: "show_image", Args: map[string]any{"query": "rabbit"}}
	m.events <- manualRealtimeEvent{Kind: "done", Generation: 1, ListenID: 1, ResponseID: "resp", Final: false}
	select {
	case result := <-m.results:
		if !strings.Contains(result, `"accepted":true`) {
			t.Fatal(result)
		}
	case <-time.After(time.Second):
		t.Fatal("no tool acknowledgement")
	}
	select {
	case query := <-queried:
		if query != "rabbit" {
			t.Fatal("incorrect search query")
		}
	case <-time.After(time.Second):
		t.Fatal("no search")
	}
	m.events <- manualRealtimeEvent{Kind: "done", Generation: 1, ListenID: 1, ResponseID: "answer", Final: true}
	if err := d.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var sha string
	var received []byte
	seq := 0
	for {
		kind, data, err := d.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg["type"] != "image_preview" {
			continue
		}
		switch msg["op"] {
		case "begin":
			if sha != "" {
				t.Fatal("duplicate image begin")
			}
			sha, _ = msg["sha256"].(string)
			if sha == "" || msg["bytes"] != float64(imageFrameBytes) {
				t.Fatal("invalid image header")
			}
		case "chunk":
			if sha == "" || msg["seq"] != float64(seq) {
				t.Fatal("incorrect chunk sequence")
			}
			part, err := base64.StdEncoding.DecodeString(msg["data"].(string))
			if err != nil {
				t.Fatal(err)
			}
			received = append(received, part...)
			seq++
		case "end":
			if !bytes.Equal(received, expected) {
				t.Fatal("device received wrong RGB565 byte order")
			}
			sum := sha256.Sum256(received)
			if sha != hex.EncodeToString(sum[:]) {
				t.Fatal("checksum does not match transmitted pixels")
			}
			return
		default:
			t.Fatal("unexpected image operation")
		}
	}
}
