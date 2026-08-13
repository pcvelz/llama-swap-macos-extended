package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// Icon rendering for the system tray, reproducing the macOS menu-bar helper's
// curated design (macos-menu IconRenderer.swift): one or two rows of EIGHT
// small rounded segments per bar — filled segments solid, empty segments the
// same tone at low alpha. Top row = first configured metric. Monochrome
// light-gray, matching the template look of the macOS original; the tray has
// no "offline" glyph by design — offline is signalled in the tooltip and the
// menu's load row, never by decorating the icon.
var (
	iconFill  = color.NRGBA{R: 230, G: 230, B: 230, A: 245}
	iconEmpty = color.NRGBA{R: 230, G: 230, B: 230, A: 60}
)

const (
	iconSize     = 32 // square canvas: Windows ICO and SNI hosts expect square
	iconSegments = 8  // same segment count as IconRenderer.swift
)

// RenderIconImage draws the segmented-bar icon. values are clamped to 0..1;
// each of the (1 or 2) rows shows round(value*8) filled segments.
func RenderIconImage(values []float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))

	rows := len(values)
	if rows < 1 {
		rows = 1
	}
	if rows > 2 {
		rows = 2
	}

	// Geometry mirrors the Swift renderer's proportions (thin vertical pills
	// with 1px gutters), centered as a wide strip on the square canvas.
	segW, gap := 3, 1
	stripW := iconSegments*segW + (iconSegments-1)*gap // 31
	segH, rowGap := 7, 3
	stripH := rows*segH + (rows-1)*rowGap
	x0 := (iconSize - stripW) / 2
	y0 := (iconSize - stripH) / 2

	for row := 0; row < rows; row++ {
		v := 0.0
		if row < len(values) {
			v = values[row]
		}
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		filled := int(v*float64(iconSegments) + 0.5)
		for seg := 0; seg < iconSegments; seg++ {
			c := iconEmpty
			if seg < filled {
				c = iconFill
			}
			sx := x0 + seg*(segW+gap)
			sy := y0 + row*(segH+rowGap)
			drawPill(img, sx, sy, segW, segH, c)
		}
	}

	return img
}

// drawPill fills a w×h rect, skipping the four corner pixels for the rounded
// look of the original's roundedRect segments.
func drawPill(img *image.NRGBA, x, y, w, h int, c color.NRGBA) {
	for py := y; py < y+h; py++ {
		for px := x; px < x+w; px++ {
			corner := (px == x || px == x+w-1) && (py == y || py == y+h-1)
			if corner {
				continue
			}
			img.SetNRGBA(px, py, c)
		}
	}
}

// RenderIconPNG encodes the bar icon as PNG (used on Linux and macOS).
func RenderIconPNG(values []float64) []byte {
	var buf bytes.Buffer
	// Encoding a fresh in-memory image never fails; ignore the error.
	_ = png.Encode(&buf, RenderIconImage(values))
	return buf.Bytes()
}

// RenderIconICO encodes the bar icon as a single-image ICO container with a
// PNG payload (valid on Windows Vista and later; used for the Windows tray).
func RenderIconICO(values []float64) []byte {
	pngBytes := RenderIconPNG(values)

	var buf bytes.Buffer
	// ICONDIR: reserved, type=1 (icon), count=1
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY
	buf.WriteByte(iconSize)                                        // width
	buf.WriteByte(iconSize)                                        // height
	buf.WriteByte(0)                                               // colors in palette
	buf.WriteByte(0)                                               // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))             // color planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))            // bits per pixel
	binary.Write(&buf, binary.LittleEndian, uint32(len(pngBytes))) // payload size
	binary.Write(&buf, binary.LittleEndian, uint32(6+16))          // payload offset
	buf.Write(pngBytes)
	return buf.Bytes()
}
