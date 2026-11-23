package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestManagedManualSocketFakeWireLossRecoveryAndIdle(t *testing.T){
	connections:=make(chan *websocket.Conn,4)
	appends:=make(chan struct{},4)
	provider:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		conn,err:=(&websocket.Upgrader{}).Upgrade(w,r,nil);if err!=nil{return};defer conn.Close()
		var update map[string]any;if err:=conn.ReadJSON(&update);err!=nil{return}
		if update["type"]!="session.update"{return}
		if err:=conn.WriteJSON(map[string]any{"type":"session.updated","session":map[string]any{"audio":map[string]any{"input":map[string]any{"turn_detection":nil}}}});err!=nil{return}
		connections<-conn
		for {_,raw,err:=conn.ReadMessage();if err!=nil{return};var msg map[string]any;if json.Unmarshal(raw,&msg)==nil && msg["type"]=="input_audio_buffer.append"{appends<-struct{}{}}}
	}));defer provider.Close()
	events:=make(chan manualRealtimeEvent,16)
	lost:=make(chan uint64,4)
	dial:=newManagedManualSocketDial("ws"+strings.TrimPrefix(provider.URL,"http"),"fake-key","test","alloy",nil,events)
	m:=newManagedManualProvider(context.Background(),dial,managedProviderSink{OnLoss:func(g uint64){lost<-g}});defer m.Close();m.Start()
	var first *websocket.Conn
	select{case first=<-connections:case<-time.After(2*time.Second):t.Fatal("first connection absent")}
	if !waitManualManagerReady(m,time.Second){t.Fatal("first not ready")}
	if err:=m.Append(1,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("idle write allowed",err)}
	if err:=m.Begin(1);err!=nil{t.Fatal(err)}
	if err:=m.Append(1,[]int16{1,2,3,4});err!=nil{t.Fatal(err)}
	select{case<-appends:case<-time.After(time.Second):t.Fatal("audio not forwarded")}
	_=first.Close()
	select{case<-lost:case<-time.After(time.Second):t.Fatal("loss not reported")}
	select{case<-connections:case<-time.After(2*time.Second):t.Fatal("no replacement")}
	if !waitManualManagerReady(m,time.Second){t.Fatal("replacement not ready")}
	if err:=m.Append(2,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("reconnect activated microphone",err)}
	if err:=m.Begin(2);err!=nil{t.Fatal(err)}
	if err:=m.Append(2,[]int16{1,2,3,4});err!=nil{t.Fatal(err)}
	select{case<-appends:case<-time.After(time.Second):t.Fatal("replacement audio not forwarded")}
}

func waitManualManagerReady(m *managedManualProvider,timeout time.Duration)bool{
	deadline:=time.Now().Add(timeout)
	for time.Now().Before(deadline){m.lifecycle.stateMu.Lock();g:=m.lifecycle.activatedGeneration;m.lifecycle.stateMu.Unlock();if g!=0&&m.IsCurrent(g){return true};time.Sleep(time.Millisecond)}
	return false
}
