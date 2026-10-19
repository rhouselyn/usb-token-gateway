package main

// Icon.go: runtime-generated tray icons.
//
// The artwork is EXACTLY the web panel favicon (see internal/webui/page.html):
// the same 64x64 SVG geometry -- a dark rounded square with the "bridge" glyph
// (two pillars over a base line) stroked with the same indigo -> teal gradient.
// Here it is rasterised with a signed-distance renderer so every size
// (16/32/48/256) stays crisp, then packed as a multi-size ICO on Windows.
//
// The favicon SVG this mirrors (for reference):
//
//	<svg viewBox="0 0 64 64">
//	  <rect width="64" height="64" rx="16" fill="#0b1017"/>
//	  <path d="M20 40h24M24 24v16M40 24v16" stroke="#6d8cff -> #37e0c9"
//	        stroke-width="5" stroke-linecap="round"/>
//	</svg>

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
)

type iconTheme struct {
	bg     color.RGBA // rounded-square background
	lo, hi color.RGBA // glyph stroke gradient (top-left -> bottom-right)
}

var (
	// running: the brand indigo -> teal used by the web icon
	themeRunning = iconTheme{
		bg: color.RGBA{0x0b, 0x10, 0x17, 0xff},
		lo: color.RGBA{0x6d, 0x8c, 0xff, 0xff},
		hi: color.RGBA{0x37, 0xe0, 0xc9, 0xff},
	}
	// locked: same artwork, amber gradient (service paused)
	themeLocked = iconTheme{
		bg: color.RGBA{0x0b, 0x10, 0x17, 0xff},
		lo: color.RGBA{0xff, 0xc8, 0x61, 0xff},
		hi: color.RGBA{0xff, 0x9d, 0x5c, 0xff},
	}
)

// iconPNG holds the tray icon bytes: a multi-size ICO on Windows (fyne/systray
// writes it to a temp file and LoadImage()s it), a plain PNG elsewhere.
var (
	iconPNG       = buildIcon(themeRunning)
	iconLockedPNG = buildIcon(themeLocked)

	iconSizes = []int{256, 48, 32, 16}
)

// ---------------------------------------------------------------- geometry
// All coordinates are in the SVG's 64x64 user space, normalised to 0..1.

type seg struct{ ax, ay, bx, by float64 }

func u(v float64) float64 { return v / 64 }

var (
	glyphSegs = []seg{
		{u(20), u(40), u(44), u(40)}, // base line
		{u(24), u(24), u(24), u(40)}, // left pillar
		{u(40), u(24), u(40), u(40)}, // right pillar
	}
	glyphStrokeHalf = u(5) / 2   // stroke-width 5
	bgRadius        = u(16)      // rect rx=16
	gradOrigin      = u(24)      // gradient box = glyph bbox (24,24)-(40,40)
	gradSpan        = u(16) * 2  // diagonal length of that bbox
)

// ---------------------------------------------------------------- encoding

func buildIcon(t iconTheme) []byte {
	imgs := make([]*image.RGBA, 0, len(iconSizes))
	for _, s := range iconSizes {
		imgs = append(imgs, renderIcon(s, t))
	}
	if runtime.GOOS == "windows" {
		return icoFromPNGs(imgs)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, imgs[0]) // largest size as a plain PNG
	return buf.Bytes()
}

// renderIcon rasterises the artwork at size x size with 1px antialiasing.
func renderIcon(size int, t iconTheme) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	inv := 1.0 / float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px := (float64(x) + 0.5) * inv
			py := (float64(y) + 0.5) * inv

			// rounded-square background (signed distance in pixels)
			sd := sdRoundRect(px-0.5, py-0.5, 0.5, 0.5, bgRadius) * float64(size)
			bgCov := clamp01(0.5 - sd)
			if bgCov <= 0 {
				continue // outside the rounded square -> transparent
			}

			// glyph coverage: distance to the nearest stroke, 1px ramp
			cov := 0.0
			for _, s := range glyphSegs {
				d := distSeg(px, py, s) * float64(size)
				if c := clamp01(0.5 + glyphStrokeHalf*float64(size) - d); c > cov {
					cov = c
				}
			}

			fill := t.bg
			if cov > 0 {
				gp := clamp01(((px - gradOrigin) + (py - gradOrigin)) / gradSpan)
				fill = mixColor(t.bg, mixColor(t.lo, t.hi, gp), cov)
			}
			img.SetRGBA(x, y, color.RGBA{fill.R, fill.G, fill.B,
				uint8(float64(fill.A) * bgCov)})
		}
	}
	return img
}

// icoFromPNGs packs images into an ICO container using PNG payloads
// (supported by Windows Vista+; that is what LoadImage reads back).
func icoFromPNGs(imgs []*image.RGBA) []byte {
	payloads := make([][]byte, len(imgs))
	for i, im := range imgs {
		var b bytes.Buffer
		_ = png.Encode(&b, im)
		payloads[i] = b.Bytes()
	}
	var out bytes.Buffer
	out.Write([]byte{0, 0, 1, 0}) // ICONDIR: reserved, type=icon
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(imgs)))
	offset := 6 + 16*len(imgs)
	for i, im := range imgs {
		w, h := im.Bounds().Dx(), im.Bounds().Dy()
		bw, bh := byte(w), byte(h)
		if w >= 256 {
			bw = 0 // 0 means 256 in the ICO directory
		}
		if h >= 256 {
			bh = 0
		}
		out.Write([]byte{bw, bh, 0, 0, 1, 0, 32, 0}) // w,h,colors,res,planes,bpp
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(payloads[i])))
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(payloads[i])
	}
	for _, p := range payloads {
		out.Write(p)
	}
	return out.Bytes()
}

// ---------------------------------------------------------------- math bits

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func mixColor(a, b color.RGBA, t float64) color.RGBA {
	lerp := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return color.RGBA{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B), lerp(a.A, b.A)}
}

func sdRoundRect(px, py, hw, hh, r float64) float64 {
	qx := math.Abs(px) - (hw - r)
	qy := math.Abs(py) - (hh - r)
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

func distSeg(px, py float64, s seg) float64 {
	dx, dy := s.bx-s.ax, s.by-s.ay
	l2 := dx*dx + dy*dy
	t := 0.0
	if l2 > 0 {
		t = clamp01(((px-s.ax)*dx + (py-s.ay)*dy) / l2)
	}
	return math.Hypot(px-(s.ax+t*dx), py-(s.ay+t*dy))
}
