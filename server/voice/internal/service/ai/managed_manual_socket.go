package ai

import (
	"context"
	"sync"
)

// managedManualSocket is an isolated integration adapter; no application path
// uses it. The consuming device actor must fence generation/listen/response
// atomically at its final write. An undrained event queue fails closed.
type managedManualSocket struct {
	conn *manualRealtimeConnection
	done chan struct{}
	once sync.Once
}

func (s *managedManualSocket) Ready(context.Context)error{return nil} // dial completed only after verified ACK
func (s *managedManualSocket) Begin(id uint32)error{return s.conn.runtime.Begin(id)}
func (s *managedManualSocket) Append(id uint32,pcm []int16)error{return s.conn.runtime.Append(id,pcm)}
func (s *managedManualSocket) Commit(id uint32)error{return s.conn.runtime.Commit(id)}
func (s *managedManualSocket) Clear(id uint32)error{return s.conn.runtime.Clear(id)}
func (s *managedManualSocket) ToolResult(id uint32,responseID,callID,result string)error{return s.conn.runtime.ToolResult(id,responseID,callID,result)}
func (s *managedManualSocket) NotifyImageResult(id uint32,status string)error{return s.conn.runtime.NotifyImageResult(id,status)}
func (s *managedManualSocket) CancelImageNotice(id uint32)error{return s.conn.runtime.CancelImageNotice(id)}
func (s *managedManualSocket) FinishImageNoticeCancel(id uint32,response string,stop func(uint32)error,resumeSame bool)error{return s.conn.runtime.FinishImageNoticeCancel(id,response,stop,resumeSame)}
func (s *managedManualSocket) FinishPlayback(id uint32,responseID string,sendStop func(uint32)error)error{return s.conn.runtime.FinishPlayback(id,responseID,sendStop)}
func (s *managedManualSocket) DeviceWrite(generation uint64,id uint32,responseID string,send func()error)error{return s.conn.runtime.DeviceWrite(generation,id,responseID,send)}
func (s *managedManualSocket) Standby(id uint32)error{return s.conn.runtime.Standby(id)}
func (s *managedManualSocket) Close(){s.once.Do(func(){close(s.done);s.conn.Close()})}

func newManagedManualSocketDial(endpoint,key,instructions,voice string,tools []map[string]any,events chan<- manualRealtimeEvent) func(context.Context,uint64,func())(managedManualConnection,error) {
	return func(ctx context.Context,generation uint64,onClose func())(managedManualConnection,error){
		conn,err:=manualDialReady(ctx,endpoint,key,instructions,voice,tools);if err!=nil{return nil,err}
		if err:=conn.startManualRuntime(generation,64);err!=nil{conn.Close();return nil,err}
		s:=&managedManualSocket{conn:conn,done:make(chan struct{})}
		go func(){
			selectLoop: for {select {
			case <-s.done: break selectLoop
			case <-ctx.Done(): break selectLoop
			case <-conn.runtime.failures: onClose();break selectLoop
			case e:=<-conn.runtime.events:
				select {case events<-e:default:onClose();break selectLoop}
			}}
		}()
		return s,nil
	}
}
