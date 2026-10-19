package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestIconEncoding checks whichever container we produced on this platform.
func TestIconEncoding(t *testing.T) {
	if len(iconPNG) == 0 || len(iconLockedPNG) == 0 {
		t.Fatal("icons not generated")
	}
	// The web panel's favicon colour and the tray icon must stay in sync.
	if themeRunning.lo.R != 0x6d || themeRunning.hi.G != 0xe0 {
		t.Fatalf("brand gradient drifted from the web favicon: %#v", themeRunning)
	}

	if !isWindows() {
		if _, err := png.Decode(bytes.NewReader(iconPNG)); err != nil {
			t.Fatalf("non-windows icon must be a valid PNG: %v", err)
		}
		return
	}

	// Windows: ICONDIR header + one 16-byte entry per rendered size.
	if !bytes.HasPrefix(iconPNG, []byte{0, 0, 1, 0}) {
		t.Fatal("windows icon must be an ICO container")
	}
	count := int(binary.LittleEndian.Uint16(iconPNG[4:6]))
	if count != len(iconSizes) {
		t.Fatalf("expected %d icon entries, got %d", len(iconSizes), count)
	}
	for i := 0; i < count; i++ {
		e := iconPNG[6+16*i : 6+16*(i+1)]
		size := int(e[0])
		if size == 0 {
			size = 256
		}
		off := binary.LittleEndian.Uint32(e[12:16])
		length := binary.LittleEndian.Uint32(e[8:12])
		if int(off)+int(length) > len(iconPNG) {
			t.Fatalf("entry %d points outside the container", i)
		}
		img, err := png.Decode(bytes.NewReader(iconPNG[off : off+length]))
		if err != nil {
			t.Fatalf("entry %d payload is not a PNG: %v", i, err)
		}
		if img.Bounds().Dx() != size {
			t.Fatalf("entry %d says %dpx but decodes to %dpx", i, size, img.Bounds().Dx())
		}
	}
}

// TestRenderIconGeometry verifies the artwork itself: the four corners stay
// transparent (rounded square), the centre is opaque, and the glyph pixels in
// the middle are brighter (gradient stroke) than the background.
func TestRenderIconGeometry(t *testing.T) {
	img := renderIcon(64, themeRunning)
	if c := img.RGBAAt(0, 0); c.A != 0 {
		t.Fatalf("corner should be transparent, got %#v", c)
	}
	mid := img.RGBAAt(32, 38) // on the base line
	if mid.A < 200 {
		t.Fatalf("glyph should be opaque, got %#v", mid)
	}
	bg := img.RGBAAt(32, 10) // background above the glyph
	if bg.A < 200 {
		t.Fatalf("background should be opaque inside the square, got %#v", bg)
	}
	if luma(mid) <= luma(bg) {
		t.Fatalf("glyph (%v) should be brighter than background (%v)", mid, bg)
	}
}

func luma(c interface {
	RGBA() (uint32, uint32, uint32, uint32)
}) float64 {
	r, g, b, _ := c.RGBA()
	return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
}

func isWindows() bool {
	return iconPNG[0] != 0x89 // PNG magic vs ICO magic
}

// TestDumpIcons writes PNG previews when USBGW_DUMP_ICONS is set, so the
// artwork can be eyeballed (or shipped as docs) without running the exe.
func TestDumpIcons(t *testing.T) {
	dir := os.Getenv("USBGW_DUMP_ICONS")
	if dir == "" {
		t.Skip("set USBGW_DUMP_ICONS=<dir> to dump icon previews")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, theme := range map[string]iconTheme{"running": themeRunning, "locked": themeLocked} {
		for _, size := range []int{16, 32, 256} {
			var buf bytes.Buffer
			if err := png.Encode(&buf, renderIcon(size, theme)); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, name+"-"+itoa(size)+".png")
			if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Log("wrote", p)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
