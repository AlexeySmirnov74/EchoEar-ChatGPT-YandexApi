package ai

import (
	"encoding/binary"
	"testing"
)

func TestImageRoundedCornersAndDisplayByteOrder(t *testing.T) {
	input:=make([]byte,imageFrameBytes)
	for i:=0;i<len(input);i+=2 { binary.LittleEndian.PutUint16(input[i:i+2],0xf800) }
	rounded,err:=roundImageCornersRGB565(input); if err!=nil { t.Fatal(err) }
	color:=func(x,y int) uint16 { i:=(y*240+x)*2; return binary.LittleEndian.Uint16(rounded[i:i+2]) }
	for _,p:=range [][2]int{{0,0},{239,0},{0,239},{239,239}} { if color(p[0],p[1])!=0 { t.Fatalf("corner %v not black",p) } }
	for _,p:=range [][2]int{{120,120},{0,120},{239,120},{120,0},{120,239}} { if color(p[0],p[1])!=0xf800 { t.Fatalf("content %v changed",p) } }
	partial:=false; for y:=0;y<16;y++ { for x:=0;x<16;x++ { v:=color(x,y); if v>0 && v<0xf800 { partial=true } } }
	if !partial { t.Fatal("rounded edge lacks antialiasing") }
	if binary.LittleEndian.Uint16(input[:2])!=0xf800 { t.Fatal("input mutated") }
	display,err:=imagePreviewToDisplayRGB565(rounded); if err!=nil {t.Fatal(err)}
	i:=(120*240+120)*2; if display[i]!=0xf8 || display[i+1]!=0 { t.Fatal("incorrect display byte order") }
}
