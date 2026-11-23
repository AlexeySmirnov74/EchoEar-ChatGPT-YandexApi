package ai

// Optional voice state telemetry. The API accepts only closed enum values,
// monotonic durations and a keyed hash of the protocol listen ID: no text,
// PCM, packet data, device IDs, addresses, provider errors or credentials.
import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

type voiceEvent uint8

const (
	voiceListen voiceEvent = iota + 1
	voicePause
	voiceVADStart
	voiceVADCommit
	voiceVADClear
	voiceStandby
	voiceDisconnect
)

func (e voiceEvent) name() string {
	switch e {
	case voiceListen:
		return "listen"
	case voicePause:
		return "pause"
	case voiceVADStart:
		return "vad_start"
	case voiceVADCommit:
		return "vad_commit"
	case voiceVADClear:
		return "vad_clear"
	case voiceStandby:
		return "standby"
	case voiceDisconnect:
		return "disconnect"
	default:
		return ""
	}
}

type voiceReason uint8

const (
	voiceNoReason voiceReason = iota
	voiceTouchPause
	voiceIdleTimeout
	voiceProviderFailure
	voiceGenerationLost
	voiceProgressTimeout
	voiceClearTimeout
	voiceBeginFailure
	voiceUnexpectedStart
	voiceVADFailure
	voiceActorClosed
)

func (r voiceReason) name() string {
	switch r {
	case voiceNoReason:
		return ""
	case voiceTouchPause:
		return "device_pause"
	case voiceIdleTimeout:
		return "idle_timeout"
	case voiceProviderFailure:
		return "provider_failure"
	case voiceGenerationLost:
		return "generation_lost"
	case voiceProgressTimeout:
		return "progress_timeout"
	case voiceClearTimeout:
		return "clear_timeout"
	case voiceBeginFailure:
		return "listen_begin_failure"
	case voiceUnexpectedStart:
		return "unexpected_listen_start"
	case voiceVADFailure:
		return "vad_failure"
	case voiceActorClosed:
		return "actor_closed"
	default:
		return "invalid"
	}
}

type voiceDiagnosticRecord struct {
	Event      string `json:"event"`
	Reason     string `json:"reason,omitempty"`
	Listen     string `json:"listen"`
	MonoMS     int64  `json:"mono_ms"`
	DurationMS int64  `json:"duration_ms"`
}
type voiceDiagnosticSink struct {
	mu           sync.Mutex
	marker, path string
	maxBytes     int64
}

func (s *voiceDiagnosticSink) write(e voiceDiagnosticRecord) {
	validEvent := false
	for kind := voiceListen; kind <= voiceDisconnect; kind++ {
		if e.Event == kind.name() {
			validEvent = true
			break
		}
	}
	validReason := e.Reason == ""
	for reason := voiceTouchPause; reason <= voiceActorClosed; reason++ {
		if e.Reason == reason.name() {
			validReason = true
			break
		}
	}
	if !validEvent || !validReason || len(e.Listen) != 16 || e.MonoMS < 0 || e.DurationMS < 0 {
		return
	}
	if _, err := hex.DecodeString(e.Listen); err != nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if int64(len(line)) > s.maxBytes {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	marker, err := os.Lstat(s.marker)
	if err != nil || !marker.Mode().IsRegular() {
		return
	}
	info, err := os.Lstat(s.path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return
		}
		if info.Size()+int64(len(line)) > s.maxBytes {
			if os.Rename(s.path, s.path+".1") != nil {
				return
			}
		}
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	if f.Chmod(0600) != nil {
		return
	}
	_, _ = f.Write(line)
}

type voiceDiagnostics struct {
	key     [32]byte
	ready   bool
	started time.Time
	entries chan voiceDiagnosticRecord
}

func newVoiceDiagnostics(capacity int, write func(voiceDiagnosticRecord)) *voiceDiagnostics {
	d := &voiceDiagnostics{started: time.Now(), entries: make(chan voiceDiagnosticRecord, capacity)}
	_, err := rand.Read(d.key[:])
	d.ready = err == nil
	go func() {
		for entry := range d.entries {
			write(entry)
		}
	}()
	return d
}
func (d *voiceDiagnostics) emit(event voiceEvent, reason voiceReason, session uint64, listenID uint32, duration time.Duration) {
	if !d.ready || event.name() == "" || reason.name() == "invalid" || session == 0 || listenID == 0 || duration < 0 {
		return
	}
	var id [12]byte
	binary.BigEndian.PutUint64(id[:8], session)
	binary.BigEndian.PutUint32(id[8:], listenID)
	mac := hmac.New(sha256.New, d.key[:])
	_, _ = mac.Write(id[:])
	hashed := mac.Sum(nil)
	record := voiceDiagnosticRecord{Event: event.name(), Reason: reason.name(), Listen: hex.EncodeToString(hashed[:8]),
		MonoMS: time.Since(d.started).Milliseconds(), DurationMS: duration.Milliseconds()}
	select {
	case d.entries <- record:
	default:
	}
}

var voiceDiagnosticsLog = &voiceDiagnosticSink{marker: "/data/voice-events-enabled", path: "/data/voice-events.log", maxBytes: 256 * 1024}
var voiceDiagnosticsCurrent = newVoiceDiagnostics(64, voiceDiagnosticsLog.write)
