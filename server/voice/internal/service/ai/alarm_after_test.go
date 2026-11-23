package ai

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestRelativeAlarmDeviceAcknowledgement(t *testing.T) {
	for _, tc := range []struct { text string; wantError bool }{
		{`{"ok":true,"id":7,"local_epoch":1709164860,"daily":false}`,false},
		{`{"ok":false,"error":"clock_unsynchronised"}`,true},
		{`{"id":7}`,true},
		{`{"ok":true,"id":7,"local_epoch":1709164860}`,true},
		{`{"ok":true,"id":7,"local_epoch":1709164860,"daily":true}`,true},
	} {
		requests:=make(chan map[string]any,2)
		b:=newDeviceMCPBridge(func(v map[string]any)error{requests<-v;return nil},"s")
		type response struct { result string; err error }; done:=make(chan response,1)
		go func(){v,e:=b.call(context.Background(),"set_alarm_after",map[string]any{"minutes":float64(2)});done<-response{v,e}}()
		req:=<-requests;p:=req["payload"].(map[string]any);params:=p["params"].(map[string]any)
		if params["name"]!="self.alarm.set_after" || params["arguments"].(map[string]any)["minutes"]!=2 {t.Fatal(req)}
		b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":tc.text}}}}})
		out:=<-done
		if (out.err!=nil)!=tc.wantError {t.Fatalf("%s %v",out.result,out.err)}
		if !tc.wantError && !strings.Contains(out.result,"2024-02-29 00:01") {t.Fatal(out.result)}
		select {case next:=<-requests: t.Fatalf("unexpected retry or cleanup: %#v",next);default:}
	}
}

func TestRelativeAlarmRejectsBadInputBeforeDevice(t *testing.T) {
	b:=newDeviceMCPBridge(func(map[string]any)error{t.Fatal("unexpected device call");return nil},"s")
	for _, args:=range []map[string]any{{},{"minutes":0.0},{"minutes":1440.0},{"minutes":1.5},{"minutes":math.NaN()},{"minutes":1.0,"extra":true}} {
		if _,err:=b.call(context.Background(),"set_alarm_after",args);err==nil {t.Fatalf("accepted %#v",args)}
	}
}
