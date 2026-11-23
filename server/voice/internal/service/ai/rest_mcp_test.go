package ai

import (
    "context"
    "encoding/json"
    "strings"
    "testing"
)

func TestRestToolsAndStrictDeviceConfirmation(t *testing.T) {
    tools := restRealtimeTools()
    if len(tools) != 3 { t.Fatal("rest tools missing") }
    for _, tool := range tools {
        if !isSpeakerTool(tool["name"].(string)) { t.Fatal("tool not dispatched") }
        if _, err := json.Marshal(tool); err != nil { t.Fatal(err) }
    }
    cases := []struct{name, device, reply, expected string; fail bool}{
        {"enter_rest", "self.rest.enter", `{"success":true,"mode":"resting","extra":"secret"}`, `{"mode":"resting","success":true}`, false},
        {"enter_rest", "self.rest.enter", `{"success":false,"mode":"awake"}`, `{"mode":"awake","success":false}`, false},
        {"enter_rest", "self.rest.enter", `{"success":true,"mode":"awake"}`, "", true},
        {"enter_rest", "self.rest.enter", `{"mode":"resting"}`, "", true},
        {"enter_rest", "self.rest.enter", `true`, "", true},
        {"get_rest_status", "self.rest.status", `{"success":true,"mode":"awake"}`, `{"mode":"awake","success":true}`, false},
        {"get_rest_status", "self.rest.status", `{"success":true,"mode":"unknown"}`, "", true},
        {"wake_from_rest", "self.rest.wake", `{"success":true,"mode":"awake"}`, `{"mode":"awake","success":true}`, false},
        {"wake_from_rest", "self.rest.wake", `{"success":true,"mode":"resting"}`, "", true},
    }
    for _, tc := range cases { t.Run(tc.name+"/"+tc.reply, func(t *testing.T) {
        requests := make(chan map[string]any,1)
        b := newDeviceMCPBridge(func(m map[string]any) error { requests <- m; return nil }, "s")
        done := make(chan struct{value string; err error},1)
        go func(){ v,e:=b.call(context.Background(),tc.name,map[string]any{}); done<-struct{value string; err error}{v,e} }()
        p := (<-requests)["payload"].(map[string]any)
        params := p["params"].(map[string]any)
        if params["name"] != tc.device || len(params["arguments"].(map[string]any)) != 0 { t.Fatal("incorrect MCP request") }
        b.handleReply(map[string]any{"payload":map[string]any{"jsonrpc":"2.0","id":float64(p["id"].(int)),"result":map[string]any{"content":[]any{map[string]any{"type":"text","text":tc.reply}}}}})
        got:=<-done
        if (got.err != nil) != tc.fail || (!tc.fail && got.value != tc.expected) || strings.Contains(got.value,"secret") { t.Fatalf("unexpected result %q error %v",got.value,got.err) }
    }) }
}

func TestRestRejectsExtraArgumentsBeforeDeviceCall(t *testing.T) {
    called := false
    b:=newDeviceMCPBridge(func(map[string]any)error{called=true;return nil},"s")
    for _, n := range []string{"enter_rest","get_rest_status","wake_from_rest"} {
        if _,err:=b.call(context.Background(),n,map[string]any{"unexpected":true});err==nil {t.Fatal("extra argument accepted")}
    }
    if called {t.Fatal("invalid command was sent")}
}
