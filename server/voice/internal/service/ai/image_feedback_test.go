package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
	"strings"

	"github.com/gorilla/websocket"
)

func TestImageFeedbackUnixDistinguishesConfirmedEmpty(t *testing.T) {
	for _,tc:=range []struct{code int;empty bool}{{204,true},{502,false},{503,false}} {
		path:=filepath.Join(t.TempDir(),"s.sock");listener,err:=net.Listen("unix",path);if err!=nil{t.Skip(err)}
		server:=&http.Server{Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(tc.code)})}
		go server.Serve(listener)
		_,err=fetchImageSearchAt(context.Background(),path,"rabbit")
		server.Close();listener.Close()
		if err==nil || errors.Is(err,errImageNoResults)!=tc.empty {t.Fatalf("status %d: %v",tc.code,err)}
	}
}

func TestImageFeedbackCapabilityPromptMatchesNegotiatedFeatures(t *testing.T) {
	for _,tc:=range []struct{name string;image,bg bool}{{"voice-only",false,false},{"image",true,false},{"image-and-background",true,true}} {
		t.Run(tc.name,func(t *testing.T){
			updates:=make(chan map[string]any,1)
			provider:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close();var update map[string]any;if c.ReadJSON(&update)!=nil{return};updates<-update;_ = c.WriteJSON(map[string]any{"type":"session.updated","session":map[string]any{"audio":map[string]any{"input":map[string]any{"turn_detection":nil}}}});for {if _,_,e=c.ReadMessage();e!=nil{return}}}));defer provider.Close()
			done:=make(chan struct{})
			server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){defer close(done);handleManagedManualWithConfig(w,r,"ws"+strings.TrimPrefix(provider.URL,"http"),"fake","base","alloy",speakerRealtimeTools())}));defer server.Close()
			headers:=http.Header{};headers.Set("Device-Id","test-device")
			d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),headers);if err!=nil{t.Fatal(err)};defer func(){_ = d.Close();select{case<-done:case<-time.After(3*time.Second):t.Error("handler did not exit")}}()
			deviceJSON(t,d,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true,"image_preview":tc.image,"image_background_v1":tc.bg}})
			var update map[string]any;select{case update=<-updates:case<-time.After(3*time.Second):t.Fatal("missing session")}
			session:=update["session"].(map[string]any);prompt:=session["instructions"].(string)
			if !strings.Contains(prompt,"talk, answer questions and tell stories") {t.Fatal("voice capability absent")}
			if !strings.Contains(prompt,"You cannot turn off the speaker by voice") {t.Fatal("unsupported power capability not excluded")}
			if strings.Contains(prompt,"search the internet for existing pictures")!=tc.image {t.Fatal("incorrect image capability")}
			if strings.Contains(prompt,"keeping a displayed picture as a background")!=tc.bg {t.Fatal("incorrect background capability")}
		})
	}
}

type feedbackActorManager struct { *imageActorManager; notices chan string }
func (m *feedbackActorManager) NotifyImageResult(_ uint32,status string) error {m.notices<-status;return nil}

func TestImageFeedbackDeliveredInNextIdleTurn(t *testing.T) {
	for _,tc:=range []struct{name string;searchErr error;want string}{{"empty",errImageNoResults,"no_results"},{"failure",errors.New("network"),"unavailable"}} {
		t.Run(tc.name,func(t *testing.T){
			m:=&feedbackActorManager{imageActorManager:&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4)},results:make(chan string,2)},notices:make(chan string,2)}
			server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
				c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
				ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){return nil,tc.searchErr}))
				runExperimentalManualActor(ctx,c,m,"session")
			}));defer server.Close()
			d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
			m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
			select{case<-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
			m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
			m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
			select{case<-m.results:case<-time.After(time.Second):t.Fatal("tool result missing")}
			m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
			_ = d.SetReadDeadline(time.Now().Add(3*time.Second))
			for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil && msg["type"]=="tts" && msg["state"]=="stop" {break}}
			select{case notice:=<-m.notices:t.Fatalf("notice interrupted previous turn: %s",notice);default:}
			deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
			select{case<-m.started:case<-time.After(time.Second):t.Fatal("no next listen")}
			select{case notice:=<-m.notices:if notice!=tc.want{t.Fatalf("notice %q",notice)};case<-time.After(2*time.Second):t.Fatal("notice missing")}
		})
	}
}

func TestImageFeedbackCancelledWhenUserStartsNewSpeech(t *testing.T) {
	entered,release:=make(chan struct{}),make(chan struct{})
	m:=&feedbackActorManager{imageActorManager:&imageActorManager{fakeActorManager:&fakeActorManager{states:make(chan manualProviderState,4),events:make(chan manualRealtimeEvent,16),failures:make(chan uint64,1),started:make(chan uint32,4),committed:make(chan struct{},1)},results:make(chan string,2)},notices:make(chan string,2)}
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		ctx:=context.WithValue(r.Context(),imagePreviewContextKey{},true);ctx=context.WithValue(ctx,imageSearchFetcherKey{},imageSearchFetcher(func(context.Context,string)([]byte,error){close(entered);<-release;return nil,errImageNoResults}))
		runExperimentalManualActor(ctx,c,m,"session")
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close();defer close(release)
	m.ready(1);deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1});select{case<-m.started:case<-time.After(time.Second):t.Fatal("no listen")}
	m.events<-manualRealtimeEvent{Kind:"tool",Generation:1,ListenID:1,ResponseID:"resp",CallID:"call",ToolName:"show_image",Args:map[string]any{"query":"rabbit"}}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:false}
	select{case<-m.results:case<-time.After(time.Second):t.Fatal("tool result missing")};select{case<-entered:case<-time.After(time.Second):t.Fatal("search not started")}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"answer",Final:true}
	_ = d.SetReadDeadline(time.Now().Add(3*time.Second));for {_,raw,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil&&msg["type"]=="tts"&&msg["state"]=="stop"{break}}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2});select{case<-m.started:case<-time.After(time.Second):t.Fatal("no next listen")}
	commitActorSpeech(t,d,m.fakeActorManager)
	select{case notice:=<-m.notices:t.Fatalf("stale notice: %s",notice);case<-time.After(150*time.Millisecond):}
}

func TestImageFeedbackRealtimeOutOfBandDoesNotCorruptNextTurn(t *testing.T) {
	ready:=make(chan struct{})
	s:=fakeManualRuntime(t,func(c *websocket.Conn){
		req:=fakeRead(t,c,"response.create");response:=req["response"].(map[string]any);meta:=response["metadata"].(map[string]any)
		if response["conversation"]!="none"||meta["kind"]!="image_search_notice" {t.Errorf("not an isolated notice: %v",response)}
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"notice1","metadata":meta}})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"notice1","delta":base64.StdEncoding.EncodeToString([]byte{1,0,2,0})})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"notice1"})
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"notice1","status":"completed"}})
		_ = fakeRead(t,c,"input_audio_buffer.append")
		_ = fakeRead(t,c,"input_audio_buffer.clear")
		_ = c.WriteJSON(map[string]any{"type":"input_audio_buffer.cleared"})
		close(ready);_,_,_=c.ReadMessage()
	})
	if err:=s.startManualRuntime(1,10);err!=nil{t.Fatal(err)};r:=s.runtime
	if err:=r.Begin(7);err!=nil{t.Fatal(err)}
	if err:=r.NotifyImageResult(7,"no_results");err!=nil{t.Fatal(err)}
	for _,kind:=range []string{"audio","audio_done","done"} {select{case e:=<-r.events:if e.Kind!=kind||e.ListenID!=7||e.ResponseID!="notice1"{t.Fatalf("bad event %+v",e)};case<-time.After(3*time.Second):t.Fatalf("missing %s",kind)}}
	if err:=r.FinishPlayback(7,"notice1",func(uint32)error{return nil});err!=nil{t.Fatal(err)}
	if err:=r.Begin(8);err!=nil{t.Fatal(err)};if err:=r.Append(8,make([]int16,deviceFrameSamples));err!=nil{t.Fatal(err)}
	if err:=r.NotifyImageResult(8,"unavailable");!errors.Is(err,errManualNotIdle){t.Fatalf("notice accepted active speech: %v",err)}
	if err:=r.Clear(8);err!=nil{t.Fatal("not-idle notice closed provider",err)}
	select{case<-ready:case<-time.After(3*time.Second):t.Fatal("clear not received")}
}

func TestImageFeedbackRealtimeCancelResumesSpeechWithoutReconnect(t *testing.T) {
	ready:=make(chan struct{})
	allowCreated:=make(chan struct{})
	s:=fakeManualRuntime(t,func(c *websocket.Conn){
		req:=fakeRead(t,c,"response.create");meta:=req["response"].(map[string]any)["metadata"].(map[string]any)
		<-allowCreated
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"notice1","metadata":meta}})
		cancel:=fakeRead(t,c,"response.cancel");if cancel["response_id"]!="notice1"{t.Errorf("out-of-band cancellation lacks response id: %v",cancel)}
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"notice1","status":"cancelled"}})
		_ = fakeRead(t,c,"input_audio_buffer.append");_ = fakeRead(t,c,"input_audio_buffer.commit");close(ready);_,_,_=c.ReadMessage()
	})
	if err:=s.startManualRuntime(1,10);err!=nil{t.Fatal(err)};r:=s.runtime
	if err:=r.Begin(7);err!=nil{t.Fatal(err)};if err:=r.NotifyImageResult(7,"no_results");err!=nil{t.Fatal(err)}
	if err:=r.CancelImageNotice(7);err!=nil{t.Fatal(err)}
	close(allowCreated)
	select{case e:=<-r.events:if e.Kind!="notice_cancelled"||e.ResponseID!="notice1"{t.Fatal(e)};case<-time.After(3*time.Second):t.Fatal("cancel ACK missing")}
	if err:=r.FinishImageNoticeCancel(7,"notice1",func(uint32)error{return nil},true);err!=nil{t.Fatal(err)}
	if err:=r.Append(7,make([]int16,deviceFrameSamples));err!=nil{t.Fatal("speech append failed",err)}
	if err:=r.Commit(7);err!=nil{t.Fatal("speech commit failed",err)}
	select{case<-ready:case<-time.After(3*time.Second):t.Fatal("provider did not receive speech")}
}

func TestImageFeedbackRealtimeCancelErrorWaitsForDone(t *testing.T) {
	ready:=make(chan struct{});created:=make(chan struct{})
	s:=fakeManualRuntime(t,func(c *websocket.Conn){
		req:=fakeRead(t,c,"response.create");meta:=req["response"].(map[string]any)["metadata"].(map[string]any)
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"notice1","metadata":meta}});close(created)
		cancel:=fakeRead(t,c,"response.cancel");eventID,_:=cancel["event_id"].(string);if eventID=="" {t.Error("cancel event id missing")}
		_ = c.WriteJSON(map[string]any{"type":"error","error":map[string]any{"event_id":eventID,"code":"already_completed"}})
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"notice1","status":"completed"}})
		_ = fakeRead(t,c,"input_audio_buffer.append");close(ready);_,_,_=c.ReadMessage()
	})
	if err:=s.startManualRuntime(1,10);err!=nil{t.Fatal(err)};r:=s.runtime
	if err:=r.Begin(7);err!=nil{t.Fatal(err)};if err:=r.NotifyImageResult(7,"no_results");err!=nil{t.Fatal(err)}
	select{case<-created:case<-time.After(3*time.Second):t.Fatal("no response created")}
	if err:=r.CancelImageNotice(7);err!=nil{t.Fatal(err)}
	select{case e:=<-r.events:if e.Kind!="notice_cancelled"{t.Fatal(e)};case<-time.After(3*time.Second):t.Fatal("done missing")}
	if err:=r.FinishImageNoticeCancel(7,"notice1",func(uint32)error{return nil},true);err!=nil{t.Fatal(err)}
	if err:=r.Append(7,make([]int16,deviceFrameSamples));err!=nil{t.Fatal("provider did not recover",err)}
	select{case<-ready:case<-time.After(3*time.Second):t.Fatal("speech not sent")}
}
