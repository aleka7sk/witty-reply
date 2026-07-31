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
	RecordFeedback(context.Context, domain.Feedback) error
	ListStyleExamples(context.Context, int64, int) ([]string, error)
	SaveStyleExample(context.Context, int64, int64, string, int) (bool, error)
	ResetStyle(context.Context, int64) error
	Stats(context.Context, int64, time.Time, int) (domain.UserStats, error)
	DeleteUser(context.Context, int64) error
	Cleanup(context.Context, time.Time) (int64, error)
}
