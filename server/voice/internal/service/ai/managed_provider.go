package ai

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

// managedProvider is an isolated OpenAI lifecycle prototype. Its sink MUST
// serialize provider generation, listen generation and device output in one
// owner; checking IsCurrent before an asynchronous write is not sufficient.
// It is not yet connected to HandleWS.
type managedProviderSink struct {
	OnReady func(uint64)
	OnSTT func(uint64,string)
	OnText func(uint64,string)
	OnAudio func(uint64,[]int16)
	OnStart func(uint64)
	OnStop func(uint64)
	OnLoss func(uint64)
	OnDeviceToolCall func(uint64,context.Context,string,map[string]any)(string,error)
}

type providerGate struct {
	mu sync.Mutex
	active bool
	rejected bool
	closedBeforeActivation bool
	onClose func()
}

func (g *providerGate) closed() {
	g.mu.Lock()
	if g.rejected { g.mu.Unlock(); return }
	if !g.active { g.closedBeforeActivation=true; g.mu.Unlock(); return }
	fn:=g.onClose
	g.mu.Unlock()
	fn()
}

func (g *providerGate) activate() {
	g.mu.Lock()
	if g.rejected { g.mu.Unlock(); return }
	g.active=true
	closed:=g.closedBeforeActivation
	fn:=g.onClose
	g.mu.Unlock()
	if closed { fn() }
}

func (g *providerGate) reject() { g.mu.Lock();g.rejected=true;g.mu.Unlock() }
func (g *providerGate) isActive() bool { g.mu.Lock();defer g.mu.Unlock();return g.active && !g.rejected && !g.closedBeforeActivation }

type managedProvider struct {
	ctx context.Context
	cancel context.CancelFunc
	r *persistentProviderRotator
	dial func(context.Context,uint64,RealtimeCallbacks)(RealtimeSession,error)
	sink managedProviderSink
	recoverKick chan struct{}
	workerDone chan struct{}
	rotationDone chan struct{}
	readyTimeout time.Duration
	backoff func(int)time.Duration
	rotationAfter time.Duration
	rotationCheck time.Duration
	stateMu sync.Mutex
	notifyMu sync.Mutex
	activatedAt time.Time
	activatedGeneration uint64
}

type managedProviderOptions struct { rotationAfter,rotationCheck time.Duration }

func newManagedProvider(parent context.Context, dial func(context.Context,RealtimeCallbacks)(RealtimeSession,error), sink managedProviderSink, opts ...managedProviderOptions) *managedProvider {
	return newManagedProviderWithGeneration(parent,func(ctx context.Context,_ uint64,cb RealtimeCallbacks)(RealtimeSession,error){return dial(ctx,cb)},sink,opts...)
}

func newManagedProviderWithGeneration(parent context.Context, dial func(context.Context,uint64,RealtimeCallbacks)(RealtimeSession,error), sink managedProviderSink, opts ...managedProviderOptions) *managedProvider {
	ctx,cancel:=context.WithCancel(parent)
	rotationAfter,rotationCheck:=55*time.Minute,30*time.Second
	if len(opts)>0 && opts[0].rotationAfter>0 && opts[0].rotationCheck>0 {rotationAfter,rotationCheck=opts[0].rotationAfter,opts[0].rotationCheck}
	m:=&managedProvider{ctx:ctx,cancel:cancel,r:newPersistentProviderRotator(nil),dial:dial,sink:sink,readyTimeout:8*time.Second,recoverKick:make(chan struct{},1),workerDone:make(chan struct{}),rotationDone:make(chan struct{}),backoff:providerRetryDelay,rotationAfter:rotationAfter,rotationCheck:rotationCheck}
	go m.recoverLoop()
	go m.rotationLoop()
	return m
}

func (m *managedProvider) prepare(ctx context.Context, generation uint64)(RealtimeSession,*providerGate,error) {
	ready:=make(chan struct{},1)
	failed:=make(chan struct{},1)
	gate:=&providerGate{}
	gate.onClose=func(){m.lost(generation)}
	onClose:=func(){
		gate.closed()
		select{case failed<-struct{}{}:default:}
	}
	current:=func()bool{return gate.isActive() && m.r.callbackCurrent(generation)}
	cb:=RealtimeCallbacks{
		OnSessionUpdated:func(){select{case ready<-struct{}{}:default:}},
		OnClose:onClose,
		OnSTT:func(v string){if current() && m.sink.OnSTT!=nil{m.sink.OnSTT(generation,v)}},
		OnText:func(v string){if current() && m.sink.OnText!=nil{m.sink.OnText(generation,v)}},
		OnAudio:func(v []int16){if current() && m.sink.OnAudio!=nil{m.sink.OnAudio(generation,v)}},
		OnStart:func(){if current() && m.sink.OnStart!=nil{m.sink.OnStart(generation)}},
		OnStop:func(){if current() && m.sink.OnStop!=nil{m.sink.OnStop(generation)}},
		OnDeviceToolCall:func(ctx context.Context,name string,args map[string]any)(string,error){
			if !current() || m.sink.OnDeviceToolCall==nil{return "",errProviderUnavailable}
			return m.sink.OnDeviceToolCall(generation,ctx,name,args)
		},
	}
	timer:=time.NewTimer(m.readyTimeout)
	defer timer.Stop()
	type dialResult struct{session RealtimeSession;err error}
	result:=make(chan dialResult)
	go func(){
		s,err:=m.dial(ctx,generation,cb)
		select{case result<-dialResult{session:s,err:err}:case <-ctx.Done():if s!=nil{s.Close()}}
	}()
	var session RealtimeSession
	var err error
	select {
	case <-ctx.Done():gate.reject();return nil,nil,ctx.Err()
	case <-failed:gate.reject();return nil,nil,errors.New("provider closed during dial")
	case <-timer.C:gate.reject();return nil,nil,errors.New("provider dial timed out")
	case result:=<-result:
		session,err=result.session,result.err
	}
	if err!=nil {gate.reject();if session!=nil{session.Close()};return nil,nil,err}
	if session==nil {gate.reject();return nil,nil,errProviderUnavailable}
	select {
	case <-ctx.Done(): err=ctx.Err()
	case <-failed: err=errors.New("provider closed during preparation")
	case <-timer.C: err=errors.New("provider session update timed out")
	case <-ready: return session,gate,nil
	}
	gate.reject()
	session.Close()
	return nil,nil,err
}

func (m *managedProvider) recoverOnce() error {
	var gate *providerGate
	var preparedGeneration uint64
	err:=m.r.recover(m.ctx,func(ctx context.Context,generation uint64)(RealtimeSession,error){
		preparedGeneration=generation;s,g,e:=m.prepare(ctx,generation);gate=g;return s,e
	})
	if gate!=nil {if err==nil {gate.activate();m.markActivated(preparedGeneration)} else {gate.reject()}}
	return err
}

func (m *managedProvider) rotate() error {
	var gate *providerGate
	var preparedGeneration uint64
	err:=m.r.rotate(m.ctx,func(ctx context.Context,_ uint64,generation uint64)(RealtimeSession,error){
		preparedGeneration=generation;s,g,e:=m.prepare(ctx,generation);gate=g;return s,e
	})
	if gate!=nil {if err==nil {gate.activate();m.markActivated(preparedGeneration)} else {gate.reject()}}
	return err
}

func (m *managedProvider) markActivated(generation uint64) {
	m.stateMu.Lock()
	ready:=m.r.callbackCurrent(generation)
	if ready {m.activatedAt=time.Now();m.activatedGeneration=generation}
	m.stateMu.Unlock()
	if ready && m.sink.OnReady!=nil {
		m.notifyMu.Lock()
		if m.IsCurrent(generation) {m.sink.OnReady(generation)}
		m.notifyMu.Unlock()
	}
}

func (m *managedProvider) rotationLoop() {
	defer close(m.rotationDone)
	ticker:=time.NewTicker(m.rotationCheck)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():return
		case now:=<-ticker.C:
			m.stateMu.Lock();activated:=m.activatedAt;m.stateMu.Unlock()
			if !activated.IsZero() && now.Sub(activated)>=m.rotationAfter { _=m.rotate() }
		}
	}
}

func (m *managedProvider) lost(generation uint64) {
	if !m.r.providerClosed(generation) { return }
	if m.sink.OnLoss!=nil { m.notifyMu.Lock();m.sink.OnLoss(generation);m.notifyMu.Unlock() }
	m.Start()
}

func (m *managedProvider) Start() {
	select {case m.recoverKick<-struct{}{}:default:}
}

func providerRetryDelay(attempt int) time.Duration {
	base:=time.Second
	for i:=0;i<attempt && base<24*time.Second;i++ {base*=2}
	if base>24*time.Second {base=24*time.Second}
	jitter:=time.Duration(rand.Int64N(int64(base/4)+1))
	return base+jitter
}

func (m *managedProvider) recoverLoop() {
	defer close(m.workerDone)
	for {
		select{case <-m.ctx.Done():return;case <-m.recoverKick:}
		attempt:=0
		for m.ctx.Err()==nil && !m.r.available() {
			_ = m.recoverOnce()
			if m.r.available(){break}
			delay:=m.backoff(attempt)
			attempt++
			timer:=time.NewTimer(delay)
			select{case <-m.ctx.Done():timer.Stop();return;case <-timer.C:}
		}
	}
}

func (m *managedProvider) IsCurrent(generation uint64)bool{
	m.stateMu.Lock();defer m.stateMu.Unlock()
	return m.activatedGeneration==generation && m.r.callbackCurrent(generation)
}
func (m *managedProvider) BeginTurn()error{
	m.stateMu.Lock();defer m.stateMu.Unlock()
	return m.r.beginTurnIfCurrent(m.activatedGeneration)
}
func (m *managedProvider) EndTurn(){m.r.endTurn()}
func (m *managedProvider) EndTurnAndWait(ctx context.Context)error{return m.r.endTurnAndWait(ctx)}
func (m *managedProvider) AppendAudio(pcm []int16)error{return m.r.useTurn(func(s RealtimeSession)error{return s.AppendAudio(pcm)})}
func (m *managedProvider) CommitAudio()error{return m.r.use(func(s RealtimeSession)error{return s.CommitAudio()})}
func (m *managedProvider) CancelResponse()error{return m.r.use(func(s RealtimeSession)error{return s.CancelResponse()})}
func (m *managedProvider) Close(){m.cancel();m.r.close();<-m.workerDone;<-m.rotationDone}
