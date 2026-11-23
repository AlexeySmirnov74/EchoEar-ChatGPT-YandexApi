package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManualDialReadyRequiresNullVADAck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    any
		present bool
		wantOK  bool
	}{
		{"null", nil, true, true},
		{"server VAD", map[string]any{"type": "server_vad"}, true, false},
		{"missing", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				var update map[string]any
				if conn.ReadJSON(&update) != nil {
					return
				}
				session := update["session"].(map[string]any)
				audio := session["audio"].(map[string]any)
				input := audio["input"].(map[string]any)
				if mode, present := input["turn_detection"]; !present || mode != nil {
					t.Error("client did not explicitly disable server VAD")
					return
				}
				if err := conn.WriteJSON(map[string]any{"type": "session.created"}); err != nil {
					return
				}
				ackInput := map[string]any{}
				if tc.present {
					ackInput["turn_detection"] = tc.mode
				}
				if err := conn.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"audio": map[string]any{"input": ackInput}}}); err != nil {
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			client, err := manualDialReady(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), "fake", "prompt", "voice", nil)
			if (err == nil) != tc.wantOK {
				t.Fatalf("got err=%v wantOK=%v", err, tc.wantOK)
			}
			if client != nil {
				client.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("server reader leaked")
			}
		})
	}
}

func TestManualDialReadyTimeoutAndErrorCloseSocket(t *testing.T) {
	for _, serverError := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "error"}[serverError], func(t *testing.T) {
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				c, e := upgrader.Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer c.Close()
				var v map[string]any
				if c.ReadJSON(&v) != nil {
					return
				}
				if serverError {
					_ = c.WriteJSON(map[string]any{"type": "error"})
				}
				_, _, _ = c.ReadMessage()
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			client, err := manualDialReady(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "fake", "prompt", "voice", nil)
			if err == nil || client != nil {
				t.Fatal("unconfirmed session accepted")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("connection not closed")
			}
		})
	}
}
