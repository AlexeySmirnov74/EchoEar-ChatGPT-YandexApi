package ai

import "errors"

// imagePreviewToDisplayRGB565 converts the search service's little-endian
// RGB565 to the byte order expected by the EchoEar's esp_emote_gfx image
// renderer. Its display uses swap=true, but opaque images are copied directly
// into the display buffer, so they must already be in display byte order.
// Keep the input unchanged: a caller may retain it as a reusable fixture.
func imagePreviewToDisplayRGB565(pixels []byte) ([]byte, error) {
	if len(pixels) != imageFrameBytes {
		return nil, errors.New("invalid image frame")
	}
	result := make([]byte, len(pixels))
	for i := 0; i < len(pixels); i += 2 {
		result[i], result[i+1] = pixels[i+1], pixels[i]
	}
	return result, nil
}
