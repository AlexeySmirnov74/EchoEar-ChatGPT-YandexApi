package ai

import (
    "context"
    "encoding/base64"
    "math"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/gorilla/websocket"
    opus "gopkg.in/hraban/opus.v2"
)

func TestManualRestConfirmsBeforeSpeechAndDoesNotClose(t *testing.T) {
    cases:=[]struct{name,deviceReply,toolOutput string}{
        {"confirmed",`{"success":true,"mode":"resting"}`,`{"mode":"resting","success":true}`},
        {"rejected",`{"success":false,"mode":"awake"}`,`{"mode":"awake","success":false}`},
        {"malformed",`true`,`{"success":false,"reason":"unconfirmed"}`},
    }
    for _,tc:=range cases {t.Run(tc.name,func(t *testing.T){ testManualRestTurn(t,tc.deviceReply,tc.toolOutput) })}
}

func testManualRestTurn(t *testing.T,deviceReply,toolOutput string) {
    providerDone:=make(chan struct{})
    manual:=fakeManualRuntime(t,func(c *websocket.Conn){
        defer close(providerDone)
        for { var msg map[string]any; if c.ReadJSON(&msg)!=nil{return}; if msg["type"]=="input_audio_buffer.commit" {break} }
        _=c.WriteJSON(map[string]any{"type":"input_audio_buffer.committed","item_id":"item1"})
        first:=fakeRead(t,c,"response.create"); meta:=first["response"].(map[string]any)["metadata"]
        _=c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp1","metadata":meta}})
        _=c.WriteJSON(map[string]any{"type":"response.output_item.done","response_id":"resp1","item":map[string]any{"type":"function_call","status":"completed","call_id":"call1","name":"enter_rest","arguments":"{}"}})
        _=c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp1","status":"completed"}})
        output:=fakeRead(t,c,"conversation.item.create")
        if output["item"].(map[string]any)["output"] != toolOutput {t.Error("wrong confirmation result")}
        next:=fakeRead(t,c,"response.create"); meta2:=next["response"].(map[string]any)["metadata"]
        _=c.WriteJSON(map[string]any{"type":"response.created","response":map[string]any{"id":"resp2","metadata":meta2}})
        _=c.WriteJSON(map[string]any{"type":"response.output_audio.delta","response_id":"resp2","delta":base64.StdEncoding.EncodeToString(make([]byte,serverFrameSamples*2))})
        _=c.WriteJSON(map[string]any{"type":"response.output_audio.done","response_id":"resp2"})
        _=c.WriteJSON(map[string]any{"type":"response.done","response":map[string]any{"id":"resp2","status":"completed"}})
        _,_,_=c.ReadMessage()
    })
    actorDone:=make(chan struct{})
    server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){ defer close(actorDone); handleExperimentalManualWS(w,r,func(context.Context,string)(*manualRealtimeConnection,error){return manual,nil}) }))
    defer server.Close()
    device,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil); if err!=nil {t.Fatal(err)}; defer device.Close()
    deviceJSON(t,device,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true}})
    hello:=deviceReadJSON(t,device); sessionID,_:=hello["session_id"].(string); if sessionID=="" {t.Fatal("no session")}
    deviceJSON(t,device,map[string]any{"type":"listen","state":"start","mode":"auto","listen_id":1})
    enc,e:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP); if e!=nil {t.Fatal(e)}
    for frame:=0;frame<22;frame++ { pcm:=make([]int16,deviceFrameSamples); if frame<4 {for i:=range pcm {pcm[i]=int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}}; buf:=make([]byte,4096); n,e:=enc.Encode(pcm,buf); if e!=nil {t.Fatal(e)}; if e=device.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil {t.Fatal(e)} }
    mcp:=deviceReadJSON(t,device); if mcp["type"]!="mcp" {t.Fatal("expected MCP before speech")}
    payload:=mcp["payload"].(map[string]any); if payload["params"].(map[string]any)["name"]!="self.rest.enter" {t.Fatal("wrong device tool")}
    deviceJSON(t,device,map[string]any{"type":"mcp","session_id":sessionID,"payload":map[string]any{"jsonrpc":"2.0","id":payload["id"],"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":deviceReply}}}}})
    start:=deviceReadJSON(t,device); if start["type"]!="tts" || start["state"]!="start" || start["listen_id"]!=float64(1) {t.Fatal("bad audio start",start)}
    typ,packet,e:=device.ReadMessage(); if e!=nil || typ!=websocket.BinaryMessage || len(packet)<5 {t.Fatal("missing speech",e)}
    stop:=deviceReadJSON(t,device); if stop["type"]!="tts" || stop["state"]!="stop" || stop["listen_id"]!=float64(1) {t.Fatal("bad audio stop",stop)}
    // The device, not the bridge, closes after its local playback queue drains.
    select {case <-actorDone:t.Fatal("server closed connection prematurely");default:}
    device.Close()
    select {case <-actorDone:case <-time.After(time.Second):t.Fatal("actor leaked")}
    select {case <-providerDone:case <-time.After(time.Second):t.Fatal("provider leaked")}
}
