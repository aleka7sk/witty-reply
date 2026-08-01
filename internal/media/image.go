// Package media validates and normalizes untrusted user media before it is
// handed to an external provider.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	defaultMaxDimension = 8_000
	defaultMaxPixels    = 12_000_000
	defaultLongSide     = 2_000
)

var (
	ErrUnsupportedImage = errors.New("unsupported image format")
	ErrInvalidImage     = errors.New("invalid image")
	ErrImageDimensions  = errors.New("image dimensions exceed safety limits")
	decodeSlot          = make(chan struct{}, 1)
)

type ImageConfig struct {
	MaxInputBytes  int
	MaxOutputBytes int
	MaxDimension   int
	MaxPixels      int
	LongSide       int
}

type NormalizedImage struct {
	Data      []byte
	MediaType string
	Width     int
	Height    int
}

func NormalizeImage(data []byte, cfg ImageConfig) (NormalizedImage, error) {
	if cfg.MaxInputBytes <= 0 || cfg.MaxOutputBytes <= 0 {
		return NormalizedImage{}, errors.New("image byte limits must be positive")
	}
	if len(data) == 0 || len(data) > cfg.MaxInputBytes {
		return NormalizedImage{}, fmt.Errorf("%w: input size", ErrInvalidImage)
	}
	if cfg.MaxDimension <= 0 {
		cfg.MaxDimension = defaultMaxDimension
	}
	if cfg.MaxPixels <= 0 {
		cfg.MaxPixels = defaultMaxPixels
	}
	if cfg.LongSide <= 0 {
		cfg.LongSide = defaultLongSide
	}

	mediaType := http.DetectContentType(data)
	if !supportedImageType(mediaType) {
		return NormalizedImage{}, fmt.Errorf("%w: %s", ErrUnsupportedImage, mediaType)
	}
	decodedConfig, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || decodedConfig.Width < 1 || decodedConfig.Height < 1 {
		return NormalizedImage{}, fmt.Errorf("%w: decode config", ErrInvalidImage)
	}
	pixels := int64(decodedConfig.Width) * int64(decodedConfig.Height)
	if decodedConfig.Width > cfg.MaxDimension || decodedConfig.Height > cfg.MaxDimension || pixels > int64(cfg.MaxPixels) {
		return NormalizedImage{}, ErrImageDimensions
	}

	// Full image decoding and high-quality resizing are memory/CPU intensive.
	// A separate process-wide slot prevents a burst of compressed images from
	// multiplying peak heap usage across Telegram workers.
	decodeSlot <- struct{}{}
	defer func() { <-decodeSlot }()

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return NormalizedImage{}, fmt.Errorf("%w: decode", ErrInvalidImage)
	}
	width, height := decodedConfig.Width, decodedConfig.Height
	if width > cfg.LongSide || height > cfg.LongSide {
		ratio := float64(cfg.LongSide) / float64(max(width, height))
		width = max(1, int(float64(width)*ratio))
		height = max(1, int(float64(height)*ratio))
		target := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), draw.Over, nil)
		source = target
	}

	// JPEG re-encoding removes EXIF/GPS metadata and keeps screenshots within
	// the Claude request-size limit. Quality 94 preserves small chat text.
	var output bytes.Buffer
	if err := jpeg.Encode(&output, source, &jpeg.Options{Quality: 94}); err != nil {
		return NormalizedImage{}, fmt.Errorf("encode normalized image: %w", err)
	}
	if output.Len() > cfg.MaxOutputBytes {
		return NormalizedImage{}, fmt.Errorf("%w: normalized output size", ErrInvalidImage)
	}
	return NormalizedImage{Data: output.Bytes(), MediaType: "image/jpeg", Width: width, Height: height}, nil
}

func supportedImageType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}
