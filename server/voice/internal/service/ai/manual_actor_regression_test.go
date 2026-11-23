package ai

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

func regressionActor(t *testing.T)(*websocket.Conn,*fakeActorManager){
	return regressionActorWithTimeout(t, 30*time.Second)
}

func regressionActorWithTimeout(t *testing.T, timeout time.Duration)(*websocket.Conn,*fakeActorManager){
	t.Helper()
	m:=&fakeActorManager{states:make(chan manualProviderState,8),events:make(chan manualRealtimeEvent,32),failures:make(chan uint64,1),started:make(chan uint32,8),committed:make(chan struct{},1)}
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){c,err:=upgrader.Upgrade(w,r,nil);if err!=nil{return};defer c.Close();runExperimentalManualActorWithProgressTimeout(r.Context(),c,m,"session",timeout)}))
	t.Cleanup(server.Close)
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};t.Cleanup(func(){d.Close()})
	m.ready(1)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":1})
	select{case<-m.started:case<-time.After(time.Second):t.Fatal("initial listen missing")}
	return d,m
}

func commitActorSpeech(t *testing.T, d *websocket.Conn, m *fakeActorManager) {
	t.Helper()
	enc, err := opus.NewEncoder(deviceSampleRate, 1, opus.AppVoIP)
	if err != nil { t.Fatal(err) }
	for frame := 0; frame < 22; frame++ {
		pcm := make([]int16, deviceFrameSamples)
		if frame < 4 { for i := range pcm { pcm[i] = int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate)) } }
		buf := make([]byte,4096)
		n, err := enc.Encode(pcm,buf)
		if err != nil { t.Fatal(err) }
		if err = d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...)); err != nil { t.Fatal(err) }
	}
	select { case <-m.committed: case <-time.After(time.Second): t.Fatal("speech not committed") }
}

func actorStopThenStandby(t *testing.T, d *websocket.Conn, id float64) {
	t.Helper()
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {
		kind, data, err := d.ReadMessage(); if err != nil { t.Fatal(err) }
		if kind == websocket.BinaryMessage { continue }
		var event map[string]any; if err := json.Unmarshal(data,&event); err != nil { t.Fatal(err) }
		if event["type"] != "tts" || event["state"] != "stop" || event["listen_id"] != id { t.Fatal(event) }
		break
	}
	event := deviceReadJSON(t,d)
	if event["type"] != "standby" || event["listen_id"] != id { t.Fatal(event) }
	_ = d.SetReadDeadline(time.Time{})
}

func TestManualActorProviderLossStopsSpeakingBeforeStandby(t *testing.T) {
	d,m:=regressionActor(t)
	commitActorSpeech(t,d,m)
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"resp",PCM:make([]int16,serverFrameSamples*6)}
	if start:=deviceReadJSON(t,d); start["type"]!="tts" || start["state"]!="start" { t.Fatal(start) }
	m.lost(1)
	actorStopThenStandby(t,d,1)
	// Firmware responds to stop by re-entering Listening with a new ID. The
	// provider is still reconnecting, so this new listen must enter standby.
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	if event:=deviceReadJSON(t,d);event["type"]!="standby" || event["listen_id"]!=float64(2){t.Fatal(event)}
	m.ready(2)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":3})
	select { case id:=<-m.started: if id!=3 {t.Fatal(id)}; case <-time.After(time.Second): t.Fatal("provider did not recover") }
}

func TestManualActorStalledAnswerStopsSpeakingAndRecovers(t *testing.T) {
	d,m:=regressionActorWithTimeout(t,300*time.Millisecond)
	commitActorSpeech(t,d,m)
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"resp",PCM:make([]int16,serverFrameSamples*6)}
	if start:=deviceReadJSON(t,d); start["type"]!="tts" || start["state"]!="start" { t.Fatal(start) }
	actorStopThenStandby(t,d,1)
	m.mu.Lock(); generation:=m.gen; m.mu.Unlock()
	if generation != 0 { t.Fatal("stalled provider was not invalidated",generation) }
}

func TestManualActorProgressingAudioCanExceedWatchdog(t *testing.T) {
	d,m:=regressionActorWithTimeout(t,300*time.Millisecond)
	commitActorSpeech(t,d,m)
	// The queue plays for >300 ms without another provider event; paced writes
	// count as progress and must not cut off a long response.
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"resp",PCM:make([]int16,serverFrameSamples*12)}
	m.events<-manualRealtimeEvent{Kind:"audio_done",Generation:1,ListenID:1,ResponseID:"resp"}
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"resp",Final:true}
	if start:=deviceReadJSON(t,d);start["type"]!="tts" || start["state"]!="start"{t.Fatal(start)}
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	frames:=0
	for { kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{frames++;continue};var event map[string]any;if err=json.Unmarshal(data,&event);err!=nil{t.Fatal(err)};if event["type"]!="tts" || event["state"]!="stop"{t.Fatal(event)};break }
	if frames!=12 {t.Fatalf("lost audio frames: %d",frames)}
	m.mu.Lock(); generation:=m.gen; m.mu.Unlock(); if generation!=1 {t.Fatal("progressing response invalidated",generation)}
}

func TestManualActorListeningDoesNotTriggerProgressWatchdog(t *testing.T) {
	d,m:=regressionActorWithTimeout(t,200*time.Millisecond)
	// No voice has been committed. Merely waiting must not replace the provider.
	time.Sleep(500*time.Millisecond)
	commitActorSpeech(t,d,m)
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"empty",Final:true}
	if start:=deviceReadJSON(t,d);start["type"]!="tts" || start["state"]!="start"{t.Fatal(start)}
	if stop:=deviceReadJSON(t,d);stop["type"]!="tts" || stop["state"]!="stop"{t.Fatal(stop)}
	m.lost(1)
	if event:=deviceReadJSON(t,d);event["type"]!="standby" {t.Fatalf("finished playback was stopped twice: %v",event)}
}

func TestManualActorEmptyAnswerTransitionsSpeakingAndAcceptsNextListen(t *testing.T){
	d,m:=regressionActor(t)
	m.events<-manualRealtimeEvent{Kind:"done",Generation:1,ListenID:1,ResponseID:"empty",Final:true}
	start:=deviceReadJSON(t,d);if start["type"]!="tts"||start["state"]!="start"{t.Fatal(start)}
	stop:=deviceReadJSON(t,d);if stop["type"]!="tts"||stop["state"]!="stop"{t.Fatal(stop)}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case id:=<-m.started:if id!=2{t.Fatal(id)};case<-time.After(time.Second):t.Fatal("next listen missing")}
}

func TestManualActorPauseWithEmptyInputKeepsProvider(t *testing.T){
	d,m:=regressionActor(t)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"pause","listen_id":1})
	if event:=deviceReadJSON(t,d);event["type"]!="standby"{t.Fatal(event)}
	m.mu.Lock();generation:=m.gen;m.mu.Unlock();if generation!=1{t.Fatal("empty pause replaced provider")}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case id:=<-m.started:if id!=2{t.Fatal(id)};case<-time.After(time.Second):t.Fatal("next listen missing")}
}

func TestManualActorAbortStopsAudioAndPreservesSocket(t *testing.T){
	d,m:=regressionActor(t)
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"old",PCM:make([]int16,serverFrameSamples*12)}
	start:=deviceReadJSON(t,d);if start["type"]!="tts"||start["state"]!="start"{t.Fatal(start)}
	deviceJSON(t,d,map[string]any{"type":"abort"})
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	for {kind,data,err:=d.ReadMessage();if err!=nil{t.Fatal(err)};if kind==websocket.BinaryMessage{continue};var event map[string]any;if err:=json.Unmarshal(data,&event);err!=nil{t.Fatal(err)};if event["type"]=="tts"&&event["state"]=="stop"{break}}
	_ = d.SetReadDeadline(time.Time{})
	if event:=deviceReadJSON(t,d);event["type"]!="standby"{t.Fatal(event)}
	m.ready(2)
	m.events<-manualRealtimeEvent{Kind:"audio",Generation:1,ListenID:1,ResponseID:"old",PCM:make([]int16,serverFrameSamples)}
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","listen_id":2})
	select{case id:=<-m.started:if id!=2{t.Fatal(id)};case<-time.After(time.Second):t.Fatal("socket did not survive abort")}
}
