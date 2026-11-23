package ai

import (
	"context"
	"errors"
	"sync"
)

var errRotationDeferred = errors.New("provider rotation deferred until the current turn finishes")
var errProviderUnavailable = errors.New("realtime provider unavailable")
var errProviderIdle = errors.New("audio input is disabled while idle")

// persistentProviderRotator is a tested handoff primitive. It is intentionally
// not wired into the running server until response-generation tagging, context
// restoration and error reporting have been integrated and tested together.
type persistentProviderRotator struct {
	mu sync.Mutex
	outputFence sync.RWMutex
	current RealtimeSession
	turnActive bool
	preparing bool
	closed bool
	inflight int
	audioInflight int
	audioDrained chan struct{}
	currentGeneration uint64
	nextGeneration uint64
	contextVersion uint64
	cancelPrepare context.CancelFunc
	cancelCurrent context.CancelFunc
}

func newPersistentProviderRotator(current RealtimeSession) *persistentProviderRotator {
	return &persistentProviderRotator{current:current,currentGeneration:1,nextGeneration:1}
}

func (r *persistentProviderRotator) callbackCurrent(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.current!=nil && generation!=0 && generation==r.currentGeneration
}

func (r *persistentProviderRotator) available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.current!=nil
}

// providerClosed returns true only once for the current provider. A delayed
// close from a deliberately retired provider must not close the device.
func (r *persistentProviderRotator) providerClosed(generation uint64) bool {
	r.outputFence.Lock()
	r.mu.Lock()
	if r.closed || r.current==nil || generation==0 || generation!=r.currentGeneration { r.mu.Unlock(); r.outputFence.Unlock(); return false }
	previous:=r.current
	previousCancel:=r.cancelCurrent
	r.current=nil
	r.currentGeneration=0
	r.cancelCurrent=nil
	r.turnActive=false
	if r.cancelPrepare!=nil { r.cancelPrepare() }
	r.mu.Unlock()
	r.outputFence.Unlock()
	previous.Close()
	if previousCancel!=nil { previousCancel() }
	return true
}

// outputIfCurrent protects the actual finite-duration device write against
// provider swaps and close. No lifecycle mutex is held during the write.
// operation must not synchronously re-enter the lifecycle manager.
func (r *persistentProviderRotator) outputIfCurrent(generation uint64, operation func(RealtimeSession)error)error {
	r.outputFence.RLock()
	defer r.outputFence.RUnlock()
	r.mu.Lock()
	if r.closed||r.current==nil||generation==0||r.currentGeneration!=generation {r.mu.Unlock();return errProviderUnavailable}
	current:=r.current
	r.mu.Unlock()
	return operation(current)
}

// recover prepares a provider after an unexpected loss. It intentionally
// starts a new context and never replays a partially delivered user utterance.
// The integration must tell the user that the interrupted turn needs retrying.
func (r *persistentProviderRotator) recover(ctx context.Context, prepare func(context.Context,uint64)(RealtimeSession,error)) error {
	r.mu.Lock()
	if r.closed { r.mu.Unlock(); return errProviderUnavailable }
	if r.current!=nil || r.preparing { r.mu.Unlock(); return errRotationDeferred }
	r.preparing=true
	r.nextGeneration++
	generation:=r.nextGeneration
	dialCtx,cancel:=context.WithCancel(ctx)
	r.cancelPrepare=cancel
	r.mu.Unlock()

	candidate,err:=prepare(dialCtx,generation)
	r.outputFence.Lock()
	r.mu.Lock()
	r.preparing=false
	r.cancelPrepare=nil
	if err!=nil || candidate==nil || r.closed || r.current!=nil || dialCtx.Err()!=nil {
		r.mu.Unlock()
		r.outputFence.Unlock()
		cancel()
		if candidate!=nil { candidate.Close() }
		if err!=nil { return err }
		return errRotationDeferred
	}
	r.current=candidate
	r.currentGeneration=generation
	r.cancelCurrent=cancel
	r.mu.Unlock()
	r.outputFence.Unlock()
	return nil
}

// beginTurn invalidates an in-progress handoff. The old provider remains in
// place, so the first audio of the new utterance is not held behind a dial.
func (r *persistentProviderRotator) beginTurn() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.current==nil { return errProviderUnavailable }
	r.turnActive=true
	if r.cancelPrepare!=nil { r.cancelPrepare() }
	return nil
}

// beginTurnIfCurrent atomically verifies the activated provider and starts the
// turn, so a simultaneous handoff cannot redirect it to an unactivated one.
func (r *persistentProviderRotator) beginTurnIfCurrent(generation uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.current==nil || generation==0 || r.currentGeneration!=generation { return errProviderUnavailable }
	r.turnActive=true
	if r.cancelPrepare!=nil { r.cancelPrepare() }
	return nil
}

func (r *persistentProviderRotator) endTurn() {
	r.mu.Lock()
	r.turnActive=false
	r.mu.Unlock()
}

// endTurnAndWait fences in-flight input before the caller sends a clear to
// Realtime. It prevents later input leases immediately and waits without
// holding the lifecycle lock. The caller serializes the next beginTurn with
// this barrier and subsequent clear acknowledgement.
func (r *persistentProviderRotator) endTurnAndWait(ctx context.Context) error {
	r.mu.Lock()
	r.turnActive = false
	if r.audioInflight == 0 { r.mu.Unlock(); return nil }
	drained := r.audioDrained
	r.mu.Unlock()
	select { case <-drained: return nil; case <-ctx.Done(): return ctx.Err() }
}

// contextChanged invalidates a context snapshot if another transcript arrives
// while the next provider is being prepared.
func (r *persistentProviderRotator) contextChanged() {
	r.mu.Lock()
	r.contextVersion++
	if r.cancelPrepare!=nil { r.cancelPrepare() }
	r.mu.Unlock()
}

// use acquires a provider lease and never holds the lifecycle lock over network
// I/O or device MCP calls. Rotation defers while a lease is outstanding.
func (r *persistentProviderRotator) use(operation func(RealtimeSession) error) error {
	r.mu.Lock()
	if r.closed || r.current==nil { r.mu.Unlock(); return errProviderUnavailable }
	current:=r.current
	r.inflight++
	r.mu.Unlock()
	defer func(){r.mu.Lock();r.inflight--;r.mu.Unlock()}()
	return operation(current)
}

// useTurn rejects new microphone writes in standby. The caller must use
// endTurnAndWait before sending a clear; endTurn alone does not drain writes.
func (r *persistentProviderRotator) useTurn(operation func(RealtimeSession) error) error {
	r.mu.Lock()
	if r.closed || r.current == nil { r.mu.Unlock(); return errProviderUnavailable }
	if !r.turnActive { r.mu.Unlock(); return errProviderIdle }
	current := r.current
	r.inflight++
	if r.audioInflight == 0 { r.audioDrained = make(chan struct{}) }
	r.audioInflight++
	r.mu.Unlock()
	defer func(){
		r.mu.Lock()
		r.inflight--
		r.audioInflight--
		if r.audioInflight == 0 { close(r.audioDrained) }
		r.mu.Unlock()
	}()
	return operation(current)
}

// rotate prepares a new session in parallel while retaining the old one. It
// switches only if no turn started and the snapshot did not change. prepare
// must fully configure the candidate and restore the supplied context version.
func (r *persistentProviderRotator) rotate(ctx context.Context, prepare func(context.Context,uint64,uint64)(RealtimeSession,error)) error {
	r.mu.Lock()
	if r.closed || r.current==nil { r.mu.Unlock(); return errProviderUnavailable }
	if r.turnActive || r.preparing || r.inflight>0 { r.mu.Unlock(); return errRotationDeferred }
	r.preparing=true
	version:=r.contextVersion
	r.nextGeneration++
	generation:=r.nextGeneration
	dialCtx,cancel:=context.WithCancel(ctx)
	r.cancelPrepare=cancel
	r.mu.Unlock()

	candidate,err:=prepare(dialCtx,version,generation)
	r.outputFence.Lock()
	r.mu.Lock()
	r.preparing=false
	r.cancelPrepare=nil
	if err!=nil || candidate==nil || r.closed || r.current==nil || r.turnActive || r.inflight>0 || r.contextVersion!=version || dialCtx.Err()!=nil {
		r.mu.Unlock()
		r.outputFence.Unlock()
		cancel()
		if candidate!=nil { candidate.Close() }
		if err!=nil { return err }
		return errRotationDeferred
	}
	previous:=r.current
	previousCancel:=r.cancelCurrent
	r.current=candidate
	r.currentGeneration=generation
	r.cancelCurrent=cancel
	r.mu.Unlock()
	r.outputFence.Unlock()
	// The current provider retains its context; the retired one is cancelled.
	previous.Close()
	if previousCancel!=nil { previousCancel() }
	return nil
}

func (r *persistentProviderRotator) close() {
	r.outputFence.Lock()
	r.mu.Lock()
	if r.closed { r.mu.Unlock(); r.outputFence.Unlock(); return }
	r.closed=true
	if r.cancelPrepare!=nil { r.cancelPrepare() }
	previous:=r.current
	previousCancel:=r.cancelCurrent
	r.current=nil
	r.cancelCurrent=nil
	r.mu.Unlock()
	r.outputFence.Unlock()
	if previous!=nil { previous.Close() }
	if previousCancel!=nil { previousCancel() }
}
