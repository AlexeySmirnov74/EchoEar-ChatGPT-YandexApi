package ai

import (
	"errors"
	"sync"
)

// realtimeTurnGuard is an isolated, fail-closed control-plane prototype. It is
// not connected to a provider. It assumes precisely one ordered OpenAI WS read
// stream and a serialized client write stream. A caller must enforce deadlines,
// install server_vad with both automatic response and interruption disabled,
// and close the provider/device pair on every error returned by this type.
// Never execute tool calls or emit output solely on the basis of a check made
// before releasing the guard's lock: see authorizeOutput/authorizeTool below.
type realtimeTurnGuard struct {
	mu               sync.Mutex
	listenID         uint32
	active           bool
	closed           bool
	clearPending     bool
	cancelPending    bool
	speechItem       string
	stoppedItem      string
	committedItem    string
	requestNonce     string
	responseID       string
	responseDone     bool
	playbackComplete bool
	dirtyContext     bool
	hadAudio         bool
	toolCallID       string
	seenResponses    map[string]struct{}
}

var errRealtimeGuard = errors.New("ambiguous or invalid realtime event; close provider")

func (g *realtimeTurnGuard) fail() error { g.closed = true; return errRealtimeGuard }

// begin must follow a completed clear barrier when changing an ID. New audio
// must not be forwarded while the barrier is in flight.
func (g *realtimeTurnGuard) begin(id uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || id == 0 || g.active || g.clearPending || g.cancelPending || g.dirtyContext || g.toolCallID != "" || (g.listenID != 0 && int32(id-g.listenID) <= 0) {
		return g.fail()
	}
	g.listenID, g.active = id, true
	g.speechItem, g.stoppedItem, g.committedItem = "", "", ""
	g.requestNonce, g.responseID, g.responseDone, g.playbackComplete = "", "", false, false
	return nil
}

// input serializes appends with pause/clear and prevents traffic while idle.
func (g *realtimeTurnGuard) input(id uint32, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.listenID || g.clearPending || g.cancelPending {
		return g.fail()
	}
	if err := send(); err != nil {
		return g.fail()
	}
	g.hadAudio = true
	return nil
}

func (g *realtimeTurnGuard) speechStarted(item string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || item == "" || g.speechItem != "" {
		return g.fail()
	}
	g.speechItem = item
	return nil
}

func (g *realtimeTurnGuard) speechStopped(item string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || item == "" || item != g.speechItem || g.stoppedItem != "" {
		return g.fail()
	}
	g.stoppedItem = item
	return nil
}

func (g *realtimeTurnGuard) committed(item string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || item == "" || item != g.speechItem || g.committedItem != "" {
		return g.fail()
	}
	g.committedItem = item
	return nil
}

// request records metadata before sending response.create. If sending fails,
// the caller must close; the returned association is never inferred from timing.
func (g *realtimeTurnGuard) request(id uint32, nonce string, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || g.cancelPending || id != g.listenID || nonce == "" || g.stoppedItem == "" || g.stoppedItem != g.committedItem || g.requestNonce != "" {
		return g.fail()
	}
	g.requestNonce = nonce
	if err := send(); err != nil {
		return g.fail()
	}
	return nil
}

// created matches metadata echoed in response.created to our request, then
// records response_id. An unsolicited or mismatched response closes the guard.
func (g *realtimeTurnGuard) created(id uint32, nonce, responseID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || id != g.listenID || nonce == "" || nonce != g.requestNonce || responseID == "" || g.responseID != "" {
		return g.fail()
	}
	if _, seen := g.seenResponses[responseID]; seen {
		return g.fail()
	}
	if g.seenResponses == nil {
		g.seenResponses = make(map[string]struct{})
	}
	// Bound memory during a long-running provider session; rotate before this.
	if len(g.seenResponses) >= 4096 {
		return g.fail()
	}
	g.seenResponses[responseID] = struct{}{}
	g.responseID = responseID
	return nil
}

// authorizeOutput holds the same lock across checking the generation and the
// final write/queue operation. The callback must have a finite write deadline.
func (g *realtimeTurnGuard) authorizeOutput(responseID string, send func(uint32) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || responseID == "" || responseID != g.responseID || g.responseDone {
		return g.fail()
	}
	if err := send(g.listenID); err != nil {
		return g.fail()
	}
	return nil
}

// authorizeTool serializes the irreversible MCP request write with pause; the
// callback must only enqueue/write the request and must not wait for the reply.
// Later completion must be validated by toolDone before returning it upstream.
func (g *realtimeTurnGuard) authorizeTool(responseID, callID string, send func(uint32) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || responseID == "" || responseID != g.responseID || g.responseDone || callID == "" || g.toolCallID != "" {
		return g.fail()
	}
	g.toolCallID = callID
	if err := send(g.listenID); err != nil {
		return g.fail()
	}
	return nil
}

func (g *realtimeTurnGuard) toolDone(id uint32, callID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || g.clearPending || id != g.listenID || callID == "" || callID != g.toolCallID {
		return g.fail()
	}
	g.toolCallID = ""
	return nil
}

func (g *realtimeTurnGuard) done(responseID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || responseID == "" || responseID != g.responseID || g.responseDone || g.toolCallID != "" {
		return g.fail()
	}
	g.responseDone = true
	if g.cancelPending {
		g.cancelPending = false
	}
	return nil
}

// played is a conservative application-level marker: the caller must prove
// that every frame was delivered and accounted for (or there was no audio).
// response.done alone does not establish what the device actually played.
func (g *realtimeTurnGuard) played(responseID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || responseID == "" || responseID != g.responseID || !g.responseDone {
		return g.fail()
	}
	g.playbackComplete = true
	return nil
}

// pause invalidates the generation before initiating clear/cancel. A response
// requested but not yet created is not safely cancellable by response_id, so
// close instead of guessing. An already-issued tool call cannot be revoked;
// if it is still outstanding, close rather than acknowledge a clean barrier.
// send is called under lock and may only write bounded-duration WS events.
func (g *realtimeTurnGuard) pause(id uint32, send func(cancelID string) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.listenID || g.clearPending || g.cancelPending || g.toolCallID != "" || (g.requestNonce != "" && g.responseID == "") {
		return g.fail()
	}
	// A committed but unanswered/partially heard turn stays in OpenAI's
	// conversation after buffer.clear. Reset unless cleanup is proven.
	if g.committedItem != "" && (!g.responseDone || !g.playbackComplete) {
		g.dirtyContext = true
	}
	// The public schema does not promise that cleared is a full barrier for
	// asynchronous VAD events already triggered by earlier append events. Even
	// a fully answered item cannot prove a later speech_started is new audio.
	if g.hadAudio {
		g.dirtyContext = true
	}
	g.active = false
	g.clearPending = true
	cancelID := ""
	if g.responseID != "" && !g.responseDone {
		cancelID = g.responseID
		g.cancelPending = true
	}
	if err := send(cancelID); err != nil {
		return g.fail()
	}
	return nil
}

// cleared has no client correlation ID in the API. Only a single outstanding
// clear is allowed and a fresh generation must wait for this and response.done.
func (g *realtimeTurnGuard) cleared() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.clearPending {
		return g.fail()
	}
	g.clearPending = false
	return nil
}
