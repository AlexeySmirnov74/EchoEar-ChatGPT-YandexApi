package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestImageCancelDiagnosticsClosedVocabulary(t *testing.T) {
	dir := t.TempDir()
	s := imageDiagnosticSink{marker: filepath.Join(dir, "enabled"), path: filepath.Join(dir, "events"), maxBytes: 4096}
	if err := os.WriteFile(s.marker, nil, 0600); err != nil { t.Fatal(err) }
	s.recordAtDetails(imageSearchCancelled, 1, 2, time.Second, time.Now(), imageCancelVoice, imagePhaseTransfer)
	s.recordAtDetails(imageSearchCancelled, 1, 2, time.Second, time.Now(), imageCancelActorClosed, imagePhaseAfterEnd)
	s.recordAtDetails(imageSearchCancelled, 1, 2, time.Second, time.Now(), imageCancelReason(250), imagePhaseTransfer)
	s.recordAtDetails(imageSearchCancelled, 1, 2, time.Second, time.Now(), imageCancelVoice, imageCancelPhase(250))
	s.recordAtDetails(imageSearchReady, 1, 2, time.Second, time.Now(), imageCancelVoice, imagePhaseTransfer)
	data, err := os.ReadFile(s.path); if err != nil { t.Fatal(err) }
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 { t.Fatalf("invalid metadata accepted, got %d records", len(lines)) }
	for i, want := range []struct{ reason, phase string }{{"voice_activity", "transfer"}, {"actor_closed", "after_end"}} {
		var got imageDiagnosticRecord
		if err := json.Unmarshal(lines[i], &got); err != nil { t.Fatal(err) }
		if got.Reason != want.reason || got.Phase != want.phase { t.Fatalf("unexpected metadata: %+v", got) }
	}
}

func TestImageActorCloseReportsIncompleteSearch(t *testing.T) {
	entries := make(chan imageDiagnosticEntry, 16)
	dispatcher := newImageDiagnosticDispatcher(16, func(e imageDiagnosticEntry) { entries <- e })
	old := imageDiagnosticDispatcherCurrent.Swap(dispatcher)
	defer func() { imageDiagnosticDispatcherCurrent.Store(old); dispatcher.stop() }()
	m := &imageActorManager{fakeActorManager: &fakeActorManager{states:make(chan manualProviderState,4), events:make(chan manualRealtimeEvent,16), failures:make(chan uint64,1), started:make(chan uint32,4)}, results:make(chan string,2)}
	started, done := make(chan struct{}), make(chan struct{})
	fetch := imageSearchFetcher(func(ctx context.Context, _ string)([]byte,error) { close(started); <-ctx.Done(); return nil, ctx.Err() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		c,err:=upgrader.Upgrade(w,r,nil); if err!=nil{return}; defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session"); close(done)
	})); defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil); if err!=nil{t.Fatal(err)}
	m.ready(1); deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select {case <-m.started: case <-time.After(time.Second): t.Fatal("listen not started")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	select {case <-started: case <-time.After(time.Second): t.Fatal("search not started")}
	_ = d.Close()
	select {case <-done: case <-time.After(3*time.Second): t.Fatal("actor failed to close")}
	deadline:=time.After(time.Second)
	for {select {
	case entry:= <-entries:
		if entry.event == imageSearchCancelled { if entry.reason != imageCancelActorClosed || entry.phase != imagePhaseSearch { t.Fatalf("unexpected cancellation: %+v",entry) }; return }
	case <-deadline: t.Fatal("no actor-close diagnostic")
	}}
}

func TestImageTransferVoiceCancellationHasTransferPhase(t *testing.T) {
	entries := make(chan imageDiagnosticEntry, 32)
	dispatcher := newImageDiagnosticDispatcher(32, func(e imageDiagnosticEntry) { entries <- e })
	old := imageDiagnosticDispatcherCurrent.Swap(dispatcher)
	defer func() { imageDiagnosticDispatcherCurrent.Store(old); dispatcher.stop() }()
	m := &imageActorManager{fakeActorManager: &fakeActorManager{states:make(chan manualProviderState,4), events:make(chan manualRealtimeEvent,16), failures:make(chan uint64,1), started:make(chan uint32,4), committed:make(chan struct{},1)}, results:make(chan string,2)}
	fetch := imageSearchFetcher(func(context.Context,string)([]byte,error) { return bytes.Repeat([]byte{5},imageFrameBytes),nil })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		c,err:=upgrader.Upgrade(w,r,nil); if err!=nil{return}; defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	})); defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil); if err!=nil{t.Fatal(err)}; defer d.Close()
	m.ready(1); deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select {case <-m.started: case <-time.After(time.Second): t.Fatal("listen not started")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select {case <-m.results: case <-time.After(time.Second): t.Fatal("tool result missing")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(3*time.Second))
	for { _,data,e:=d.ReadMessage(); if e!=nil{t.Fatal(e)}; var msg map[string]any; _=json.Unmarshal(data,&msg); if msg["type"]=="tts"&&msg["state"]=="stop" { break } }
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select {case <-m.started: case <-time.After(time.Second): t.Fatal("second listen not started")}
	for { _,data,e:=d.ReadMessage(); if e!=nil{t.Fatal(e)}; var msg map[string]any; _=json.Unmarshal(data,&msg); if msg["type"]=="image_preview"&&msg["op"]=="begin" { break } }
	commitActorSpeech(t,d,m.fakeActorManager)
	deadline:=time.After(time.Second)
	for {select {
	case entry:= <-entries:
		if entry.event == imageSearchCancelled { if entry.reason != imageCancelVoice || entry.phase != imagePhaseTransfer { t.Fatalf("unexpected cancellation: %+v",entry) }; return }
	case <-deadline: t.Fatal("no voice cancellation diagnostic")
	}}
}
