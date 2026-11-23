package ai

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

// The model may take time to start speaking. User speech during this period
// must not be discarded just because the notification was queued.
func TestFeedbackReviewSpeechAfterNoticeRequestBeforeAudio(t *testing.T) {
	m:=&feedbackActorManager{imageActorManager:&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)},notices:make(chan string,2)}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true)
		ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return nil,errImageNoResults}))
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){_ = d.Close();select{case<-done:case<-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case<-m.started:case<-time.After(time.Second):t.Fatal("first listen missing")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case<-m.results:case<-time.After(time.Second):t.Fatal("search tool result missing")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(3*time.Second))
	for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case<-m.started:case<-time.After(time.Second):t.Fatal("second listen missing")}
	select{case<-m.notices:case<-time.After(2*time.Second):t.Fatal("notice missing")}
	// No response.created/audio event is emitted; user starts speaking here.
	commitActorSpeech(t,d,m.fakeActorManager)
}

type delayedCancelActorManager struct {
	*feedbackActorManager
	requested chan struct{}
	release chan struct{}
	mu sync.Mutex
	pcm []int16
}
func (m *delayedCancelActorManager) CancelImageNotice(id uint32)error {
	close(m.requested);go func(){<-m.release;m.events<-manualRealtimeEvent{Kind:"notice_cancelled",Generation:1,ListenID:id,ResponseID:"notice"}}();return nil
}
func (m *delayedCancelActorManager) Append(_ uint32,pcm []int16)error {m.mu.Lock();m.pcm=append(m.pcm,pcm...);m.mu.Unlock();return nil}

func TestFeedbackReviewDelayedCancelKeepsBeginningOfSpeech(t *testing.T) {
	base:=&feedbackActorManager{imageActorManager:&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)},notices:make(chan string,2)}
	m:=&delayedCancelActorManager{feedbackActorManager:base,requested:make(chan struct{}),release:make(chan struct{})}
	var once sync.Once;release:=func(){once.Do(func(){close(m.release)})};defer release()
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close();ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return nil,errImageNoResults}));runExperimentalManualActor(ctx,c,m,"session")}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){_ = d.Close();select{case<-done:case<-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1});select{case<-m.started:case<-time.After(time.Second):t.Fatal("first listen missing")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false};select{case<-m.results:case<-time.After(time.Second):t.Fatal("tool result missing")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(3*time.Second));for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2});select{case<-m.started:case<-time.After(time.Second):t.Fatal("next listen missing")};select{case<-m.notices:case<-time.After(2*time.Second):t.Fatal("notice missing")}
	enc,err:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP);if err!=nil{t.Fatal(err)}
	for frame:=0;frame<75;frame++ {pcm:=make([]int16,deviceFrameSamples);if frame<60 {frequency:=880.0;if frame<5 {frequency=440};for i:=range pcm {pcm[i]=int16(10000*math.Sin(2*math.Pi*frequency*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}};buf:=make([]byte,4096);n,e:=enc.Encode(pcm,buf);if e!=nil{t.Fatal(e)};if e=d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil{t.Fatal(e)}}
	select{case<-m.requested:case<-time.After(time.Second):t.Fatal("cancel not requested")};time.Sleep(3*time.Second);release()
	select{case<-m.committed:case<-time.After(2*time.Second):t.Fatal("delayed speech not committed")}
	m.mu.Lock();pcm:=append([]int16(nil),m.pcm...);m.mu.Unlock();if len(pcm)<deviceFrameSamples*5 {t.Fatal("lost speech")}
	var a,b float64;for i,s:=range pcm[:deviceFrameSamples*4] {a+=float64(s)*math.Sin(2*math.Pi*440*float64(i)/deviceSampleRate);b+=float64(s)*math.Sin(2*math.Pi*880*float64(i)/deviceSampleRate)}
	if math.Abs(a)<=math.Abs(b) {t.Fatal("beginning of speech was truncated")}
}

type finishedNoticeActorManager struct {*feedbackActorManager}
func (*finishedNoticeActorManager) CancelImageNotice(uint32)error{return errManualNotIdle}

func TestFeedbackReviewSpeechDuringNoticePlayback(t *testing.T) {runFeedbackSpeechDuringPlayback(t,false)}
func TestFeedbackReviewSpeechAfterNoticeDoneBeforeDrain(t *testing.T) {runFeedbackSpeechDuringPlayback(t,true)}
func runFeedbackSpeechDuringPlayback(t *testing.T,finished bool) {
	m:=&feedbackActorManager{imageActorManager:&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)},notices:make(chan string,2)}
	var manager manualActorRuntime=m;if finished {manager=&finishedNoticeActorManager{m}}
	done:=make(chan struct{})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		defer close(done);c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return nil,errImageNoResults}))
		runExperimentalManualActor(ctx,c,manager,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
	defer func(){_ = d.Close();select{case<-done:case<-time.After(3*time.Second):t.Error("actor did not exit")}}()
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1});select{case<-m.started:case<-time.After(time.Second):t.Fatal("first listen missing")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false};select{case<-m.results:case<-time.After(time.Second):t.Fatal("tool result missing")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(3*time.Second));for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2});select{case<-m.started:case<-time.After(time.Second):t.Fatal("second listen missing")};select{case<-m.notices:case<-time.After(2*time.Second):t.Fatal("notice missing")}
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:2,ResponseID:"notice",PCM:make([]int16,serverFrameSamples*50)}
	if finished {m.events<-manualRealtimeEvent{Kind:"audio_done",Generation:1,ListenID:2,ResponseID:"notice"};m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:2,ResponseID:"notice",Final:true}}
	for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="start"{break}}
	if finished {time.Sleep(200*time.Millisecond)}
	enc,err:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP);if err!=nil{t.Fatal(err)}
	for frame:=0;frame<22;frame++ {pcm:=make([]int16,deviceFrameSamples);if frame<4 {for i:=range pcm {pcm[i]=int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}};buf:=make([]byte,4096);n,e:=enc.Encode(pcm,buf);if e!=nil{t.Fatal(e)};if e=d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil{t.Fatal(e)}}
	for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":3});select{case<-m.started:case<-time.After(time.Second):t.Fatal("next listen missing")}
	select{case<-m.committed:case<-time.After(2*time.Second):t.Fatal("speech not committed after playback interruption")}
}
