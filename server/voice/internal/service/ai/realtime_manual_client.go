package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// manualRealtimeConnection is stage 1 only: a separately dialled, verified
// manual-turn OpenAI WebSocket. No background delivery, device callbacks,
// RealtimeSession implementation, or production HandleWS path uses it yet.
// Exactly one owner may read conn after manualDialReady returns.
type manualRealtimeConnection struct {
	conn    *websocket.Conn
	runtime *manualRealtimeRuntime
}

func (s *manualRealtimeConnection) Close() {
	if s != nil && s.runtime != nil {
		s.runtime.close(false)
		return
	}
	if s != nil && s.conn != nil {
		_ = s.conn.Close()
	}
}

var errManualSessionAck = errors.New("manual Realtime session not acknowledged or has non-null VAD")

// manualDialReady sends one initial update and waits for its session.updated.
// It is deliberately a fresh connection: an ACK from an earlier update cannot
// be confused with ours. On any mismatch, timeout or cancellation it closes the
// socket; no secrets, audio, or complete server events are logged.
func manualDialReady(ctx context.Context, endpoint, apiKey, instructions, voice string, tools []map[string]any) (*manualRealtimeConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	header := http.Header{"Authorization": []string{"Bearer " + apiKey}}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close()
		}
	}()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return nil, err
	}
	update := openAIRealtimeSessionUpdate(instructions, voice, tools)
	session := update["session"].(map[string]any)
	audio := session["audio"].(map[string]any)
	input := audio["input"].(map[string]any)
	input["turn_detection"] = nil
	if err := conn.WriteJSON(update); err != nil {
		return nil, err
	}
	for {
		kind, raw, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if kind != websocket.TextMessage {
			return nil, errManualSessionAck
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			return nil, errManualSessionAck
		}
		switch event["type"] {
		case "session.created", "rate_limits.updated":
			continue
		case "session.updated":
			session, ok := event["session"].(map[string]any)
			if !ok {
				return nil, errManualSessionAck
			}
			audio, ok := session["audio"].(map[string]any)
			if !ok {
				return nil, errManualSessionAck
			}
			input, ok := audio["input"].(map[string]any)
			if !ok {
				return nil, errManualSessionAck
			}
			mode, present := input["turn_detection"]
			if !present || mode != nil {
				return nil, errManualSessionAck
			}
			// If cancellation raced the ACK, fail closed rather than return a
			// connection that a cancellation callback may concurrently close.
			if !stopCancel() || ctx.Err() != nil {
				return nil, errManualSessionAck
			}
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
				return nil, err
			}
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				return nil, err
			}
			keep = true
			return &manualRealtimeConnection{conn: conn}, nil
		default:
			return nil, errManualSessionAck
		}
	}
}
