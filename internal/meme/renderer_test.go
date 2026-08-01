package meme

import (
	"bytes"
	"errors"
	"image/png"
	"testing"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestRendererCreatesCyrillicPNG(t *testing.T) {
	renderer, err := NewRenderer(Config{Width: 640, Height: 640, Brand: "ОСТРОУМНЫЙ ОТВЕТ"})
	if errors.Is(err, ErrFontUnavailable) {
		t.Skip("host has no DejaVu Cyrillic font")
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderer.Render(domain.Meme{
		Headline: "Когда аргументы закончились",
		Caption:  "но сообщение всё ещё печатается",
		Footer:   "Спокойно. Мы только начали.",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("invalid PNG: %v", err)
	}
	if decoded.Bounds().Dx() != 640 || decoded.Bounds().Dy() != 640 {
		t.Fatalf("bounds = %v", decoded.Bounds())
	}
	if len(data) < 10_000 {
		t.Fatalf("PNG unexpectedly small: %d bytes", len(data))
	}
}

func TestRendererRejectsEmptyMeme(t *testing.T) {
	renderer, err := NewRenderer(Config{})
	if errors.Is(err, ErrFontUnavailable) {
		t.Skip("host has no DejaVu Cyrillic font")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renderer.Render(domain.Meme{}); !errors.Is(err, ErrEmptyMeme) {
		t.Fatalf("error = %v", err)
	}
}

func TestFallbackTextWorksWithoutRenderer(t *testing.T) {
	value := domain.Meme{Headline: "Заголовок", Caption: "Подпись", Footer: "Финал"}
	if got := FallbackText(value); got != "Заголовок\nПодпись\nФинал" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestConfiguredMissingFontFallsBackToSystemDejaVu(t *testing.T) {
	renderer, err := NewRenderer(Config{FontPath: "/definitely/missing/font.ttf"})
	if errors.Is(err, ErrFontUnavailable) {
		t.Skip("host has no system DejaVu font")
	}
	if err != nil {
		t.Fatal(err)
	}
	if renderer.FontPath() == "/definitely/missing/font.ttf" {
		t.Fatal("missing configured font was selected")
	}
}
