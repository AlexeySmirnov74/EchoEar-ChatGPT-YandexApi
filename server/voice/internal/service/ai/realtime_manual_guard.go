package ai

import (
	"errors"
	"strings"
	"sync"
)

// manualTurnGuard models an alternative opt-in design using turn_detection:null
// and a local VAD. It is NOT connected to realtime_client or HandleWS. All
// inbound events must be processed on one ordered OpenAI WebSocket reader.
// Input/output/tool write callbacks must be bounded and must not wait for an
// event from that reader. Any error makes the guard terminal; close the session.
type manualTurnGuard struct {
	mu               sync.Mutex
	closed           bool
	id               uint32
	active           bool
	inputSent        bool
	commitPending    bool
	clearPending     bool
	itemID           string
	requestNonce     string
	responseID       string
	responseDone     bool
	playbackComplete bool
	toolCall         string
	toolResultSent   bool
	seenResponses    map[string]bool
	seenItems        map[string]bool
	seenNonces       map[string]bool
}

var errManualGuard = errors.New("ambiguous manual-turn event; close provider")
var errManualNotIdle = errors.New("manual turn has active input")

// Reserve an out-of-band result notification only on an empty listening turn.
// A not-yet-empty buffer is an ordinary retry condition, not a broken session.
func (g *manualTurnGuard) requestNotice(id uint32, nonce string, send func() error) error {
	g.mu.Lock();defer g.mu.Unlock()
	if g.closed || !g.active || id!=g.id {return errManualNotIdle}
	if g.inputSent || g.commitPending || g.clearPending || g.itemID!="" || g.requestNonce!="" || g.toolCall!="" {return errManualNotIdle}
	if nonce=="" || g.seenNonces[nonce] {return g.fail()}
	if g.seenNonces==nil {g.seenNonces=map[string]bool{}}
	if len(g.seenNonces)>=4096{return g.fail()}
	g.seenNonces[nonce]=true;g.itemID="notice:"+nonce;g.requestNonce=nonce;g.responseID="";g.responseDone=false;g.playbackComplete=false;g.toolResultSent=false
	if err:=send();err!=nil{return g.fail()};return nil
}
func (g *manualTurnGuard) cancelNotice(id uint32,send func()error)error {
	g.mu.Lock();defer g.mu.Unlock()
	if g.closed || !g.active || id!=g.id || !strings.HasPrefix(g.itemID,"notice:") || g.responseDone {return errManualNotIdle}
	if err:=send();err!=nil{return g.fail()};return nil
}
func (g *manualTurnGuard) resumeAfterNotice(id uint32)error {
	g.mu.Lock();defer g.mu.Unlock()
	if g.closed || !g.active || id!=g.id || !strings.HasPrefix(g.itemID,"notice:") || !g.responseDone || !g.playbackComplete {return g.fail()}
	g.itemID="";g.requestNonce="";g.responseID="";g.responseDone=false;g.playbackComplete=false;return nil
}

func (g *manualTurnGuard) fail() error { g.closed = true; return errManualGuard }

// beginNext is allowed after a complete response and fully accounted-for output
// only. At the boundary no audio can still be appended to the old input buffer.
func (g *manualTurnGuard) beginNext(id uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || id == 0 || g.clearPending || (g.id != 0 && int32(id-g.id) <= 0) || (g.active && (!g.responseDone || !g.playbackComplete || g.toolCall != "" || g.toolResultSent)) {
		return g.fail()
	}
	g.id = id
	g.active = true
	g.inputSent = false
	g.commitPending = false
	g.itemID = ""
	g.requestNonce = ""
	g.responseID = ""
	g.responseDone = false
	g.playbackComplete = false
	g.toolCall = ""
	g.toolResultSent = false
	return nil
}

// standby preserves only a provider with an empty input buffer and no barriers.
func (g *manualTurnGuard) standby(id uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || g.inputSent || g.commitPending || g.clearPending || g.itemID != "" || g.requestNonce != "" || g.toolCall != "" {
		return g.fail()
	}
	g.active = false
	return nil
}

func (g *manualTurnGuard) appendAudio(id uint32, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || g.commitPending || g.clearPending || g.itemID != "" {
		return g.fail()
	}
	if err := send(); err != nil {
		return g.fail()
	}
	g.inputSent = true
	return nil
}

// commit serializes after all append writes; no new append may follow until the
// next generation. Exactly one commit must be in flight.
func (g *manualTurnGuard) commit(id uint32, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || !g.inputSent || g.commitPending || g.clearPending || g.itemID != "" {
		return g.fail()
	}
	g.commitPending = true
	if err := send(); err != nil {
		return g.fail()
	}
	return nil
}

// clearShortBurst sends exactly one clear for a false VAD activation. The
// caller buffers any incoming PCM within a fixed bound until cleared(), then
// replays it in order. Missing/ambiguous ACK, buffer overflow or timeout closes.
func (g *manualTurnGuard) clearShortBurst(id uint32, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || !g.inputSent || g.commitPending || g.clearPending || g.itemID != "" {
		return g.fail()
	}
	g.clearPending = true
	if err := send(); err != nil {
		return g.fail()
	}
	return nil
}

func (g *manualTurnGuard) cleared() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.clearPending {
		return g.fail()
	}
	g.clearPending = false
	g.inputSent = false
	return nil
}

// committed associates the only in-flight commit with its item_id. The API
// says commit creates an item and clears the input buffer.
func (g *manualTurnGuard) committed(itemID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || !g.commitPending || itemID == "" || g.itemID != "" || g.seenItems[itemID] {
		return g.fail()
	}
	if g.seenItems == nil {
		g.seenItems = map[string]bool{}
	}
	if len(g.seenItems) >= 4096 {
		return g.fail()
	}
	g.seenItems[itemID] = true
	g.commitPending = false
	g.itemID = itemID
	return nil
}

// requestResponse records the nonce before writing response.create. A tool
// continuation may start only after both response.done and toolResult.
func (g *manualTurnGuard) requestResponse(id uint32, nonce string, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || g.itemID == "" || nonce == "" || g.commitPending || g.toolCall != "" || g.seenNonces[nonce] {
		return g.fail()
	}
	if g.requestNonce != "" && (!g.responseDone || !g.toolResultSent) {
		return g.fail()
	}
	if g.seenNonces == nil {
		g.seenNonces = map[string]bool{}
	}
	if len(g.seenNonces) >= 4096 {
		return g.fail()
	}
	g.seenNonces[nonce] = true
	g.requestNonce = nonce
	g.responseID = ""
	g.responseDone = false
	g.playbackComplete = false
	g.toolResultSent = false
	if err := send(); err != nil {
		return g.fail()
	}
	return nil
}

func (g *manualTurnGuard) responseCreated(id uint32, nonce, responseID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || nonce == "" || nonce != g.requestNonce || responseID == "" || g.responseID != "" || g.seenResponses[responseID] {
		return g.fail()
	}
	if g.seenResponses == nil {
		g.seenResponses = map[string]bool{}
	}
	if len(g.seenResponses) >= 4096 {
		return g.fail()
	}
	g.seenResponses[responseID] = true
	g.responseID = responseID
	return nil
}

func (g *manualTurnGuard) output(responseID string, send func(uint32) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || responseID == "" || responseID != g.responseID || g.responseDone {
		return g.fail()
	}
	if err := send(g.id); err != nil {
		return g.fail()
	}
	return nil
}

// deviceWrite is for an already accepted, queued device event. It remains
// valid after response.done while the same response is being drained, but not
// after a newer response/listen or a completed playback transition.
func (g *manualTurnGuard) deviceWrite(id uint32, responseID string, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || responseID == "" || responseID != g.responseID || g.playbackComplete {
		return g.fail()
	}
	if err := send(); err != nil {
		return g.fail()
	}
	return nil
}

func (g *manualTurnGuard) tool(responseID, callID string, send func(uint32) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || responseID == "" || responseID != g.responseID || g.responseDone || callID == "" || g.toolCall != "" {
		return g.fail()
	}
	g.toolCall = callID
	if err := send(g.id); err != nil {
		return g.fail()
	}
	return nil
}

// done may arrive before the asynchronous MCP response. It must not trigger
// a follow-up response until the pending tool result has been sent.
func (g *manualTurnGuard) done(responseID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || responseID == "" || responseID != g.responseID || g.responseDone {
		return g.fail()
	}
	g.responseDone = true
	return nil
}

func (g *manualTurnGuard) toolResult(id uint32, callID string, send func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || id != g.id || callID == "" || callID != g.toolCall || g.toolResultSent {
		return g.fail()
	}
	if err := send(); err != nil {
		return g.fail()
	}
	g.toolCall = ""
	g.toolResultSent = true
	return nil
}

func (g *manualTurnGuard) played(responseID string) error {
	return g.finishPlayback(responseID, func(uint32) error { return nil })
}

// finishPlayback records completion before publishing tts:stop under the same
// lock used by beginNext. It means server output was drained, not that the
// device physically played every sample; no device playback ACK exists here.
func (g *manualTurnGuard) finishPlayback(responseID string, sendStop func(uint32) error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.active || responseID == "" || responseID != g.responseID || !g.responseDone || g.toolCall != "" || g.toolResultSent {
		return g.fail()
	}
	g.playbackComplete = true
	if err := sendStop(g.id); err != nil {
		return g.fail()
	}
	return nil
}

// Interruptions are intentionally fail-closed. Reset retains correctness when
// an already committed item, partial assistant output, or device tool exists.
func (g *manualTurnGuard) interrupt() error { g.mu.Lock(); defer g.mu.Unlock(); return g.fail() }
