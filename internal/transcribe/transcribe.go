// Package transcribe defines a small provider-neutral speech-to-text boundary.
package transcribe

import (
	"context"
	"errors"
)

var (
	ErrDisabled        = errors.New("audio transcription is disabled")
	ErrEmptyAudio      = errors.New("audio is empty")
	ErrAudioTooLarge   = errors.New("audio exceeds configured limit")
	ErrEmptyTranscript = errors.New("transcription provider returned empty text")
)

type Audio struct {
	Data      []byte
	Filename  string
	MediaType string
	Language  string
	Prompt    string
}

type Result struct {
	Text       string
	Language   string
	Duration   float64
	Confidence float64 // 0 means that the provider did not report confidence.
	Provider   string
	Model      string
}

type Transcriber interface {
	Transcribe(context.Context, Audio) (Result, error)
}

type Disabled struct{}

func NewDisabled() Disabled { return Disabled{} }

func (Disabled) Transcribe(context.Context, Audio) (Result, error) {
	return Result{}, ErrDisabled
}
