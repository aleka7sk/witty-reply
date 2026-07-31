package session

import (
	"errors"
	"testing"
	"time"
)

func TestCallbackRoundTripOwnershipAndSize(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	codec, err := NewCallbackCodec([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	codec = codec.WithClockForTest(func() time.Time { return now })
	want := CallbackPayload{
		Action: ActionFeedbackUp, UserID: 8_999_123_456,
		InteractionID: 9_223_372_036, Revision: 77, Candidate: 2,
	}
	encoded, err := codec.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 64 {
		t.Fatalf("callback_data length = %d", len(encoded))
	}
	got, err := codec.DecodeForUser(encoded, want.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != want.Action || got.UserID != want.UserID || got.InteractionID != want.InteractionID || got.Revision != want.Revision || got.Candidate != want.Candidate {
		t.Fatalf("decoded payload = %+v, want %+v", got, want)
	}
	if _, err := codec.DecodeForUser(encoded, want.UserID+1); !errors.Is(err, ErrCallbackOwner) {
		t.Fatalf("owner error = %v", err)
	}
}

func TestCallbackRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	codec, err := NewCallbackCodec([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	codec = codec.WithClockForTest(func() time.Time { return now })
	encoded, err := codec.Encode(CallbackPayload{
		Action: ActionMore, UserID: 1, InteractionID: 2,
		Candidate: -1, IssuedAt: now.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(encoded); !errors.Is(err, ErrCallbackExpiry) {
		t.Fatalf("expiry error = %v", err)
	}

	valid, err := codec.Encode(CallbackPayload{Action: ActionMore, UserID: 1, InteractionID: 2, Candidate: -1})
	if err != nil {
		t.Fatal(err)
	}
	tampered := []byte(valid)
	if tampered[len(tampered)-1] == 'A' {
		tampered[len(tampered)-1] = 'B'
	} else {
		tampered[len(tampered)-1] = 'A'
	}
	if _, err := codec.Decode(string(tampered)); !errors.Is(err, ErrCallbackMAC) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestCallbackSupportsConsentAndStyleActions(t *testing.T) {
	codec, err := NewCallbackCodec([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []Action{ActionConsent, ActionSaveStyle, ActionResetStyle} {
		encoded, err := codec.Encode(CallbackPayload{
			Action: action, UserID: 101, InteractionID: 101, Candidate: -1,
		})
		if err != nil {
			t.Fatalf("Encode(%d): %v", action, err)
		}
		decoded, err := codec.DecodeForUser(encoded, 101)
		if err != nil {
			t.Fatalf("DecodeForUser(%d): %v", action, err)
		}
		if decoded.Action != action || decoded.InteractionID != 101 {
			t.Fatalf("decoded = %+v, action = %d", decoded, action)
		}
	}
}

func TestActionNumericStability(t *testing.T) {
	actions := []Action{
		ActionFeedbackUp, ActionFeedbackDown, ActionToneSmart, ActionTonePlayful,
		ActionToneSharp, ActionToneBoundary, ActionMeme, ActionFunnier,
		ActionSharper, ActionSofter, ActionShorter, ActionMore, ActionRetry,
		ActionCancel, ActionConfirmDelete, ActionCancelDelete, ActionConsent,
		ActionSaveStyle, ActionResetStyle,
	}
	for index, action := range actions {
		if want := Action(index + 1); action != want {
			t.Fatalf("action at index %d = %d, want stable value %d", index, action, want)
		}
	}
}
