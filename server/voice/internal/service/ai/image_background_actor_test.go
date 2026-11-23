package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestImageBackgroundRestoreAndClearRequireAcknowledgement(t *testing.T) {
	store:=&backgroundStore{dir:t.TempDir()};lease,err:=store.acquire("device");if err!=nil{t.Fatal(err)}
	frame:=bytes.Repeat([]byte{0xf8,0},imageFrameBytes/2)
	if err=store.write("device",lease,frame);err!=nil{t.Fatal(err)};store.release("device",lease)
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)}
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store})
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return bytes.Clone(frame),nil}))
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1)
	if err=d.SetReadDeadline(time.Now().Add(20*time.Second));err!=nil{t.Fatal(err)}
	var id float64
	for {_,data,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(data,&msg)!=nil{continue};if msg["type"]!="image_preview"{continue};if msg["op"]=="begin" {id=msg["id"].(float64);if msg["duration_ms"]!=float64(30000){t.Fatal("incorrect duration")}};if msg["op"]=="end"{break}}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":id+1,"status":"ok"})
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":id,"status":"ok"})
	msg:=deviceReadJSON(t,d);if msg["type"]!="image_preview"||msg["op"]!="pin"||msg["id"]!=id{t.Fatalf("restore pin missing: %v",msg)}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":id,"status":"ok"})
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case<-m.started:case<-time.After(time.Second):t.Fatal("listen not started")}
	_ = d.SetReadDeadline(time.Now().Add(20*time.Second))
	// Show B2 over pinned B1; firmware then reports B1 visible after timeout.
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"search",CallID:"search",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"search",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("no search result")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	var temporaryID float64
	for {_,data,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var x map[string]any;if json.Unmarshal(data,&x)!=nil||x["type"]!="image_preview"{continue};if x["op"]=="begin"{temporaryID=x["id"].(float64)};if x["op"]=="end"{break}}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":temporaryID,"status":"ok"})
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":id,"status":"ok"})
	time.Sleep(150*time.Millisecond)
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"pin-resp",CallID:"pin",ToolName:"pin_image_background",Args:map[string]any{}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"pin-resp",Final:false}
	msg=deviceReadJSON(t,d);if msg["type"]!="image_preview"||msg["op"]!="pin"||msg["id"]!=id{t.Fatalf("pin missing: %v",msg)}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":id+1,"status":"ok"})
	select {case <-m.results:t.Fatal("success before matching pin ACK");case <-time.After(100*time.Millisecond):}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":id,"status":"error","reason":"no_preview"})
	select{case result:=<-m.results:if !strings.Contains(result,`"success":false`){t.Fatal("failed pin reported success",result)};case<-time.After(time.Second):t.Fatal("no failed pin result")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"clear",ToolName:"clear_image_background",Args:map[string]any{}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	msg=deviceReadJSON(t,d);if msg["type"]!="image_preview"||msg["op"]!="clear"{t.Fatalf("clear missing: %v",msg)}
	clearID:=msg["id"].(float64)
	select {case <-m.results:t.Fatal("success before clear ACK");case <-time.After(100*time.Millisecond):}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"clear","id":clearID,"status":"ok"})
	select{case result:=<-m.results:if !strings.Contains(result,`"success":true`){t.Fatal(result)};case<-time.After(time.Second):t.Fatal("no clear result")}
	check:=&backgroundStore{dir:store.dir};other,_:=check.acquire("device");pixels,err:=check.read("device",other);if err!=nil||pixels!=nil{t.Fatal("clear was not persisted",err)}
}

func TestImageBackgroundPinPersistsShownFrameAfterAck(t *testing.T) {
	store:=&backgroundStore{dir:t.TempDir()}
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,4)}
	input:=bytes.Repeat([]byte{0,0xf8},imageFrameBytes/2)
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:"device",store:store})
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return bytes.Clone(input),nil}))
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case<-m.started:case<-time.After(time.Second):t.Fatal("listen not started")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"search",CallID:"search",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"search",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("no search result")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	if err=d.SetReadDeadline(time.Now().Add(20*time.Second));err!=nil{t.Fatal(err)}
	var id float64
	for {_,data,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(data,&msg)!=nil||msg["type"]!="image_preview"{continue};if msg["op"]=="begin"{id=msg["id"].(float64)};if msg["op"]=="end"{break}}
	// A new utterance after end must not erase the still-pending show ACK.
	commitActorSpeech(t,d,m.fakeActorManager)
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"show","id":id,"status":"ok"})
	time.Sleep(150*time.Millisecond)
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"pin",CallID:"pin",ToolName:"pin_image_background",Args:map[string]any{}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"pin",Final:false}
	msg:=deviceReadJSON(t,d);if msg["type"]!="image_preview"||msg["op"]!="pin"||msg["id"]!=id{t.Fatalf("pin missing: %v",msg)}
	check:=&backgroundStore{dir:store.dir};other,_:=check.acquire("device");pixels,err:=check.read("device",other);if err!=nil||pixels!=nil{t.Fatal("frame persisted before pin ACK",err)}
	deviceJSON(t,d,map[string]any{"type":"image_background","op":"pin","id":id,"status":"ok"})
	select{case result:=<-m.results:if !strings.Contains(result,`"success":true`){t.Fatal(result)};case<-time.After(time.Second):t.Fatal("no pin result")}
	pixels,err=check.read("device",other);if err!=nil||len(pixels)!=imageFrameBytes{t.Fatal("frame not persisted",err)}
	if pixels[0]!=0||pixels[1]!=0 {t.Fatal("background corner is not rounded")};i:=(120*240+120)*2;if pixels[i]!=0xf8||pixels[i+1]!=0 {t.Fatal("persisted display byte order incorrect")}
}
