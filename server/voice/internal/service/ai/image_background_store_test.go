package ai

import (
	"bytes"
	"os"
	"testing"
)

func TestBackgroundStoreAtomicRoundTripAndSessionLease(t *testing.T) {
	s:=&backgroundStore{dir:t.TempDir()};device:="speaker";old,err:=s.acquire(device);if err!=nil{t.Fatal(err)}
	pixels:=bytes.Repeat([]byte{0xf8,0},imageFrameBytes/2)
	if err=s.write(device,old,pixels);err!=nil{t.Fatal(err)}
	newLease,err:=s.acquire(device);if err!=nil{t.Fatal(err)}
	got,err:=s.read(device,newLease);if err!=nil||!bytes.Equal(got,pixels){t.Fatal("frame did not survive reconnect",err)}
	if err=s.write(device,old,nil);err!=errBackgroundStaleSession{t.Fatal("stale session modified state",err)}
	s.release(device,old);if !s.current(device,newLease){t.Fatal("stale close released current lease")}
	if err=s.writePending(device,newLease);err!=nil{t.Fatal(err)}
	got,err=s.read(device,newLease);if err!=nil||got!=nil{t.Fatal("uncertain operation resurrected old background",err)}
	if err=s.write(device,newLease,pixels);err!=nil{t.Fatal(err)}
	if err=s.write(device,newLease,nil);err!=nil{t.Fatal(err)}
	got,err=s.read(device,newLease);if err!=nil||got!=nil{t.Fatal("clear not persisted",err)}
	path,_:=s.path(device);if err=os.WriteFile(path,[]byte("corrupt"),0600);err!=nil{t.Fatal(err)}
	if _,err=s.read(device,newLease);err==nil{t.Fatal("accepted corrupted file")}
	if _,err=s.acquire("");err==nil{t.Fatal("accepted empty device ID")}
}
