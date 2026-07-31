// Package meme renders safe, locally generated meme cards. It deliberately
// does not download templates or fonts at runtime.
package meme

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	stdlibdraw "image/draw"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var (
	ErrFontUnavailable = errors.New("cyrillic-compatible font is unavailable")
	ErrEmptyMeme       = errors.New("meme headline and caption are empty")
)

const (
	defaultWidth  = 1080
	defaultHeight = 1080
	maxTextRunes  = 700
)

type Config struct {
	FontPath string
	Brand    string
	Width    int
	Height   int
}

type Renderer struct {
	font     *opentype.Font
	fontPath string
	brand    string
	width    int
	height   int
}

var systemFontCandidates = []string{
	"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	"/usr/local/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
	"/usr/local/share/fonts/dejavu/DejaVuSans-Bold.ttf",
	"/opt/homebrew/share/fonts/dejavu/DejaVuSans-Bold.ttf",
	`C:\Windows\Fonts\DejaVuSans-Bold.ttf`,
	`C:\Windows\Fonts\DejaVuSans.ttf`,
}

func NewRenderer(cfg Config) (*Renderer, error) {
	if cfg.Width <= 0 {
		cfg.Width = defaultWidth
	}
	if cfg.Height <= 0 {
		cfg.Height = defaultHeight
	}
	if cfg.Width < 320 || cfg.Height < 320 || cfg.Width > 4096 || cfg.Height > 4096 {
		return nil, errors.New("meme dimensions must be between 320 and 4096 pixels")
	}
	if strings.TrimSpace(cfg.Brand) == "" {
		cfg.Brand = "WITTY REPLY"
	}

	paths := make([]string, 0, len(systemFontCandidates)+1)
	if strings.TrimSpace(cfg.FontPath) != "" {
		paths = append(paths, strings.TrimSpace(cfg.FontPath))
	}
	paths = append(paths, systemFontCandidates...)
	var parseErrors []error
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			continue
		}
		parsed, err := opentype.Parse(data)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Errorf("parse %s: %w", path, err))
			continue
		}
		if !supportsCyrillic(parsed) {
			parseErrors = append(parseErrors, fmt.Errorf("%s does not contain required Cyrillic glyphs", path))
			continue
		}
		return &Renderer{
			font: parsed, fontPath: path, brand: cleanText(cfg.Brand),
			width: cfg.Width, height: cfg.Height,
		}, nil
	}
	if len(parseErrors) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrFontUnavailable, errors.Join(parseErrors...))
	}
	return nil, ErrFontUnavailable
}

func (r *Renderer) FontPath() string { return r.fontPath }

// Render creates a standalone PNG card. Callers should run generated text
// through safety.Filter before rendering.
func (r *Renderer) Render(value domain.Meme) ([]byte, error) {
	headline := limitText(cleanText(value.Headline), maxTextRunes)
	caption := limitText(cleanText(value.Caption), maxTextRunes)
	footer := limitText(cleanText(value.Footer), maxTextRunes)
	if headline == "" && caption == "" {
		return nil, ErrEmptyMeme
	}

	canvas := image.NewRGBA(image.Rect(0, 0, r.width, r.height))
	drawGradient(canvas, color.RGBA{R: 22, G: 16, B: 42, A: 255}, color.RGBA{R: 55, G: 22, B: 74, A: 255})
	drawGlow(canvas, image.Pt(r.width*82/100, r.height*18/100), r.width/3, color.RGBA{R: 215, G: 74, B: 139, A: 38})
	drawGlow(canvas, image.Pt(r.width*16/100, r.height*82/100), r.width/3, color.RGBA{R: 112, G: 86, B: 220, A: 32})

	margin := r.width * 7 / 100
	panelTop := r.height * 15 / 100
	panelBottom := r.height * 88 / 100
	drawRoundedRect(canvas, image.Rect(margin, panelTop, r.width-margin, panelBottom), r.width/35, color.RGBA{R: 17, G: 14, B: 31, A: 220})
	drawRoundedRect(canvas, image.Rect(margin, panelTop, margin+r.width/100, panelBottom), r.width/200, color.RGBA{R: 232, G: 93, B: 151, A: 255})

	brandFace, err := r.face(scaleSize(r.width, 25))
	if err != nil {
		return nil, err
	}
	headlineFace, err := r.face(scaleSize(r.width, 62))
	if err != nil {
		return nil, err
	}
	captionFace, err := r.face(scaleSize(r.width, 40))
	if err != nil {
		return nil, err
	}
	footerFace, err := r.face(scaleSize(r.width, 27))
	if err != nil {
		return nil, err
	}

	brand := fitLine(brandFace, strings.ToUpper(r.brand), r.width-2*margin)
	drawText(canvas, brandFace, brand, margin, r.height*9/100, color.RGBA{R: 242, G: 187, B: 215, A: 255})
	textLeft := margin + r.width*5/100
	textWidth := r.width - margin - textLeft - r.width*4/100
	y := panelTop + r.height*11/100
	if headline != "" {
		y = drawParagraph(canvas, headlineFace, headline, textLeft, y, textWidth, 3, color.RGBA{R: 255, G: 250, B: 252, A: 255})
		y += r.height * 4 / 100
	}
	if caption != "" {
		drawRoundedRect(canvas, image.Rect(textLeft, y-r.height/40, r.width-margin-r.width*4/100, y+r.height/100), r.width/100, color.RGBA{R: 230, G: 90, B: 150, A: 55})
		captionBaseline := y + r.height/25
		captionBottom := panelBottom - r.height*4/100
		if footer != "" {
			captionBottom = panelBottom - r.height*11/100
		}
		lineHeight := captionFace.Metrics().Height.Ceil() + captionFace.Metrics().Height.Ceil()/5
		maxCaptionLines := maxInt((captionBottom-captionBaseline)/maxInt(lineHeight, 1), 1)
		maxCaptionLines = minInt(maxCaptionLines, 7)
		drawParagraph(canvas, captionFace, caption, textLeft, captionBaseline, textWidth, maxCaptionLines, color.RGBA{R: 230, G: 218, B: 235, A: 255})
	}
	if footer != "" {
		footerY := panelBottom - r.height*5/100
		drawText(canvas, footerFace, fitLine(footerFace, footer, textWidth), textLeft, footerY, color.RGBA{R: 181, G: 164, B: 193, A: 255})
	}
	drawText(canvas, footerFace, "↗ ОТВЕТ ГОТОВ", margin, r.height*95/100, color.RGBA{R: 202, G: 185, B: 214, A: 255})

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		return nil, fmt.Errorf("encode meme PNG: %w", err)
	}
	return encoded.Bytes(), nil
}

// RenderOrFallback makes missing host fonts non-fatal for the bot. The caller
// can send fallbackText as a regular Telegram message when err is non-nil.
func RenderOrFallback(cfg Config, value domain.Meme) (pngData []byte, fallbackText string, err error) {
	fallbackText = FallbackText(value)
	renderer, err := NewRenderer(cfg)
	if err != nil {
		return nil, fallbackText, err
	}
	pngData, err = renderer.Render(value)
	if err != nil {
		return nil, fallbackText, err
	}
	return pngData, fallbackText, nil
}

func FallbackText(value domain.Meme) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{value.Headline, value.Caption, value.Footer} {
		if part = cleanText(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

func (r *Renderer) face(size float64) (font.Face, error) {
	face, err := opentype.NewFace(r.font, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, fmt.Errorf("create font face: %w", err)
	}
	return face, nil
}

func supportsCyrillic(parsed *opentype.Font) bool {
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: 16, DPI: 72})
	if err != nil {
		return false
	}
	for _, r := range "ПриветӘҚҢӨҰҮІ" {
		if _, ok := face.GlyphAdvance(r); !ok {
			return false
		}
	}
	return true
}

func scaleSize(width int, at1080 float64) float64 {
	return at1080 * float64(width) / defaultWidth
}

func drawText(dst *image.RGBA, face font.Face, text string, x, baseline int, ink color.Color) {
	drawer := font.Drawer{Dst: dst, Src: image.NewUniform(ink), Face: face, Dot: fixed.P(x, baseline)}
	drawer.DrawString(text)
}

func drawParagraph(dst *image.RGBA, face font.Face, text string, x, baseline, width, maxLines int, ink color.Color) int {
	if maxLines <= 0 {
		return baseline
	}
	lines := wrapText(face, text, width)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = fitEllipsis(face, lines[maxLines-1], width)
	}
	lineHeight := face.Metrics().Height.Ceil() + face.Metrics().Height.Ceil()/5
	for _, line := range lines {
		drawText(dst, face, line, x, baseline, ink)
		baseline += lineHeight
	}
	return baseline
}

func wrapText(face font.Face, text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	measure := font.Drawer{Face: face}
	result := make([]string, 0, 4)
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			continue
		}
		line := ""
		for _, word := range words {
			pieces := splitWord(&measure, word, maxWidth)
			for _, piece := range pieces {
				candidate := piece
				if line != "" {
					candidate = line + " " + piece
				}
				if measure.MeasureString(candidate).Ceil() <= maxWidth {
					line = candidate
					continue
				}
				if line != "" {
					result = append(result, line)
				}
				line = piece
			}
		}
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func splitWord(measure *font.Drawer, word string, maxWidth int) []string {
	if measure.MeasureString(word).Ceil() <= maxWidth {
		return []string{word}
	}
	parts := make([]string, 0, 2)
	current := make([]rune, 0, utf8.RuneCountInString(word))
	for _, r := range word {
		candidate := string(append(current, r))
		if len(current) > 0 && measure.MeasureString(candidate).Ceil() > maxWidth {
			parts = append(parts, string(current))
			current = current[:0]
		}
		current = append(current, r)
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}
	return parts
}

func fitEllipsis(face font.Face, line string, maxWidth int) string {
	measure := font.Drawer{Face: face}
	runes := []rune(strings.TrimSpace(line))
	for len(runes) > 0 && measure.MeasureString(string(runes)+"…").Ceil() > maxWidth {
		runes = runes[:len(runes)-1]
	}
	return strings.TrimSpace(string(runes)) + "…"
}

func fitLine(face font.Face, line string, maxWidth int) string {
	measure := font.Drawer{Face: face}
	if measure.MeasureString(line).Ceil() <= maxWidth {
		return line
	}
	return fitEllipsis(face, line, maxWidth)
}

func drawGradient(dst *image.RGBA, top, bottom color.RGBA) {
	bounds := dst.Bounds()
	denominator := maxInt(bounds.Dy()-1, 1)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		n := y - bounds.Min.Y
		shade := color.RGBA{
			R: uint8((int(top.R)*(denominator-n) + int(bottom.R)*n) / denominator),
			G: uint8((int(top.G)*(denominator-n) + int(bottom.G)*n) / denominator),
			B: uint8((int(top.B)*(denominator-n) + int(bottom.B)*n) / denominator),
			A: 255,
		}
		stdlibdraw.Draw(dst, image.Rect(bounds.Min.X, y, bounds.Max.X, y+1), image.NewUniform(shade), image.Point{}, stdlibdraw.Src)
	}
}

func drawGlow(dst *image.RGBA, center image.Point, radius int, glow color.RGBA) {
	if radius <= 0 {
		return
	}
	bounds := dst.Bounds().Intersect(image.Rect(center.X-radius, center.Y-radius, center.X+radius+1, center.Y+radius+1))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dx, dy := x-center.X, y-center.Y
			distanceSquared := dx*dx + dy*dy
			if distanceSquared > radius*radius {
				continue
			}
			alpha := int(glow.A) * (radius*radius - distanceSquared) / (radius * radius)
			pixel := glow
			pixel.A = uint8(alpha)
			blendPixel(dst, x, y, pixel)
		}
	}
}

func drawRoundedRect(dst *image.RGBA, rect image.Rectangle, radius int, fill color.RGBA) {
	rect = rect.Intersect(dst.Bounds())
	if rect.Empty() {
		return
	}
	if radius <= 0 {
		stdlibdraw.Draw(dst, rect, image.NewUniform(fill), image.Point{}, stdlibdraw.Over)
		return
	}
	radius = minInt(radius, minInt(rect.Dx(), rect.Dy())/2)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			dx := maxInt(maxInt(rect.Min.X+radius-x, 0), x-(rect.Max.X-radius-1))
			dy := maxInt(maxInt(rect.Min.Y+radius-y, 0), y-(rect.Max.Y-radius-1))
			if dx*dx+dy*dy <= radius*radius {
				blendPixel(dst, x, y, fill)
			}
		}
	}
}

func blendPixel(dst *image.RGBA, x, y int, source color.RGBA) {
	destination := dst.RGBAAt(x, y)
	a := int(source.A)
	inverse := 255 - a
	dst.SetRGBA(x, y, color.RGBA{
		R: uint8((int(source.R)*a + int(destination.R)*inverse) / 255),
		G: uint8((int(source.G)*a + int(destination.G)*inverse) / 255),
		B: uint8((int(source.B)*a + int(destination.B)*inverse) / 255),
		A: 255,
	})
}

func cleanText(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			continue
		default:
			b.WriteRune(r)
		}
	}
	lines := strings.Split(b.String(), "\n")
	for index := range lines {
		lines[index] = strings.Join(strings.Fields(lines[index]), " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func limitText(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return strings.TrimSpace(string(runes[:maximum-1])) + "…"
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
