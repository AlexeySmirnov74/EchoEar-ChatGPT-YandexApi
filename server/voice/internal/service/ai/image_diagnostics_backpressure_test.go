package ai

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestImageDiagnosticBlockedFilesystemDoesNotBlockActor(t *testing.T) {
	// Simulate a filesystem call that never completes until released. One
	// further event occupies the entire queue; later records must be dropped.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	dispatcher := newImageDiagnosticDispatcher(1, func(imageDiagnosticEntry) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	old := imageDiagnosticDispatcherCurrent.Swap(dispatcher)
	defer func() {
		imageDiagnosticDispatcherCurrent.Store(old)
		close(release)
		dispatcher.stop()
	}()
	recordImageEvent(imageSearchStarted, 1, 1, 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	recordImageEvent(imageSearchStarted, 1, 2, 0)
	if len(dispatcher.entries) != 1 {
		t.Fatal("queue not full")
	}

	m := &imageActorManager{fakeActorManager: &fakeActorManager{
		states: make(chan manualProviderState, 4), events: make(chan manualRealtimeEvent, 16),
		failures: make(chan uint64, 1), started: make(chan uint32, 4),
	}, results: make(chan string, 2)}
	queried := make(chan struct{}, 1)
	fetch := imageSearchFetcher(func(context.Context, string) ([]byte, error) {
		queried <- struct{}{}
		return bytes.Repeat([]byte{0x00, 0xf8}, imageFrameBytes/2), nil
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
	case <-queried:
	case <-time.After(time.Second):
		t.Fatal("blocked diagnostics stalled image search")
	}
	select {
	case <-m.results:
	case <-time.After(time.Second):
		t.Fatal("blocked diagnostics stalled tool acknowledgement")
	}
	m.events <- manualRealtimeEvent{Kind: "done", Generation: 1, ListenID: 1, ResponseID: "answer", Final: true}
	if err := d.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		kind, data, err := d.ReadMessage()
		if err != nil {
			t.Fatal("blocked diagnostics stalled actor: ", err)
		}
		if kind == websocket.TextMessage && bytes.Contains(data, []byte(`"type":"image_preview"`)) && bytes.Contains(data, []byte(`"op":"begin"`)) {
			return
		}
	}
}
