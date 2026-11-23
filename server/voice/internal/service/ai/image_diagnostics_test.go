package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestImageDiagnosticsOptInAndFields(t *testing.T) {
	dir := t.TempDir()
	s := imageDiagnosticSink{marker: filepath.Join(dir, "enabled"), path: filepath.Join(dir, "events"), maxBytes: 4096}
	s.record(imageSearchStarted, 123, 4, time.Second)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) { t.Fatal("sink wrote without marker") }
	if err := os.WriteFile(s.marker, nil, 0600); err != nil { t.Fatal(err) }
	s.record(imageDiagnosticEvent(255), 123, 4, time.Second)
	if _, err := os.Stat(s.path); !os.IsNotExist(err) { t.Fatal("unknown event accepted") }
	s.record(imageSearchReady, 123, 4, 1500*time.Millisecond)
	data, err := os.ReadFile(s.path); if err != nil { t.Fatal(err) }
	var record imageDiagnosticRecord
	if err := json.Unmarshal(data, &record); err != nil { t.Fatal(err) }
	if record.Event != "search_ready" || record.Session != 123 || record.Sequence != 4 || record.ElapsedMS != 1500 { t.Fatalf("unexpected record: %+v", record) }
	if _, err := time.Parse(time.RFC3339Nano, record.Time); err != nil { t.Fatal(err) }
	if mode := fileMode(t, s.path); mode != 0600 { t.Fatalf("wrong mode %o", mode) }
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path); if err != nil { t.Fatal(err) }; return info.Mode().Perm()
}

func TestImageDiagnosticsBoundedRotation(t *testing.T) {
	dir := t.TempDir()
	s := imageDiagnosticSink{marker: filepath.Join(dir, "enabled"), path: filepath.Join(dir, "events"), maxBytes: 350}
	if err := os.WriteFile(s.marker, nil, 0600); err != nil { t.Fatal(err) }
	for i:=0; i<30; i++ { s.record(imageTransferEnd, 123, uint64(i), time.Second) }
	for _, path := range []string{s.path, s.path+".1"} {
		info, err := os.Stat(path); if err != nil { t.Fatal(err) }
		if info.Size() > s.maxBytes || info.Mode().Perm() != 0600 { t.Fatalf("unbounded or insecure log: %v", info) }
		data, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") { if !json.Valid([]byte(line)) { t.Fatal("invalid record") } }
	}
	entries, err := os.ReadDir(dir); if err != nil { t.Fatal(err) }; if len(entries) != 3 { t.Fatal("unexpected archive count") }
}

func TestImageDiagnosticsConcurrentAndUnavailable(t *testing.T) {
	dir := t.TempDir()
	s := imageDiagnosticSink{marker: filepath.Join(dir, "enabled"), path: filepath.Join(dir, "events"), maxBytes: 1 << 20}
	if err := os.WriteFile(s.marker, nil, 0600); err != nil { t.Fatal(err) }
	var wg sync.WaitGroup
	for i:=0; i<100; i++ { wg.Add(1); go func(i int) { defer wg.Done(); s.record(imageSpeechSent, uint64(i+1), 1, time.Millisecond) }(i) }
	wg.Wait()
	data, err := os.ReadFile(s.path); if err != nil { t.Fatal(err) }
	lines := strings.Split(strings.TrimSpace(string(data)), "\n"); if len(lines)!=100 { t.Fatalf("got %d lines", len(lines)) }
	for _, line := range lines { if !json.Valid([]byte(line)) { t.Fatal("invalid concurrent record") } }
	broken := imageDiagnosticSink{marker: s.marker, path: filepath.Join(dir,"missing","events"), maxBytes: 1024}
	broken.record(imageSearchStarted, 1, 1, time.Second) // Must never panic or return an error to the conversation.
}

func TestImageDiagnosticSessions(t *testing.T) {
	seen := make(map[uint64]bool)
	for i:=0; i<100; i++ { id:=newImageDiagnosticSession(); if id==0 || seen[id] { t.Fatal("invalid or duplicate session id") }; seen[id]=true }
}
