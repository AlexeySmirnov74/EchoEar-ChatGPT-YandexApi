package ai

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestImagePreviewDisplayByteOrder(t *testing.T) {
	// Red, green, blue, white, black, magenta, encoded by the search service.
	input := make([]byte, imageFrameBytes)
	copy(input, []byte{0x00, 0xf8, 0xe0, 0x07, 0x1f, 0x00, 0xff, 0xff, 0x00, 0x00, 0x1f, 0xf8})
	original := bytes.Clone(input)
	got, err := imagePreviewToDisplayRGB565(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f, 0xff, 0xff, 0x00, 0x00, 0xf8, 0x1f}
	if !bytes.Equal(got[:len(want)], want) {
		t.Fatalf("incorrect RGB565 bytes: %x", got[:len(want)])
	}
	if !bytes.Equal(input, original) {
		t.Fatal("modified input")
	}
	for _, invalid := range [][]byte{nil, {1}, make([]byte, imageFrameBytes-1), make([]byte, imageFrameBytes+2)} {
		if _, err := imagePreviewToDisplayRGB565(invalid); err == nil {
			t.Fatalf("accepted invalid frame of %d bytes", len(invalid))
		}
	}
}

func TestConvertedImagePreviewTransferAndHash(t *testing.T) {
	input := make([]byte, imageFrameBytes)
	for i := range input {
		input[i] = byte(i*37 + 3)
	}
	converted, err := imagePreviewToDisplayRGB565(input)
	if err != nil {
		t.Fatal(err)
	}
	preview := imagePreviewDemo{pixels: converted, id: 7}
	begin := preview.next()
	if begin["op"] != "begin" {
		t.Fatal("missing begin")
	}
	var transmitted []byte
	for {
		message := preview.next()
		if message == nil {
			t.Fatal("missing end")
		}
		if message["op"] == "end" {
			break
		}
		if message["op"] != "chunk" {
			t.Fatal("unexpected message")
		}
		part, err := base64.StdEncoding.DecodeString(message["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		transmitted = append(transmitted, part...)
	}
	if !bytes.Equal(transmitted, converted) {
		t.Fatal("transferred pixels differ")
	}
	sum := sha256.Sum256(transmitted)
	if begin["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatal("hash does not match transmitted pixels")
	}
}
