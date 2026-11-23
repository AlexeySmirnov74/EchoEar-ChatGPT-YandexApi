package ai

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"net/http"
)

// This fixture is compiled only into the separately built experimental server.
//go:embed assets/preview-test.rgb565
var experimentPreviewPixels []byte

type imagePreviewContextKey struct{}
type imageBackgroundContextKey struct{}
type imageBackgroundContext struct { device string; store *backgroundStore; ready chan<- struct{} }

func withImagePreviewRequest(r *http.Request, hello map[string]any) *http.Request {
	features, _ := hello["features"].(map[string]any)
	if enabled, _ := features["image_preview"].(bool); enabled {
		ctx:=context.WithValue(r.Context(), imagePreviewContextKey{}, true)
		if background, _:=features["image_background_v1"].(bool); background {
			device:=r.Header.Get("Device-Id")
			if _,err:=speakerBackgrounds.path(device);err==nil {ctx=context.WithValue(ctx,imageBackgroundContextKey{},imageBackgroundContext{device:device,store:speakerBackgrounds})}
		}
		return r.WithContext(ctx)
	}
	return r
}

type imagePreviewDemo struct {
	step int
	pixels []byte
	id int
	durationMS int
}

// next returns at most one small frame per actor tick. Audio and device events
// remain on the existing single WebSocket writer and are interleaved normally.
func (p *imagePreviewDemo) next() map[string]any {
	const chunk = 1024
	pixels := p.pixels
	if pixels == nil { pixels = experimentPreviewPixels }
	imageID := p.id
	if imageID == 0 { imageID = 1 }
	if len(pixels) != 240*240*2 || p.step < 0 {
		return nil
	}
	if p.step == 0 {
		p.step++
		sum := sha256.Sum256(pixels)
		duration:=p.durationMS; if duration==0 {duration=15000}
		return map[string]any{"type": "image_preview", "op": "begin", "id": imageID,
			"width": 240, "height": 240, "bytes": len(pixels),
			"duration_ms": duration, "sha256": hex.EncodeToString(sum[:])}
	}
	index := (p.step - 1) * chunk
	if index < len(pixels) {
		end := min(index+chunk, len(pixels))
		sequence := p.step - 1
		p.step++
		return map[string]any{"type": "image_preview", "op": "chunk", "id": imageID,
			"seq": sequence, "data": base64.StdEncoding.EncodeToString(pixels[index:end])}
	}
	p.step = -1
	return map[string]any{"type": "image_preview", "op": "end", "id": imageID}
}
