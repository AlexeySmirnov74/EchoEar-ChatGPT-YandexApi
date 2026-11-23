package ai

import (
    "encoding/json"
    "errors"
    "fmt"
    "time"
)

type alarmWire struct {
    ID int `json:"id"`
    When int64 `json:"local_epoch"`
    Daily bool `json:"daily"`
    Ringing bool `json:"ringing"`
}

func translateAlarmReply(name, raw string) (string,error) {
    var r struct { Ok bool `json:"ok"`; Error string `json:"error"`; alarmWire; Alarms []alarmWire `json:"alarms"` }
    if json.Unmarshal([]byte(raw), &r) != nil { return "",errors.New("invalid alarm device response") }
    if !r.Ok { return "",fmt.Errorf("alarm device rejected request: %s",r.Error) }
    if name == "delete_alarm" { return `{"ok":true}`,nil }
    convert:=func(a alarmWire)(map[string]any,error){
        if a.ID<1 || a.When<1700000000 || a.When>4102444800 {return nil,errors.New("invalid alarm device data")}
        return map[string]any{"id":a.ID,"next_local":time.Unix(a.When,0).UTC().Format("2006-01-02 15:04"),"daily":a.Daily,"ringing":a.Ringing},nil
    }
    if name=="set_alarm" { a,err:=convert(r.alarmWire);if err!=nil{return "",err};b,_:=json.Marshal(a);return string(b),nil }
    if name!="list_alarms" || r.Alarms==nil || len(r.Alarms)>8 {return "",errors.New("invalid alarm list")}
    alarms:=make([]map[string]any,0,len(r.Alarms));seen:=map[int]bool{}
    for _,a:=range r.Alarms { if seen[a.ID] {return "",errors.New("duplicate alarm id")};seen[a.ID]=true;item,err:=convert(a);if err!=nil{return "",err};alarms=append(alarms,item) }
    b,_:=json.Marshal(map[string]any{"alarms":alarms});return string(b),nil
}
