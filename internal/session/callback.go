package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	callbackVersion  = byte(1)
	callbackBodySize = 27
	callbackMACSize  = 12
	callbackMaxBytes = 64
)

var (
	ErrCallbackSecret = errors.New("callback secret must contain at least 16 bytes")
	ErrCallbackFormat = errors.New("invalid callback data")
	ErrCallbackMAC    = errors.New("invalid callback signature")
	ErrCallbackOwner  = errors.New("callback belongs to another user")
	ErrCallbackExpiry = errors.New("callback has expired")
	ErrCallbackFuture = errors.New("callback timestamp is in the future")
)

type Action uint8

const (
	ActionFeedbackUp Action = iota + 1
	ActionFeedbackDown
	ActionToneSmart
	ActionTonePlayful
	ActionToneSharp
	ActionToneBoundary
	ActionMeme
	ActionFunnier
	ActionSharper
	ActionSofter
	ActionShorter
	ActionMore
	ActionRetry
	ActionCancel
	ActionConfirmDelete
	ActionCancelDelete
	ActionConsent
	ActionSaveStyle
	ActionResetStyle
)

func (a Action) Valid() bool {
	return a >= ActionFeedbackUp && a <= ActionResetStyle
}

type CallbackPayload struct {
	Action        Action
	UserID        int64
	InteractionID int64
	Revision      uint32
	Candidate     int8 // -1 means that the action is not candidate-specific.
	IssuedAt      time.Time
}

type CallbackCodec struct {
	secret []byte
	maxAge time.Duration
	now    func() time.Time
}

func NewCallbackCodec(secret []byte, maxAge time.Duration) (*CallbackCodec, error) {
	if len(secret) < 16 {
		return nil, ErrCallbackSecret
	}
	secretCopy := append([]byte(nil), secret...)
	return &CallbackCodec{secret: secretCopy, maxAge: maxAge, now: time.Now}, nil
}

// WithClockForTest returns a copy using a deterministic clock.
func (c *CallbackCodec) WithClockForTest(now func() time.Time) *CallbackCodec {
	clone := *c
	if now != nil {
		clone.now = now
	}
	return &clone
}

func (c *CallbackCodec) Encode(payload CallbackPayload) (string, error) {
	if !payload.Action.Valid() || payload.UserID <= 0 || payload.InteractionID <= 0 || payload.Candidate < -1 {
		return "", ErrCallbackFormat
	}
	issuedAt := payload.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = c.now()
	}
	seconds := issuedAt.Unix()
	if seconds < 0 || seconds > int64(^uint32(0)) {
		return "", ErrCallbackFormat
	}

	body := make([]byte, callbackBodySize)
	body[0] = callbackVersion
	body[1] = byte(payload.Action)
	binary.BigEndian.PutUint64(body[2:10], uint64(payload.UserID))
	binary.BigEndian.PutUint64(body[10:18], uint64(payload.InteractionID))
	binary.BigEndian.PutUint32(body[18:22], payload.Revision)
	body[22] = byte(payload.Candidate)
	binary.BigEndian.PutUint32(body[23:27], uint32(seconds))

	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write(body)
	raw := append(body, mac.Sum(nil)[:callbackMACSize]...)
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > callbackMaxBytes {
		return "", fmt.Errorf("%w: encoded data exceeds %d bytes", ErrCallbackFormat, callbackMaxBytes)
	}
	return encoded, nil
}

func (c *CallbackCodec) Decode(encoded string) (CallbackPayload, error) {
	if encoded == "" || len(encoded) > callbackMaxBytes {
		return CallbackPayload{}, ErrCallbackFormat
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != callbackBodySize+callbackMACSize {
		return CallbackPayload{}, ErrCallbackFormat
	}
	body, suppliedMAC := raw[:callbackBodySize], raw[callbackBodySize:]
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write(body)
	expectedMAC := mac.Sum(nil)[:callbackMACSize]
	if subtle.ConstantTimeCompare(suppliedMAC, expectedMAC) != 1 {
		return CallbackPayload{}, ErrCallbackMAC
	}
	if body[0] != callbackVersion {
		return CallbackPayload{}, ErrCallbackFormat
	}

	payload := CallbackPayload{
		Action:        Action(body[1]),
		UserID:        int64(binary.BigEndian.Uint64(body[2:10])),
		InteractionID: int64(binary.BigEndian.Uint64(body[10:18])),
		Revision:      binary.BigEndian.Uint32(body[18:22]),
		Candidate:     int8(body[22]),
		IssuedAt:      time.Unix(int64(binary.BigEndian.Uint32(body[23:27])), 0).UTC(),
	}
	if !payload.Action.Valid() || payload.UserID <= 0 || payload.InteractionID <= 0 || payload.Candidate < -1 {
		return CallbackPayload{}, ErrCallbackFormat
	}
	now := c.now()
	if payload.IssuedAt.After(now.Add(2 * time.Minute)) {
		return CallbackPayload{}, ErrCallbackFuture
	}
	if c.maxAge > 0 && now.Sub(payload.IssuedAt) > c.maxAge {
		return CallbackPayload{}, ErrCallbackExpiry
	}
	return payload, nil
}

func (c *CallbackCodec) DecodeForUser(encoded string, userID int64) (CallbackPayload, error) {
	payload, err := c.Decode(encoded)
	if err != nil {
		return CallbackPayload{}, err
	}
	var sameOwner int
	left, right := uint64(payload.UserID), uint64(userID)
	for shift := 0; shift < 64; shift += 8 {
		sameOwner |= subtle.ConstantTimeByteEq(byte(left>>shift), byte(right>>shift)) ^ 1
	}
	if sameOwner != 0 {
		return CallbackPayload{}, ErrCallbackOwner
	}
	return payload, nil
}
