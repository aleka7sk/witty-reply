package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

const maxThreadClaimTokenRunes = 128

func normalizeLegacyThreadDraft(draft domain.ThreadDraft) domain.ThreadDraft {
	if draft.Objective == "" {
		draft.Objective = domain.ThreadObjectiveLegacy
	}
	if draft.ScenarioID == "" {
		draft.ScenarioID = "legacy_unspecified"
	}
	return draft
}

func validateThreadMaterialChoice(updateID int64, kind domain.ThreadMaterialKind, value string) error {
	if updateID <= 0 || !kind.Valid() || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') ||
		utf8.RuneCountInString(value) > domain.MaxThreadMaterialRunes {
		return ErrThreadBriefState
	}
	for _, character := range value {
		if (unicode.IsControl(character) && character != '\n' && character != '\t') || unicode.In(character, unicode.Cf) {
			return ErrThreadBriefState
		}
	}
	switch kind {
	case domain.ThreadMaterialNone:
		if value != "" {
			return ErrThreadBriefState
		}
	case domain.ThreadMaterialText:
		if strings.TrimSpace(value) == "" {
			return ErrThreadBriefState
		}
	default:
		return ErrThreadBriefState
	}
	return nil
}

func validateThreadField(name, value string, maxRunes int, required bool) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("thread draft %s is not valid UTF-8", name)
	}
	count := utf8.RuneCountInString(value)
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("thread draft %s is required", name)
	}
	if count > maxRunes {
		return fmt.Errorf("thread draft %s exceeds %d characters", name, maxRunes)
	}
	return nil
}

func normalizeThreadErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "invalid_error_code"
	}
	runes := []rune(value)
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}

func validateThreadClaim(token string, now time.Time, lease time.Duration) error {
	if err := validateThreadField("claim token", token, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("thread draft claim time is required")
	}
	if lease <= 0 {
		return fmt.Errorf("thread draft claim lease must be positive")
	}
	return nil
}

func sortThreadDraftsNewestFirst(drafts []domain.ThreadDraft) {
	sort.Slice(drafts, func(left, right int) bool {
		if drafts[left].CreatedAt.Equal(drafts[right].CreatedAt) {
			return drafts[left].ID > drafts[right].ID
		}
		return drafts[left].CreatedAt.After(drafts[right].CreatedAt)
	})
}
