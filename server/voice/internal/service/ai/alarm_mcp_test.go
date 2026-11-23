package ai

import (
  "context"
  "math"
  "strings"
  "testing"
)

func TestAlarmMCPRejectsBadArguments(t *testing.T) {
  b:=newDeviceMCPBridge(func(map[string]any)error{t.Error("unexpected request");return nil},"s")
  invalid:=[]map[string]any{
    {}, {"hour":24.0,"minute":0.0,"day":0.0,"daily":false},
    {"hour":7.5,"minute":0.0,"day":0.0,"daily":false},
    {"hour":math.NaN(),"minute":0.0,"day":0.0,"daily":false},
    {"hour":7.0,"minute":0.0,"day":0.0,"daily":"false"},
    {"hour":7.0,"minute":0.0,"day":0.0,"daily":false,"extra":true},
  }
  for _,args:=range invalid {if _,err:=b.call(context.Background(),"set_alarm",args);err==nil {t.Fatalf("accepted %#v",args)}}
}
func TestAlarmReplyWallTimeAndErrors(t *testing.T) {
  r,err:=translateAlarmReply("set_alarm",`{"ok":true,"id":2,"local_epoch":1728027000,"daily":false}`)
  if err!=nil || !strings.Contains(r,"2024-10-04 07:30") {t.Fatalf("%s %v",r,err)}
  r,err=translateAlarmReply("list_alarms",`{"ok":true,"alarms":[]}`)
  if err!=nil || !strings.Contains(r,`"alarms":[]`) {t.Fatalf("%s %v",r,err)}
  if _,err=translateAlarmReply("set_alarm",`{"ok":false,"error":"clock_unsynchronised"}`);err==nil {t.Fatal("unconfirmed alarm accepted")}
  if _,err=translateAlarmReply("list_alarms",`{"ok":true,"alarms":[{"id":1,"local_epoch":1728027000},{"id":1,"local_epoch":1728027000}]}`);err==nil {t.Fatal("duplicate alarms accepted")}
}
func TestAlarmMCPRequiresDeviceAcknowledgement(t *testing.T) {
  requests:=make(chan map[string]any,1)
  b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"s")
  done:=make(chan error,1)
  go func(){_,err:=b.call(context.Background(),"set_alarm",map[string]any{"hour":7.0,"minute":30.0,"day":0.0,"daily":false});done<-err}()
  req:=<-requests;p:=req["payload"].(map[string]any)
  if p["params"].(map[string]any)["name"]!="self.alarm.set" {t.Fatal(req)}
  b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":`{"ok":false,"error":"clock_unsynchronised"}`}}}}})
  if <-done==nil {t.Fatal("clock failure was accepted")}
}
