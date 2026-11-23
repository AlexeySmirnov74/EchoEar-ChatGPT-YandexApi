package ai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/os/gcfg"
	"github.com/gorilla/websocket"
)

func TestHelloRoutingPreservesLegacyAndNegotiatesCapability(t *testing.T) {
	for _,tc:=range []struct{name string; enabled bool; feature any; manual bool}{
		{"stock",true,nil,false},{"false",true,false,false},{"persistent",true,true,true},{"disabled",false,true,false},
	} {t.Run(tc.name,func(t *testing.T){
		t.Setenv("STACKCHAN_DATA_DIR",t.TempDir())
		previous:=g.Cfg().GetAdapter();defer g.Cfg().SetAdapter(previous)
		adapter,err:=gcfg.NewAdapterContent(fmt.Sprintf(`{"ai":{"ha_enabled":false,"conversation_idle_seconds":0,"experimental_manual_enabled":%t}}`,tc.enabled));if err!=nil{t.Fatal(err)};g.Cfg().SetAdapter(adapter)
		var legacyCount,manualCount atomic.Int32
		server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
			dial:=func(context.Context,string,*haWSClient,RealtimeCallbacks)(RealtimeSession,error){legacyCount.Add(1);return &fakePersistentProvider{input:make(chan struct{},1),cancelled:make(chan struct{},1)},nil}
			handleConfiguredWS(w,r,dial,func(c *websocket.Conn,_ map[string]any){manualCount.Add(1);_ = c.WriteJSON(map[string]any{"type":"manual-test"})})
		}));defer server.Close()
		c,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)};defer c.Close()
		hello:=map[string]any{"type":"hello"};if tc.feature!=nil{hello["features"]=map[string]any{"persistent_idle":tc.feature}}
		if err:=c.WriteJSON(hello);err!=nil{t.Fatal(err)}
		_ = c.SetReadDeadline(time.Now().Add(2*time.Second));var reply map[string]any;if err:=c.ReadJSON(&reply);err!=nil{t.Fatal(err)}
		if tc.manual {if reply["type"]!="manual-test"||manualCount.Load()!=1||legacyCount.Load()!=0{t.Fatal("wrong manual route",reply)}} else {
			if reply["type"]!="hello"||manualCount.Load()!=0||legacyCount.Load()!=1{t.Fatal("wrong legacy route",reply)}
			if features,ok:=reply["features"].(map[string]any);ok&&features["persistent_idle"]==true{t.Fatal("unsafe legacy capability advertised")}
		}
	})}
}

func TestHelloRouterRejectsInvalidBeforeOpeningProvider(t *testing.T){
	for _,payload:=range []string{`not-json`,`{"type":"listen"}`,strings.Repeat("x",65537)}{
		var called atomic.Int32
		server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
			fn:=func(*websocket.Conn,map[string]any){called.Add(1)};handleHelloRoutedWSWith(w,r,fn,fn)
		}))
		c,_,err:=websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http"),nil);if err!=nil{t.Fatal(err)}
		_ = c.WriteMessage(websocket.TextMessage,[]byte(payload));_ = c.SetReadDeadline(time.Now().Add(time.Second));_,_,_ = c.ReadMessage();c.Close();server.Close()
		if called.Load()!=0{t.Fatal("invalid hello opened provider")}
	}
}
