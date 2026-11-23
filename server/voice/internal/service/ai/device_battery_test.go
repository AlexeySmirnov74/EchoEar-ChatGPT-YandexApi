package ai

import (
 "context"
 "strings"
 "testing"
 "time"
)

func TestBatteryEstimateReadValidationAndPrivacy(t *testing.T) {
 cases:=[]struct{name,status,want string;wantErr bool}{
  {"estimated",`{"available":true,"estimated_percent":49,"approximate":true,"charging_status":"unknown","sensitive":"private"}`,`{"approximate":true,"available":true,"charging_status":"unknown","estimated_percent":49}`,false},
  {"unavailable",`{"available":false}`,`{"available":false}`,false},
  {"unavailable with bogus number",`{"available":false,"estimated_percent":70}`,`{"available":false}`,false},
  {"empty","{}","",true},
  {"bad JSON","not json","",true},
  {"bad available",`{"available":"true"}`,"",true},
  {"missing number",`{"available":true,"approximate":true,"charging_status":"unknown"}`,"",true},
  {"out of range",`{"available":true,"estimated_percent":101,"approximate":true,"charging_status":"unknown"}`,"",true},
  {"negative",`{"available":true,"estimated_percent":-1,"approximate":true,"charging_status":"unknown"}`,"",true},
  {"noninteger",`{"available":true,"estimated_percent":49.5,"approximate":true,"charging_status":"unknown"}`,"",true},
  {"false accuracy",`{"available":true,"estimated_percent":49,"approximate":false,"charging_status":"unknown"}`,"",true},
  {"guessed charging",`{"available":true,"estimated_percent":49,"approximate":true,"charging_status":"charging"}`,"",true},
 }
 for _,tc:=range cases { t.Run(tc.name,func(t *testing.T){
  requests:=make(chan map[string]any,1)
  b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"battery-session")
  type response struct{value string;err error};done:=make(chan response,1)
  go func(){v,e:=b.call(context.Background(),"get_battery_estimate",map[string]any{});done<-response{v,e}}()
  p:=(<-requests)["payload"].(map[string]any); params:=p["params"].(map[string]any)
  if params["name"]!="self.battery.get_estimate" || len(params["arguments"].(map[string]any))!=0 {t.Fatal(params)}
  select{case <-done:t.Fatal("returned before device response");default:}
  b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":tc.status}}}}})
  select{case got:=<-done:
   if (got.err!=nil)!=tc.wantErr || got.value!=tc.want || strings.Contains(got.value,"private") {t.Fatalf("got %+v",got)}
  case <-time.After(time.Second):t.Fatal("no response")}
 }) }
}

func TestBatteryToolRejectedArgumentsAndDeviceError(t *testing.T) {
 requests:=make(chan map[string]any,1)
 b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"s")
 if _,err:=b.call(context.Background(),"get_battery_estimate",map[string]any{"query":true});err==nil{t.Fatal("accepted extra arguments")}
 select{case <-requests:t.Fatal("sent invalid request");default:}
 done:=make(chan error,1);go func(){_,e:=b.call(context.Background(),"get_battery_estimate",map[string]any{});done<-e}()
 p:=(<-requests)["payload"].(map[string]any)
 b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"error":map[string]any{"message":"unknown tool"}}})
 if <-done==nil {t.Fatal("unknown device capability was accepted as a reading")}
}
