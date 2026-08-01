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

type memoryThreadMediaAttachKey struct {
	UserID   int64
	UpdateID int64
}

type memoryThreadMediaAttachOperation struct {
	DraftID int64
	MediaID int64
}

type Memory struct {
	mu                          sync.RWMutex
	users                       map[int64]domain.User
	updates                     map[int64]domain.UpdateJob
	usage                       map[memoryUsageKey]int
	reservations                map[memoryQuotaReservationKey]memoryQuotaReservation
	generations                 map[int64]domain.GenerationRecord
	threadBriefs                map[int64]domain.ThreadBrief
	threadFinalistSets          map[int64]domain.ThreadFinalistSet
	threadDrafts                map[int64]domain.ThreadDraft
	threadMedia                 map[int64]domain.ThreadMedia
	threadMediaAttachOperations map[memoryThreadMediaAttachKey]memoryThreadMediaAttachOperation
	feedback                    []domain.Feedback
	examples                    map[int64][]string
	nextID                      int64
	now                         func() time.Time
}

func NewMemory() *Memory {
	return &Memory{
		users: make(map[int64]domain.User), updates: make(map[int64]domain.UpdateJob), usage: make(map[memoryUsageKey]int),
		reservations: make(map[memoryQuotaReservationKey]memoryQuotaReservation),
		generations:  make(map[int64]domain.GenerationRecord), threadBriefs: make(map[int64]domain.ThreadBrief),
		threadFinalistSets:          make(map[int64]domain.ThreadFinalistSet),
		threadDrafts:                make(map[int64]domain.ThreadDraft),
		threadMedia:                 make(map[int64]domain.ThreadMedia),
		threadMediaAttachOperations: make(map[memoryThreadMediaAttachKey]memoryThreadMediaAttachOperation),
		examples:                    make(map[int64][]string), nextID: 1, now: time.Now,
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

func (m *Memory) StartThreadBrief(
	_ context.Context,
	telegramID, startUpdateID int64,
	voice domain.ThreadVoice,
) (domain.ThreadBrief, bool, error) {
	brief := domain.ThreadBrief{TelegramID: telegramID, StartUpdateID: startUpdateID, Voice: voice}
	if err := brief.ValidateForStart(); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.threadBriefs {
		if existing.TelegramID == brief.TelegramID && existing.StartUpdateID == brief.StartUpdateID {
			return existing, false, nil
		}
	}
	if _, ok := m.users[brief.TelegramID]; !ok {
		return domain.ThreadBrief{}, false, ErrNotFound
	}
	now := m.now().UTC()
	for id, existing := range m.threadBriefs {
		if existing.TelegramID == brief.TelegramID && existing.Current {
			existing.Current = false
			existing.UpdatedAt = now
			m.threadBriefs[id] = existing
		}
	}
	for id, set := range m.threadFinalistSets {
		if set.TelegramID == brief.TelegramID && set.Current {
			set.State = domain.ThreadFinalistSetCancelled
			set.Current = false
			set.Revision++
			set.UpdatedAt = now
			m.threadFinalistSets[id] = set
		}
	}
	// Starting a new editorial flow makes every older publication keyboard
	// stale immediately, before any potentially slow AI request begins.
	for id, draft := range m.threadDrafts {
		if draft.TelegramID == brief.TelegramID && draft.Current {
			draft.Current = false
			draft.UpdatedAt = now
			m.threadDrafts[id] = draft
		}
	}
	brief.ID = m.nextID
	m.nextID++
	brief.State = domain.ThreadBriefAwaitingGoal
	brief.Revision = 1
	brief.Current = true
	brief.ErrorCode = ""
	brief.CreatedAt = now
	brief.UpdatedAt = now
	if err := brief.Validate(); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	m.threadBriefs[brief.ID] = brief
	return brief, true, nil
}

func (m *Memory) GetThreadBrief(_ context.Context, id, telegramID int64) (domain.ThreadBrief, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	brief, ok := m.threadBriefs[id]
	if !ok || brief.TelegramID != telegramID {
		return domain.ThreadBrief{}, ErrNotFound
	}
	return brief, nil
}

func (m *Memory) GetCurrentThreadBrief(_ context.Context, telegramID int64) (domain.ThreadBrief, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, brief := range m.threadBriefs {
		if brief.TelegramID == telegramID && brief.Current {
			return brief, nil
		}
	}
	return domain.ThreadBrief{}, ErrNotFound
}

func (m *Memory) SetThreadBriefObjective(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	objective domain.ThreadObjective,
) (domain.ThreadBrief, error) {
	if !objective.Selectable() {
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	brief, ok := m.threadBriefs[id]
	if !ok || brief.TelegramID != telegramID {
		return domain.ThreadBrief{}, ErrNotFound
	}
	if brief.Current && brief.Revision == revision+1 && revision < ^uint32(0) &&
		brief.State == domain.ThreadBriefAwaitingMaterial && brief.Objective == objective {
		return brief, nil
	}
	if !brief.Current || brief.Revision != revision || revision == ^uint32(0) ||
		(brief.State != domain.ThreadBriefAwaitingGoal && brief.State != domain.ThreadBriefAwaitingMaterial) {
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	brief.Objective = objective
	brief.State = domain.ThreadBriefAwaitingMaterial
	brief.Revision++
	brief.ErrorCode = ""
	brief.UpdatedAt = m.now().UTC()
	if err := brief.Validate(); err != nil {
		return domain.ThreadBrief{}, err
	}
	m.threadBriefs[id] = brief
	return brief, nil
}

func (m *Memory) SetThreadBriefMaterial(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	updateID int64,
	kind domain.ThreadMaterialKind,
	value string,
) (domain.ThreadBrief, error) {
	if err := validateThreadMaterialChoice(updateID, kind, value); err != nil {
		return domain.ThreadBrief{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.threadBriefs {
		if existing.TelegramID != telegramID || existing.MaterialUpdateID != updateID {
			continue
		}
		if existing.ID == id && existing.MaterialKind == kind && existing.MaterialText == value {
			return existing, nil
		}
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	brief, ok := m.threadBriefs[id]
	if !ok || brief.TelegramID != telegramID {
		return domain.ThreadBrief{}, ErrNotFound
	}
	if !brief.Current || brief.Revision != revision || revision == ^uint32(0) ||
		brief.State != domain.ThreadBriefAwaitingMaterial {
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	brief.MaterialKind = kind
	brief.MaterialText = value
	brief.MaterialUpdateID = updateID
	brief.State = domain.ThreadBriefMaterialReady
	brief.Revision++
	brief.ErrorCode = ""
	brief.UpdatedAt = m.now().UTC()
	if err := brief.Validate(); err != nil {
		return domain.ThreadBrief{}, err
	}
	m.threadBriefs[id] = brief
	return brief, nil
}

func (m *Memory) CreateThreadFinalistSet(
	_ context.Context,
	input domain.ThreadFinalistSet,
) (domain.ThreadFinalistSet, bool, error) {
	if err := input.ValidateForCreate(); err != nil {
		return domain.ThreadFinalistSet{}, false, err
	}
	input.Candidates = normalizedThreadFinalists(input.Candidates)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.threadFinalistSets {
		if existing.TelegramID == input.TelegramID && existing.GenerationUpdateID == input.GenerationUpdateID {
			if sameThreadFinalistGeneration(existing, input) {
				return cloneThreadFinalistSet(existing), false, nil
			}
			return domain.ThreadFinalistSet{}, false, ErrThreadFinalistSetState
		}
		if existing.TelegramID == input.TelegramID && existing.GenerationID == input.GenerationID {
			return domain.ThreadFinalistSet{}, false, ErrThreadFinalistSetState
		}
		if existing.TelegramID == input.TelegramID && existing.Current {
			return domain.ThreadFinalistSet{}, false, ErrThreadFinalistSetState
		}
	}
	if _, ok := m.users[input.TelegramID]; !ok {
		return domain.ThreadFinalistSet{}, false, ErrNotFound
	}
	now := m.now().UTC()
	var nextBrief *domain.ThreadBrief
	var nextBase *domain.ThreadDraft
	if input.BaseDraftID == 0 {
		brief, ok := m.threadBriefs[input.BriefID]
		if !ok || brief.TelegramID != input.TelegramID {
			return domain.ThreadFinalistSet{}, false, ErrNotFound
		}
		if !brief.Current || brief.State != domain.ThreadBriefMaterialReady || brief.Revision != input.SourceRevision ||
			brief.Voice != input.Voice || brief.Objective != input.Objective || input.TargetDraftRevision != 1 ||
			input.PreserveMediaMode != "" || input.PreserveMediaID != 0 || input.SourceRevision == ^uint32(0) {
			return domain.ThreadFinalistSet{}, false, ErrThreadBriefState
		}
		brief.State = domain.ThreadBriefCandidatesReady
		brief.Revision++
		brief.ErrorCode = ""
		brief.UpdatedAt = now
		if err := brief.Validate(); err != nil {
			return domain.ThreadFinalistSet{}, false, err
		}
		nextBrief = &brief
	} else {
		base, ok := m.threadDrafts[input.BaseDraftID]
		if !ok || base.TelegramID != input.TelegramID {
			return domain.ThreadFinalistSet{}, false, ErrNotFound
		}
		if !base.Current || base.Revision != input.SourceRevision || input.SourceRevision == ^uint32(0) ||
			(base.State != domain.ThreadDraftReady && base.State != domain.ThreadDraftFailed) ||
			input.TargetDraftRevision != input.SourceRevision+1 {
			return domain.ThreadFinalistSet{}, false, ErrThreadDraftState
		}
		if base.Objective.Selectable() && base.Objective != input.Objective {
			return domain.ThreadFinalistSet{}, false, ErrThreadFinalistSetState
		}
		if input.BriefID != 0 && input.BriefID != base.BriefID {
			return domain.ThreadFinalistSet{}, false, ErrThreadFinalistSetState
		}
		input.BriefID = base.BriefID
		if err := m.validatePreservedThreadMediaLocked(input, base); err != nil {
			return domain.ThreadFinalistSet{}, false, err
		}
		base.Current = false
		base.UpdatedAt = now
		nextBase = &base
	}
	input.ID = m.nextID
	input.State = domain.ThreadFinalistSetReady
	input.Revision = 1
	input.Current = true
	input.SelectedPosition = -1
	input.SelectionUpdateID = 0
	input.SelectedDraftID = 0
	input.CreatedAt = now
	input.UpdatedAt = now
	if err := input.Validate(); err != nil {
		return domain.ThreadFinalistSet{}, false, err
	}
	m.nextID++
	if nextBrief != nil {
		m.threadBriefs[nextBrief.ID] = *nextBrief
	}
	if nextBase != nil {
		m.threadDrafts[nextBase.ID] = *nextBase
	}
	m.threadFinalistSets[input.ID] = cloneThreadFinalistSet(input)
	return cloneThreadFinalistSet(input), true, nil
}

func (m *Memory) GetThreadFinalistSet(_ context.Context, id, telegramID int64) (domain.ThreadFinalistSet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set, ok := m.threadFinalistSets[id]
	if !ok || set.TelegramID != telegramID {
		return domain.ThreadFinalistSet{}, ErrNotFound
	}
	return cloneThreadFinalistSet(set), nil
}

func (m *Memory) GetCurrentThreadFinalistSet(_ context.Context, telegramID int64) (domain.ThreadFinalistSet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, set := range m.threadFinalistSets {
		if set.TelegramID == telegramID && set.Current {
			return cloneThreadFinalistSet(set), nil
		}
	}
	return domain.ThreadFinalistSet{}, ErrNotFound
}

func (m *Memory) GetThreadFinalistSetByGenerationUpdate(
	_ context.Context,
	telegramID, updateID int64,
) (domain.ThreadFinalistSet, error) {
	if updateID <= 0 {
		return domain.ThreadFinalistSet{}, ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, set := range m.threadFinalistSets {
		if set.TelegramID == telegramID && set.GenerationUpdateID == updateID {
			return cloneThreadFinalistSet(set), nil
		}
	}
	return domain.ThreadFinalistSet{}, ErrNotFound
}

func (m *Memory) SelectThreadFinalist(
	_ context.Context,
	setID, telegramID int64,
	setRevision uint32,
	position int,
	selectionUpdateID int64,
) (domain.ThreadDraft, bool, error) {
	if setID <= 0 || telegramID <= 0 || setRevision == 0 || setRevision == ^uint32(0) || position < 0 || position >= 5 || selectionUpdateID <= 0 {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, replay := range m.threadFinalistSets {
		if replay.TelegramID != telegramID || replay.SelectionUpdateID != selectionUpdateID {
			continue
		}
		if replay.ID == setID && replay.State == domain.ThreadFinalistSetSelected && replay.SelectedPosition == position {
			draft, ok := m.threadDrafts[replay.SelectedDraftID]
			if !ok || draft.TelegramID != telegramID || draft.FinalistSetID != setID {
				return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
			}
			return draft, false, nil
		}
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	set, ok := m.threadFinalistSets[setID]
	if !ok || set.TelegramID != telegramID {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	if !set.Current || set.State != domain.ThreadFinalistSetReady || set.Revision != setRevision {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	candidate := set.Candidates[position]
	if candidate.Position != position || !candidate.Selectable {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	var nextBrief *domain.ThreadBrief
	if set.BaseDraftID == 0 {
		brief, exists := m.threadBriefs[set.BriefID]
		if !exists || brief.TelegramID != telegramID {
			return domain.ThreadDraft{}, false, ErrNotFound
		}
		if !brief.Current || brief.State != domain.ThreadBriefCandidatesReady || brief.Revision != set.SourceRevision+1 {
			return domain.ThreadDraft{}, false, ErrThreadBriefState
		}
		brief.State = domain.ThreadBriefDraftReady
		brief.Revision++
		brief.ErrorCode = ""
		brief.UpdatedAt = m.now().UTC()
		if err := brief.Validate(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		nextBrief = &brief
	} else {
		base, exists := m.threadDrafts[set.BaseDraftID]
		if !exists || base.TelegramID != telegramID || base.Current || base.Revision != set.SourceRevision ||
			(base.State != domain.ThreadDraftReady && base.State != domain.ThreadDraftFailed) {
			return domain.ThreadDraft{}, false, ErrThreadDraftState
		}
		if base.BriefID != set.BriefID || (base.Objective.Selectable() && base.Objective != set.Objective) {
			return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
		}
		if err := m.validatePreservedThreadMediaLocked(set, base); err != nil {
			return domain.ThreadDraft{}, false, err
		}
	}
	for _, current := range m.threadDrafts {
		if current.TelegramID == telegramID && current.Current {
			return domain.ThreadDraft{}, false, ErrThreadDraftState
		}
	}
	now := m.now().UTC()
	draft := domain.ThreadDraft{
		TelegramID: telegramID, Voice: set.Voice, Goal: candidate.Goal,
		BriefID: set.BriefID, FinalistSetID: set.ID, Objective: set.Objective,
		ScenarioID: candidate.ScenarioID, GenerationID: set.GenerationID,
		GenerationUpdateID: set.GenerationUpdateID, PhotoQuery: candidate.PhotoQuery,
		Text: candidate.Text, Provider: set.Provider, Model: set.Model,
		Revision: set.TargetDraftRevision, MediaMode: domain.ThreadMediaText,
		State: domain.ThreadDraftReady, Current: true,
	}
	if set.PreserveMediaMode != "" {
		draft.MediaMode = set.PreserveMediaMode
		draft.MediaID = set.PreserveMediaID
	} else if candidate.VisualMode != domain.ThreadFinalistVisualTextOnly {
		draft.MediaMode = domain.ThreadMediaImagePending
	}
	if err := draft.ValidateForCreate(); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	draft.ID = m.nextID
	draft.State = domain.ThreadDraftReady
	draft.Current = true
	draft.CreatedAt = now
	draft.UpdatedAt = now
	nextSet := set
	nextSet.State = domain.ThreadFinalistSetSelected
	nextSet.Current = false
	nextSet.SelectedPosition = position
	nextSet.SelectionUpdateID = selectionUpdateID
	nextSet.SelectedDraftID = draft.ID
	nextSet.Revision++
	nextSet.UpdatedAt = now
	if err := nextSet.Validate(); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	m.nextID++
	m.threadDrafts[draft.ID] = draft
	m.threadFinalistSets[nextSet.ID] = cloneThreadFinalistSet(nextSet)
	if nextBrief != nil {
		m.threadBriefs[nextBrief.ID] = *nextBrief
	}
	return draft, true, nil
}

func (m *Memory) CancelThreadFinalistSet(
	_ context.Context,
	setID, telegramID int64,
	setRevision uint32,
) (domain.ThreadDraft, bool, error) {
	if setID <= 0 || telegramID <= 0 || setRevision == 0 || setRevision == ^uint32(0) {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.threadFinalistSets[setID]
	if !ok || set.TelegramID != telegramID {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	if set.State == domain.ThreadFinalistSetCancelled && set.Revision == setRevision+1 {
		if set.BaseDraftID == 0 {
			for _, brief := range m.threadBriefs {
				if brief.TelegramID == telegramID && brief.Current {
					return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
				}
			}
			for _, currentSet := range m.threadFinalistSets {
				if currentSet.TelegramID == telegramID && currentSet.Current {
					return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
				}
			}
			for _, draft := range m.threadDrafts {
				if draft.TelegramID == telegramID && draft.Current {
					return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
				}
			}
			return domain.ThreadDraft{}, false, nil
		}
		base, exists := m.threadDrafts[set.BaseDraftID]
		if !exists || base.TelegramID != telegramID || !base.Current || base.Revision != set.SourceRevision ||
			(base.State != domain.ThreadDraftReady && base.State != domain.ThreadDraftFailed) {
			return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
		}
		return base, true, nil
	}
	if !set.Current || set.State != domain.ThreadFinalistSetReady || set.Revision != setRevision {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	now := m.now().UTC()
	var restored domain.ThreadDraft
	restoredOK := false
	var nextBrief *domain.ThreadBrief
	var nextBase *domain.ThreadDraft
	if set.BaseDraftID == 0 {
		brief, exists := m.threadBriefs[set.BriefID]
		if !exists || brief.TelegramID != telegramID {
			return domain.ThreadDraft{}, false, ErrNotFound
		}
		if !brief.Current || brief.State != domain.ThreadBriefCandidatesReady || brief.Revision != set.SourceRevision+1 {
			return domain.ThreadDraft{}, false, ErrThreadBriefState
		}
		brief.State = domain.ThreadBriefCancelled
		brief.Current = false
		brief.Revision++
		brief.ErrorCode = ""
		brief.UpdatedAt = now
		if err := brief.Validate(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		nextBrief = &brief
	} else {
		base, exists := m.threadDrafts[set.BaseDraftID]
		if !exists || base.TelegramID != telegramID || base.Current || base.Revision != set.SourceRevision ||
			(base.State != domain.ThreadDraftReady && base.State != domain.ThreadDraftFailed) {
			return domain.ThreadDraft{}, false, ErrThreadDraftState
		}
		for _, current := range m.threadDrafts {
			if current.TelegramID == telegramID && current.Current {
				return domain.ThreadDraft{}, false, ErrThreadDraftState
			}
		}
		base.Current = true
		base.UpdatedAt = now
		nextBase = &base
		restored, restoredOK = base, true
	}
	nextSet := set
	nextSet.State = domain.ThreadFinalistSetCancelled
	nextSet.Current = false
	nextSet.Revision++
	nextSet.UpdatedAt = now
	if err := nextSet.Validate(); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	if nextBrief != nil {
		m.threadBriefs[nextBrief.ID] = *nextBrief
	}
	if nextBase != nil {
		m.threadDrafts[nextBase.ID] = *nextBase
	}
	m.threadFinalistSets[nextSet.ID] = cloneThreadFinalistSet(nextSet)
	return restored, restoredOK, nil
}

func (m *Memory) validatePreservedThreadMediaLocked(set domain.ThreadFinalistSet, base domain.ThreadDraft) error {
	if set.PreserveMediaMode == "" {
		return nil
	}
	if base.MediaMode != set.PreserveMediaMode || base.MediaID != set.PreserveMediaID {
		return ErrThreadDraftState
	}
	if set.PreserveMediaMode == domain.ThreadMediaImagePending && set.PreserveMediaID == 0 {
		return nil
	}
	mediaValue, ok := m.threadMedia[set.PreserveMediaID]
	if !ok || mediaValue.TelegramID != set.TelegramID {
		return ErrNotFound
	}
	if mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourceTelegram &&
		!(mediaValue.EffectiveSourceKind() == domain.ThreadMediaSourcePexels && mediaValue.AttachUpdateID > 0) {
		return ErrThreadDraftState
	}
	return nil
}

func normalizedThreadFinalists(values []domain.ThreadFinalist) []domain.ThreadFinalist {
	result := make([]domain.ThreadFinalist, len(values))
	for _, value := range values {
		if value.Position >= 0 && value.Position < len(values) {
			result[value.Position] = value
		}
	}
	return result
}

func cloneThreadFinalistSet(value domain.ThreadFinalistSet) domain.ThreadFinalistSet {
	value.Candidates = append([]domain.ThreadFinalist(nil), value.Candidates...)
	return value
}

func sameThreadFinalistGeneration(stored, requested domain.ThreadFinalistSet) bool {
	if stored.TelegramID != requested.TelegramID || (requested.BriefID != 0 && stored.BriefID != requested.BriefID) ||
		stored.BaseDraftID != requested.BaseDraftID || stored.GenerationID != requested.GenerationID ||
		stored.GenerationUpdateID != requested.GenerationUpdateID || stored.Voice != requested.Voice ||
		stored.Objective != requested.Objective || stored.Provider != requested.Provider || stored.Model != requested.Model ||
		stored.SourceRevision != requested.SourceRevision || stored.TargetDraftRevision != requested.TargetDraftRevision ||
		stored.PreserveMediaMode != requested.PreserveMediaMode || stored.PreserveMediaID != requested.PreserveMediaID ||
		len(stored.Candidates) != len(requested.Candidates) {
		return false
	}
	for index := range stored.Candidates {
		if stored.Candidates[index] != requested.Candidates[index] {
			return false
		}
	}
	return true
}

func (m *Memory) CreateThreadDraftForBrief(
	_ context.Context,
	briefID int64,
	briefRevision uint32,
	draft domain.ThreadDraft,
	mediaValue *domain.ThreadMedia,
) (domain.ThreadDraft, bool, error) {
	if briefID <= 0 || draft.TelegramID <= 0 || draft.GenerationUpdateID <= 0 {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if draft.FinalistSetID != 0 {
		return domain.ThreadDraft{}, false, ErrThreadFinalistSetState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.threadDrafts {
		if existing.TelegramID != draft.TelegramID || existing.GenerationUpdateID != draft.GenerationUpdateID {
			continue
		}
		if existing.BriefID == briefID {
			return existing, false, nil
		}
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	brief, ok := m.threadBriefs[briefID]
	if !ok || brief.TelegramID != draft.TelegramID {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	if !brief.Current || brief.Revision != briefRevision || briefRevision == ^uint32(0) ||
		brief.State != domain.ThreadBriefMaterialReady {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if draft.Voice != brief.Voice {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if draft.BriefID != 0 && draft.BriefID != briefID {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	draft.BriefID = briefID
	draft.Objective = brief.Objective
	if draft.MediaMode == "" {
		draft.MediaMode = domain.ThreadMediaText
	}
	if mediaValue != nil {
		if draft.MediaMode != domain.ThreadMediaImage || draft.MediaID != 0 ||
			mediaValue.TelegramID != draft.TelegramID || mediaValue.AttachUpdateID != 0 {
			return domain.ThreadDraft{}, false, ErrThreadDraftState
		}
		if err := mediaValue.ValidateForStore(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		validationDraft := draft
		validationDraft.MediaID = 1
		if err := validationDraft.ValidateForCreate(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		for _, existing := range m.threadMedia {
			if existing.DeliveryKey == mediaValue.DeliveryKey {
				return domain.ThreadDraft{}, false, errors.New("thread media delivery key already exists")
			}
		}
	} else if err := draft.ValidateForCreate(); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	if _, ok := m.users[draft.TelegramID]; !ok {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	now := m.now().UTC()
	if mediaValue != nil {
		storedMedia := *mediaValue
		storedMedia.ID = m.nextID
		m.nextID++
		storedMedia.SourceKind = storedMedia.EffectiveSourceKind()
		storedMedia.Data = append([]byte(nil), storedMedia.Data...)
		storedMedia.CreatedAt = now
		m.threadMedia[storedMedia.ID] = storedMedia
		draft.MediaID = storedMedia.ID
	}
	for id, previous := range m.threadDrafts {
		if previous.TelegramID == draft.TelegramID && previous.Current {
			previous.Current = false
			previous.UpdatedAt = now
			m.threadDrafts[id] = previous
		}
	}
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
	brief.State = domain.ThreadBriefDraftReady
	brief.Revision++
	brief.ErrorCode = ""
	brief.UpdatedAt = now
	m.threadBriefs[brief.ID] = brief
	return draft, true, nil
}

func (m *Memory) GetThreadDraftByGenerationUpdate(_ context.Context, telegramID, updateID int64) (domain.ThreadDraft, error) {
	if updateID <= 0 {
		return domain.ThreadDraft{}, ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, draft := range m.threadDrafts {
		if draft.TelegramID == telegramID && draft.GenerationUpdateID == updateID {
			return draft, nil
		}
	}
	return domain.ThreadDraft{}, ErrNotFound
}

func (m *Memory) CancelThreadBrief(_ context.Context, id, telegramID int64, revision uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	brief, ok := m.threadBriefs[id]
	if !ok || brief.TelegramID != telegramID {
		return ErrNotFound
	}
	if brief.State == domain.ThreadBriefCancelled && revision < ^uint32(0) && brief.Revision == revision+1 {
		return nil
	}
	if !brief.Current || brief.Revision != revision || revision == ^uint32(0) || brief.State == domain.ThreadBriefCancelled {
		return ErrThreadBriefState
	}
	now := m.now().UTC()
	brief.State = domain.ThreadBriefCancelled
	brief.Current = false
	brief.Revision++
	brief.ErrorCode = ""
	brief.UpdatedAt = now
	if err := brief.Validate(); err != nil {
		return err
	}
	m.threadBriefs[id] = brief
	for setID, set := range m.threadFinalistSets {
		if set.BriefID == id && set.TelegramID == telegramID && set.Current && set.State == domain.ThreadFinalistSetReady {
			set.State = domain.ThreadFinalistSetCancelled
			set.Current = false
			set.Revision++
			set.UpdatedAt = now
			m.threadFinalistSets[setID] = set
		}
	}
	for draftID, draft := range m.threadDrafts {
		if draft.BriefID == id && draft.TelegramID == telegramID && draft.Current &&
			(draft.State == domain.ThreadDraftReady || draft.State == domain.ThreadDraftFailed) {
			draft.State = domain.ThreadDraftCancelled
			draft.Current = false
			draft.ErrorCode = ""
			draft.UpdatedAt = now
			m.threadDrafts[draftID] = draft
		}
	}
	return nil
}

func (m *Memory) CreateThreadDraft(_ context.Context, draft domain.ThreadDraft) (int64, error) {
	if draft.FinalistSetID != 0 {
		return 0, ErrThreadFinalistSetState
	}
	draft = normalizeLegacyThreadDraft(draft)
	if err := draft.ValidateForCreate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[draft.TelegramID]; !ok {
		return 0, ErrNotFound
	}
	if draft.BriefID > 0 {
		brief, ok := m.threadBriefs[draft.BriefID]
		if !ok || brief.TelegramID != draft.TelegramID {
			return 0, ErrNotFound
		}
		if !brief.Current || brief.State != domain.ThreadBriefDraftReady || brief.Voice != draft.Voice ||
			brief.Objective != draft.Objective {
			return 0, ErrThreadBriefState
		}
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
	if draft.FinalistSetID != 0 {
		return 0, ErrThreadFinalistSetState
	}
	draft = normalizeLegacyThreadDraft(draft)
	if draft.MediaMode != domain.ThreadMediaImage || draft.MediaID != 0 || mediaValue.TelegramID != draft.TelegramID ||
		mediaValue.AttachUpdateID != 0 {
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
	if draft.BriefID > 0 {
		brief, ok := m.threadBriefs[draft.BriefID]
		if !ok || brief.TelegramID != draft.TelegramID {
			return 0, ErrNotFound
		}
		if !brief.Current || brief.State != domain.ThreadBriefDraftReady || brief.Voice != draft.Voice ||
			brief.Objective != draft.Objective {
			return 0, ErrThreadBriefState
		}
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

func (m *Memory) AttachLicensedThreadDraftMedia(
	_ context.Context,
	id, telegramID int64,
	revision uint32,
	mediaValue domain.ThreadMedia,
) (domain.ThreadDraft, error) {
	if mediaValue.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourcePexels || mediaValue.AttachUpdateID <= 0 {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadDraft{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	operationKey := memoryThreadMediaAttachKey{UserID: telegramID, UpdateID: mediaValue.AttachUpdateID}
	if operation, exists := m.threadMediaAttachOperations[operationKey]; exists {
		if operation.DraftID == id {
			existingDraft, draftExists := m.threadDrafts[id]
			if draftExists && existingDraft.TelegramID == telegramID && existingDraft.MediaID == operation.MediaID {
				return existingDraft, nil
			}
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	draft, ok := m.threadDrafts[id]
	if !ok || draft.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if !draft.Current || draft.Revision != revision ||
		(draft.State != domain.ThreadDraftReady && draft.State != domain.ThreadDraftFailed) ||
		draft.Revision == ^uint32(0) {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if draft.MediaID > 0 {
		current, exists := m.threadMedia[draft.MediaID]
		if !exists || current.TelegramID != telegramID {
			return domain.ThreadDraft{}, ErrNotFound
		}
		if current.SourceAssetID == mediaValue.SourceAssetID || current.Digest == mediaValue.Digest {
			return domain.ThreadDraft{}, ErrThreadDraftState
		}
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
	mediaValue.SourceKind = domain.ThreadMediaSourcePexels
	mediaValue.Data = append([]byte(nil), mediaValue.Data...)
	mediaValue.CreatedAt = now
	m.threadMedia[mediaValue.ID] = mediaValue

	draft.Revision++
	draft.PhotoQuery = mediaValue.SourceQuery
	draft.MediaMode = domain.ThreadMediaImage
	draft.MediaID = mediaValue.ID
	resetThreadDraftForPreview(&draft)
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	m.threadMediaAttachOperations[operationKey] = memoryThreadMediaAttachOperation{DraftID: id, MediaID: mediaValue.ID}
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

func (m *Memory) GetThreadDraftByMediaAttachUpdate(_ context.Context, telegramID, attachUpdateID int64) (domain.ThreadDraft, error) {
	if attachUpdateID <= 0 {
		return domain.ThreadDraft{}, ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	operation, exists := m.threadMediaAttachOperations[memoryThreadMediaAttachKey{UserID: telegramID, UpdateID: attachUpdateID}]
	if !exists {
		return domain.ThreadDraft{}, ErrNotFound
	}
	draft, exists := m.threadDrafts[operation.DraftID]
	if !exists || draft.TelegramID != telegramID || draft.MediaID != operation.MediaID {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	return draft, nil
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
	for _, set := range m.threadFinalistSets {
		if set.PreserveMediaID == id {
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
	now := m.now().UTC()
	draft.UpdatedAt = now
	m.threadDrafts[id] = draft
	if draft.BriefID > 0 {
		brief, exists := m.threadBriefs[draft.BriefID]
		if exists && brief.TelegramID == telegramID && brief.Current && brief.State == domain.ThreadBriefDraftReady {
			brief.State = domain.ThreadBriefCancelled
			brief.Current = false
			if brief.Revision < ^uint32(0) {
				brief.Revision++
			}
			brief.ErrorCode = ""
			brief.UpdatedAt = now
			m.threadBriefs[brief.ID] = brief
		}
	}
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
	for id, brief := range m.threadBriefs {
		if brief.TelegramID == telegramID {
			delete(m.threadBriefs, id)
		}
	}
	for id, set := range m.threadFinalistSets {
		if set.TelegramID == telegramID {
			delete(m.threadFinalistSets, id)
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
	for key := range m.threadMediaAttachOperations {
		if key.UserID == telegramID {
			delete(m.threadMediaAttachOperations, key)
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
	}
	for id, set := range m.threadFinalistSets {
		if !set.UpdatedAt.Before(before) {
			continue
		}
		if selected, ok := m.threadDrafts[set.SelectedDraftID]; ok && !selected.UpdatedAt.Before(before) {
			continue
		}
		delete(m.threadFinalistSets, id)
		if selected, ok := m.threadDrafts[set.SelectedDraftID]; ok && selected.FinalistSetID == id {
			selected.FinalistSetID = 0
			m.threadDrafts[selected.ID] = selected
		}
	}
	for id, draft := range m.threadDrafts {
		if !draft.UpdatedAt.Before(before) || m.threadDraftReferencedByFinalistSetLocked(id) {
			continue
		}
		delete(m.threadDrafts, id)
	}
	for id, brief := range m.threadBriefs {
		if !brief.UpdatedAt.Before(before) {
			continue
		}
		retainedDraft := false
		for _, draft := range m.threadDrafts {
			if draft.BriefID == id && !draft.UpdatedAt.Before(before) {
				retainedDraft = true
				break
			}
		}
		if !retainedDraft {
			for _, set := range m.threadFinalistSets {
				if set.BriefID == id {
					retainedDraft = true
					break
				}
			}
		}
		if !retainedDraft {
			delete(m.threadBriefs, id)
		}
	}
	for id := range m.threadMedia {
		m.deleteOrphanThreadMediaLocked(id)
	}
	for key, operation := range m.threadMediaAttachOperations {
		if _, exists := m.threadDrafts[operation.DraftID]; !exists {
			delete(m.threadMediaAttachOperations, key)
		}
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

func (m *Memory) threadDraftReferencedByFinalistSetLocked(id int64) bool {
	for _, set := range m.threadFinalistSets {
		if set.BaseDraftID == id || set.SelectedDraftID == id {
			return true
		}
	}
	return false
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
