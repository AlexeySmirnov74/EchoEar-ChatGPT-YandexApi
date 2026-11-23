package ai

import (
 "context"
 "errors"
 "math"
 "strings"
 "testing"
 "time"
)

func TestBrightnessBridgeLimitsAndAcknowledgement(t *testing.T) {
 requests:=make(chan map[string]any,1)
 b:=newDeviceMCPBridge(func(v map[string]any) error { requests<-v; return nil },"test-session")
 for _,args:=range []map[string]any{{},{"brightness":0.0},{"brightness":9.0},{"brightness":101.0},{"brightness":10.5},{"brightness":math.NaN()},{"brightness":math.Inf(1)},{"brightness":"50"},{"brightness":50.0,"extra":true}} {
  if _,err:=b.call(context.Background(),"set_screen_brightness",args);err==nil { t.Fatalf("accepted invalid args: %#v",args) }
 }
 select {case <-requests:t.Fatal("invalid value reached hardware");default:}
 for _,n:=range []float64{10,50,100} {
  done:=make(chan error,1)
  go func(){result,err:=b.call(context.Background(),"set_screen_brightness",map[string]any{"brightness":n});if err==nil && result!=`{"success":true}`{err=errors.New("invalid result")};done<-err}()
  p:=(<-requests)["payload"].(map[string]any)
  params:=p["params"].(map[string]any)
  if params["name"]!="self.screen.set_brightness" || params["arguments"].(map[string]any)["brightness"]!=int(n){t.Fatalf("wrong MCP request: %#v",params)}
  select{case <-done:t.Fatal("reported success before confirmation");default:}
  b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":"true"}}}}})
  select {case err:=<-done:if err!=nil{t.Fatal(err)};case <-time.After(time.Second):t.Fatal("no result")}
 }
}

func TestBrightnessStatusPrivacyAndInvalidResponses(t *testing.T) {
 for _,tc:=range []struct{status string;wantErr bool}{
  {`{"screen":{"brightness":37},"network":{"ssid":"private"}}`,false},
  {`{"screen":{}}`,true}, {`{"screen":{"brightness":-1}}`,true}, {`{"screen":{"brightness":101}}`,true}, {`not-json`,true},
 } {
  t.Run(tc.status,func(t *testing.T){
   requests:=make(chan map[string]any,1)
   b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"s")
   done:=make(chan struct{value string; err error},1)
   go func(){v,e:=b.call(context.Background(),"get_screen_brightness",map[string]any{});done<-struct{value string;err error}{v,e}}()
   p:=(<-requests)["payload"].(map[string]any)
   if p["params"].(map[string]any)["name"]!="self.get_device_status"{t.Fatal(p)}
   b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":tc.status}}}}})
   got:=<-done
   if (got.err!=nil)!=tc.wantErr {t.Fatalf("result=%+v",got)}
   if !tc.wantErr && (got.value!=`{"brightness":37}` || strings.Contains(got.value,"private")){t.Fatal(got.value)}
  })
 }
}

func TestBrightnessRejectedDeviceCommand(t *testing.T){
 requests:=make(chan map[string]any,1);b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"s")
 done:=make(chan error,1);go func(){_,e:=b.call(context.Background(),"set_screen_brightness",map[string]any{"brightness":60.0});done<-e}()
 p:=(<-requests)["payload"].(map[string]any)
 b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"isError":true,"content":[]any{map[string]any{"type":"text","text":"true"}}}}})
 if <-done==nil {t.Fatal("device rejection was reported as success")}
}
