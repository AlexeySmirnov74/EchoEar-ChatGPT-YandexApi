package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"math"

	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

// The real runtime's response guard must continue authorizing the first
// response while its queued audio drains, before a tool continuation starts.
func TestImageToolWaitsForPreviousResponseAudio(t *testing.T) {
	resultDelay := make(chan time.Duration,1)
	manual := fakeManualRuntime(t, func(c *websocket.Conn) {
		for { var msg map[string]any; if c.ReadJSON(&msg)!=nil{return}; if msg["type"]=="input_audio_buffer.commit"{break} }
		_ = c.WriteJSON(map[string]any{"type":"input_audio_buffer.committed","item_id":"item1"})
		first:=fakeRead(t,c,"response.create")
		meta:=first["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp1","metadata":meta}})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"resp1","delta":base64.StdEncoding.EncodeToString(make([]byte,serverFrameSamples*20*2))})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"resp1"})
		_ = c.WriteJSON(map[string]any{"type":"response.output_item.done","response_id":"resp1","item":map[string]any{"type":"function_call","status":"completed","call_id":"call1","name":"show_image","arguments":`{"query":"rabbit"}`}})
		started:=time.Now()
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp1","status":"completed"}})
		_ = fakeRead(t,c,"conversation.item.create")
		resultDelay<-time.Since(started)
		next:=fakeRead(t,c,"response.create")
		meta2:=next["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp2","metadata":meta2}})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"resp2","delta":base64.StdEncoding.EncodeToString(make([]byte,serverFrameSamples*2))})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"resp2"})
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp2","status":"completed"}})
		_,_,_=c.ReadMessage()
	})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		fetch:=imageSearchFetcher(func(ctx context.Context,_ string)([]byte,error){<-ctx.Done();return nil,ctx.Err()})
		r=r.WithContext(context.WithValue(r.Context(),imageSearchFetcherKey{},fetch))
		handleExperimentalManualWS(w,r,func(context.Context,string)(*manualRealtimeConnection,error){return manual,nil})
	}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	deviceJSON(t,d,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true,"image_preview":true}})
	_ = deviceReadJSON(t,d)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	enc,err:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP);if err!=nil{t.Fatal(err)}
	for frame:=0;frame<22;frame++ {pcm:=make([]int16,deviceFrameSamples);if frame<4{for i:=range pcm{pcm[i]=int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}};buf:=make([]byte,4096);n,e:=enc.Encode(pcm,buf);if e!=nil{t.Fatal(e)};if e=d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil{t.Fatal(e)}}
	_ = d.SetReadDeadline(time.Now().Add(6*time.Second))
	frames,starts,stops:=0,0,0
	for stops==0 {kind,data,e:=d.ReadMessage();if e!=nil{t.Fatal(e)};if kind==websocket.BinaryMessage{frames++;continue};var msg map[string]any;if json.Unmarshal(data,&msg)!=nil{t.Fatal("invalid JSON")};if msg["type"]=="tts"&&msg["state"]=="start"{starts++};if msg["type"]=="tts"&&msg["state"]=="stop"{stops++};if msg["type"]=="standby"{t.Fatal("provider reset while audio was pending")}}
	if frames!=21 || starts!=1 {t.Fatalf("audio frames=%d starts=%d",frames,starts)}
	select{case delay:=<-resultDelay:if delay<700*time.Millisecond{t.Fatalf("continuation sent before audio drained: %v",delay)};case<-time.After(time.Second):t.Fatal("no tool result")}
}

// Exercise the actual runtime guard and actor together: an exceptionally fast
// image search must not outrun either the tool's response.done or the final
// continuation response. The data is synthetic and no external API is used.
func TestFastImageWaitsForFinalRuntimeResponse(t *testing.T) {
	searchFinished := make(chan struct{})
	allowToolDone := make(chan struct{})
	continuationReady := make(chan struct{})
	allowFinalDone := make(chan struct{})
	testDone := make(chan struct{})
	manual := fakeManualRuntime(t, func(c *websocket.Conn) {
		for { var msg map[string]any; if c.ReadJSON(&msg)!=nil{return}; if msg["type"]=="input_audio_buffer.commit"{break} }
		_ = c.WriteJSON(map[string]any{"type":"input_audio_buffer.committed","item_id":"item1"})
		first:=fakeRead(t,c,"response.create")
		meta:=first["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp1","metadata":meta}})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"resp1","delta":base64.StdEncoding.EncodeToString(make([]byte,serverFrameSamples*6*2))})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"resp1"})
		_ = c.WriteJSON(map[string]any{"type":"response.output_item.done","response_id":"resp1","item":map[string]any{"type":"function_call","status":"completed","call_id":"call1","name":"show_image","arguments":`{"query":"rabbit"}`}})
		select{case <-searchFinished:case <-testDone:return}
		select{case <-allowToolDone:case <-testDone:return}
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp1","status":"completed"}})
		_ = fakeRead(t,c,"conversation.item.create")
		next:=fakeRead(t,c,"response.create")
		meta2:=next["response"].(map[string]any)["metadata"]
		_ = c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp2","metadata":meta2}})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"resp2","delta":base64.StdEncoding.EncodeToString(make([]byte,serverFrameSamples*6*2))})
		_ = c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"resp2"})
		close(continuationReady)
		select{case <-allowFinalDone:case <-testDone:return}
		_ = c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp2","status":"completed"}})
		_,_,_=c.ReadMessage()
	})
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		fetch:=imageSearchFetcher(func(context.Context,string)([]byte,error){close(searchFinished);return make([]byte,imageFrameBytes),nil})
		r=r.WithContext(context.WithValue(r.Context(),imageSearchFetcherKey{},fetch))
		handleExperimentalManualWS(w,r,func(context.Context,string)(*manualRealtimeConnection,error){return manual,nil})
	}));defer server.Close();defer close(testDone)
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	deviceJSON(t,d,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true,"image_preview":true}})
	_ = deviceReadJSON(t,d)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	enc,err:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP);if err!=nil{t.Fatal(err)}
	for frame:=0;frame<22;frame++ {pcm:=make([]int16,deviceFrameSamples);if frame<4{for i:=range pcm{pcm[i]=int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}};buf:=make([]byte,4096);n,e:=enc.Encode(pcm,buf);if e!=nil{t.Fatal(e)};if e=d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil{t.Fatal(e)}}
	messages:=make(chan map[string]any,256)
	go func(){for {kind,data,e:=d.ReadMessage();if e!=nil{return};if kind==websocket.BinaryMessage{continue};var msg map[string]any;if json.Unmarshal(data,&msg)==nil{messages<-msg}}}()
	assertNoImageOrStop:=func(duration time.Duration){
		deadline:=time.NewTimer(duration);defer deadline.Stop()
		for {select{case msg:=<-messages:
			if msg["type"]=="image_preview" || (msg["type"]=="tts" && msg["state"]=="stop") {t.Fatalf("premature device event: %v",msg)}
		case <-deadline.C:return}}
	}
	select{case <-searchFinished:case<-time.After(3*time.Second):t.Fatal("fast search never started")}
	assertNoImageOrStop(500*time.Millisecond)
	close(allowToolDone)
	select{case <-continuationReady:case<-time.After(3*time.Second):t.Fatal("continuation did not start")}
	assertNoImageOrStop(500*time.Millisecond)
	close(allowFinalDone)
	stopped:=false
	deadline:=time.After(3*time.Second)
	for {select{case msg:=<-messages:
		if msg["type"]=="tts" && msg["state"]=="stop" {stopped=true}
		if msg["type"]=="image_preview" && msg["op"]=="begin" {if !stopped{t.Fatal("image began before final speech stop")};return}
	case <-deadline:t.Fatal("image did not begin after final speech stop")}}
}
