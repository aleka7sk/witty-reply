package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestNormalizeImage(t *testing.T) {
	imageValue := image.NewRGBA(image.Rect(0, 0, 20, 10))
	imageValue.Set(1, 1, color.RGBA{R: 255, A: 255})
	var source bytes.Buffer
	if err := png.Encode(&source, imageValue); err != nil {
		t.Fatal(err)
	}

	result, err := NormalizeImage(source.Bytes(), ImageConfig{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.MediaType != "image/jpeg" || result.Width != 20 || result.Height != 10 || len(result.Data) == 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestNormalizeImageRejectsContentType(t *testing.T) {
	_, err := NormalizeImage([]byte("not an image"), ImageConfig{MaxInputBytes: 100, MaxOutputBytes: 100})
	if !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeImageRejectsPixelBombBeforeFullDecode(t *testing.T) {
	imageValue := image.NewRGBA(image.Rect(0, 0, 20, 10))
	var source bytes.Buffer
	if err := png.Encode(&source, imageValue); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeImage(source.Bytes(), ImageConfig{
		MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxPixels: 100,
	})
	if !errors.Is(err, ErrImageDimensions) {
		t.Fatalf("error = %v, want ErrImageDimensions", err)
	}
}
