package ai

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVoiceDiagnosticsOptInBoundedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	s := &voiceDiagnosticSink{marker: filepath.Join(dir, "enabled"), path: filepath.Join(dir, "events"), maxBytes: 300}
	record := voiceDiagnosticRecord{Event: "vad_commit", Listen: "0123456789abcdef", MonoMS: 10, DurationMS: 700}
	s.write(record)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("written with no opt-in")
	}
	if err := os.WriteFile(s.marker, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		record.MonoMS = int64(i)
		s.write(record)
	}
	for _, path := range []string{s.path, s.path + ".1"} {
		info, err := os.Stat(path)
		if err != nil || info.Size() > s.maxBytes || info.Mode().Perm() != 0600 {
			t.Fatal("not bounded or private", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var row map[string]any
			if json.Unmarshal(line, &row) != nil || len(row) != 4 {
				t.Fatal("unexpected diagnostic record")
			}
			for _, key := range []string{"event", "listen", "mono_ms", "duration_ms"} {
				if _, ok := row[key]; !ok {
					t.Fatal("unexpected field")
				}
			}
		}
	}
	before, _ := os.ReadFile(s.path)
	for _, bad := range []voiceDiagnosticRecord{
		{Event: "private transcript", Listen: record.Listen},
		{Event: record.Event, Listen: "private transcript"},
		{Event: record.Event, Listen: record.Listen, Reason: "private transcript"},
	} {
		s.write(bad)
	}
	after, _ := os.ReadFile(s.path)
	if !bytes.Equal(before, after) || strings.Contains(string(after), "private transcript") {
		t.Fatal("arbitrary text entered sink")
	}
	if err := os.Remove(s.marker); err != nil {
		t.Fatal(err)
	}
	s.write(record)
	after, _ = os.ReadFile(s.path)
	if !bytes.Equal(before, after) {
		t.Fatal("marker removal did not disable writes")
	}
}

func TestVoiceDiagnosticQueueIsBoundedAndHashesSessionAndListen(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	output := make(chan voiceDiagnosticRecord, 4)
	d := newVoiceDiagnostics(1, func(r voiceDiagnosticRecord) { close(entered); <-release; output <- r })
	d.emit(voiceListen, voiceNoReason, 1, 12345, 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	start := time.Now()
	for i := 0; i < 1000; i++ {
		d.emit(voiceVADStart, voiceNoReason, 1, 12345, 0)
	}
	if time.Since(start) > time.Second || len(d.entries) != 1 {
		t.Fatal("queue must drop on saturation")
	}
	// Avoid closing a channel from the writer more than once in this test.
	queued := <-d.entries
	close(release)
	first := <-output
	if first.Listen != queued.Listen || len(first.Listen) != 16 || first.Listen == "0000000000003039" {
		t.Fatal("bad listen hash")
	}
	close(d.entries)

	results := make(chan voiceDiagnosticRecord, 4)
	other := newVoiceDiagnostics(4, func(r voiceDiagnosticRecord) { results <- r })
	other.emit(voiceListen, voiceNoReason, 1, 12345, 0)
	other.emit(voiceListen, voiceNoReason, 2, 12345, 0)
	a, b := <-results, <-results
	if a.Listen == b.Listen {
		t.Fatal("session-scoped hashes collided")
	}
	other.emit(voiceEvent(255), voiceNoReason, 1, 12345, 0)
	other.emit(voiceListen, voiceReason(255), 1, 12345, 0)
	other.emit(voiceListen, voiceNoReason, 1, 0, 0)
	select {
	case <-results:
		t.Fatal("invalid metadata emitted")
	case <-time.After(20 * time.Millisecond):
	}
	close(other.entries)
}
