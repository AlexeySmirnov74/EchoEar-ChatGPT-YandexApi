package ai

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// This separate endpoint is opt-in on both server config and query parameter;
// the default HandleWS and legacy behavior are unchanged. It is experimental,
// not suitable for deployment before real-device and fault-injection tests.
type manualProviderDial func(context.Context, string) (*manualRealtimeConnection, error)
type manualDeviceFrame struct {
	kind int
	data []byte
	err  error
}
type manualToolSend struct {
	event manualRealtimeEvent
	data  map[string]any
	ack   chan error
}
type manualToolResult struct {
	event  manualRealtimeEvent
	result string
	err    error
}
type backgroundFrame struct { id int; pixels []byte; restore bool; deadline time.Time }
type backgroundOperation struct { op string; id int; stage string; token uint64; deadline time.Time; event *manualRealtimeEvent; frame *backgroundFrame; wasUncertain bool }
type manualAudioFrame struct {
	generation uint64
	responseID string
	data       []byte
}
type manualProviderState struct {
	Generation uint64
	Ready      bool
}
type manualActorRuntime interface {
	Begin(uint32) error
	Append(uint32, []int16) error
	Commit(uint32) error
	Clear(uint32) error
	ToolResult(uint32, string, string, string) error
	NotifyImageResult(uint32,string) error
	CancelImageNotice(uint32) error
	FinishImageNoticeCancel(uint32,string,func(uint32)error,bool) error
	FinishPlayback(uint32, string, func(uint32) error) error
	DeviceWrite(uint64, uint32, string, func() error) error
	Standby(uint32) error
	EndTurnAndWait(context.Context) error
	Invalidate(uint64)
	GenerationEvents() <-chan manualProviderState
	Events() <-chan manualRealtimeEvent
	Failures() <-chan uint64
}

func defaultManualProvider(ctx context.Context, deviceID string) (*manualRealtimeConnection, error) {
	apiKey := aiString(ctx, "openai_api_key", "")
	if apiKey == "" {
		return nil, errors.New("missing API key")
	}
	profile := deviceProfileFor(ctx, deviceID)
	model := override(profile.OpenAIRealtimeModel, aiString(ctx, "openai_realtime_model", "gpt-realtime"))
	voice := override(profile.OpenAITTSVoice, aiString(ctx, "openai_tts_voice", "alloy"))
	endpoint, err := realtimeDialURL(ctx, model)
	if err != nil {
		return nil, err
	}
	prompt := override(profile.SystemPrompt, globalSystemPrompt(ctx))
	return manualDialReady(ctx, endpoint, apiKey, prompt, voice, speakerRealtimeTools())
}

func handleExperimentalManualWS(w http.ResponseWriter, r *http.Request, dial manualProviderDial) {
	if dial == nil {
		dial = defaultManualProvider
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	c.SetReadLimit(64 * 1024)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hello map[string]any
	if c.ReadJSON(&hello) != nil {
		return
	}
	features, _ := hello["features"].(map[string]any)
	if hello["type"] != "hello" || features["persistent_idle"] != true {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	provider, err := dial(ctx, r.Header.Get("Device-Id"))
	if err != nil {
		return
	}
	defer provider.Close()
	if provider.startManualRuntime(1, 64) != nil {
		return
	}
	runtime := provider.runtime
	sessionID := uuid.NewString()
	if writeManualJSON(c, map[string]any{"type": "hello", "transport": "websocket", "session_id": sessionID, "features": map[string]any{"persistent_idle": true}, "audio_params": map[string]any{"sample_rate": serverSampleRate, "frame_duration": frameDurationMs}}) != nil {
		return
	}
	if features["image_preview"] == true {
		ctx = context.WithValue(ctx, imagePreviewContextKey{}, true)
	}
	runExperimentalManualActor(ctx, c, runtime, sessionID)
}

func writeManualJSON(c *websocket.Conn, v any) error {
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return c.WriteJSON(v)
}
func writeManualAudio(c *websocket.Conn, frame []byte) error {
	if len(frame) > 65535 {
		return errManualGuard
	}
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return c.WriteMessage(websocket.BinaryMessage, append([]byte{0, 0, byte(len(frame) >> 8), byte(len(frame))}, frame...))
}

func runExperimentalManualActor(parent context.Context, c *websocket.Conn, rt manualActorRuntime, sessionID string) {
	runExperimentalManualActorWithProgressTimeout(parent, c, rt, sessionID, 30*time.Second)
}

// A timeout applies only while a committed answer is being generated or played.
// Idle listening has its own standby policy and must never trip this watchdog.
func runExperimentalManualActorWithProgressTimeout(parent context.Context, c *websocket.Conn, rt manualActorRuntime, sessionID string, progressTimeout time.Duration) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	initialGeneration:=uint64(1)
	if states:=rt.GenerationEvents();states!=nil {
		initialGeneration=0
		for initialGeneration==0 {select {case <-ctx.Done():return;case state:=<-states:if state.Ready {initialGeneration=state.Generation}}}
	}
	input := make(chan manualDeviceFrame, 128)
	go func() {
		for {
			kind, data, err := c.ReadMessage()
			select {
			case input <- manualDeviceFrame{kind, data, err}:
			case <-ctx.Done():
				return
			default:
				_ = c.Close()
				return
			}
			if err != nil {
				return
			}
		}
	}()
	toolSends := make(chan manualToolSend, 2)
	toolResults := make(chan manualToolResult, 2)
	imageResults := make(chan imageSearchResult, 2)
	var imageCancel context.CancelFunc
	var imageSequence uint64
	diagnosticSession := newImageDiagnosticSession()
	voiceSession := newImageDiagnosticSession()
	var imageAwaitingSpeech bool
	var imageNotice string
	var imageNoticeUntil time.Time
	var imageRequestListenID uint32
	var noticeActive, noticeCancelling, noticeWaitingNext bool
	var noticeAudio []int16
	var noticeWaitDeadline time.Time
	var imageStarted time.Time
	defer func() { if imageCancel != nil { imageCancel() } }()
	decoder, err := newOpusDecoder()
	if err != nil {
		return
	}
	noticeVAD,err:=newStreamLocalVAD(aiInt(parent,"experimental_manual_vad_threshold",400),aiInt(parent,"experimental_manual_min_speech_ms",120),aiInt(parent,"experimental_manual_silence_ms",600),aiInt(parent,"experimental_manual_preroll_ms",300),aiInt(parent,"experimental_manual_max_turn_ms",30000))
	if err!=nil{return}
	vad, err := newStreamLocalVAD(
		aiInt(parent, "experimental_manual_vad_threshold", 400),
		aiInt(parent, "experimental_manual_min_speech_ms", 120),
		aiInt(parent, "experimental_manual_silence_ms", 600),
		aiInt(parent, "experimental_manual_preroll_ms", 300),
		aiInt(parent, "experimental_manual_max_turn_ms", 30000),
	)
	if err != nil {
		return
	}
	gate, err := newManualClearGate(deviceSampleRate * 2)
	if err != nil {
		return
	}
	idleSeconds := min(300, max(1, aiInt(parent, "experimental_manual_idle_seconds", 15)))
	var listenID uint32
	var standbyReason voiceReason
	defer func() { voiceDiagnosticsCurrent.emit(voiceDisconnect, voiceActorClosed, voiceSession, listenID, 0) }()
	currentGeneration := initialGeneration
	lastGeneration := currentGeneration
	var turnCancel context.CancelFunc
	var bridge *deviceMCPBridge
	var nextMCPID int
	var encoder *opusStreamEncoder
	var encoderResponse string
	var frames []manualAudioFrame
	var ttsStarted bool
	var ttsRequested time.Time
	var finalResponse string
	var finalDone bool
	audioDone := map[string]bool{}
	sawAudio := map[string]bool{}
	var busy bool
	var lastProgress time.Time
	lastActivity := time.Now()
	ticker := time.NewTicker(frameDurationMs * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(50 * time.Second)
	defer heartbeat.Stop()
	var preview *imagePreviewDemo
	bg, bgConfigured := parent.Value(imageBackgroundContextKey{}).(imageBackgroundContext)
	bgEnabled:=false
	var bgLease uint64
	var bgLeaseRef *atomic.Uint64
	var bgShown, bgWaiting, bgPinned *backgroundFrame
	var bgOperation *backgroundOperation
	var bgRestoring bool
	var bgSaved []byte
	var bgNeedsRestore bool
	var bgUncertain bool
	var bgRestoreAttempts int
	var bgRestoreRetry time.Time
	var bgToken uint64
	bgDisk:=make(chan backgroundDiskResult,4)
	bgOpen:=make(chan backgroundDiskResult)
	var bgOpenPending bool
	var bgOpenRetry time.Time
	openBG:=func(){bgOpenPending=submitBackgroundDisk(backgroundDiskJob{operation:"open",device:bg.device,store:bg.store,deadline:time.Now().Add(6*time.Second),reply:bgOpen,ctx:ctx});if !bgOpenPending {bgOpenRetry=time.Now().Add(5*time.Second)}}
	queueBG:=func(op string,pixels []byte,token uint64) bool {
		return submitBackgroundDisk(backgroundDiskJob{operation:op,token:token,device:bg.device,lease:bgLease,store:bg.store,pixels:pixels,deadline:time.Now().Add(6*time.Second),reply:bgDisk})
	}
	if bgConfigured {openBG()}
	defer func(){if bgLease!=0 {bg.store.release(bg.device,bgLease)}}()
	var pendingTool *manualToolResult
	var intermediateDone string
	var imagePhase imageCancelPhase
	cancelImage := func(reason imageCancelReason) {
		if !imageStarted.IsZero() && (imageCancel != nil || preview != nil || imageAwaitingSpeech) {
			recordImageCancel(reason, imagePhase, diagnosticSession, imageSequence, time.Since(imageStarted))
		}
		if imageCancel != nil { imageCancel(); imageCancel = nil }
		imageSequence++
		imageAwaitingSpeech = false
		imageNotice=""
		imageStarted = time.Time{}
		preview = nil
		bgWaiting = nil
		if bgRestoring {bgNeedsRestore=true;bgRestoring=false}
	}
	// An EOF, context cancellation or write failure can close the actor without
	// going through cancelImage. Report that case without changing teardown.
	defer func() {
		if !imageStarted.IsZero() && (imageCancel != nil || preview != nil || imageAwaitingSpeech) {
			recordImageCancel(imageCancelActorClosed, imagePhase, diagnosticSession, imageSequence, time.Since(imageStarted))
		}
	}()
	resetTurn := func(notify bool) error {
		id := listenID
		reason := standbyReason
		standbyReason = voiceNoReason
		wasSpeaking := ttsStarted
		noticeActive=false;noticeCancelling=false;noticeWaitingNext=false;noticeAudio=nil;noticeVAD.reset()
		listenID = 0
		if turnCancel != nil {
			turnCancel()
			turnCancel = nil
		}
		vad.reset()
		gate, _ = newManualClearGate(deviceSampleRate * 2)
		bridge = nil
		encoder = nil
		encoderResponse = ""
		frames = nil
		ttsStarted = false
		ttsRequested = time.Time{}
		finalResponse = ""
		finalDone = false
		audioDone = map[string]bool{}
		sawAudio = map[string]bool{}
		busy = false
		pendingTool = nil
		intermediateDone = ""
		lastProgress = time.Time{}
		if notify && id != 0 {
			// Standby is accepted only while Listening. Stop the previous
			// speaking state first even when its provider is already gone.
			if wasSpeaking {
				if err := writeManualJSON(c, map[string]any{"type": "tts", "state": "stop", "listen_id": id}); err != nil {
					return err
				}
			}
			err := writeManualJSON(c, map[string]any{"type": "standby", "listen_id": id})
			if err == nil { voiceDiagnosticsCurrent.emit(voiceStandby, reason, voiceSession, id, 0) }
			return err
		}
		return nil
	}
	endTurn := func() {
		endCtx, endCancel := context.WithTimeout(ctx, 2*time.Second)
		defer endCancel()
		_ = rt.EndTurnAndWait(endCtx)
	}
	failProvider := func() error {
		cancelImage(imageCancelProvider)
		old := currentGeneration
		endTurn()
		if err := resetTurn(true); err != nil {
			return err
		}
		currentGeneration = 0
		if old != 0 {
			rt.Invalidate(old)
		}
		return nil
	}
	// The actor owns all device writes and the generation state. It processes
	// buffered frames in order only after the matching clear ACK.
	var feed func([]int16) error
	feed = func(pcm []int16) error {
		if gate.pending {
			return gate.hold(time.Now(), pcm)
		}
		wasActive := vad.active
		turnSamples := vad.totalSamples
		event, err := vad.feed(pcm)
		if err != nil {
			standbyReason = voiceVADFailure
			return err
		}
		if !wasActive && vad.active { voiceDiagnosticsCurrent.emit(voiceVADStart, voiceNoReason, voiceSession, listenID, 0) }
		turnDuration := time.Duration(turnSamples+len(pcm)) * time.Second / deviceSampleRate
		if event.commit { voiceDiagnosticsCurrent.emit(voiceVADCommit, voiceNoReason, voiceSession, listenID, turnDuration) }
		if event.clear { voiceDiagnosticsCurrent.emit(voiceVADClear, voiceNoReason, voiceSession, listenID, turnDuration) }
		// A new actual utterance supersedes an unfinished image request. Merely
		// entering listen mode does not: that also happens automatically after
		// our spoken acknowledgement. Ignore noise shorter than minSpeech.
		if (imageCancel != nil || imageNotice!="" || (preview != nil && !bgRestoring)) && ((vad.active && vad.speechSamples >= vad.minSpeech) || event.commit) {
			cancelImage(imageCancelVoice)
		}
		if len(event.pcm) > 0 {
			lastActivity = time.Now()
			if err := rt.Append(listenID, event.pcm); err != nil {
				return err
			}
		}
		if event.clear {
			if err := gate.begin(time.Now(), 2*time.Second); err != nil {
				return err
			}
			return rt.Clear(listenID)
		}
		if event.commit {
			busy = true
			lastProgress = time.Now()
			return rt.Commit(listenID)
		}
		return nil
	}
	replayNotice:=func(buffer []int16)error {
		for len(buffer)>0 {n:=min(deviceFrameSamples,len(buffer));if err:=feed(buffer[:n]);err!=nil{return err};buffer=buffer[n:];if busy {break}}
		return nil
	}
	finishNoticeCancellation:=func(response string)error {
		wasSpeaking:=ttsStarted
		stop:=func(id uint32)error{if wasSpeaking{return writeManualJSON(c,map[string]any{"type":"tts","state":"stop","listen_id":id})};return nil}
		if err:=rt.FinishImageNoticeCancel(listenID,response,stop,!wasSpeaking);err!=nil{return err}
		noticeActive=false;noticeCancelling=false;ttsStarted=false;busy=false;lastProgress=time.Time{};frames=nil;encoder=nil;finalDone=false;finalResponse=""
		if wasSpeaking {endTurn();noticeWaitingNext=true;noticeWaitDeadline=time.Now().Add(5*time.Second)} else {buffer:=noticeAudio;noticeAudio=nil;if err:=replayNotice(buffer);err!=nil{return err}}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case failed := <-rt.Failures():
			if failed != currentGeneration || currentGeneration == 0 {
				continue
			}
			cancelImage(imageCancelProvider)
			endTurn()
			standbyReason = voiceProviderFailure
			if resetTurn(true) != nil {
				return
			}
			currentGeneration = 0
		case state := <-rt.GenerationEvents():
			if !state.Ready {
				if state.Generation != currentGeneration || currentGeneration == 0 {
					continue
				}
				cancelImage(imageCancelProvider)
				endTurn()
				standbyReason = voiceGenerationLost
				if resetTurn(true) != nil {
					return
				}
				currentGeneration = 0
				continue
			}
			if state.Generation == currentGeneration && currentGeneration != 0 {
				continue
			}
			if state.Generation <= lastGeneration {
				continue
			}
			if currentGeneration != 0 {
				cancelImage(imageCancelProvider)
				endTurn()
				standbyReason = voiceGenerationLost
				if resetTurn(true) != nil {
					return
				}
			}
			currentGeneration = state.Generation
			lastGeneration = state.Generation
		case wire := <-input:
			if bgEnabled && (bgLeaseRef==nil || bgLeaseRef.Load()!=bgLease) {return}
			if wire.err != nil {
				return
			}
			if wire.kind == websocket.BinaryMessage {
				if currentGeneration == 0 || listenID == 0 || (busy && !noticeActive) || len(wire.data) < 4 {
					continue
				}
				n := int(binary.BigEndian.Uint16(wire.data[2:4]))
				if n == 0 || len(wire.data) != 4+n {
					continue
				}
				pcm, e := decodeOpusFrame(decoder, wire.data[4:])
				if e == nil {
					if noticeActive || noticeWaitingNext {
						noticeAudio=append(noticeAudio,pcm...)
						if !noticeCancelling && !noticeWaitingNext && len(noticeAudio)>deviceSampleRate {noticeAudio=append([]int16(nil),noticeAudio[len(noticeAudio)-deviceSampleRate:]...)}
						if len(noticeAudio)>deviceSampleRate*7 {if failProvider()!=nil{return};continue}
						if noticeActive && !noticeCancelling {
							v,ve:=noticeVAD.feed(pcm);if ve!=nil {if failProvider()!=nil{return};continue}
							if (noticeVAD.active && noticeVAD.speechSamples>=noticeVAD.minSpeech)||v.commit {
								noticeCancelling=true;frames=nil;encoder=nil;encoderResponse=""
								if e:=rt.CancelImageNotice(listenID);e!=nil {if errors.Is(e,errManualNotIdle)&&finalDone {if finishNoticeCancellation(finalResponse)!=nil {if failProvider()!=nil{return}}} else if !errors.Is(e,errManualNotIdle){if failProvider()!=nil{return}}}
							}
						}
						continue
					}
					if feed(pcm) != nil {
						if failProvider() != nil {
							return
						}
						continue
					}
				}
				continue
			}
			var msg map[string]any
			if json.Unmarshal(wire.data, &msg) != nil {
				continue
			}
			typ, _ := msg["type"].(string)
			switch typ {
			case "image_background":
				if !bgEnabled { continue }
				op,_:=msg["op"].(string);status,_:=msg["status"].(string);rawID,ok:=msg["id"].(float64)
				if !ok || rawID<1 || rawID>2147483647 || rawID!=float64(int(rawID)) {continue};id:=int(rawID)
				if op=="hide" && status=="ok" && bgShown!=nil && bgShown.id==id {bgShown=nil;continue}
				if op=="show" && status=="ok" && bgPinned!=nil && bgPinned.id==id && (bgWaiting==nil || bgWaiting.id!=id) {bgShown=bgPinned;continue}
				if op=="show" && bgWaiting!=nil && bgWaiting.id==id {
					frame:=bgWaiting;bgWaiting=nil
					if status!="ok" {bgRestoring=false;bgRestoreRetry=time.Now().Add(3*time.Second);continue}
					bgShown=frame
					if frame.restore && bgRestoring && bgOperation==nil {
						if writeManualJSON(c,map[string]any{"type":"image_preview","op":"pin","id":id})!=nil{return}
						bgOperation=&backgroundOperation{op:"pin",id:id,stage:"ack",deadline:time.Now().Add(8*time.Second),frame:frame}
					}
					continue
				}
				if bgOperation==nil || bgOperation.stage!="ack" || bgOperation.op!=op || bgOperation.id!=id {continue}
				operation:=bgOperation
				if operation.event==nil {bgOperation=nil;bgRestoring=false;if status=="ok"{bgPinned=operation.frame;bgNeedsRestore=false;bgRestoreAttempts=0}else{bgShown=nil;bgRestoreRetry=time.Now().Add(3*time.Second)};continue}
				if status!="ok" {
					bgToken++;operation.token=bgToken;operation.stage="rollback";operation.deadline=time.Now().Add(6*time.Second)
					rollback:=bgSaved;if operation.wasUncertain {rollback=nil;bgSaved=nil;bgNeedsRestore=false}
					if !queueBG("commit",rollback,bgToken) {bgOperation=nil;bgUncertain=true;if operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {pendingTool=&manualToolResult{event:*operation.event,result:`{"success":false,"status":"uncertain"}`}}};continue
				}
				if op=="clear" {bgShown=nil;bgPinned=nil;bgSaved=nil;bgNeedsRestore=false}
				if op=="pin" {bgPinned=operation.frame}
				bgToken++;operation.token=bgToken;operation.stage="commit";operation.deadline=time.Now().Add(6*time.Second)
				var pixels []byte;if op=="pin" {pixels=operation.frame.pixels}
				if !queueBG("commit",pixels,bgToken) {bgOperation=nil;bgUncertain=true;if operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {pendingTool=&manualToolResult{event:*operation.event,result:`{"success":false,"status":"uncertain"}`}}}
			case "heartbeat":
			case "abort":
				if failProvider() != nil { return }
			case "listen":
				state, _ := msg["state"].(string)
				id, e := listenIDFrom(msg)
				if e != nil {
					return
				}
				if state == "pause" {
					if id != listenID || listenID == 0 {
						continue
					}
					old := currentGeneration
					voiceDiagnosticsCurrent.emit(voicePause, voiceTouchPause, voiceSession, id, 0)
					standbyReason = voiceTouchPause
					endTurn()
					// Empty input has no outstanding commit/clear and can safely
					// enter standby without paying the reconnect latency.
					if !busy && !gate.pending && !vad.active && rt.Standby(id) == nil {
						if resetTurn(true) != nil { return }
						continue
					}
					cancelImage(imageCancelListenPause)
					if resetTurn(true) != nil {
						return
					}
					currentGeneration = 0
					if old != 0 {
						rt.Invalidate(old)
					}
					continue
				}
				if state != "start" {
					continue
				}
				if currentGeneration == 0 {
					if writeManualJSON(c, map[string]any{"type": "standby", "listen_id": id}) != nil {
						return
					}
					voiceDiagnosticsCurrent.emit(voiceStandby, voiceGenerationLost, voiceSession, id, 0)
					continue
				}
				if busy || gate.pending {
					standbyReason = voiceUnexpectedStart
					if failProvider() != nil {
						return
					}
					continue
				}
				if rt.Begin(id) != nil {
					cancelImage(imageCancelListenBegin)
					old := currentGeneration
					endTurn()
					standbyReason = voiceBeginFailure
					if resetTurn(true) != nil {
						return
					}
					currentGeneration = 0
					rt.Invalidate(old)
					continue
				}
				listenID = id
				voiceDiagnosticsCurrent.emit(voiceListen, voiceNoReason, voiceSession, id, 0)
				if turnCancel != nil {
					turnCancel()
				}
				_, turnCancel = context.WithCancel(ctx)
				vad.reset()
				decoder, e = newOpusDecoder()
				if e != nil {
					return
				}
				lastActivity = time.Now()
				encoder = nil
				encoderResponse = ""
				frames = nil
				ttsStarted = false
				ttsRequested = time.Time{}
				finalResponse = ""
				finalDone = false
				audioDone = map[string]bool{}
				sawAudio = map[string]bool{}
				if noticeWaitingNext {
					noticeWaitingNext=false;buffer:=noticeAudio;noticeAudio=nil
					if replayNotice(buffer)!=nil {if failProvider()!=nil{return}}
				}
			case "mcp":
				if bridge != nil {
					bridge.handleReply(msg)
				}
			}
		case event := <-rt.Events():
			if bgEnabled && (bgLeaseRef==nil || bgLeaseRef.Load()!=bgLease) {return}
			if event.Generation != currentGeneration || event.ListenID != listenID || listenID == 0 || currentGeneration == 0 {
				continue
			}
			// Count only useful provider events, never heartbeats or stale events.
			if busy && (event.Kind == "audio" || event.Kind == "audio_done" || event.Kind == "done" || event.Kind == "tool") {
				lastProgress = time.Now()
			}
			if noticeActive && noticeCancelling && event.Kind=="done" && event.Final {event.Kind="notice_cancelled"}
			switch event.Kind {
			case "notice_cancelled":
				if !noticeActive || !noticeCancelling {continue}
				if finishNoticeCancellation(event.ResponseID)!=nil {if failProvider()!=nil{return}}
				continue
			case "cleared":
				buffer, e := gate.acknowledge(time.Now())
				if e != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				for _, pcm := range buffer {
					if feed(pcm) != nil {
						if failProvider() != nil {
							return
						}
						break
					}
				}
			case "audio":
				if noticeActive && noticeCancelling {continue}
				if encoder == nil {
					encoder, err = newOpusStreamEncoder()
					if err != nil {
						if failProvider() != nil {
							return
						}
						continue
					}
					encoderResponse = event.ResponseID
				} else if encoderResponse != event.ResponseID {
					if failProvider() != nil {
						return
					}
					continue
				}
				encoded, e := encoder.Encode(event.PCM)
				if e != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				if len(frames)+len(encoded) > frameQueueSize {
					if failProvider() != nil {
						return
					}
					continue
				}
				for _, f := range encoded {
					frames = append(frames, manualAudioFrame{event.Generation, event.ResponseID, f})
				}
				sawAudio[event.ResponseID] = true
				if ttsRequested.IsZero() {
					ttsRequested = time.Now()
				}
			case "audio_done":
				if noticeActive && noticeCancelling {continue}
				if encoder != nil {
					if encoderResponse != event.ResponseID {
						if failProvider() != nil {
							return
						}
						continue
					}
					encoded := encoder.Flush()
					if len(frames)+len(encoded) > frameQueueSize {
						if failProvider() != nil {
							return
						}
						continue
					}
					for _, f := range encoded {
						frames = append(frames, manualAudioFrame{event.Generation, event.ResponseID, f})
					}
					encoder = nil
					encoderResponse = ""
				}
				audioDone[event.ResponseID] = true
			case "done":
				if event.Final {
					finalDone = true
					finalResponse = event.ResponseID
					if !sawAudio[event.ResponseID] {
						audioDone[event.ResponseID] = true
					}
				} else {
					intermediateDone = event.ResponseID
				}
			case "tool":
				if event.ToolName=="pin_image_background" || event.ToolName=="clear_image_background" {
					if !bgEnabled || bgOperation!=nil || len(event.Args)!=0 {
						pendingTool=&manualToolResult{event:event,result:`{"success":false}`};continue
					}
					op:="pin";id:=0;var frame *backgroundFrame
					if event.ToolName=="pin_image_background" {
						if bgShown==nil {pendingTool=&manualToolResult{event:event,result:`{"success":false}`};continue};frame=bgShown;id=frame.id
					} else {
						op="clear";cancelImage(imageCancelSuperseded);id=nextBackgroundID()
					}
					bgToken++;e:=event;bgOperation=&backgroundOperation{op:op,id:id,stage:"prepare",token:bgToken,deadline:time.Now().Add(6*time.Second),event:&e,frame:frame,wasUncertain:bgUncertain}
					if !queueBG("prepare",nil,bgToken) {bgOperation=nil;pendingTool=&manualToolResult{event:event,result:`{"success":false}`}}
					continue
				}
				if event.ToolName == "show_image" {
					enabled, _ := parent.Value(imagePreviewContextKey{}).(bool)
					query, valid := imageSearchQuery(event.Args)
					if !enabled || !valid {
						pendingTool = &manualToolResult{event:event, result:`{"accepted":false}`}
						continue
					}
					if !imageStarted.IsZero() && (imageCancel != nil || preview != nil || imageAwaitingSpeech) {
						recordImageCancel(imageCancelSuperseded, imagePhase, diagnosticSession, imageSequence, time.Since(imageStarted))
					}
					if imageCancel != nil { imageCancel() }
					imageSequence++
					seq := imageSequence
					imageStarted = time.Now()
					imageNotice="";imageRequestListenID=listenID
					imagePhase = imagePhaseSearch
					imageAwaitingSpeech = true
					recordImageEvent(imageSearchStarted, diagnosticSession, seq, 0)
					preview = nil
					bgWaiting=nil;if bgRestoring {bgNeedsRestore=true;bgRestoring=false}
					pendingTool = &manualToolResult{event:event, result:`{"accepted":true,"status":"searching","source":"existing_images","display_confirmed":false}`}
					var searchCtx context.Context
					searchCtx, imageCancel = context.WithTimeout(ctx, 25*time.Second)
					fetcher := imageSearchFetcher(fetchImageSearch)
					if custom, ok := parent.Value(imageSearchFetcherKey{}).(imageSearchFetcher); ok { fetcher = custom }
					go func() {
						pixels, e := fetcher(searchCtx, query)
						select { case imageResults <- imageSearchResult{sequence: seq, pixels: pixels, err: e}: case <-ctx.Done(): }
					}()
					continue
				}
				if bridge != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				toolEvent := event
				if nextMCPID >= int(^uint32(0)>>1) {
					if failProvider() != nil {
						return
					}
					continue
				}
				nextMCPID++
				bridge = newDeviceMCPBridge(func(v map[string]any) error {
					request := manualToolSend{event: toolEvent, data: v, ack: make(chan error, 1)}
					select {
					case toolSends <- request:
					case <-ctx.Done():
						return ctx.Err()
					default:
						return errManualGuard
					}
					select {
					case e := <-request.ack:
						return e
					case <-ctx.Done():
						return ctx.Err()
					}
				}, sessionID)
				bridge.nextID = nextMCPID - 1
				b := bridge
				toolCtx, toolCancel := context.WithCancel(ctx)
				if turnCancel != nil {
					turnCancel()
				}
				turnCancel = toolCancel
				go func() {
					result, e := b.call(toolCtx, toolEvent.ToolName, toolEvent.Args)
					select {
					case toolResults <- manualToolResult{event: toolEvent, result: result, err: e}:
					case <-ctx.Done():
					}
				}()
			}
		case request := <-toolSends:
			if request.event.Generation != currentGeneration || request.event.ListenID != listenID || request.event.ResponseID == "" || currentGeneration == 0 {
				request.ack <- errManualGuard
				continue
			}
			e := rt.DeviceWrite(request.event.Generation, listenID, request.event.ResponseID, func() error { return writeManualJSON(c, request.data) })
			request.ack <- e
			if e != nil {
				if failProvider() != nil {
					return
				}
				continue
			}
		case result := <-toolResults:
			if result.event.Generation != currentGeneration || result.event.ListenID != listenID || currentGeneration == 0 {
				continue
			}
			if busy { lastProgress = time.Now() }
			// A missing/failed battery probe is a normal unavailable reading.
			// Return that to the model instead of dropping the user's turn.
			if result.err != nil && result.event.ToolName == "get_battery_estimate" {
				result.err = nil
				result.result = `{"available":false}`
			}
			if result.err != nil && (result.event.ToolName == "enter_rest" || result.event.ToolName == "get_rest_status" || result.event.ToolName == "wake_from_rest") {
				// The command might have reached the device even if its reply was
				// lost. Preserve the voice turn, but never fabricate confirmation.
				result.err = nil
				result.result = `{"success":false,"reason":"unconfirmed"}`
			}
			if result.err != nil && (result.event.ToolName == "set_alarm" || result.event.ToolName == "set_alarm_after" || result.event.ToolName == "list_alarms" || result.event.ToolName == "delete_alarm" || result.event.ToolName == "set_alarm_volume_floor") {
				result.err = nil
				result.result = `{"ok":false,"reason":"device_failed_or_unavailable"}`
				if result.event.ToolName == "set_alarm" || result.event.ToolName == "set_alarm_after" { result.result = `{"ok":false,"reason":"unconfirmed_may_have_been_saved"}` }
			}
			if result.err != nil {
				cancelImage(imageCancelOtherTool)
				old := currentGeneration
				endTurn()
				if resetTurn(true) != nil {
					return
				}
				currentGeneration = 0
				rt.Invalidate(old)
				continue
			}
			pendingTool = &result
			bridge = nil
		case result := <-imageResults:
			if result.sequence != imageSequence { continue }
			if imageCancel != nil { imageCancel(); imageCancel = nil }
			if result.err != nil || len(result.pixels) != imageFrameBytes {
				imagePhase = imagePhaseFailed
				recordImageEvent(imageSearchFailed, diagnosticSession, imageSequence, time.Since(imageStarted))
				imageNotice="unavailable";if errors.Is(result.err,errImageNoResults) {imageNotice="no_results"};imageNoticeUntil=time.Now().Add(15*time.Second)
				if writeManualJSON(c, map[string]any{"type":"alert", "status":"Картинка", "message":"Не удалось найти картинку", "emotion":"neutral"}) != nil { return }
				continue
			}
			pixels:=result.pixels
			if bgEnabled {var e error; pixels,e=roundImageCornersRGB565(pixels);if e!=nil {continue}}
			displayPixels, err := imagePreviewToDisplayRGB565(pixels)
			if err != nil {
				imagePhase = imagePhaseFailed
				recordImageEvent(imageSearchFailed, diagnosticSession, imageSequence, time.Since(imageStarted))
				if writeManualJSON(c, map[string]any{"type":"alert", "status":"Картинка", "message":"Не удалось подготовить картинку", "emotion":"neutral"}) != nil { return }
				continue
			}
			recordImageEvent(imageSearchReady, diagnosticSession, imageSequence, time.Since(imageStarted))
			imagePhase = imagePhaseReady
			preview = &imagePreviewDemo{pixels: displayPixels, id: int(result.sequence%2147483646)+2}
			if bgEnabled {preview.durationMS=30000;preview.id=nextBackgroundID()}
		case opened:=<-bgOpen:
			bgOpenPending=false
			if opened.lease==0 {bgOpenRetry=time.Now().Add(5*time.Second);continue}
			bgLease=opened.lease;bgLeaseRef=opened.leaseRef;bgEnabled=true
			if opened.err==nil && opened.pixels!=nil {bgSaved=opened.pixels;bgNeedsRestore=true}
			if bg.ready!=nil {select {case bg.ready<-struct{}{}:default:}}
		case disk:=<-bgDisk:
			if bgEnabled && (bgLeaseRef==nil || bgLeaseRef.Load()!=bgLease) {return}
			if disk.operation=="read" {
				if disk.token==bgToken && disk.err==nil && disk.pixels!=nil {bgSaved=disk.pixels;bgNeedsRestore=true};continue
			}
			if bgOperation==nil || bgOperation.token!=disk.token || (bgOperation.stage!=disk.operation && !(bgOperation.stage=="rollback"&&disk.operation=="commit")) {continue}
			operation:=bgOperation
			if disk.err!=nil {bgOperation=nil;bgUncertain=true;if operation.event!=nil && operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {result:=`{"success":false}`;if operation.stage!="prepare" {result=`{"success":false,"status":"uncertain"}`};pendingTool=&manualToolResult{event:*operation.event,result:result}};continue}
			if disk.operation=="prepare" {
				if writeManualJSON(c,map[string]any{"type":"image_preview","op":operation.op,"id":operation.id})!=nil{return}
				operation.stage="ack";operation.deadline=time.Now().Add(8*time.Second);continue
			}
			bgOperation=nil
			if operation.stage=="rollback" {bgUncertain=operation.wasUncertain;if operation.event!=nil && operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {pendingTool=&manualToolResult{event:*operation.event,result:`{"success":false}`}};continue}
			bgUncertain=false
			if operation.op=="pin" {bgSaved=operation.frame.pixels;bgPinned=operation.frame;bgNeedsRestore=false}
			if operation.event!=nil && operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {pendingTool=&manualToolResult{event:*operation.event,result:`{"success":true}`}}
		case <-ticker.C:
			if noticeWaitingNext && time.Now().After(noticeWaitDeadline) {if failProvider()!=nil{return};continue}
			if bgConfigured && !bgEnabled && !bgOpenPending && !time.Now().Before(bgOpenRetry) {openBG()}
			if bgEnabled && (bgLeaseRef==nil || bgLeaseRef.Load()!=bgLease) {return}
			if imageNotice!="" && time.Now().After(imageNoticeUntil) {imageNotice=""}
			if imageNotice!="" && listenID!=0 && listenID!=imageRequestListenID && !busy && !imageAwaitingSpeech && !vad.active && !gate.pending {
				e:=rt.NotifyImageResult(listenID,imageNotice)
				if e==nil {imageNotice="";busy=true;noticeActive=true;noticeCancelling=false;noticeWaitingNext=false;noticeAudio=nil;noticeVAD.reset();lastProgress=time.Now()} else if !errors.Is(e,errManualNotIdle) {imageNotice="";if failProvider()!=nil{return}}
			}
			if bgWaiting!=nil && time.Now().After(bgWaiting.deadline) {if bgWaiting.restore {bgRestoring=false;bgNeedsRestore=true;bgRestoreRetry=time.Now().Add(3*time.Second)};bgWaiting=nil}
			if bgEnabled && bgNeedsRestore && bgSaved!=nil && !bgRestoring && bgRestoreAttempts<3 && !time.Now().Before(bgRestoreRetry) && preview==nil && bgWaiting==nil && bgOperation==nil && imageCancel==nil && !busy && bgShown==nil {
				preview=&imagePreviewDemo{pixels:bgSaved,id:nextBackgroundID(),durationMS:30000};bgRestoring=true;bgRestoreAttempts++
			}
			if bgOperation!=nil && time.Now().After(bgOperation.deadline) {
				operation:=bgOperation;bgOperation=nil;bgRestoring=false;if operation.event==nil {bgShown=nil;bgRestoreRetry=time.Now().Add(3*time.Second)} else {bgUncertain=true}
				if operation.event!=nil && operation.event.Generation==currentGeneration && operation.event.ListenID==listenID && listenID!=0 {result:=`{"success":false}`;if operation.stage!="prepare" {result=`{"success":false,"status":"uncertain"}`};pendingTool=&manualToolResult{event:*operation.event,result:result}}
			}
			if preview != nil && !imageAwaitingSpeech {
				if frame := preview.next(); frame != nil {
					if writeManualJSON(c, frame) != nil { return }
					if frame["op"] == "begin" && !bgRestoring && !imageStarted.IsZero() { imagePhase = imagePhaseTransfer; recordImageEvent(imageTransferBegin, diagnosticSession, imageSequence, time.Since(imageStarted)) }
					if frame["op"] == "end" { if !bgRestoring && !imageStarted.IsZero() {imagePhase = imagePhaseAfterEnd; recordImageEvent(imageTransferEnd, diagnosticSession, imageSequence, time.Since(imageStarted))};if bgEnabled {bgWaiting=&backgroundFrame{id:preview.id,pixels:preview.pixels,restore:bgRestoring,deadline:time.Now().Add(8*time.Second)}};preview=nil }
				} else {
					preview = nil
				}
			}
			if busy && !lastProgress.IsZero() && progressTimeout > 0 && time.Since(lastProgress) >= progressTimeout {
				standbyReason = voiceProgressTimeout
				if failProvider() != nil { return }
				continue
			}
			if gate.pending && !time.Now().Before(gate.deadline) {
				standbyReason = voiceClearTimeout
				cancelImage(imageCancelClearTimeout)
				old := currentGeneration
				endTurn()
				if resetTurn(true) != nil {
					return
				}
				currentGeneration = 0
				rt.Invalidate(old)
				continue
			}
			if !ttsStarted && len(frames) > 0 && (len(frames) >= 5 || (!ttsRequested.IsZero() && time.Since(ttsRequested) >= 900*time.Millisecond) || finalDone) {
				if rt.DeviceWrite(frames[0].generation, listenID, frames[0].responseID, func() error {
					return writeManualJSON(c, map[string]any{"type": "tts", "state": "start", "listen_id": listenID})
				}) != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				ttsStarted = true
			}
			if ttsStarted && len(frames) > 0 {
				frame := frames[0]
				frames = frames[1:]
				if rt.DeviceWrite(frame.generation, listenID, frame.responseID, func() error { return writeManualAudio(c, frame.data) }) != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				if busy { lastProgress = time.Now() }
			}
			// Keep the guard on the old response until its audio has been sent.
			// ToolResult can cause response.create and change the guarded ID.
			if pendingTool != nil && intermediateDone == pendingTool.event.ResponseID &&
				(!sawAudio[intermediateDone] || audioDone[intermediateDone]) && encoder == nil && len(frames) == 0 {
				result := pendingTool
				pendingTool = nil
				intermediateDone = ""
				if rt.ToolResult(listenID, result.event.ResponseID, result.event.CallID, result.result) != nil {
					if failProvider() != nil { return }
					continue
				}
			}
			if finalDone && audioDone[finalResponse] && len(frames) == 0 {
				// Firmware accepts tts:stop only after entering Speaking. Even an
				// empty successful response needs the paired start transition.
				if !ttsStarted {
					if rt.DeviceWrite(currentGeneration, listenID, finalResponse, func() error {
						return writeManualJSON(c, map[string]any{"type":"tts","state":"start","listen_id":listenID})
					}) != nil { return }
					ttsStarted = true
				}
				if rt.FinishPlayback(listenID, finalResponse, func(id uint32) error {
					return writeManualJSON(c, map[string]any{"type": "tts", "state": "stop", "listen_id": id})
				}) != nil {
					if failProvider() != nil {
						return
					}
					continue
				}
				if imageAwaitingSpeech {
					imageAwaitingSpeech = false
					recordImageEvent(imageSpeechSent, diagnosticSession, imageSequence, time.Since(imageStarted))
				}
				ttsStarted = false
				noticeActive=false;noticeCancelling=false;noticeAudio=nil
				finalDone = false
				busy = false
				lastProgress = time.Time{}
				endTurn()
				lastActivity = time.Now()
			}
			if listenID != 0 && !busy && time.Since(lastActivity) > time.Duration(idleSeconds)*time.Second {
				standbyReason = voiceIdleTimeout
				id := listenID
				endTurn()
				if err := rt.Standby(id); err != nil {
					cancelImage(imageCancelStandby)
					old := currentGeneration
					if resetTurn(true) != nil {
						return
					}
					currentGeneration = 0
					rt.Invalidate(old)
					continue
				}
				if resetTurn(true) != nil {
					return
				}
			}
		case <-heartbeat.C:
			if writeManualJSON(c, map[string]any{"type": "heartbeat"}) != nil {
				return
			}
		}
	}
}
