package ai

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	opus "gopkg.in/hraban/opus.v2"
)

type fakeWireMessage struct {
	kind int
	body []byte
	err  error
}

func readFakeWire(c *websocket.Conn, done <-chan struct{}) <-chan fakeWireMessage {
	out := make(chan fakeWireMessage, 16)
	go func() {
		defer close(out)
		for {
			kind, body, err := c.ReadMessage()
			select {
			case out <- fakeWireMessage{kind, body, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// This is an isolated test actor, not the production HandleWS integration. It
// exercises actual WebSocket framing, Opus input, streaming energy gating,
// explicit commit, response correlation, asynchronous MCP, and TTS output.
func TestFakeDeviceVADProviderToolAndTTS(t *testing.T) {
	providerDone := make(chan struct{})
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		responseCount := 0
		for {
			var msg map[string]any
			if err := c.ReadJSON(&msg); err != nil {
				return
			}
			switch msg["type"] {
			case "input_audio_buffer.append":
				audio, _ := msg["audio"].(string)
				decoded, err := base64.StdEncoding.DecodeString(audio)
				if err != nil || len(decoded) == 0 || len(decoded)%2 != 0 {
					t.Errorf("invalid PCM append")
					return
				}
			case "input_audio_buffer.commit":
				_ = c.WriteJSON(map[string]any{"type": "input_audio_buffer.committed", "item_id": "item1"})
			case "response.create":
				responseCount++
				response, _ := msg["response"].(map[string]any)
				metadata, _ := response["metadata"].(map[string]any)
				idString, ok := metadata["listen_id"].(string)
				idNumber, parseErr := strconv.ParseUint(idString, 10, 32)
				if !ok || parseErr != nil || idNumber != 1 || idString != strconv.FormatUint(idNumber, 10) {
					t.Errorf("invalid listen metadata")
					return
				}
				id := "resp1"
				if responseCount == 2 {
					id = "resp2"
				}
				_ = c.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "metadata": metadata}})
				if responseCount == 1 {
					_ = c.WriteJSON(map[string]any{"type": "response.output_item.done", "response_id": id, "item": map[string]any{"type": "function_call", "call_id": "call1", "name": "set_speaker_volume", "arguments": "{\"volume\":40}"}})
				} else {
					_ = c.WriteJSON(map[string]any{"type": "response.output_audio.delta", "response_id": id, "delta": base64.StdEncoding.EncodeToString(make([]byte, serverFrameSamples*2))})
				}
				_ = c.WriteJSON(map[string]any{"type": "response.done", "response": map[string]any{"id": id}})
			case "conversation.item.create":
			}
		}
	}))
	defer fakeProvider.Close()
	bridgeDone := make(chan error, 1)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			bridgeDone <- err
			return
		}
		defer device.Close()
		provider, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(fakeProvider.URL, "http"), nil)
		if err != nil {
			bridgeDone <- err
			return
		}
		defer provider.Close()
		g := &manualTurnGuard{}
		vad, _ := newStreamLocalVAD(80, 120, 120, 60, 3000)
		dec, _ := newOpusDecoder()
		finished := make(chan struct{})
		defer close(finished)
		deviceIn, providerIn := readFakeWire(device, finished), readFakeWire(provider, finished)
		var currentResponse string
		var done, toolPending, toolSent, output bool
		fail := func(err error) { bridgeDone <- err }
		request := func(nonce string) error {
			return g.requestResponse(1, nonce, func() error {
				return provider.WriteJSON(map[string]any{"type": "response.create", "response": map[string]any{"metadata": map[string]any{"listen_id": "1", "nonce": nonce}}})
			})
		}
		for {
			select {
			case wire := <-deviceIn:
				if wire.err != nil {
					return
				}
				if wire.kind == websocket.BinaryMessage {
					if len(wire.body) < 4 {
						continue
					}
					pcm, err := decodeOpusFrame(dec, wire.body[4:])
					if err != nil {
						fail(err)
						return
					}
					event, err := vad.feed(pcm)
					if err != nil {
						fail(err)
						return
					}
					if len(event.pcm) > 0 {
						if err := g.appendAudio(1, func() error {
							samples := resamplePCM16(event.pcm, deviceSampleRate, serverSampleRate)
							raw := make([]byte, len(samples)*2)
							for i, s := range samples {
								binary.LittleEndian.PutUint16(raw[i*2:], uint16(s))
							}
							return provider.WriteJSON(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(raw)})
						}); err != nil {
							fail(err)
							return
						}
					}
					if event.clear {
						fail(errManualGuard)
						return
					}
					if event.commit {
						if err := g.commit(1, func() error { return provider.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"}) }); err != nil {
							fail(err)
							return
						}
					}
					continue
				}
				var msg map[string]any
				_ = json.Unmarshal(wire.body, &msg)
				switch msg["type"] {
				case "hello":
					_ = device.WriteJSON(map[string]any{"type": "hello", "features": map[string]any{"persistent_idle": true}})
				case "listen":
					if err := g.beginNext(1); err != nil {
						fail(err)
						return
					}
				case "mcp":
					if err := g.toolResult(1, "call1", func() error {
						return provider.WriteJSON(map[string]any{"type": "conversation.item.create", "item": map[string]any{"type": "function_call_output", "call_id": "call1", "output": "ok"}})
					}); err != nil {
						fail(err)
						return
					}
					toolPending = false
					toolSent = true
					if done {
						done = false
						toolSent = false
						if err := request("nonce2"); err != nil {
							fail(err)
							return
						}
					}
				}
			case wire := <-providerIn:
				if wire.err != nil {
					return
				}
				var msg map[string]any
				_ = json.Unmarshal(wire.body, &msg)
				switch msg["type"] {
				case "input_audio_buffer.committed":
					if err := g.committed("item1"); err != nil {
						fail(err)
						return
					}
					if err := request("nonce1"); err != nil {
						fail(err)
						return
					}
				case "response.created":
					response := msg["response"].(map[string]any)
					metadata := response["metadata"].(map[string]any)
					idText, ok := metadata["listen_id"].(string)
					parsed, e := strconv.ParseUint(idText, 10, 32)
					if !ok || e != nil || parsed == 0 || idText != strconv.FormatUint(parsed, 10) {
						fail(errManualGuard)
						return
					}
					currentResponse = response["id"].(string)
					if err := g.responseCreated(uint32(parsed), metadata["nonce"].(string), currentResponse); err != nil {
						fail(err)
						return
					}
				case "response.output_item.done":
					if err := g.tool(currentResponse, "call1", func(uint32) error { return device.WriteJSON(map[string]any{"type": "mcp", "call_id": "call1"}) }); err != nil {
						fail(err)
						return
					}
					toolPending = true
				case "response.output_audio.delta":
					raw, e := base64.StdEncoding.DecodeString(msg["delta"].(string))
					if e != nil || len(raw)%2 != 0 {
						fail(errManualGuard)
						return
					}
					pcm := make([]int16, len(raw)/2)
					for i := range pcm {
						pcm[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
					}
					frames, e := encodeOpusFrames(pcm)
					if e != nil || len(frames) == 0 {
						fail(errManualGuard)
						return
					}
					if err := g.output(currentResponse, func(id uint32) error {
						if err := device.WriteJSON(map[string]any{"type": "tts", "state": "start", "listen_id": id}); err != nil {
							return err
						}
						f := frames[0]
						return device.WriteMessage(websocket.BinaryMessage, append([]byte{0, 0, byte(len(f) >> 8), byte(len(f))}, f...))
					}); err != nil {
						fail(err)
						return
					}
					output = true
				case "response.done":
					if err := g.done(currentResponse); err != nil {
						fail(err)
						return
					}
					done = true
					if currentResponse == "resp1" {
						close(providerDone)
					}
					if toolSent && !toolPending {
						done = false
						toolSent = false
						if err := request("nonce2"); err != nil {
							fail(err)
							return
						}
					}
					if output {
						if err := g.finishPlayback(currentResponse, func(id uint32) error {
							return device.WriteJSON(map[string]any{"type": "tts", "state": "stop", "listen_id": id})
						}); err != nil {
							fail(err)
							return
						}
						bridgeDone <- nil
						return
					}
				}
			case <-time.After(5 * time.Second):
				fail(errManualGuard)
				return
			}
		}
	}))
	defer bridge.Close()
	device, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(bridge.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	deviceJSON(t, device, map[string]any{"type": "hello", "features": map[string]any{"persistent_idle": true}})
	_ = deviceReadJSON(t, device)
	deviceJSON(t, device, map[string]any{"type": "listen", "state": "start", "listen_id": 1})
	enc, err := opus.NewEncoder(deviceSampleRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatal(err)
	}
	for frame := 0; frame < 6; frame++ {
		pcm := make([]int16, deviceFrameSamples)
		if frame < 3 {
			for i := range pcm {
				pcm[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i+frame*deviceFrameSamples)/deviceSampleRate))
			}
		}
		buf := make([]byte, 4096)
		n, e := enc.Encode(pcm, buf)
		if e != nil {
			t.Fatal(e)
		}
		packet := append([]byte{0, 0, byte(n >> 8), byte(n)}, buf[:n]...)
		if e = device.WriteMessage(websocket.BinaryMessage, packet); e != nil {
			t.Fatal(e)
		}
	}
	mcp := deviceReadJSON(t, device)
	if mcp["type"] != "mcp" {
		t.Fatal(mcp)
	}
	select {
	case <-providerDone:
	case <-time.After(time.Second):
		t.Fatal("provider done was not processed")
	}
	deviceJSON(t, device, map[string]any{"type": "mcp", "call_id": "call1"})
	start := deviceReadJSON(t, device)
	if start["type"] != "tts" || start["state"] != "start" || start["listen_id"] != float64(1) {
		t.Fatal(start)
	}
	typ, encoded, err := device.ReadMessage()
	if err != nil || typ != websocket.BinaryMessage || len(encoded) < 5 {
		t.Fatal(typ, err)
	}
	if _, err := decodeFramesToPCM([][]byte{encoded[4:]}); err != nil {
		t.Fatal("invalid outgoing opus", err)
	}
	stop := deviceReadJSON(t, device)
	if stop["type"] != "tts" || stop["state"] != "stop" {
		t.Fatal(stop)
	}
	if err := <-bridgeDone; err != nil {
		t.Fatal(err)
	}
}
