package view_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/raulfrk/herdr-plugin-kit/ui/theme"
	"github.com/raulfrk/herdr-plugin-kit/ui/view"
	"github.com/rivo/uniseg"
	"pgregory.net/rapid"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestFrameUnicodeAndClipping(t *testing.T) {
	frame, err := view.NewFrame(6, 1)
	if err != nil {
		t.Fatal(err)
	}
	style := view.Style{Foreground: "#ffffff", Background: "#112233"}
	frame.PutText(-1, 0, "界Ae\u0301Z", style)
	if got := sgr.ReplaceAllString(view.ANSI(frame), ""); got != " Ae\u0301Z  " {
		t.Fatalf("plain frame = %q", got)
	}
	cell, _ := frame.CellAt(2, 0)
	if cell.Text != "e\u0301" || cell.Width != 1 {
		t.Fatalf("combining cell = %+v", cell)
	}

	frame.PutText(4, 0, "界", style)
	frame.PutText(5, 0, "x", style)
	lead, _ := frame.CellAt(4, 0)
	last, _ := frame.CellAt(5, 0)
	if lead.Text != "" || last.Text != "x" || last.Continuation {
		t.Fatalf("wide overwrite left %+v last %+v", lead, last)
	}
}

func TestFrameClippingPreservesEdgeSentinels(t *testing.T) {
	frame, _ := view.NewFrame(4, 1)
	frame.PutText(0, 0, "abcd", view.Style{})
	frame.PutText(-1, 0, "界", view.Style{Bold: true})
	frame.PutText(3, 0, "界", view.Style{Bold: true})
	if got := sgr.ReplaceAllString(view.ANSI(frame), ""); got != "abcd" {
		t.Fatalf("clipped frame = %q", got)
	}
	for x, want := range []string{"a", "b", "c", "d"} {
		cell, _ := frame.CellAt(x, 0)
		if cell.Text != want || cell.Continuation || cell.Width != 1 {
			t.Fatalf("cell %d = %+v", x, cell)
		}
	}
}

func TestANSIAndPNGDeterminismDimensionsAndStyleParity(t *testing.T) {
	frame, _ := view.NewFrame(4, 2)
	style := view.Style{Foreground: "#fefefe", Background: "#123456", Bold: true, Dim: true, Underline: true}
	frame.Fill(0, 0, 4, 2, style)
	frame.PutText(0, 0, "A界", style)
	ansiA, ansiB := view.ANSI(frame), view.ANSI(frame)
	if ansiA != ansiB {
		t.Fatal("ANSI output differs")
	}
	if strings.Contains(ansiA, "\x1b[2J") || strings.Contains(ansiA, "\x1b[?") || !strings.HasSuffix(ansiA, "\x1b[0m") {
		t.Fatalf("ANSI state handling = %q", ansiA)
	}
	if !strings.Contains(ansiA, "\x1b[0;1;2;4;38;2;254;254;254;48;2;18;52;86m") {
		t.Fatalf("ANSI style = %q", ansiA)
	}
	plain := sgr.ReplaceAllString(ansiA, "")
	for row, line := range strings.Split(plain, "\n") {
		if width := uniseg.StringWidth(line); width != frame.Width() {
			t.Fatalf("row %d width = %d", row, width)
		}
	}
	pngA, err := view.PNG(frame)
	if err != nil {
		t.Fatal(err)
	}
	pngB, _ := view.PNG(frame)
	if !bytes.Equal(pngA, pngB) {
		t.Fatal("PNG output differs")
	}
	decoded, err := png.Decode(bytes.NewReader(pngA))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds().Size(); got.X != 4*view.PNGCellWidth || got.Y != 2*view.PNGCellHeight {
		t.Fatalf("PNG size = %v", got)
	}
	wantBackground, _ := theme.Color("#123456").RGBA()
	if got := decoded.At(31, 31); got != wantBackground {
		t.Fatalf("background = %v, want %v", got, wantBackground)
	}
	if got := decoded.At(0, 14); got != (color.RGBA{R: 127, G: 127, B: 127, A: 255}) {
		t.Fatalf("dim underline = %v", got)
	}
}

func TestANSIAlwaysResetsAndEmitsOnlySGRControls(t *testing.T) {
	frame, _ := view.NewFrame(8, 2)
	frame.PutText(0, 0, "styled", view.Style{Foreground: "#ffffff"})
	frame.PutText(0, 1, "\x1b[2Jok", view.Style{})
	output := view.ANSI(frame)
	if !strings.HasSuffix(output, "\x1b[0m") {
		t.Fatalf("missing final reset: %q", output)
	}
	withoutSGR := sgr.ReplaceAllString(output, "")
	if strings.ContainsRune(withoutSGR, '\x1b') {
		t.Fatalf("non-SGR control emitted: %q", output)
	}

	unstyled, _ := view.NewFrame(1, 1)
	if got := view.ANSI(unstyled); got != " \x1b[0m" {
		t.Fatalf("unstyled ANSI = %q", got)
	}
}

func TestPNGStyleFieldsAndReset(t *testing.T) {
	render := func(style view.Style) image.Image {
		frame, _ := view.NewFrame(1, 1)
		frame.PutText(0, 0, "A", style)
		encoded, err := view.PNG(frame)
		if err != nil {
			t.Fatal(err)
		}
		again, err := view.PNG(frame)
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatal("generated PNG output is nondeterministic")
		}
		decoded, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	count := func(img image.Image, target color.RGBA) int {
		total := 0
		for y := 0; y < 16; y++ {
			for x := 0; x < 8; x++ {
				if color.RGBAModel.Convert(img.At(x, y)).(color.RGBA) == target {
					total++
				}
			}
		}
		return total
	}
	foreground := color.RGBA{R: 128, G: 128, B: 128, A: 255}
	regular := render(view.Style{Foreground: "#808080"})
	bold := render(view.Style{Foreground: "#808080", Bold: true})
	if count(bold, foreground) <= count(regular, foreground) {
		t.Fatal("bold PNG did not add foreground pixels")
	}
	dim := render(view.Style{Foreground: "#808080", Dim: true})
	if count(dim, color.RGBA{R: 64, G: 64, B: 64, A: 255}) == 0 {
		t.Fatal("dim PNG did not reduce foreground")
	}
	underline := render(view.Style{Foreground: "#808080", Underline: true})
	if color.RGBAModel.Convert(underline.At(0, 14)).(color.RGBA) != foreground {
		t.Fatal("underline PNG omitted underline row")
	}
	reset := render(view.Style{Foreground: theme.Reset, Background: theme.Reset})
	if _, _, _, alpha := reset.At(0, 0).RGBA(); alpha != 0 {
		t.Fatal("reset PNG was not transparent")
	}
}

func TestFrameRejectsOverflowAndClipsExtremeRectangles(t *testing.T) {
	if _, err := view.NewFrame(math.MaxInt, 2); err == nil {
		t.Fatal("overflowing frame accepted")
	}
	frame, _ := view.NewFrame(2, 1)
	frame.Fill(math.MaxInt, 0, math.MaxInt, 1, view.Style{})
	frame.Fill(math.MinInt, 0, math.MaxInt, 1, view.Style{})
	frame.PutText(math.MaxInt, 0, "界", view.Style{})
}

func TestPropertyFrameNeverContainsOrphanContinuation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		width := rapid.IntRange(0, 20).Draw(t, "width")
		frame, err := view.NewFrame(width, 1)
		if err != nil {
			t.Fatal(err)
		}
		texts := []string{"a", "界", "e\u0301", "🙂", "ab", "界x"}
		for range rapid.IntRange(0, 30).Draw(t, "operations") {
			x := rapid.IntRange(-3, width+3).Draw(t, "x")
			if rapid.Bool().Draw(t, "fill") {
				frame.Fill(x, 0, rapid.IntRange(1, 3).Draw(t, "fill_width"), 1, view.Style{Bold: true})
			} else {
				frame.PutText(x, 0, rapid.SampledFrom(texts).Draw(t, "text"), view.Style{})
			}
		}
		for x := 0; x < width; x++ {
			cell, _ := frame.CellAt(x, 0)
			if !cell.Continuation {
				continue
			}
			lead := x - 1
			for lead >= 0 {
				previous, _ := frame.CellAt(lead, 0)
				if !previous.Continuation {
					break
				}
				lead--
			}
			if lead < 0 {
				t.Fatalf("orphan at %d", x)
			}
			leading, _ := frame.CellAt(lead, 0)
			if leading.Width <= x-lead {
				t.Fatalf("orphan at %d after lead %+v", x, leading)
			}
		}
	})
}

func TestPropertyRendererDimensionsAndDeterminism(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		width := rapid.IntRange(0, 12).Draw(t, "width")
		height := rapid.IntRange(0, 6).Draw(t, "height")
		frame, err := view.NewFrame(width, height)
		if err != nil {
			t.Fatal(err)
		}
		style := view.Style{Foreground: "#abcdef", Background: "#102030"}
		for y := 0; y < height; y++ {
			frame.PutText(rapid.IntRange(-2, width+1).Draw(t, "x"), y, rapid.SampledFrom([]string{"A", "界", "e\u0301", "🙂x"}).Draw(t, "text"), style)
		}
		ansi := view.ANSI(frame)
		if view.ANSI(frame) != ansi {
			t.Fatal("ANSI output is nondeterministic")
		}
		if width == 0 || height == 0 {
			if ansi != "" {
				t.Fatalf("zero-geometry ANSI = %q", ansi)
			}
		} else {
			lines := strings.Split(sgr.ReplaceAllString(ansi, ""), "\n")
			if len(lines) != height {
				t.Fatalf("ANSI rows = %d, want %d", len(lines), height)
			}
			for _, line := range lines {
				if got := uniseg.StringWidth(line); got != width {
					t.Fatalf("ANSI width = %d, want %d", got, width)
				}
			}
		}
		encoded, err := view.PNG(frame)
		if width == 0 || height == 0 {
			if err != nil || len(encoded) != 0 {
				t.Fatal("zero-geometry PNG was not an empty no-op")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		again, err := view.PNG(frame)
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatal("generated PNG output is nondeterministic")
		}
		decoded, err := png.Decode(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if got := decoded.Bounds().Size(); got.X != width*view.PNGCellWidth || got.Y != height*view.PNGCellHeight {
			t.Fatalf("PNG size = %v", got)
		}
	})
}

func TestZeroGeometryRenderersAreNoOps(t *testing.T) {
	for _, dimensions := range [][2]int{{0, 0}, {0, 2}, {2, 0}} {
		frame, err := view.NewFrame(dimensions[0], dimensions[1])
		if err != nil {
			t.Fatal(err)
		}
		if got := view.ANSI(frame); got != "" {
			t.Fatalf("ANSI(%v) = %q", dimensions, got)
		}
		if got, err := view.PNG(frame); err != nil || len(got) != 0 {
			t.Fatalf("PNG(%v) = %d bytes, %v", dimensions, len(got), err)
		}
	}
}
