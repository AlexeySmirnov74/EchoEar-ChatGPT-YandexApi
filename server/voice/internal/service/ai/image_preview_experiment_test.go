package ai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"testing"
)

func TestImagePreviewOnlyNegotiated(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	for _, test := range []struct { hello map[string]any; want bool }{
		{map[string]any{}, false},
		{map[string]any{"features": map[string]any{"image_preview": false}}, false},
		{map[string]any{"features": map[string]any{"image_preview": true}}, true},
	} {
		got, _ := withImagePreviewRequest(r, test.hello).Context().Value(imagePreviewContextKey{}).(bool)
		if got != test.want { t.Fatalf("negotiation: got %v, want %v", got, test.want) }
	}
}

func TestImagePreviewDemoSendsBoundedOrderedCompleteImage(t *testing.T) {
	var demo imagePreviewDemo
	begin := demo.next()
	if begin["op"] != "begin" || begin["width"] != 240 || begin["height"] != 240 || begin["bytes"] != 115200 {
		t.Fatal("invalid begin", begin)
	}
	var received []byte
	for sequence := 0; ; sequence++ {
		frame := demo.next()
		if frame["op"] == "end" { break }
		if frame["type"] != "image_preview" || frame["op"] != "chunk" || frame["id"] != 1 || frame["seq"] != sequence {
			t.Fatal("invalid sequence", frame)
		}
		encoded, ok := frame["data"].(string)
		if !ok { t.Fatal("data is not text") }
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 || len(data) > 1024 { t.Fatal("invalid payload", err, len(data)) }
		received = append(received, data...)
	}
	if !bytes.Equal(received, experimentPreviewPixels) { t.Fatal("pixel data changed") }
	sum := sha256.Sum256(received)
	if begin["sha256"] != hex.EncodeToString(sum[:]) { t.Fatal("invalid checksum") }
	if demo.next() != nil { t.Fatal("demo unexpectedly repeated") }
}
