package ai

import (
	"context"
	"sync"
)

// managedActorRuntime joins the isolated manual client and recovery manager.
// Its bounded state queue cancels the actor on overflow instead of dropping a
// generation transition. Provider events are similarly bounded by the socket
// forwarder. It is used only by the explicitly enabled manual endpoint.
type managedActorRuntime struct {
	*managedManualProvider
	events chan manualRealtimeEvent
	states chan manualProviderState
	ready chan struct{}
	readyOnce sync.Once
	cancel context.CancelFunc
}

func newManagedActorRuntime(parent context.Context,endpoint,key,instructions,voice string,tools []map[string]any)(*managedActorRuntime,context.Context){
	ctx,cancel:=context.WithCancel(parent)
	a:=&managedActorRuntime{events:make(chan manualRealtimeEvent,256),states:make(chan manualProviderState,32),ready:make(chan struct{}),cancel:cancel}
	notify:=func(state manualProviderState){select{case a.states<-state:default:cancel()}}
	sink:=managedProviderSink{
		OnReady:func(g uint64){notify(manualProviderState{Generation:g,Ready:true});a.readyOnce.Do(func(){close(a.ready)})},
		OnLoss:func(g uint64){notify(manualProviderState{Generation:g,Ready:false})},
	}
	dial:=newManagedManualSocketDial(endpoint,key,instructions,voice,tools,a.events)
	a.managedManualProvider=newManagedManualProvider(ctx,dial,sink)
	a.Start()
	return a,ctx
}
func (a *managedActorRuntime) Events()<-chan manualRealtimeEvent{return a.events}
func (a *managedActorRuntime) GenerationEvents()<-chan manualProviderState{return a.states}
func (a *managedActorRuntime) Failures()<-chan uint64{return nil} // loss is reported in states
func (a *managedActorRuntime) Close(){a.cancel();a.managedManualProvider.Close()}
