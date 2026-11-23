package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Regression: a returning device should restore the saved background even if
// the user speaks during the chunked transfer. Speech must continue to work.
func TestBackgroundReviewSpeechDuringRestore(t *testing.T) {
	store := &backgroundStore{dir:t.TempDir()}
	lease,err:=store.acquire("device");if err!=nil{t.Fatal(err)}
	if err=store.write("device",lease,bytes.Repeat([]byte{0xf8,0},imageFrameBytes/2));err!=nil{t.Fatal(err)}
	store.release("device",lease)
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done)
		c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store})
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){_ = d.Close();select{case <-done:case <-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case <-time.After(time.Second):t.Fatal("listen failed")}
	messages:=make(chan string,512)
	go func(){defer close(messages);for {_,raw,e:=d.ReadMessage();if e!=nil{return};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="image_preview"{messages<-msg["op"].(string)}}}()
	select{case op:=<-messages:if op!="begin"{t.Fatalf("expected begin: %s",op)};case <-time.After(3*time.Second):t.Fatal("restore did not start")}
	commitActorSpeech(t,d,m.fakeActorManager)
	deadline:=time.After(10*time.Second)
	for {select{case op,ok:=<-messages:if !ok{t.Fatal("connection closed")};if op=="end"{return};case <-deadline:t.Fatal("background restoration was cancelled by speech")}}
}

// A stalled disk operation must not block the actor's audio input path.
func TestBackgroundReviewSpeechWhileDiskBlocked(t *testing.T) {
	ready:=make(chan struct{},1)
	entered:=make(chan struct{});release:=make(chan struct{});var once sync.Once
	unblock:=func(){once.Do(func(){close(release)})};defer unblock()
	store:=&backgroundStore{dir:t.TempDir(),beforeWrite:func(pending bool){if pending {close(entered);<-release}}}
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store,ready:ready})
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){unblock();_ = d.Close();select{case <-done:case <-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case <-time.After(time.Second):t.Fatal("listen failed")}
	select{case <-ready:case <-time.After(2*time.Second):t.Fatal("background did not initialize")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"clear",ToolName:"clear_image_background",Args:map[string]any{}}
	select{case <-entered:case <-time.After(2*time.Second):t.Fatal("disk did not block")}
	commitActorSpeech(t,d,m.fakeActorManager)
}

// A new voice session must start even if another background operation has
// stalled the global disk worker before this device can acquire a lease.
func TestBackgroundReviewNewVoiceSessionWhileDiskBlocked(t *testing.T) {
	entered:=make(chan struct{});release:=make(chan struct{});var once sync.Once
	unblock:=func(){once.Do(func(){close(release)})};defer unblock()
	blocked:=&backgroundStore{dir:t.TempDir(),beforeWrite:func(pending bool){if pending {close(entered);<-release}}}
	lease,err:=blocked.acquire("old");if err!=nil{t.Fatal(err)}
	if !submitBackgroundDisk(backgroundDiskJob{operation:"prepare",device:"old",lease:lease,store:blocked,deadline:time.Now().Add(10*time.Second),reply:make(chan backgroundDiskResult,1)}) {t.Fatal("disk queue full")}
	select{case <-entered:case <-time.After(time.Second):t.Fatal("worker did not block")}
	store:=&backgroundStore{dir:t.TempDir()}
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"new",store:store})
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){unblock();_ = d.Close();select{case <-done:case <-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case <-time.After(time.Second):t.Fatal("voice session waited for disk")}
	commitActorSpeech(t,d,m.fakeActorManager)
}

func TestBackgroundReviewOldSessionCannotSendPreparedCommand(t *testing.T) {
	ready,entered,release:=make(chan struct{},1),make(chan struct{}),make(chan struct{});var once sync.Once
	unblock:=func(){once.Do(func(){close(release)})};defer unblock()
	store:=&backgroundStore{dir:t.TempDir(),beforeWrite:func(pending bool){if pending {close(entered);<-release}}}
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store,ready:ready})
		runExperimentalManualActor(ctx,c,m,"old")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){unblock();_ = d.Close();select{case <-done:case <-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case <-time.After(time.Second):t.Fatal("listen failed")}
	select{case <-ready:case <-time.After(2*time.Second):t.Fatal("background did not initialize")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"clear",ToolName:"clear_image_background",Args:map[string]any{}}
	select{case <-entered:case <-time.After(2*time.Second):t.Fatal("prepare did not block")}
	lease,err:=store.acquire("device");if err!=nil{t.Fatal(err)};defer store.release("device",lease)
	unblock();_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {_,data,e:=d.ReadMessage();if e!=nil{break};var msg map[string]any;if json.Unmarshal(data,&msg)==nil && msg["type"]=="image_preview" {t.Fatal("old actor sent command after session was superseded")}}
	select{case <-done:case <-time.After(time.Second):t.Fatal("old actor did not stop")}
}
// Simulate clear applying on the device with its ACK lost. A later explicit
// device refusal must never restore the pre-clear image over the pending mark.
func TestBackgroundReviewUnknownClearThenFailedPinDoesNotResurrect(t *testing.T) {
	store:=&backgroundStore{dir:t.TempDir()};lease,err:=store.acquire("device");if err!=nil{t.Fatal(err)}
	frame:=bytes.Repeat([]byte{0xf8,0},imageFrameBytes/2)
	if err=store.write("device",lease,frame);err!=nil{t.Fatal(err)};store.release("device",lease)
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,4)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store})
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){_ = d.Close();select{case <-done:case <-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);_ = d.SetReadDeadline(time.Now().Add(35*time.Second))
	var originalID float64
	for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)!=nil||msg["type"]!="image_preview"{continue};if msg["op"]=="begin"{originalID=msg["id"].(float64)};if msg["op"]=="end"{break}}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":originalID,"status":"ok"})
	msg:=deviceReadJSON(t,d);if msg["op"]!="pin"||msg["id"]!=originalID{t.Fatal("restore pin missing",msg)}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":originalID,"status":"ok"})
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case <-time.After(time.Second):t.Fatal("listen not started")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"clear",CallID:"clear",ToolName:"clear_image_background",Args:map[string]any{}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"clear",Final:false}
	msg=deviceReadJSON(t,d);if msg["op"]!="clear"{t.Fatal("clear missing",msg)}
	// The device applies clear but its acknowledgement is deliberately lost.
	select{case result:=<-m.results:
		var status map[string]any;if json.Unmarshal([]byte(result),&status)!=nil||status["status"]!="uncertain" {t.Fatalf("lost ACK must be uncertain: %s",result)}
	case <-time.After(11*time.Second):t.Fatal("no clear timeout result")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"pin",CallID:"pin",ToolName:"pin_image_background",Args:map[string]any{}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"pin",Final:false}
	msg=deviceReadJSON(t,d);if msg["op"]!="pin"{t.Fatal("pin missing",msg)}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":originalID,"status":"error","reason":"no_preview"})
	select{case <-m.results:case <-time.After(4*time.Second):t.Fatal("no pin result")}
	check:=&backgroundStore{dir:store.dir};newLease,e:=check.acquire("device");if e!=nil{t.Fatal(e)}
	pixels,e:=check.read("device",newLease);if e!=nil{t.Fatal(e)}
	if pixels!=nil {t.Fatal("old background resurrected after unknown clear")}
}

