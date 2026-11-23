package ai

import (
	"errors"
	"testing"
	"time"
)

func TestProviderLossWaitsForTaggedDeviceWrite(t *testing.T){
	r:=newPersistentProviderRotator(&manualLifecycleAdapter{conn:&manualManagerFake{onClose:func(){}}})
	started,release,done:=make(chan struct{}),make(chan struct{}),make(chan error,1)
	go func(){done<-r.outputIfCurrent(1,func(RealtimeSession)error{close(started);<-release;return nil})}()
	<-started
	closed:=make(chan bool,1)
	go func(){closed<-r.providerClosed(1)}()
	select{case<-closed:t.Fatal("provider removed before write finished");case<-time.After(20*time.Millisecond):}
	close(release)
	if err:=<-done;err!=nil{t.Fatal(err)}
	if !<-closed{t.Fatal("current provider was not closed")}
	if err:=r.outputIfCurrent(1,func(RealtimeSession)error{t.Error("old write accepted");return nil});!errors.Is(err,errProviderUnavailable){t.Fatal(err)}
}
