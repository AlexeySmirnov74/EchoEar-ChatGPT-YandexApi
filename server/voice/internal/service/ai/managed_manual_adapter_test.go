package ai

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type manualManagerFake struct {
	mu sync.Mutex
	ready chan struct{}
	onClose func()
	closeOnce sync.Once
	frames int
}
func (f *manualManagerFake) Ready(ctx context.Context)error{select{case<-f.ready:return nil;case<-ctx.Done():return ctx.Err()}}
func (*manualManagerFake) Begin(uint32)error{return nil}
func (f *manualManagerFake) Append(_ uint32,_ []int16)error{f.mu.Lock();f.frames++;f.mu.Unlock();return nil}
func (*manualManagerFake) Commit(uint32)error{return nil}
func (*manualManagerFake) Clear(uint32)error{return nil}
func (*manualManagerFake) ToolResult(uint32,string,string,string)error{return nil}
func (*manualManagerFake) NotifyImageResult(uint32,string)error{return nil}
func (*manualManagerFake) CancelImageNotice(uint32)error{return nil}
func (*manualManagerFake) FinishImageNoticeCancel(uint32,string,func(uint32)error,bool)error{return nil}
func (*manualManagerFake) FinishPlayback(uint32,string,func(uint32)error)error{return nil}
func (*manualManagerFake) DeviceWrite(_ uint64,_ uint32,_ string,send func()error)error{return send()}
func (*manualManagerFake) Standby(uint32)error{return nil}
func (f *manualManagerFake) Close(){f.closeOnce.Do(f.onClose)}
func (f *manualManagerFake) count()int{f.mu.Lock();defer f.mu.Unlock();return f.frames}

func TestManualManagerReadyFailureRecoveryAndIdle(t *testing.T){
	type made struct{generation uint64; f *manualManagerFake}
	created:=make(chan made,4)
	loss:=make(chan uint64,4)
	m:=newManagedManualProvider(context.Background(),func(_ context.Context,generation uint64,onClose func())(managedManualConnection,error){
		f:=&manualManagerFake{ready:make(chan struct{}),onClose:onClose};created<-made{generation,f};return f,nil
	},managedProviderSink{OnLoss:func(g uint64){loss<-g}})
	defer m.Close()
	m.Start()
	var first made
	select{case first=<-created:case<-time.After(time.Second):t.Fatal("dial not started")}
	if m.lifecycle.r.available(){t.Fatal("installed without session.updated")}
	close(first.f.ready)
	deadline:=time.Now().Add(time.Second)
	for !m.IsCurrent(first.generation)&&time.Now().Before(deadline){time.Sleep(time.Millisecond)}
	if !m.IsCurrent(first.generation){t.Fatal("first provider not installed")}
	if err:=m.Append(1,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("idle input accepted",err)}
	if err:=m.Begin(1);err!=nil{t.Fatal(err)}
	if err:=m.Append(1,[]int16{1});err!=nil{t.Fatal(err)}
	if first.f.count()!=1{t.Fatal("audio not forwarded")}
	first.f.onClose()
	select{case g:=<-loss:if g!=first.generation{t.Fatal("wrong lost generation")};case<-time.After(time.Second):t.Fatal("loss not detected")}
	var second made
	select{case second=<-created:case<-time.After(time.Second):t.Fatal("replacement not created")}
	close(second.f.ready)
	deadline=time.Now().Add(time.Second)
	for !m.IsCurrent(second.generation)&&time.Now().Before(deadline){time.Sleep(time.Millisecond)}
	if !m.IsCurrent(second.generation){t.Fatal("replacement not ready")}
	first.f.onClose() // delayed close for retired generation
	if !m.IsCurrent(second.generation){t.Fatal("stale close removed replacement")}
	if err:=m.Append(2,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("recovery started microphone",err)}
	if err:=m.Begin(2);err!=nil{t.Fatal(err)}
	if err:=m.Append(2,[]int16{1});err!=nil{t.Fatal(err)}
	if err:=m.EndTurnAndWait(context.Background());err!=nil{t.Fatal(err)}
	if err:=m.Append(2,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("paused input accepted",err)}
	m.Invalidate(first.generation)
	if !m.IsCurrent(second.generation){t.Fatal("old deadline invalidated current provider")}
	m.Invalidate(second.generation)
	select{case g:=<-loss:if g!=second.generation{t.Fatal("wrong invalidated generation")};case<-time.After(time.Second):t.Fatal("deadline loss not reported")}
	var third made
	select{case third=<-created:case<-time.After(time.Second):t.Fatal("deadline did not schedule recovery")}
	close(third.f.ready)
	deadline=time.Now().Add(time.Second)
	for !m.IsCurrent(third.generation)&&time.Now().Before(deadline){time.Sleep(time.Millisecond)}
	if !m.IsCurrent(third.generation){t.Fatal("provider did not recover after deadline")}
	if err:=m.Append(3,[]int16{1});!errors.Is(err,errProviderIdle){t.Fatal("deadline recovery enabled microphone",err)}
}
