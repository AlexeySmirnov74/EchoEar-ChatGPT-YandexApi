package ai

// The image diagnostic sink deliberately accepts no text from a user, model,
// URL, error, image, or network. It is independently opt-in via a root-owned
// marker, and logging failures must never affect a conversation.
import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type imageDiagnosticEvent uint8

const (
	imageSearchStarted imageDiagnosticEvent = iota + 1
	imageSearchReady
	imageSearchFailed
	imageSpeechSent
	imageTransferBegin
	imageTransferEnd
	imageSearchCancelled
)

func (event imageDiagnosticEvent) name() string {
	switch event {
	case imageSearchStarted:
		return "search_started"
	case imageSearchReady:
		return "search_ready"
	case imageSearchFailed:
		return "search_failed"
	case imageSpeechSent:
		return "speech_sent"
	case imageTransferBegin:
		return "transfer_begin"
	case imageTransferEnd:
		return "transfer_end"
	case imageSearchCancelled:
		return "search_cancelled"
	default:
		return ""
	}
}

type imageDiagnosticRecord struct {
	Time      string `json:"time"`
	Event     string `json:"event"`
	Session   uint64 `json:"session"`
	Sequence  uint64 `json:"sequence"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Reason    string `json:"reason,omitempty"`
	Phase     string `json:"phase,omitempty"`
}

// Cancellation metadata is a closed vocabulary: no query, model output,
// error string, device ID or other user data may enter this sink.
type imageCancelReason uint8
const (
	imageCancelVoice imageCancelReason = iota + 1
	imageCancelProvider
	imageCancelListenPause
	imageCancelListenBegin
	imageCancelOtherTool
	imageCancelClearTimeout
	imageCancelStandby
	imageCancelSuperseded
	imageCancelActorClosed
)
func (r imageCancelReason) name() string {
	switch r {
	case imageCancelVoice: return "voice_activity"
	case imageCancelProvider: return "provider_or_abort"
	case imageCancelListenPause: return "listen_pause"
	case imageCancelListenBegin: return "listen_begin_failed"
	case imageCancelOtherTool: return "other_tool_failed"
	case imageCancelClearTimeout: return "clear_timeout"
	case imageCancelStandby: return "standby_failed"
	case imageCancelSuperseded: return "new_image_request"
	case imageCancelActorClosed: return "actor_closed"
	default: return ""
	}
}
type imageCancelPhase uint8
const (
	imagePhaseSearch imageCancelPhase = iota + 1
	imagePhaseReady
	imagePhaseTransfer
	imagePhaseAfterEnd
	imagePhaseFailed
)
func (p imageCancelPhase) name() string {
	switch p {
	case imagePhaseSearch: return "search"
	case imagePhaseReady: return "ready"
	case imagePhaseTransfer: return "transfer"
	case imagePhaseAfterEnd: return "after_end"
	case imagePhaseFailed: return "failed"
	default: return ""
	}
}

type imageDiagnosticSink struct {
	mu           sync.Mutex
	marker, path string
	maxBytes     int64
}

var imageDiagnosticLog = &imageDiagnosticSink{
	marker:   "/data/image-experiment-events-enabled",
	path:     "/data/image-experiment-events.log",
	maxBytes: 512 * 1024,
}

var imageDiagnosticFallback atomic.Uint64

// Filesystem calls must never run on a conversation actor. One bounded queue
// and one worker cap the damage if the filesystem becomes slow or hangs: new
// diagnostics are simply dropped while audio and image traffic continue.
type imageDiagnosticEntry struct {
	event             imageDiagnosticEvent
	session, sequence uint64
	elapsed           time.Duration
	at                time.Time
	reason            imageCancelReason
	phase             imageCancelPhase
}

type imageDiagnosticDispatcher struct {
	entries chan imageDiagnosticEntry
	done    chan struct{}
	stopped chan struct{}
}

func newImageDiagnosticDispatcher(capacity int, write func(imageDiagnosticEntry)) *imageDiagnosticDispatcher {
	d := &imageDiagnosticDispatcher{entries: make(chan imageDiagnosticEntry, capacity), done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(d.stopped)
		for {
			select {
			case <-d.done:
				return
			case entry := <-d.entries:
				write(entry)
			}
		}
	}()
	return d
}

func (d *imageDiagnosticDispatcher) submit(entry imageDiagnosticEntry) {
	select {
	case <-d.done:
		return
	default:
	}
	select {
	case d.entries <- entry:
	default:
	}
}

// stop is for controlled teardown; a blocked writer must first be released.
func (d *imageDiagnosticDispatcher) stop() {
	close(d.done)
	<-d.stopped
}

var imageDiagnosticDispatcherCurrent atomic.Pointer[imageDiagnosticDispatcher]

func init() {
	imageDiagnosticDispatcherCurrent.Store(newImageDiagnosticDispatcher(64, func(entry imageDiagnosticEntry) {
		imageDiagnosticLog.recordAtDetails(entry.event, entry.session, entry.sequence, entry.elapsed, entry.at, entry.reason, entry.phase)
	}))
}

func newImageDiagnosticSession() uint64 {
	var data [8]byte
	if _, err := rand.Read(data[:]); err == nil {
		if id := binary.BigEndian.Uint64(data[:]); id != 0 {
			return id
		}
	}
	// Remain functional even if the system RNG fails.
	return uint64(time.Now().UnixNano()) ^ imageDiagnosticFallback.Add(1)
}

func recordImageEvent(event imageDiagnosticEvent, session, sequence uint64, elapsed time.Duration) {
	imageDiagnosticDispatcherCurrent.Load().submit(imageDiagnosticEntry{event: event, session: session, sequence: sequence, elapsed: elapsed, at: time.Now()})
}

func recordImageCancel(reason imageCancelReason, phase imageCancelPhase, session, sequence uint64, elapsed time.Duration) {
	imageDiagnosticDispatcherCurrent.Load().submit(imageDiagnosticEntry{event: imageSearchCancelled, session: session, sequence: sequence, elapsed: elapsed, at: time.Now(), reason: reason, phase: phase})
}

func (sink *imageDiagnosticSink) record(event imageDiagnosticEvent, session, sequence uint64, elapsed time.Duration) {
	sink.recordAt(event, session, sequence, elapsed, time.Now())
}

func (sink *imageDiagnosticSink) recordAt(event imageDiagnosticEvent, session, sequence uint64, elapsed time.Duration, at time.Time) {
	sink.recordAtDetails(event, session, sequence, elapsed, at, 0, 0)
}

func (sink *imageDiagnosticSink) recordAtDetails(event imageDiagnosticEvent, session, sequence uint64, elapsed time.Duration, at time.Time, reason imageCancelReason, phase imageCancelPhase) {
	name := event.name()
	if name == "" || session == 0 || elapsed < 0 {
		return
	}
	if reason != 0 || phase != 0 {
		if event != imageSearchCancelled || reason.name() == "" || phase.name() == "" { return }
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	marker, err := os.Lstat(sink.marker)
	if err != nil || !marker.Mode().IsRegular() {
		return
	}
	record := imageDiagnosticRecord{Time: at.UTC().Format(time.RFC3339Nano), Event: name,
		Session: session, Sequence: sequence, ElapsedMS: elapsed.Milliseconds(), Reason: reason.name(), Phase: phase.name()}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if int64(len(line)) > sink.maxBytes {
		return
	}
	info, err := os.Lstat(sink.path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return
		}
		if info.Size()+int64(len(line)) > sink.maxBytes {
			// Rename replaces at most one old archive. /data is owner-only.
			if err := os.Rename(sink.path, sink.path+".1"); err != nil {
				return
			}
		}
	}
	f, err := os.OpenFile(sink.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return
	}
	_, _ = f.Write(line)
}
