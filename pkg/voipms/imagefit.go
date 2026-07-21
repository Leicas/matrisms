package voipms

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers webp decoding for image.Decode

	_ "image/gif" // registers gif decoding (animated GIFs shrink to their first frame)
	_ "image/png" // registers png decoding
)

// fitDimensions is the ladder of max-edge sizes tried when shrinking; each
// step is JPEG-encoded at fitQuality and the first result under the cap wins.
var fitDimensions = []int{4096, 2048, 1600, 1280, 1024, 800, 640, 480}

const fitQuality = 80

// FitImageToMMS re-encodes an image so it fits under maxBytes (the VoIP.ms
// per-attachment MMS cap). Data already under the cap is returned unchanged.
// Oversized images are decoded (JPEG/PNG/GIF/WebP), scaled down through
// fitDimensions, and re-encoded as JPEG — transparency is flattened onto
// white, animated GIFs keep only their first frame.
func FitImageToMMS(data []byte, mime string, maxBytes int) ([]byte, string, error) {
	if len(data) <= maxBytes {
		return data, mime, nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("image is %d bytes (cap %d) and could not be decoded for shrinking: %w", len(data), maxBytes, err)
	}

	bounds := src.Bounds()
	longEdge := max(bounds.Dx(), bounds.Dy())

	for _, dim := range fitDimensions {
		if dim > longEdge {
			continue // never upscale
		}
		encoded, err := encodeJPEGScaled(src, dim)
		if err != nil {
			return nil, "", err
		}
		if len(encoded) <= maxBytes {
			return encoded, "image/jpeg", nil
		}
	}
	// Tiny-but-heavy originals (long edge below the smallest ladder step):
	// one attempt at the original size.
	if longEdge < fitDimensions[len(fitDimensions)-1] {
		encoded, err := encodeJPEGScaled(src, longEdge)
		if err != nil {
			return nil, "", err
		}
		if len(encoded) <= maxBytes {
			return encoded, "image/jpeg", nil
		}
	}
	return nil, "", fmt.Errorf("image could not be shrunk under %d bytes", maxBytes)
}

// encodeJPEGScaled scales the image so its long edge is maxEdge (keeping
// aspect ratio), flattens it onto white, and JPEG-encodes it.
func encodeJPEGScaled(src image.Image, maxEdge int) ([]byte, error) {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w >= h {
		h = h * maxEdge / w
		w = maxEdge
	} else {
		w = w * maxEdge / h
		h = maxEdge
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	// JPEG has no alpha channel; flatten onto white so transparent PNG/WebP
	// regions don't come out black.
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, xdraw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: fitQuality}); err != nil {
		return nil, fmt.Errorf("jpeg encode: %w", err)
	}
	return buf.Bytes(), nil
}
