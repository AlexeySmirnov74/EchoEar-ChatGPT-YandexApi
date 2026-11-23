package ai

import (
	"encoding/binary"
	"errors"
)

// roundImageCornersRGB565 blends only a small rounded corner with black.
// Search-service pixels are little-endian RGB565; the later display conversion
// swaps bytes for the LCD. Supersampling avoids jagged edges on this screen.
func roundImageCornersRGB565(pixels []byte) ([]byte, error) {
	const width, height, radius, samples = 240, 240, 16, 4
	if len(pixels) != width*height*2 { return nil, errors.New("invalid image frame") }
	out := append([]byte(nil), pixels...)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x >= radius && x < width-radius || y >= radius && y < height-radius { continue }
			inside := 0
			cx, cy := float64(radius), float64(radius)
			if x >= width-radius { cx = width-radius }
			if y >= height-radius { cy = height-radius }
			for sy := 0; sy < samples; sy++ { for sx := 0; sx < samples; sx++ {
				dx := float64(x)+(float64(sx)+0.5)/samples-cx
				dy := float64(y)+(float64(sy)+0.5)/samples-cy
				if dx*dx+dy*dy <= radius*radius { inside++ }
			} }
			if inside == samples*samples { continue }
			i := (y*width+x)*2
			v := binary.LittleEndian.Uint16(pixels[i:i+2])
			r, g, b := int(v>>11), int((v>>5)&63), int(v&31)
			blend := func(n int) uint16 { return uint16((n*inside+samples*samples/2)/(samples*samples)) }
			binary.LittleEndian.PutUint16(out[i:i+2], blend(r)<<11|blend(g)<<5|blend(b))
		}
	}
	return out,nil
}
