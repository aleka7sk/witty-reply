package store

import (
	"context"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

type Store interface {
	Ping(context.Context) error
	Close()
	EnqueueUpdate(context.Context, domain.UpdateJob) (bool, error)
	ClaimUpdate(context.Context, string, time.Time, time.Duration) (domain.UpdateJob, error)
	RenewUpdate(context.Context, int64, string, time.Time, time.Duration) error
	CompleteUpdate(context.Context, int64, string, time.Time) error
	RetryUpdate(context.Context, int64, string, time.Time, time.Time, string, int) (bool, error)
	QueueStats(context.Context, time.Time) (int64, time.Duration, error)
	UpsertUser(context.Context, domain.User) (domain.User, error)
	GetUser(context.Context, int64) (domain.User, error)
	SetConsent(context.Context, int64, bool) error
	SetDefaultTone(context.Context, int64, domain.Tone) error
	ConsumeQuota(context.Context, int64, int64, domain.QuotaCategory, int, time.Time) (domain.QuotaDecision, error)
	RefundQuota(context.Context, int64, int64, domain.QuotaCategory) error
	SaveGeneration(context.Context, domain.GenerationRecord) (int64, error)
	GetGeneration(context.Context, int64, int64) (domain.GenerationRecord, error)
	StartThreadBrief(context.Context, int64, int64, domain.ThreadVoice) (domain.ThreadBrief, bool, error)
	GetThreadBrief(context.Context, int64, int64) (domain.ThreadBrief, error)
	GetCurrentThreadBrief(context.Context, int64) (domain.ThreadBrief, error)
	SetThreadBriefObjective(context.Context, int64, int64, uint32, domain.ThreadObjective) (domain.ThreadBrief, error)
	SetThreadBriefMaterial(context.Context, int64, int64, uint32, int64, domain.ThreadMaterialKind, string) (domain.ThreadBrief, error)
	CreateThreadDraftForBrief(context.Context, int64, uint32, domain.ThreadDraft, *domain.ThreadMedia) (domain.ThreadDraft, bool, error)
	GetThreadDraftByGenerationUpdate(context.Context, int64, int64) (domain.ThreadDraft, error)
	CancelThreadBrief(context.Context, int64, int64, uint32) error
	CreateThreadDraft(context.Context, domain.ThreadDraft) (int64, error)
	CreateThreadDraftWithMedia(context.Context, domain.ThreadDraft, domain.ThreadMedia) (int64, error)
	GetThreadDraft(context.Context, int64, int64) (domain.ThreadDraft, error)
	GetCurrentThreadDraft(context.Context, int64) (domain.ThreadDraft, error)
	SetThreadDraftMediaMode(context.Context, int64, int64, uint32, domain.ThreadMediaMode) (domain.ThreadDraft, error)
	AttachThreadDraftMedia(context.Context, int64, int64, uint32, domain.ThreadMedia) (domain.ThreadDraft, error)
	AttachLicensedThreadDraftMedia(context.Context, int64, int64, uint32, domain.ThreadMedia) (domain.ThreadDraft, error)
	GetThreadMedia(context.Context, int64, int64) (domain.ThreadMedia, error)
	GetThreadMediaByDeliveryKey(context.Context, string) (domain.ThreadMedia, error)
	GetThreadDraftByMediaUpdate(context.Context, int64, int64) (domain.ThreadDraft, error)
	GetThreadDraftByMediaAttachUpdate(context.Context, int64, int64) (domain.ThreadDraft, error)
	ListRecentThreadTexts(context.Context, int64, int) ([]string, error)
	ClaimThreadDraft(context.Context, int64, int64, uint32, string, time.Time, time.Duration) (domain.ThreadDraft, bool, error)
	SetThreadContainer(context.Context, int64, int64, string, string) error
	BeginThreadPublish(context.Context, int64, int64, string, time.Time, time.Duration) error
	CompleteThreadDraft(context.Context, int64, int64, string, string, string) error
	ConfirmThreadDraftPublished(context.Context, int64, int64, string) error
	FailThreadDraft(context.Context, int64, int64, string, string, bool) error
	CancelThreadDraft(context.Context, int64, int64, uint32) error
	RecordFeedback(context.Context, domain.Feedback) error
	ListStyleExamples(context.Context, int64, int) ([]string, error)
	SaveStyleExample(context.Context, int64, int64, string, int) (bool, error)
	ResetStyle(context.Context, int64) error
	Stats(context.Context, int64, time.Time, int) (domain.UserStats, error)
	DeleteUser(context.Context, int64) error
	Cleanup(context.Context, time.Time) (int64, error)
}

var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)
