package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestImageQueryValidation(t *testing.T) {
	for _, tc := range []struct{ args map[string]any; valid bool }{
		{map[string]any{"query":" зайчик "},true}, {map[string]any{"query":""},false},
		{map[string]any{"query":"x\ny"},false}, {map[string]any{"query":strings.Repeat("a",201)},false},
		{map[string]any{"query":"cat","other":true},false}, {map[string]any{"query":7},false},
	} { _, valid := imageSearchQuery(tc.args); if valid != tc.valid { t.Fatalf("unexpected query validation result") } }
}

func TestImageSearchUnixContract(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "search.sock")
	listener, err := net.Listen("unix",socket); if err != nil { t.Skipf("unix sockets unavailable: %v",err) }
	defer listener.Close()
	reply := bytes.Repeat([]byte{23},imageFrameBytes)
	status := 200
	var mu sync.Mutex
	server := &http.Server{Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		if r.Method!="POST" || r.URL.Path!="/search" || r.Header.Get("Content-Type")!="application/json" { t.Error("invalid request") }
		var body map[string]string; if json.NewDecoder(r.Body).Decode(&body)!=nil || body["query"]!="зайчик" {t.Error("invalid body")}
		mu.Lock(); bodyReply, code := append([]byte(nil), reply...), status; mu.Unlock()
		w.Header().Set("Content-Type","application/octet-stream"); w.WriteHeader(code); _,_=w.Write(bodyReply)
	})}
	go server.Serve(listener); defer server.Close()
	ctx,cancel:=context.WithTimeout(context.Background(),time.Second); defer cancel()
	data,err:=fetchImageSearchAt(ctx,socket,"зайчик"); if err!=nil || !bytes.Equal(data,reply) {t.Fatalf("valid image: %v",err)}
	mu.Lock(); reply=reply[:len(reply)-1]; mu.Unlock()
	if _,err=fetchImageSearchAt(ctx,socket,"зайчик");err==nil {t.Fatal("accepted short frame")}
	mu.Lock(); reply=append(reply,23,23); mu.Unlock()
	if _,err=fetchImageSearchAt(ctx,socket,"зайчик");err==nil {t.Fatal("accepted oversized frame")}
	mu.Lock(); status=503; mu.Unlock()
	if _,err=fetchImageSearchAt(ctx,socket,"зайчик");err==nil {t.Fatal("accepted failure status")}
}

type imageActorManager struct { *fakeActorManager; results chan string }
func (m *imageActorManager) ToolResult(_ uint32,_,_,result string) error {m.results<-result;return nil}
type imageBrokenBeginManager struct { *imageActorManager }
func (m *imageBrokenBeginManager) Begin(id uint32) error { if id == 2 { return errManualGuard }; return m.imageActorManager.Begin(id) }

func TestImageSearchDoesNotBlockAudioAndSuppressesOldResult(t *testing.T) {
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,4)}
	entered:=make(chan string,2); releaseOld:=make(chan struct{}); releaseNew:=make(chan struct{})
	fetch:=imageSearchFetcher(func(_ context.Context, query string)([]byte,error){
		entered<-query
		if query=="old" { <-releaseOld; return bytes.Repeat([]byte{1},imageFrameBytes),nil }
		<-releaseNew; return bytes.Repeat([]byte{2},imageFrameBytes),nil
	})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"old",ToolName:"show_image",Args:map[string]any{"query":"old"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case result:=<-m.results:if !strings.Contains(result,`"accepted":true`){t.Fatal(result)};case<-time.After(time.Second):t.Fatal("tool acknowledgement blocked")}
	select{case <-entered:case<-time.After(time.Second):t.Fatal("search not started")}
	// While search is blocked, ordinary answer audio must still make progress.
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"answer",PCM:make([]int16,serverFrameSamples*6)}
	m.events<-manualRealtimeEvent{Kind:"audio_done",Generation:1,ListenID:1,ResponseID:"answer"}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{continue};var msg map[string]any;if json.Unmarshal(data,&msg)!=nil{t.Fatal("bad json")};if msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("next listen blocked")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:2,ResponseID:"resp2",CallID:"new",ToolName:"show_image",Args:map[string]any{"query":"new"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:2,ResponseID:"resp2",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("second tool blocked")}
	select{case <-entered:case<-time.After(time.Second):t.Fatal("second search not started")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:2,ResponseID:"answer2",Final:true}
	close(releaseOld); close(releaseNew)
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{continue};var msg map[string]any;_ = json.Unmarshal(data,&msg);if msg["type"]=="image_preview" && msg["op"]=="begin" {if msg["id"]!=float64(4){t.Fatalf("stale image: %v",msg)};break}}
}

func TestImageTransferWaitsForSpeechToFinish(t *testing.T) {
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)}
	ready:=make(chan struct{})
	fetch:=imageSearchFetcher(func(context.Context,string)([]byte,error){close(ready);return bytes.Repeat([]byte{5},imageFrameBytes),nil})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("tool acknowledgement blocked")}
	select{case <-ready:case<-time.After(time.Second):t.Fatal("search not started")}
	messages:=make(chan string,256)
	go func(){for {kind,data,err:=d.ReadMessage();if err!=nil{return};if kind==websocket.BinaryMessage{continue};var msg map[string]any;if json.Unmarshal(data,&msg)==nil {messages<-fmt.Sprint(msg["type"],"/",msg["state"],"/",msg["op"])}}}()
	select{case msg:=<-messages:t.Fatalf("image sent before reply completed: %s",msg);case <-time.After(300*time.Millisecond):}
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"answer",PCM:make([]int16,serverFrameSamples*6)}
	m.events<-manualRealtimeEvent{Kind:"audio_done",Generation:1,ListenID:1,ResponseID:"answer"}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	timeout:=time.After(3*time.Second);stopped:=false
	for {select{case msg:=<-messages:
		if msg=="tts/stop/<nil>" {stopped=true}
		if msg=="image_preview/<nil>/begin" {if !stopped {t.Fatal("image began before speech stop")};return}
	case <-timeout:t.Fatal("no image after speech stop")}}
}

func TestImageSearchSurvivesSpeechCompletionAndNextListen(t *testing.T) {
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)}
	entered:=make(chan struct{}); release:=make(chan struct{})
	fetch:=imageSearchFetcher(func(ctx context.Context,_ string)([]byte,error){
		close(entered)
		select {case <-release:return bytes.Repeat([]byte{5},imageFrameBytes),nil;case <-ctx.Done():return nil,ctx.Err()}
	})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"зайчик"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("tool acknowledgement blocked")}
	select{case <-entered:case<-time.After(time.Second):t.Fatal("search not started")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{continue};var msg map[string]any;if json.Unmarshal(data,&msg)!=nil{t.Fatal("bad json")};if msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("next listen blocked")}
	close(release)
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{continue};var msg map[string]any;_ = json.Unmarshal(data,&msg);if msg["type"]=="image_preview"&&msg["op"]=="begin"{break};if msg["type"]=="alert"{t.Fatal("search was cancelled after speech",msg)}}
}

func TestImageSearchCancelledOnBeginFailure(t *testing.T) {
	base:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)}
	m:=&imageBrokenBeginManager{base}
	entered:=make(chan struct{}); cancelled:=make(chan struct{})
	fetch:=imageSearchFetcher(func(ctx context.Context,_ string)([]byte,error){close(entered); <-ctx.Done();close(cancelled);return nil,ctx.Err()})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"зайчик"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("tool acknowledgement blocked")}
	select{case <-entered:case<-time.After(time.Second):t.Fatal("search not started")}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case <-cancelled:case<-time.After(time.Second):t.Fatal("provider failure did not cancel search")}
}

func TestImageSearchCancelledByNewSpeech(t *testing.T) {
	m:=&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)}
	entered:=make(chan struct{}); cancelled:=make(chan struct{})
	fetch:=imageSearchFetcher(func(ctx context.Context,_ string)([]byte,error){close(entered); <-ctx.Done();close(cancelled);return nil,ctx.Err()})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},fetch)
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case <-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"зайчик"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case <-m.results:case<-time.After(time.Second):t.Fatal("tool acknowledgement blocked")}
	select{case <-entered:case<-time.After(time.Second):t.Fatal("search not started")}
	commitActorSpeech(t,d,m.fakeActorManager)
	select{case <-cancelled:case<-time.After(time.Second):t.Fatal("new speech did not cancel old image search")}
}
