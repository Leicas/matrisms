package voipms

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
)

// noisyImage builds a random-noise RGBA image, which compresses poorly —
// ideal for forcing the shrink ladder to do real work.
func noisyImage(w, h int) *image.RGBA {
	rng := rand.New(rand.NewSource(42))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(rng.Intn(256))
	}
	return img
}

func TestFitImageToMMSPassthrough(t *testing.T) {
	data := []byte("small enough")
	out, mime, err := FitImageToMMS(data, "image/gif", 1000)
	if err != nil || !bytes.Equal(out, data) || mime != "image/gif" {
		t.Fatalf("small payload must pass through unchanged: %v %q", err, mime)
	}
}

func TestFitImageToMMSShrinksOversizedJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, noisyImage(2500, 2000), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= MMSMaxMediaBytes {
		t.Fatalf("test image unexpectedly small (%d bytes); make it bigger", buf.Len())
	}
	out, mime, err := FitImageToMMS(buf.Bytes(), "image/jpeg", MMSMaxMediaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > MMSMaxMediaBytes {
		t.Errorf("shrunk image still %d bytes (cap %d)", len(out), MMSMaxMediaBytes)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", mime)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("shrunk output not decodable: %v", err)
	}
	if img.Bounds().Dx() > 2500 {
		t.Error("image was upscaled")
	}
}

func TestFitImageToMMSFlattensTransparentPNG(t *testing.T) {
	// Fully transparent PNG with noisy (invisible) color channels: big on
	// disk to force the re-encode, and after flattening the JPEG must decode
	// to (near-)white pixels, not black.
	rng := rand.New(rand.NewSource(7))
	img := image.NewNRGBA(image.Rect(0, 0, 600, 600))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = byte(rng.Intn(256))
		img.Pix[i+1] = byte(rng.Intn(256))
		img.Pix[i+2] = byte(rng.Intn(256))
		img.Pix[i+3] = 0
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	cap := buf.Len() / 2 // smaller than the PNG, plenty for a white JPEG
	out, mime, err := FitImageToMMS(buf.Bytes(), "image/png", cap)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > cap {
		t.Errorf("output %d bytes exceeds cap %d", len(out), cap)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", mime)
	}
	decoded, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(decoded.Bounds().Dx()/2, decoded.Bounds().Dy()/2).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent region not flattened to white: got %v", color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255})
	}
}

func TestFitImageToMMSUndecodableErrors(t *testing.T) {
	if _, _, err := FitImageToMMS(bytes.Repeat([]byte{0xde, 0xad}, 1000), "image/jpeg", 10); err == nil {
		t.Error("garbage over the cap should error")
	}
}
