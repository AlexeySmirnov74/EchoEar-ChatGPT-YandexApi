package ai

import (
 "testing"
 "time"
)

func TestManualServerTimeBounds(t *testing.T) {
 for _,tc:=range []struct{ms int64;want bool}{
  {1699999999999,false}, {1700000000000,true}, {1800000000123,true}, {4102358399999,true}, {4102358400000,false},
 } {
  got:=manualServerTime(time.UnixMilli(tc.ms))
  if (got!=nil)!=tc.want {t.Fatalf("timestamp %d: %#v",tc.ms,got)}
  if tc.want && (got["timestamp"]!=tc.ms || got["timezone_offset"]!=300) {t.Fatalf("wrong clock payload: %#v",got)}
 }
}
