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

func TestManagedDeviceWSRecoversProviderWithoutClosingDevice(t *testing.T){
	connections:=make(chan *websocket.Conn,4);appends:=make(chan struct{},16)
	provider:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		c,e:=(&websocket.Upgrader{}).Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
		var update map[string]any;if c.ReadJSON(&update)!=nil{return}
		if c.WriteJSON(map[string]any{"type":"session.updated","session":map[string]any{"audio":map[string]any{"input":map[string]any{"turn_detection":nil}}}})!=nil{return}
		connections<-c
		for {_,raw,e:=c.ReadMessage();if e!=nil{return};var event map[string]any;if json.Unmarshal(raw,&event)==nil&&event["type"]=="input_audio_buffer.append"{select{case appends<-struct{}{}:default:}}}
	}));defer provider.Close()
	endpoint:="ws"+strings.TrimPrefix(provider.URL,"http")
	server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){handleManagedManualWithConfig(w,r,endpoint,"fake-key","test","alloy",nil)}));defer server.Close()
	d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer d.Close()
	deviceJSON(t,d,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true}})
	var first *websocket.Conn
	select{case first=<-connections:case<-time.After(2*time.Second):t.Fatal("first provider absent")}
	if h:=deviceReadJSON(t,d);h["type"]!="hello"{t.Fatal(h)}else { clock,ok:=h["server_time"].(map[string]any);if !ok {t.Fatal("missing device clock",h)};stamp,ok:=clock["timestamp"].(float64);if !ok || clock["timezone_offset"]!=float64(300) || stamp<1700000000000 || stamp>=4102358400000 {t.Fatal("invalid device clock in hello",h)} }
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","mode":"auto","listen_id":1})
	// A generation loss while listening must put this same device WS in standby.
	time.Sleep(30*time.Millisecond)
	_ = first.Close()
	_ = d.SetReadDeadline(time.Now().Add(2*time.Second))
	if state:=deviceReadJSON(t,d);state["type"]!="standby"||state["listen_id"]!=float64(1){t.Fatalf("standby missing: %v",state)}
	_ = d.SetReadDeadline(time.Time{})
	select{case<-connections:case<-time.After(2*time.Second):t.Fatal("replacement absent")}
	time.Sleep(50*time.Millisecond)
	deviceJSON(t,d,map[string]any{"type":"listen","state":"start","mode":"auto","listen_id":2})
	enc,err:=opus.NewEncoder(deviceSampleRate,1,opus.AppVoIP);if err!=nil{t.Fatal(err)}
	for frame:=0;frame<4;frame++{
		pcm:=make([]int16,deviceFrameSamples);for i:=range pcm{pcm[i]=int16(10000*math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))}
		buf:=make([]byte,4096);n,e:=enc.Encode(pcm,buf);if e!=nil{t.Fatal(e)}
		if e=d.WriteMessage(websocket.BinaryMessage,append([]byte{0,0,byte(n>>8),byte(n)},buf[:n]...));e!=nil{t.Fatal(e)}
	}
	select{case<-appends:case<-time.After(2*time.Second):t.Fatal("new speech did not reach replacement")}
}

func TestManagedImageInstructionsAndToolAreOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false:"disabled", true:"enabled"}[enabled], func(t *testing.T) {
			updates := make(chan map[string]any,1)
			provider:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
				c,e:=(&websocket.Upgrader{}).Upgrade(w,r,nil);if e!=nil{return};defer c.Close()
				var update map[string]any;if c.ReadJSON(&update)!=nil{return}
				updates<-update
				if c.WriteJSON(map[string]any{"type":"session.updated","session":map[string]any{"audio":map[string]any{"input":map[string]any{"turn_detection":nil}}}})!=nil{return}
				for {if _,_,e:=c.ReadMessage();e!=nil{return}}
			}));defer provider.Close()
			endpoint:="ws"+strings.TrimPrefix(provider.URL,"http")
			handlerDone:=make(chan struct{})
			server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){defer close(handlerDone);handleManagedManualWithConfig(w,r,endpoint,"fake-key","base instructions","alloy",nil)}));defer server.Close()
			d,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
			defer func(){_ = d.Close(); select{case <-handlerDone:case <-time.After(3*time.Second):t.Error("device handler did not stop")}}()
			deviceJSON(t,d,map[string]any{"type":"hello","features":map[string]any{"persistent_idle":true,"image_preview":enabled}})
			var update map[string]any
			select{case update=<-updates:case<-time.After(2*time.Second):t.Fatal("session update absent")}
			session,ok:=update["session"].(map[string]any);if !ok{t.Fatal("session absent")}
			instructions,_:=session["instructions"].(string)
			hasRule:=strings.Contains(instructions,"must call the show_image tool")
			if !strings.HasPrefix(instructions,"base instructions") || hasRule!=enabled {t.Fatal("incorrect image instructions for capability")}
			if enabled {
				for _, rule := range []string{"cannot draw or generate pictures", "searching for an existing one", "Base every search on the current request", "do not receive visual confirmation or see the picture"} {
					if !strings.Contains(instructions, rule) { t.Fatalf("missing image behavior rule: %s", rule) }
				}
			} else if strings.Contains(instructions,"cannot draw or generate pictures") {
				t.Fatal("image-only policy leaked into a session without image capability")
			}
			tools,_:=session["tools"].([]any);hasTool:=false
			for _,raw:=range tools{tool,ok:=raw.(map[string]any);if ok&&tool["name"]=="show_image"{hasTool=true}}
			if hasTool!=enabled{t.Fatal("incorrect image tool for capability")}
		})
	}
}
