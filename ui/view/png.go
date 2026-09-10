package view

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	PNGCellWidth  = 8
	PNGCellHeight = 16
)

// PNG renders exactly PNGCellWidth by PNGCellHeight pixels per Frame cell.
// Reset colours remain transparent, making host-default colour use explicit.
func PNG(frame *Frame) ([]byte, error) {
	if frame.width == 0 || frame.height == 0 {
		return nil, nil
	}
	imageFrame := image.NewRGBA(image.Rect(0, 0, frame.width*PNGCellWidth, frame.height*PNGCellHeight))
	for y := 0; y < frame.height; y++ {
		for x := 0; x < frame.width; x++ {
			cell, _ := frame.CellAt(x, y)
			if background, ok := cell.Style.Background.RGBA(); ok {
				draw.Draw(imageFrame, image.Rect(x*PNGCellWidth, y*PNGCellHeight, (x+1)*PNGCellWidth, (y+1)*PNGCellHeight), image.NewUniform(background), image.Point{}, draw.Src)
			}
			if cell.Text == "" {
				continue
			}
			foreground, ok := cell.Style.Foreground.RGBA()
			if !ok {
				continue
			}
			if cell.Style.Dim {
				foreground.R /= 2
				foreground.G /= 2
				foreground.B /= 2
			}
			span := max(cell.Width, 1)
			clip := image.Rect(x*PNGCellWidth, y*PNGCellHeight, min(x+span, frame.width)*PNGCellWidth, (y+1)*PNGCellHeight)
			origin := fixed.P(x*PNGCellWidth, y*PNGCellHeight+13)
			drawer := font.Drawer{Dst: imageFrame.SubImage(clip).(draw.Image), Src: image.NewUniform(foreground), Face: basicfont.Face7x13, Dot: origin}
			drawer.DrawString(cell.Text)
			if cell.Style.Bold {
				drawer.Dot = origin
				drawer.Dot.X += fixed.I(1)
				drawer.DrawString(cell.Text)
			}
			if cell.Style.Underline {
				draw.Draw(imageFrame, image.Rect(x*PNGCellWidth, (y+1)*PNGCellHeight-2, clip.Max.X, (y+1)*PNGCellHeight-1), image.NewUniform(foreground), image.Point{}, draw.Src)
			}
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageFrame); err != nil {
		return nil, fmt.Errorf("encode PNG: %w", err)
	}
	return encoded.Bytes(), nil
}
