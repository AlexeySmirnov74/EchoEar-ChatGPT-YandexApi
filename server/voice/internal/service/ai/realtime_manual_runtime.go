package ai

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// The consuming actor must fence all three identifiers again at the final
// device write; a correct tag alone is not an atomic write authorization.
type manualRealtimeEvent struct {
	Kind       string
	Generation uint64
	ListenID   uint32
	ResponseID string
	PCM        []int16
	CallID     string
	ToolName   string
	Args       map[string]any
	Final      bool
}

type manualRealtimeRuntime struct {
	mu              sync.Mutex
	conn            *websocket.Conn
	guard           manualTurnGuard
	generation      uint64
	events          chan manualRealtimeEvent
	failures        chan uint64
	id              uint32
	responseID      string
	toolID          string
	toolResponse    string
	responseDone    bool
	toolResultSent  bool
	responseHasTool bool
	noticeCancelling bool
	noticeCancelEventID string
	closed          bool
	timer           *time.Timer
	timerEpoch      uint64
	ackTimeout      time.Duration
}

// startManualRuntime transfers the already-negotiated socket to one event
// reader. A connection may be started once. No application path calls this yet.
func (s *manualRealtimeConnection) startManualRuntime(generation uint64, queueSize int) error {
	if s == nil || s.conn == nil || s.runtime != nil || generation == 0 || queueSize < 1 || queueSize > 256 {
		return errManualGuard
	}
	r := &manualRealtimeRuntime{conn: s.conn, generation: generation, events: make(chan manualRealtimeEvent, queueSize), failures: make(chan uint64, 1), ackTimeout: 5 * time.Second}
	s.runtime = r
	go r.readLoop()
	return nil
}

func (r *manualRealtimeRuntime) emit(e manualRealtimeEvent) error {
	e.Generation = r.generation
	select {
	case r.events <- e:
		return nil
	default:
		return errors.New("manual event queue full")
	}
}

func (r *manualRealtimeRuntime) send(v map[string]any) error {
	if r.closed {
		return errManualGuard
	}
	if err := r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return r.conn.WriteJSON(v)
}
func (r *manualRealtimeRuntime) arm(duration time.Duration) {
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timerEpoch++
	epoch := r.timerEpoch
	r.timer = time.AfterFunc(duration, func() {
		r.mu.Lock()
		if r.closed || r.timerEpoch != epoch {
			r.mu.Unlock()
			return
		}
		r.closed = true
		r.timerEpoch++
		r.timer = nil
		_ = r.conn.Close()
		gen := r.generation
		r.mu.Unlock()
		select {
		case r.failures <- gen:
		default:
		}
	})
}
func (r *manualRealtimeRuntime) disarm() {
	r.timerEpoch++
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}
func (r *manualRealtimeRuntime) closeLocked() func() {
	if r.closed {
		return nil
	}
	r.closed = true
	r.disarm()
	_ = r.conn.Close()
	gen := r.generation
	return func() {
		select {
		case r.failures <- gen:
		default:
		}
	}
}
func (r *manualRealtimeRuntime) close(failure bool) {
	r.mu.Lock()
	notify := r.closeLocked()
	r.mu.Unlock()
	if failure && notify != nil {
		notify()
	}
}
func (r *manualRealtimeRuntime) operation(fn func() error) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errManualGuard
	}
	err := fn()
	var notify func()
	if err != nil && !errors.Is(err, errManualNotIdle) {
		notify = r.closeLocked()
	}
	r.mu.Unlock()
	if notify != nil {
		notify()
	}
	return err
}

func (r *manualRealtimeRuntime) Begin(id uint32) error {
	return r.operation(func() error {
		if err := r.guard.beginNext(id); err != nil {
			return err
		}
		r.id = id
		r.responseID = ""
		r.toolID = ""
		r.toolResponse = ""
		r.responseDone = false
		r.toolResultSent = false
		r.responseHasTool = false
		return nil
	})
}
func (r *manualRealtimeRuntime) Append(id uint32, pcm []int16) error {
	return r.operation(func() error {
		if len(pcm) == 0 || len(pcm) > 5*deviceSampleRate {
			return errManualGuard
		}
		return r.guard.appendAudio(id, func() error {
			samples := resamplePCM16(pcm, deviceSampleRate, serverSampleRate)
			raw := make([]byte, len(samples)*2)
			for i, v := range samples {
				binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
			}
			return r.send(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(raw)})
		})
	})
}
func (r *manualRealtimeRuntime) Commit(id uint32) error {
	return r.operation(func() error {
		if err := r.guard.commit(id, func() error { return r.send(map[string]any{"type": "input_audio_buffer.commit"}) }); err != nil {
			return err
		}
		r.arm(r.ackTimeout)
		return nil
	})
}
func (r *manualRealtimeRuntime) Clear(id uint32) error {
	return r.operation(func() error {
		if err := r.guard.clearShortBurst(id, func() error { return r.send(map[string]any{"type": "input_audio_buffer.clear"}) }); err != nil {
			return err
		}
		r.arm(r.ackTimeout)
		return nil
	})
}
func (r *manualRealtimeRuntime) ToolResult(id uint32, responseID, callID, result string) error {
	return r.operation(func() error {
		if id != r.id || responseID != r.toolResponse || callID != r.toolID {
			return errManualGuard
		}
		if err := r.guard.toolResult(id, callID, func() error {
			return r.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "function_call_output", "call_id": callID, "output": result}})
		}); err != nil {
			return err
		}
		r.toolResultSent = true
		if r.responseDone {
			return r.requestResponse()
		}
		return nil
	})
}
func (r *manualRealtimeRuntime) NotifyImageResult(id uint32, status string) error {
	return r.operation(func() error {
		if id!=r.id {return errManualNotIdle}
		instruction:=""
		switch status {
		case "no_results": instruction="Say exactly in Russian: К сожалению, я не нашла подходящую картинку."
		case "unavailable": instruction="Say exactly in Russian: Сейчас не получается найти картинку. Попробуй ещё раз чуть позже."
		default:return errManualNotIdle
		}
		nonce:=uuid.NewString()
		err:=r.guard.requestNotice(id,nonce,func() error{return r.send(map[string]any{"type":"response.create","response":map[string]any{"conversation":"none","output_modalities":[]string{"audio"},"tools":[]any{},"instructions":instruction,"metadata":map[string]any{"listen_id":strconv.FormatUint(uint64(id),10),"nonce":nonce,"kind":"image_search_notice"}}})})
		if err==nil {r.responseID="";r.responseDone=false;r.responseHasTool=false;r.toolID="";r.toolResponse="";r.toolResultSent=false}
		return err
	})
}
func (r *manualRealtimeRuntime) CancelImageNotice(id uint32)error {
	return r.operation(func()error {
		if r.noticeCancelling {return errManualNotIdle}
		// An unspecified response_id cancels only the default conversation.
		// Wait for response.created before cancelling an out-of-band notice.
		r.noticeCancelEventID=uuid.NewString()
		err:=r.guard.cancelNotice(id,func()error{if r.responseID=="" {return nil};return r.send(map[string]any{"type":"response.cancel","response_id":r.responseID,"event_id":r.noticeCancelEventID})})
		if err==nil {r.noticeCancelling=true;r.arm(5*time.Second)};return err
	})
}
func (r *manualRealtimeRuntime) FinishImageNoticeCancel(id uint32,response string,stop func(uint32)error,resumeSame bool)error {
	return r.operation(func()error {
		if err:=r.guard.finishPlayback(response,stop);err!=nil{return err}
		if resumeSame {if err:=r.guard.resumeAfterNotice(id);err!=nil{return err};r.responseID="";r.responseDone=false}
		r.noticeCancelling=false;return nil
	})
}
func (r *manualRealtimeRuntime) FinishPlayback(id uint32, responseID string, sendStop func(uint32) error) error {
	return r.operation(func() error {
		if id != r.id {
			return errManualGuard
		}
		return r.guard.finishPlayback(responseID, sendStop)
	})
}

func (r *manualRealtimeRuntime) Events() <-chan manualRealtimeEvent           { return r.events }
func (r *manualRealtimeRuntime) Failures() <-chan uint64                      { return r.failures }
func (r *manualRealtimeRuntime) GenerationEvents() <-chan manualProviderState { return nil }
func (r *manualRealtimeRuntime) Standby(id uint32) error {
	return r.operation(func() error { return r.guard.standby(id) })
}
func (r *manualRealtimeRuntime) EndTurnAndWait(context.Context) error { return nil }
func (r *manualRealtimeRuntime) Invalidate(generation uint64) {
	if generation == r.generation {
		r.close(true)
	}
}
func (r *manualRealtimeRuntime) DeviceWrite(generation uint64, id uint32, responseID string, send func() error) error {
	return r.operation(func() error {
		if generation != r.generation {
			return errManualGuard
		}
		return r.guard.deviceWrite(id, responseID, send)
	})
}
func (r *manualRealtimeRuntime) requestResponse() error {
	nonce := uuid.NewString()
	id := r.id
	if err := r.guard.requestResponse(id, nonce, func() error {
		return r.send(map[string]any{"type": "response.create", "response": map[string]any{"metadata": map[string]any{"listen_id": strconv.FormatUint(uint64(id), 10), "nonce": nonce}}})
	}); err != nil {
		return err
	}
	r.responseID = ""
	r.responseDone = false
	r.toolID = ""
	r.toolResponse = ""
	r.toolResultSent = false
	r.responseHasTool = false
	r.arm(r.ackTimeout)
	return nil
}

func parseManualResponse(event map[string]any) (string, uint32, string, error) {
	response, ok := event["response"].(map[string]any)
	if !ok {
		return "", 0, "", errManualGuard
	}
	id, _ := response["id"].(string)
	meta, ok := response["metadata"].(map[string]any)
	if !ok {
		return "", 0, "", errManualGuard
	}
	text, ok := meta["listen_id"].(string)
	n, err := strconv.ParseUint(text, 10, 32)
	nonce, nok := meta["nonce"].(string)
	if !ok || err != nil || n == 0 || text != strconv.FormatUint(n, 10) || !nok || nonce == "" || id == "" {
		return "", 0, "", errManualGuard
	}
	return id, uint32(n), nonce, nil
}

func (r *manualRealtimeRuntime) readLoop() {
	for {
		kind, raw, err := r.conn.ReadMessage()
		if err != nil {
			r.close(true)
			return
		}
		if kind != websocket.TextMessage {
			r.close(true)
			return
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			r.close(true)
			return
		}
		if err = r.operation(func() error { return r.handle(event) }); err != nil {
			return
		}
	}
}

func (r *manualRealtimeRuntime) handle(event map[string]any) error {
	typ, _ := event["type"].(string)
	switch typ {
	case "input_audio_buffer.committed":
		item, _ := event["item_id"].(string)
		if err := r.guard.committed(item); err != nil {
			return err
		}
		r.disarm()
		return r.requestResponse()
	case "input_audio_buffer.cleared":
		if err := r.guard.cleared(); err != nil {
			return err
		}
		r.disarm()
		return r.emit(manualRealtimeEvent{Kind: "cleared", ListenID: r.id})
	case "response.created":
		responseID, id, nonce, err := parseManualResponse(event)
		if err != nil {
			return err
		}
		if err = r.guard.responseCreated(id, nonce, responseID); err != nil {
			return err
		}
		r.responseID = responseID
		if r.noticeCancelling {if err:=r.send(map[string]any{"type":"response.cancel","response_id":responseID,"event_id":r.noticeCancelEventID});err!=nil{return err};r.arm(5*time.Second);return nil}
		r.arm(2 * time.Minute)
		return nil
	case "response.output_audio.delta", "response.audio.delta":
		responseID, _ := event["response_id"].(string)
		encoded, _ := event["delta"].(string)
		if len(encoded) == 0 || len(encoded) > 512*1024 {
			return errManualGuard
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) == 0 || len(raw)%2 != 0 {
			return errManualGuard
		}
		pcm := make([]int16, len(raw)/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
		}
		return r.guard.output(responseID, func(id uint32) error {
			return r.emit(manualRealtimeEvent{Kind: "audio", ListenID: id, ResponseID: responseID, PCM: pcm})
		})
	case "response.output_audio.done", "response.audio.done":
		responseID, _ := event["response_id"].(string)
		return r.guard.output(responseID, func(id uint32) error {
			return r.emit(manualRealtimeEvent{Kind: "audio_done", ListenID: id, ResponseID: responseID})
		})
	case "response.output_item.done":
		responseID, _ := event["response_id"].(string)
		item, ok := event["item"].(map[string]any)
		if !ok {
			return errManualGuard
		}
		if item["type"] != "function_call" {
			return r.guard.output(responseID, func(uint32) error { return nil })
		}
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		arguments, _ := item["arguments"].(string)
		var args map[string]any
		if item["status"] != "completed" || name == "" || json.Unmarshal([]byte(arguments), &args) != nil || args == nil {
			return errManualGuard
		}
		if err := r.guard.tool(responseID, callID, func(id uint32) error {
			return r.emit(manualRealtimeEvent{Kind: "tool", ListenID: id, ResponseID: responseID, CallID: callID, ToolName: name, Args: args})
		}); err != nil {
			return err
		}
		r.toolID = callID
		r.toolResponse = responseID
		r.responseHasTool = true
		// The actor must first drain any audio preceding this tool call.
		r.arm(2*time.Minute)
		return nil
	case "response.done":
		response, ok := event["response"].(map[string]any)
		if !ok {
			return errManualGuard
		}
		responseID, _ := response["id"].(string)
		status, _ := response["status"].(string)
		if r.noticeCancelling && (status=="cancelled" || status=="completed") {
			if err:=r.guard.done(responseID);err!=nil{return err};r.responseDone=true;r.disarm()
			return r.emit(manualRealtimeEvent{Kind:"notice_cancelled",ListenID:r.id,ResponseID:responseID})
		}
		if status != "completed" {
			return errManualGuard
		}
		if err := r.guard.done(responseID); err != nil {
			return err
		}
		r.responseDone = true
		if err := r.emit(manualRealtimeEvent{Kind: "done", ListenID: r.id, ResponseID: responseID, Final: !r.responseHasTool}); err != nil {
			return err
		}
		if r.responseHasTool {
			if r.toolResultSent {
				return r.requestResponse()
			}
			r.arm(2*time.Minute)
			return nil
		}
		r.disarm()
		return nil
	case "error":
		problem,_:=event["error"].(map[string]any);eventID,_:=problem["event_id"].(string)
		// A cancel may race the provider completing the notice. An error
		// explicitly tied to this cancel leaves the session intact; we still
		// wait for response.done (or the bounded cancellation timeout).
		if eventID!="" && eventID==r.noticeCancelEventID {r.noticeCancelEventID="";return nil};return errManualGuard
	case "rate_limits.updated", "conversation.item.added", "conversation.item.done", "conversation.item.created", "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed":
		return nil
	case "response.output_item.added", "response.content_part.added", "response.content_part.done", "response.output_audio_transcript.delta", "response.output_audio_transcript.done", "response.audio_transcript.delta", "response.audio_transcript.done", "response.output_text.delta", "response.output_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
		responseID, _ := event["response_id"].(string)
		return r.guard.output(responseID, func(uint32) error { return nil })
	default:
		return errors.New("unexpected event in manual realtime session")
	}
}
