package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

type memoryUsageKey struct {
	UserID   int64
	Day      string
	Category domain.QuotaCategory
}

type memoryQuotaReservationKey struct {
	UserID        int64
	ReservationID int64
	Category      domain.QuotaCategory
}

type memoryQuotaReservation struct {
	state     string
	usageKey  memoryUsageKey
	decision  domain.QuotaDecision
	updatedAt time.Time
}

type Memory struct {
	mu           sync.RWMutex
	users        map[int64]domain.User
	updates      map[int64]domain.UpdateJob
	usage        map[memoryUsageKey]int
	reservations map[memoryQuotaReservationKey]memoryQuotaReservation
	generations  map[int64]domain.GenerationRecord
	threadDrafts map[int64]domain.ThreadDraft
	threadMedia  map[int64]domain.ThreadMedia
	feedback     []domain.Feedback
	examples     map[int64][]string
	nextID       int64
	now          func() time.Time
}

func NewMemory() *Memory {
	return &Memory{
		users: make(map[int64]domain.User), updates: make(map[int64]domain.UpdateJob), usage: make(map[memoryUsageKey]int),
		reservations: make(map[memoryQuotaReservationKey]memoryQuotaReservation),
		generations:  make(map[int64]domain.GenerationRecord), threadDrafts: make(map[int64]domain.ThreadDraft),
		threadMedia: make(map[int64]domain.ThreadMedia),
		examples:    make(map[int64][]string), nextID: 1, now: time.Now,
	}
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Close()                     {}

func (m *Memory) EnqueueUpdate(_ context.Context, job domain.UpdateJob) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.updates[job.UpdateID]; exists {
		return false, nil
	}
	now := m.now().UTC()
	job.Payload = append([]byte(nil), job.Payload...)
	job.Status = domain.UpdateJobPending
	job.Attempts = 0
	job.AvailableAt = now
	job.EnqueuedAt = now
	job.LeaseUntil = time.Time{}
	job.LeaseToken = ""
	m.updates[job.UpdateID] = job
	if job.Superseding {
		for updateID, older := range m.updates {
			if older.ActorID != job.ActorID || older.UpdateID >= job.UpdateID || !older.Supersedable {
				continue
			}
			if older.Status != domain.UpdateJobPending && older.Status != domain.UpdateJobProcessing {
				continue
			}
			older.Status = domain.UpdateJobSuperseded
			older.Payload = nil
			older.AvailableAt = now
			older.LeaseUntil = time.Time{}
			older.LeaseToken = ""
			m.updates[updateID] = older
		}
	}
	return true, nil
}

func (m *Memory) ClaimUpdate(_ context.Context, leaseToken string, now time.Time, lease time.Duration) (domain.UpdateJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var selected domain.UpdateJob
	found := false
	for _, candidate := range m.updates {
		eligible := candidate.Status == domain.UpdateJobPending && !candidate.AvailableAt.After(now)
		eligible = eligible || candidate.Status == domain.UpdateJobProcessing && !candidate.LeaseUntil.After(now)
		if !eligible || m.hasEarlierActiveJob(candidate) {
			continue
		}
		if !found || updateJobLess(candidate, selected) {
			selected = candidate
			found = true
		}
	}
	if !found {
		return domain.UpdateJob{}, ErrNotFound
	}
	selected.Status = domain.UpdateJobProcessing
	selected.Attempts++
	selected.LeaseToken = leaseToken
	selected.LeaseUntil = now.Add(lease)
	m.updates[selected.UpdateID] = selected
	selected.Payload = append([]byte(nil), selected.Payload...)
	return selected, nil
}

func (m *Memory) RenewUpdate(_ context.Context, updateID int64, leaseToken string, now time.Time, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, exists := m.updates[updateID]
	if !exists {
		// DeleteUser intentionally removes the command currently deleting the
		// actor. Treat its missing row as an idempotent finalization success.
		return nil
	}
	if job.Status == domain.UpdateJobSuperseded {
		return ErrSuperseded
	}
	if job.Status != domain.UpdateJobProcessing || job.LeaseToken != leaseToken || !job.LeaseUntil.After(now) {
		return ErrLeaseLost
	}
	job.LeaseUntil = now.Add(lease)
	m.updates[updateID] = job
	return nil
}

func (m *Memory) CompleteUpdate(_ context.Context, updateID int64, leaseToken string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, exists := m.updates[updateID]
	if !exists {
		return nil
	}
	if job.Status == domain.UpdateJobSuperseded {
		return ErrSuperseded
	}
	if job.Status != domain.UpdateJobProcessing || job.LeaseToken != leaseToken {
		return ErrLeaseLost
	}
	job.Status = domain.UpdateJobCompleted
	job.Payload = nil
	job.AvailableAt = now
	job.LeaseUntil = time.Time{}
	job.LeaseToken = ""
	m.updates[updateID] = job
	return nil
}

func (m *Memory) RetryUpdate(_ context.Context, updateID int64, leaseToken string, now, retryAt time.Time, _ string, maxAttempts int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, exists := m.updates[updateID]
	if !exists {
		return false, nil
	}
	if job.Status == domain.UpdateJobSuperseded {
		return false, ErrSuperseded
	}
	if job.Status != domain.UpdateJobProcessing || job.LeaseToken != leaseToken {
		return false, ErrLeaseLost
	}
	dead := job.Attempts >= maxAttempts
	job.LeaseUntil = time.Time{}
	job.LeaseToken = ""
	if dead {
		job.Status = domain.UpdateJobDead
		job.Payload = nil
		job.AvailableAt = now
	} else {
		job.Status = domain.UpdateJobPending
		job.AvailableAt = retryAt
	}
	m.updates[updateID] = job
	return dead, nil
}

func (m *Memory) QueueStats(_ context.Context, now time.Time) (int64, time.Duration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var pending int64
	var oldest time.Time
	for _, job := range m.updates {
		if job.Status != domain.UpdateJobPending && job.Status != domain.UpdateJobProcessing {
			continue
		}
		pending++
		if oldest.IsZero() || job.EnqueuedAt.Before(oldest) {
			oldest = job.EnqueuedAt
		}
	}
	if oldest.IsZero() || now.Before(oldest) {
		return pending, 0, nil
	}
	return pending, now.Sub(oldest), nil
}

func (m *Memory) hasEarlierActiveJob(candidate domain.UpdateJob) bool {
	for _, other := range m.updates {
		if other.ActorID != candidate.ActorID || other.UpdateID == candidate.UpdateID {
			continue
		}
		if other.Status != domain.UpdateJobPending && other.Status != domain.UpdateJobProcessing {
			continue
		}
		if updateJobLess(other, candidate) {
			return true
		}
	}
	return false
}

func updateJobLess(left, right domain.UpdateJob) bool {
	return left.UpdateID < right.UpdateID
}

func (m *Memory) UpsertUser(_ context.Context, user domain.User) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	current, exists := m.users[user.TelegramID]
	if exists {
		if user.Language != "" {
			current.Language = user.Language
		}
		current.UpdatedAt = now
		m.users[user.TelegramID] = current
		return current, nil
	}
	if user.DefaultTone == "" {
		user.DefaultTone = domain.ToneMix
	}
	user.CreatedAt = now
	user.UpdatedAt = now
	m.users[user.TelegramID] = user
	return user, nil
}

func (m *Memory) GetUser(_ context.Context, telegramID int64) (domain.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.users[telegramID]
	if !ok {
		return domain.User{}, ErrNotFound
	}
	return user, nil
}

func (m *Memory) SetConsent(_ context.Context, telegramID int64, consent bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[telegramID]
	if !ok {
		return ErrNotFound
	}
	if consent {
		now := m.now().UTC()
		user.ConsentedAt = &now
	} else {
		user.ConsentedAt = nil
	}
	user.UpdatedAt = m.now().UTC()
	m.users[telegramID] = user
	return nil
}

func (m *Memory) SetDefaultTone(_ context.Context, telegramID int64, tone domain.Tone) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[telegramID]
	if !ok {
		return ErrNotFound
	}
	user.DefaultTone = tone
	user.UpdatedAt = m.now().UTC()
	m.users[telegramID] = user
	return nil
}

func (m *Memory) ConsumeQuota(_ context.Context, telegramID, reservationID int64, category domain.QuotaCategory, limit int, now time.Time) (domain.QuotaDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[telegramID]; !ok {
		return domain.QuotaDecision{}, ErrNotFound
	}
	if reservationID <= 0 || category == "" || limit < 1 {
		return domain.QuotaDecision{}, fmt.Errorf("invalid quota reservation")
	}
	reservationKey := memoryQuotaReservationKey{UserID: telegramID, ReservationID: reservationID, Category: category}
	if existing, ok := m.reservations[reservationKey]; ok {
		switch existing.state {
		case "charged", "denied":
			return existing.decision, nil
		case "refunded":
			// A failed attempt released its slot. Re-evaluate the retry against
			// the current quota window, then persist that new decision.
		default:
			return domain.QuotaDecision{}, fmt.Errorf("invalid quota reservation state %q", existing.state)
		}
	}
	day := now.Format("2006-01-02")
	usageKey := memoryUsageKey{UserID: telegramID, Day: day, Category: category}
	used := m.usage[usageKey]
	decision := domain.QuotaDecision{Allowed: used < limit, Used: used, Limit: limit, ResetsAt: nextDay(now), Plan: "free"}
	state := "denied"
	if decision.Allowed {
		used++
		m.usage[usageKey] = used
		decision.Used = used
		state = "charged"
	}
	m.reservations[reservationKey] = memoryQuotaReservation{
		state: state, usageKey: usageKey, decision: decision, updatedAt: now,
	}
	return decision, nil
}

func (m *Memory) RefundQuota(_ context.Context, telegramID, reservationID int64, category domain.QuotaCategory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reservationID <= 0 || category == "" {
		return fmt.Errorf("invalid quota reservation")
	}
	key := memoryQuotaReservationKey{UserID: telegramID, ReservationID: reservationID, Category: category}
	reservation, ok := m.reservations[key]
	if !ok || reservation.state == "refunded" || reservation.state == "denied" {
		return nil
	}
	if reservation.state != "charged" {
		return fmt.Errorf("invalid quota reservation state %q", reservation.state)
	}
	if m.usage[reservation.usageKey] < 1 {
		return fmt.Errorf("charged quota reservation has no usage slot")
	}
	m.usage[reservation.usageKey]--
	reservation.state = "refunded"
	reservation.updatedAt = m.now().UTC()
	m.reservations[key] = reservation
	return nil
}

func (m *Memory) SaveGeneration(_ context.Context, record domain.GenerationRecord) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[record.TelegramID]; !ok {
		return 0, ErrNotFound
	}
	record.ID = m.nextID
	m.nextID++
	if record.CreatedAt.IsZero() {
		record.CreatedAt = m.now().UTC()
	}
	m.generations[record.ID] = record
	return record.ID, nil
}

func (m *Memory) GetGeneration(_ context.Context, id, telegramID int64) (domain.GenerationRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.generations[id]
	if !ok || record.TelegramID != telegramID {
		return domain.GenerationRecord{}, ErrNotFound
	}
	return record, nil
}

func (m *Memory) CreateThreadDraft(_ context.Context, draft domain.ThreadDraft) (int64, error) {
	if err := draft.ValidateForCreate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[draft.TelegramID]; !ok {
		return 0, ErrNotFound
	}
	if draft.MediaMode == "" {
		draft.MediaMode = domain.ThreadMediaText
	}
	if draft.MediaID > 0 {
		mediaValue, ok := m.threadMedia[draft.MediaID]
		if !ok || mediaValue.TelegramID != draft.TelegramID {
			return 0, ErrNotFound
		}
	}
	for id, previous := range m.threadDrafts {
		if previous.TelegramID == draft.TelegramID && previous.Current {
			previous.Current = false
			previous.UpdatedAt = m.now().UTC()
			m.threadDrafts[id] = previous
		}
	}
	now := m.now().UTC()
	draft.ID = m.nextID
	m.nextID++
	draft.State = domain.ThreadDraftReady
	draft.Current = true
	draft.ContainerID = ""
	draft.PostID = ""
	draft.Permalink = ""
	draft.ErrorCode = ""
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	draft.PublishStartedAt = nil
	draft.MediaRightsConfirmedAt = nil
	draft.CreatedAt = now
	draft.UpdatedAt = now
	draft.PublishedAt = nil
	m.threadDrafts[draft.ID] = draft
	return draft.ID, nil
}

func (m *Memory) CreateThreadDraftWithMedia(_ context.Context, draft domain.ThreadDraft, mediaValue domain.ThreadMedia) (int64, error) {
	if draft.MediaMode != domain.ThreadMediaImage || draft.MediaID != 0 || mediaValue.TelegramID != draft.TelegramID {
		return 0, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return 0, err
	}
	validationDraft := draft
	validationDraft.MediaID = 1
	if err := validationDraft.ValidateForCreate(); err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[draft.TelegramID]; !ok {
		return 0, ErrNotFound
	}
	for _, existing := range m.threadMedia {
		if existing.DeliveryKey == mediaValue.DeliveryKey {
			return 0, errors.New("thread media delivery key already exists")
		}
		if mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourceTelegram &&
			existing.TelegramID == mediaValue.TelegramID && existing.SourceUpdateID == mediaValue.SourceUpdateID {
			return 0, ErrThreadDraftState
		}
	}

	now := m.now().UTC()
	mediaValue.ID = m.nextID
	m.nextID++
	mediaValue.SourceKind = mediaValue.EffectiveSourceKind()
	mediaValue.Data = append([]byte(nil), mediaValue.Data...)
	mediaValue.CreatedAt = now
	m.threadMedia[mediaValue.ID] = mediaValue

	for id, previous := range m.threadDrafts {
		if previous.TelegramID == draft.TelegramID && previous.Current {
			previous.Current = false
			previous.UpdatedAt = now
			m.threadDrafts[id] = previous
		}
	}
	draft.ID = m.nextID
	m.nextID++
	draft.MediaID = mediaValue.ID
	draft.State = domain.ThreadDraftReady
	draft.Current = true
	draft.ContainerID = ""
	draft.PostID = ""
	draft.Permalink = ""
	draft.ErrorCode = ""
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	draft.PublishStartedAt = nil
	draft.MediaRightsConfirmedAt = nil
	draft.CreatedAt = now
	draft.UpdatedAt = now
	draft.PublishedAt = nil
	m.threadDrafts[draft.ID] = draft
	return draft.ID, nil
}

func (m *Memory) GetThreadDraft(_ context.Context, id, telegramID int64) (domain.ThreadDraft, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	return draft, nil
}

func (m *Memory) GetCurrentThreadDraft(_ context.Context, telegramID int64) (domain.ThreadDraft, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, draft := range m.threadDrafts {
		if draft.TelegramID == telegramID && draft.Current {
			return draft, nil
		}
	}
	return domain.ThreadDraft{}, ErrNotFound
}

func (m *Memory) SetThreadDraftMediaMode(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	mode domain.ThreadMediaMode,
) (domain.ThreadDraft, error) {
	if mode != domain.ThreadMediaText && mode != domain.ThreadMediaImagePending && mode != domain.ThreadMediaImage {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if !draft.Current || draft.Revision != revision ||
		(draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) ||
		draft.Revision == ^uint32(0) {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if mode == domain.ThreadMediaImage && (draft.MediaMode != domain.ThreadMediaImagePending || draft.MediaID <= 0) {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	oldMediaID := draft.MediaID
	draft.Revision++
	draft.MediaMode = mode
	if mode == domain.ThreadMediaText {
		draft.MediaID = 0
	}
	resetThreadDraftForPreview(&draft)
	draft.UpdatedAt = m.now().UTC()
	m.threadDrafts[id] = draft
	if draft.MediaID != oldMediaID {
		m.deleteOrphanThreadMediaLocked(oldMediaID)
	}
	return draft, nil
}

func (m *Memory) AttachThreadDraftMedia(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	mediaValue domain.ThreadMedia,
) (domain.ThreadDraft, error) {
	if mediaValue.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourceTelegram {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadDraft{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.threadMedia {
		if mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourceTelegram &&
			existing.TelegramID == telegramID && existing.SourceUpdateID == mediaValue.SourceUpdateID {
			existingDraft, exists := m.threadDrafts[id]
			if exists && existingDraft.TelegramID == telegramID && existingDraft.MediaID == existing.ID {
				return existingDraft, nil
			}
			return domain.ThreadDraft{}, ErrThreadDraftState
		}
	}
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if !draft.Current || draft.Revision != revision || draft.MediaMode != domain.ThreadMediaImagePending ||
		(draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) ||
		draft.Revision == ^uint32(0) {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	for _, existing := range m.threadMedia {
		if existing.DeliveryKey == mediaValue.DeliveryKey {
			return domain.ThreadDraft{}, errors.New("thread media delivery key already exists")
		}
	}
	oldMediaID := draft.MediaID
	now := m.now().UTC()
	mediaValue.ID = m.nextID
	m.nextID++
	mediaValue.SourceKind = mediaValue.EffectiveSourceKind()
	mediaValue.Data = append([]byte(nil), mediaValue.Data...)
	mediaValue.CreatedAt = now
	m.threadMedia[mediaValue.ID] = mediaValue

	draft.Revision++
	draft.MediaMode = domain.ThreadMediaImage
	draft.MediaID = mediaValue.ID
	resetThreadDraftForPreview(&draft)
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	m.deleteOrphanThreadMediaLocked(oldMediaID)
	return draft, nil
}

func (m *Memory) GetThreadMedia(_ context.Context, id, telegramID int64) (domain.ThreadMedia, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mediaValue, ok := m.threadMedia[id]
	if !ok || mediaValue.TelegramID != telegramID {
		return domain.ThreadMedia{}, ErrNotFound
	}
	mediaValue.Data = append([]byte(nil), mediaValue.Data...)
	return mediaValue, nil
}

func (m *Memory) GetThreadMediaByDeliveryKey(_ context.Context, deliveryKey string) (domain.ThreadMedia, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mediaValue := range m.threadMedia {
		if mediaValue.DeliveryKey == deliveryKey {
			mediaValue.Data = append([]byte(nil), mediaValue.Data...)
			return mediaValue, nil
		}
	}
	return domain.ThreadMedia{}, ErrNotFound
}

func (m *Memory) GetThreadDraftByMediaUpdate(_ context.Context, telegramID, sourceUpdateID int64) (domain.ThreadDraft, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var mediaID int64
	for id, mediaValue := range m.threadMedia {
		if mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourceTelegram &&
			mediaValue.TelegramID == telegramID && mediaValue.SourceUpdateID == sourceUpdateID {
			mediaID = id
			break
		}
	}
	if mediaID == 0 {
		return domain.ThreadDraft{}, ErrNotFound
	}
	for _, draft := range m.threadDrafts {
		if draft.TelegramID == telegramID && draft.MediaID == mediaID {
			return draft, nil
		}
	}
	return domain.ThreadDraft{}, ErrNotFound
}

func resetThreadDraftForPreview(draft *domain.ThreadDraft) {
	draft.State = domain.ThreadDraftReady
	draft.ContainerID = ""
	draft.PostID = ""
	draft.Permalink = ""
	draft.ErrorCode = ""
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	draft.PublishStartedAt = nil
	draft.MediaRightsConfirmedAt = nil
	draft.PublishedAt = nil
}

func (m *Memory) deleteOrphanThreadMediaLocked(id int64) {
	if id <= 0 {
		return
	}
	for _, draft := range m.threadDrafts {
		if draft.MediaID == id {
			return
		}
	}
	delete(m.threadMedia, id)
}

func (m *Memory) ListRecentThreadTexts(_ context.Context, telegramID int64, limit int) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	drafts := make([]domain.ThreadDraft, 0)
	for _, draft := range m.threadDrafts {
		if draft.TelegramID == telegramID {
			drafts = append(drafts, draft)
		}
	}
	sortThreadDraftsNewestFirst(drafts)
	if len(drafts) > limit {
		drafts = drafts[:limit]
	}
	texts := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		texts = append(texts, draft.Text)
	}
	return texts, nil
}

func (m *Memory) ClaimThreadDraft(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	claimToken string,
	now time.Time,
	lease time.Duration,
) (domain.ThreadDraft, bool, error) {
	if err := validateThreadClaim(claimToken, now, lease); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	if !draft.Current || draft.Revision != revision {
		return draft, false, nil
	}
	if draft.MediaMode == domain.ThreadMediaImagePending ||
		(draft.MediaMode == domain.ThreadMediaImage && draft.MediaID <= 0) {
		return draft, false, nil
	}
	now = now.UTC()
	if draft.State == domain.ThreadDraftPublishing {
		expired := draft.ClaimExpiresAt == nil || !draft.ClaimExpiresAt.After(now)
		if !expired {
			return draft, false, nil
		}
		if draft.PublishStartedAt != nil {
			draft.State = domain.ThreadDraftUnknown
			draft.ErrorCode = "publish_lease_expired"
			draft.ClaimToken = ""
			draft.ClaimExpiresAt = nil
			draft.UpdatedAt = now
			m.threadDrafts[id] = draft
			return draft, false, nil
		}
	} else if draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed {
		return draft, false, nil
	}
	expiresAt := now.Add(lease)
	draft.State = domain.ThreadDraftPublishing
	draft.ErrorCode = ""
	draft.ClaimToken = claimToken
	draft.ClaimExpiresAt = &expiresAt
	draft.PublishStartedAt = nil
	if draft.MediaMode == domain.ThreadMediaImage {
		confirmedAt := now
		draft.MediaRightsConfirmedAt = &confirmedAt
	} else {
		draft.MediaRightsConfirmedAt = nil
	}
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	return draft, true, nil
}

func (m *Memory) SetThreadContainer(_ context.Context, id, telegramID int64, claimToken, containerID string) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	if err := validateThreadField("container id", containerID, 255, true); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	now := m.now().UTC()
	if draft.State != domain.ThreadDraftPublishing || draft.ClaimToken != claimToken ||
		draft.PublishStartedAt != nil || draft.ClaimExpiresAt == nil || !draft.ClaimExpiresAt.After(now) ||
		(draft.ContainerID != "" && draft.ContainerID != containerID) {
		return ErrThreadDraftState
	}
	draft.ContainerID = containerID
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) BeginThreadPublish(
	_ context.Context,
	id, telegramID int64,
	claimToken string,
	now time.Time,
	lease time.Duration,
) error {
	if err := validateThreadClaim(claimToken, now, lease); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	now = now.UTC()
	if draft.State != domain.ThreadDraftPublishing || draft.ClaimToken != claimToken ||
		draft.ContainerID == "" || draft.PublishStartedAt != nil ||
		draft.ClaimExpiresAt == nil || !draft.ClaimExpiresAt.After(now) {
		return ErrThreadDraftState
	}
	expiresAt := now.Add(lease)
	draft.PublishStartedAt = &now
	draft.ClaimExpiresAt = &expiresAt
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) CompleteThreadDraft(_ context.Context, id, telegramID int64, claimToken, postID, permalink string) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	if err := validateThreadField("post id", postID, 255, false); err != nil {
		return err
	}
	if err := validateThreadField("permalink", permalink, 2048, false); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	if draft.State == domain.ThreadDraftPublished && draft.PostID == postID && draft.Permalink == permalink {
		return nil
	}
	if draft.State != domain.ThreadDraftPublishing || draft.ClaimToken != claimToken || draft.PublishStartedAt == nil {
		return ErrThreadDraftState
	}
	now := m.now().UTC()
	draft.State = domain.ThreadDraftPublished
	draft.PostID = postID
	draft.Permalink = permalink
	draft.ErrorCode = ""
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	draft.UpdatedAt = now
	draft.PublishedAt = &now
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) ConfirmThreadDraftPublished(_ context.Context, id, telegramID int64, containerID string) error {
	if err := validateThreadField("container id", containerID, 255, true); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	if draft.State == domain.ThreadDraftPublished && draft.ContainerID == containerID {
		return nil
	}
	if draft.ContainerID != containerID ||
		(draft.State != domain.ThreadDraftUnknown &&
			(draft.State != domain.ThreadDraftPublishing || draft.PublishStartedAt == nil)) {
		return ErrThreadDraftState
	}
	now := m.now().UTC()
	draft.State = domain.ThreadDraftPublished
	draft.ErrorCode = ""
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	draft.UpdatedAt = now
	draft.PublishedAt = &now
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) FailThreadDraft(_ context.Context, id, telegramID int64, claimToken, errorCode string, unknown bool) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	errorCode = normalizeThreadErrorCode(errorCode)
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	target := domain.ThreadDraftFailed
	if unknown {
		target = domain.ThreadDraftUnknown
	}
	if draft.State == target && draft.ErrorCode == errorCode {
		return nil
	}
	if draft.State != domain.ThreadDraftPublishing || draft.ClaimToken != claimToken || (unknown && draft.PublishStartedAt == nil) {
		return ErrThreadDraftState
	}
	draft.State = target
	draft.ErrorCode = errorCode
	draft.ClaimToken = ""
	draft.ClaimExpiresAt = nil
	if !unknown {
		draft.PublishStartedAt = nil
		draft.ContainerID = ""
	}
	draft.UpdatedAt = m.now().UTC()
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) CancelThreadDraft(_ context.Context, id, telegramID int64, revision uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return ErrNotFound
	}
	if !draft.Current || draft.Revision != revision || (draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) {
		return ErrNotFound
	}
	draft.State = domain.ThreadDraftCancelled
	draft.Current = false
	draft.ErrorCode = ""
	draft.UpdatedAt = m.now().UTC()
	m.threadDrafts[id] = draft
	return nil
}

func (m *Memory) RecordFeedback(_ context.Context, feedback domain.Feedback) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.generations[feedback.GenerationID]
	if !ok || record.TelegramID != feedback.TelegramID {
		return ErrNotFound
	}
	for index, existing := range m.feedback {
		if existing.GenerationID == feedback.GenerationID && existing.TelegramID == feedback.TelegramID && existing.Candidate == feedback.Candidate && existing.Action == feedback.Action {
			m.feedback[index] = feedback
			return nil
		}
	}
	m.feedback = append(m.feedback, feedback)
	return nil
}

func (m *Memory) ListStyleExamples(_ context.Context, telegramID int64, limit int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := m.examples[telegramID]
	if limit > 0 && len(values) > limit {
		values = values[len(values)-limit:]
	}
	return append([]string(nil), values...), nil
}

func (m *Memory) SaveStyleExample(_ context.Context, telegramID, generationID int64, text string, limit int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.generations[generationID]
	if !ok || record.TelegramID != telegramID {
		return false, ErrNotFound
	}
	for _, existing := range m.examples[telegramID] {
		if existing == text {
			return true, nil
		}
	}
	if limit < 1 || len(m.examples[telegramID]) >= limit {
		return false, nil
	}
	m.examples[telegramID] = append(m.examples[telegramID], text)
	return true, nil
}

func (m *Memory) ResetStyle(_ context.Context, telegramID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.examples, telegramID)
	user, ok := m.users[telegramID]
	if !ok {
		return ErrNotFound
	}
	user.DefaultTone = domain.ToneMix
	user.UpdatedAt = m.now().UTC()
	m.users[telegramID] = user
	return nil
}

func (m *Memory) Stats(_ context.Context, telegramID int64, now time.Time, defaultLimit int) (domain.UserStats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.users[telegramID]; !ok {
		return domain.UserStats{}, ErrNotFound
	}
	day := now.Format("2006-01-02")
	used := 0
	for key, value := range m.usage {
		if key.UserID == telegramID && key.Day == day && key.Category == domain.QuotaText {
			used += value
		}
	}
	var total int64
	for _, generation := range m.generations {
		if generation.TelegramID == telegramID {
			total++
		}
	}
	return domain.UserStats{UsedToday: used, DailyLimit: defaultLimit, Total: total, Plan: "free"}, nil
}

func (m *Memory) DeleteUser(_ context.Context, telegramID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users, telegramID)
	delete(m.examples, telegramID)
	for updateID, job := range m.updates {
		if job.ActorID == telegramID {
			delete(m.updates, updateID)
		}
	}
	for key := range m.usage {
		if key.UserID == telegramID {
			delete(m.usage, key)
		}
	}
	for key := range m.reservations {
		if key.UserID == telegramID {
			delete(m.reservations, key)
		}
	}
	for id, record := range m.generations {
		if record.TelegramID == telegramID {
			delete(m.generations, id)
		}
	}
	for id, draft := range m.threadDrafts {
		if draft.TelegramID == telegramID {
			delete(m.threadDrafts, id)
		}
	}
	for id, mediaValue := range m.threadMedia {
		if mediaValue.TelegramID == telegramID {
			delete(m.threadMedia, id)
		}
	}
	filtered := m.feedback[:0]
	for _, item := range m.feedback {
		if item.TelegramID != telegramID {
			filtered = append(filtered, item)
		}
	}
	m.feedback = filtered
	return nil
}

func (m *Memory) Cleanup(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	var deleted int64
	for id, record := range m.generations {
		if record.CreatedAt.Before(before) {
			delete(m.generations, id)
			deleted++
		}
	}
	for id, draft := range m.threadDrafts {
		if draft.State == domain.ThreadDraftPublishing &&
			(draft.ClaimExpiresAt == nil || !draft.ClaimExpiresAt.After(now)) {
			if draft.PublishStartedAt == nil {
				draft.State = domain.ThreadDraftFailed
				draft.ErrorCode = "publish_lease_expired_before_attempt"
			} else {
				draft.State = domain.ThreadDraftUnknown
				draft.ErrorCode = "publish_lease_expired"
			}
			draft.ClaimToken = ""
			draft.ClaimExpiresAt = nil
			draft.UpdatedAt = now
			m.threadDrafts[id] = draft
		}
		if draft.UpdatedAt.Before(before) {
			delete(m.threadDrafts, id)
		}
	}
	for id := range m.threadMedia {
		m.deleteOrphanThreadMediaLocked(id)
	}
	for id, job := range m.updates {
		if (job.Status == domain.UpdateJobCompleted || job.Status == domain.UpdateJobDead || job.Status == domain.UpdateJobSuperseded) && job.AvailableAt.Before(before) {
			delete(m.updates, id)
		}
	}
	for key, reservation := range m.reservations {
		if !reservation.updatedAt.Before(before) {
			continue
		}
		job, exists := m.updates[key.ReservationID]
		if exists && job.ActorID == key.UserID && (job.Status == domain.UpdateJobPending || job.Status == domain.UpdateJobProcessing) {
			continue
		}
		delete(m.reservations, key)
	}
	return deleted, nil
}

func nextDay(now time.Time) time.Time {
	y, month, day := now.Date()
	return time.Date(y, month, day+1, 0, 0, 0, 0, now.Location())
}

func (m *Memory) String() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return fmt.Sprintf("memory store: %d users, %d generations, %d thread drafts", len(m.users), len(m.generations), len(m.threadDrafts))
}
