package ai

import (
	"context"
	"errors"
)

// managedManualConnection is the narrow lifecycle/command boundary for the
// experimental manual Realtime client. Ready must verify session.updated and
// turn_detection:null, not merely a TCP handshake. This adapter is not wired
// into HandleWS. Its caller must independently fence every output/tool write
// atomically by provider generation, listen ID, and response ID.
type managedManualConnection interface {
	Ready(context.Context) error
	Begin(uint32) error
	Append(uint32, []int16) error
	Commit(uint32) error
	Clear(uint32) error
	ToolResult(uint32, string, string, string) error
	NotifyImageResult(uint32,string) error
	CancelImageNotice(uint32) error
	FinishImageNoticeCancel(uint32,string,func(uint32)error,bool) error
	FinishPlayback(uint32, string, func(uint32) error) error
	DeviceWrite(uint64, uint32, string, func() error) error
	Standby(uint32) error
	Close()
}

type manualLifecycleAdapter struct{ conn managedManualConnection }
func (a *manualLifecycleAdapter) AppendAudio([]int16) error { return errors.New("listen ID required") }
func (a *manualLifecycleAdapter) CommitAudio() error { return errors.New("listen ID required") }
func (a *manualLifecycleAdapter) CancelResponse() error { return errors.New("manual session requires reset") }
func (a *manualLifecycleAdapter) Close() { a.conn.Close() }

type managedManualProvider struct{ lifecycle *managedProvider }

// dial delivers failures to onClose exactly when the underlying WS is lost;
// it may deliver other events directly to a generation-aware owner. That owner
// must not consider IsCurrent alone an atomic output authorization.
func newManagedManualProvider(parent context.Context, dial func(context.Context,uint64,func())(managedManualConnection,error), sink managedProviderSink, opts ...managedProviderOptions) *managedManualProvider {
	lifecycle:=newManagedProviderWithGeneration(parent,func(ctx context.Context,generation uint64,cb RealtimeCallbacks)(RealtimeSession,error){
		conn,err:=dial(ctx,generation,cb.OnClose)
		if err!=nil{return nil,err}
		if conn==nil{return nil,errProviderUnavailable}
		if err:=conn.Ready(ctx);err!=nil{conn.Close();return nil,err}
		cb.OnSessionUpdated()
		return &manualLifecycleAdapter{conn:conn},nil
	},sink,opts...)
	return &managedManualProvider{lifecycle:lifecycle}
}

func (m *managedManualProvider) Start(){m.lifecycle.Start()}
func (m *managedManualProvider) IsCurrent(generation uint64)bool{return m.lifecycle.IsCurrent(generation)}
func (m *managedManualProvider) Begin(id uint32)error{
	if err:=m.lifecycle.BeginTurn();err!=nil{return err}
	err:=m.lifecycle.r.use(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.Begin(id)})
	if err!=nil{m.lifecycle.EndTurn()}
	return err
}
func (m *managedManualProvider) Append(id uint32,pcm []int16)error{
	return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.Append(id,pcm)})
}
func (m *managedManualProvider) Commit(id uint32)error{
	return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.Commit(id)})
}
func (m *managedManualProvider) Clear(id uint32)error{
	return m.lifecycle.r.use(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.Clear(id)})
}
func (m *managedManualProvider) ToolResult(id uint32,responseID,callID,result string)error{
	return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.ToolResult(id,responseID,callID,result)})
}
func (m *managedManualProvider) NotifyImageResult(id uint32,status string)error{
	return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.NotifyImageResult(id,status)})
}
func (m *managedManualProvider) CancelImageNotice(id uint32)error{return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.CancelImageNotice(id)})}
func (m *managedManualProvider) FinishImageNoticeCancel(id uint32,response string,stop func(uint32)error,resumeSame bool)error{return m.lifecycle.r.useTurn(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.FinishImageNoticeCancel(id,response,stop,resumeSame)})}
func (m *managedManualProvider) FinishPlayback(id uint32,responseID string,sendStop func(uint32)error)error{
	m.lifecycle.stateMu.Lock();generation:=m.lifecycle.activatedGeneration;m.lifecycle.stateMu.Unlock()
	return m.lifecycle.r.outputIfCurrent(generation,func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.FinishPlayback(id,responseID,sendStop)})
}
func (m *managedManualProvider) DeviceWrite(generation uint64,id uint32,responseID string,send func()error)error{
	return m.lifecycle.r.outputIfCurrent(generation,func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.DeviceWrite(generation,id,responseID,send)})
}
func (m *managedManualProvider) Standby(id uint32)error{
	return m.lifecycle.r.use(func(s RealtimeSession)error{return s.(*manualLifecycleAdapter).conn.Standby(id)})
}
func (m *managedManualProvider) EndTurnAndWait(ctx context.Context)error{return m.lifecycle.EndTurnAndWait(ctx)}
// Invalidate retires only the provider whose ACK/write failed. It is safe for
// the actor to call on a deadline; delayed failures from older generations do
// not replace or interrupt a newer provider.
func (m *managedManualProvider) Invalidate(generation uint64){m.lifecycle.lost(generation)}
func (m *managedManualProvider) Close(){m.lifecycle.Close()}
