package bot

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

var (
	// ErrQueueFull remains for source compatibility. Durable Enqueue no longer
	// rejects work because an in-process channel is full.
	ErrQueueFull       = errors.New("bot update queue is full")
	ErrProcessorClosed = errors.New("bot update processor is closed")
)

const (
	queuePayloadVersion    byte = 1
	defaultLease                = 5 * time.Minute
	defaultPollInterval         = 250 * time.Millisecond
	defaultRetryBase            = 500 * time.Millisecond
	defaultRetryMaximum         = time.Minute
	defaultMaxAttempts          = 8
	finalizeTimeout             = 5 * time.Second
	maxLeaseRenewalTimeout      = 5 * time.Second
)

type Processor struct {
	service *Service
	store   store.Store
	cipher  *queueCipher
	notify  chan struct{}
	metrics *observability.Metrics
	logger  *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	activeMu          sync.Mutex
	active            map[string]activeClaim
	supersededThrough map[int64]int64

	acceptMu  sync.RWMutex
	accepting bool
	closeOnce sync.Once
	workerID  string
	sequence  atomic.Uint64

	lease        time.Duration
	pollInterval time.Duration
	retryBase    time.Duration
	retryMaximum time.Duration
	maxAttempts  int
}

type activeClaim struct {
	actorID      int64
	updateID     int64
	supersedable bool
	cancel       context.CancelCauseFunc
}

type ProcessorOption func(*Processor) error

// NewProcessor starts workers backed by Store's durable inbox. Enqueue returns
// only after the encrypted update is committed, making it safe for webhook and
// polling transports to acknowledge/advance their offset.
func NewProcessor(
	parent context.Context,
	service *Service,
	dataStore store.Store,
	callbackSecret string,
	workers, wakeBuffer int,
	metrics *observability.Metrics,
	logger *slog.Logger,
	options ...ProcessorOption,
) (*Processor, error) {
	if service == nil || dataStore == nil || workers < 1 || wakeBuffer < 1 {
		return nil, errors.New("invalid processor configuration")
	}
	if parent == nil {
		parent = context.Background()
	}
	if metrics == nil {
		metrics = observability.NewMetrics()
	}
	if logger == nil {
		logger = slog.Default()
	}
	queueCipher, err := newQueueCipher(callbackSecret)
	if err != nil {
		return nil, err
	}
	workerID, err := randomWorkerID()
	if err != nil {
		return nil, fmt.Errorf("create processor identity: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	processor := &Processor{
		service: service, store: dataStore, cipher: queueCipher,
		notify: make(chan struct{}, wakeBuffer), metrics: metrics, logger: logger,
		ctx: ctx, cancel: cancel, accepting: true, workerID: workerID,
		active: make(map[string]activeClaim), supersededThrough: make(map[int64]int64),
		lease: defaultLease, pollInterval: defaultPollInterval,
		retryBase: defaultRetryBase, retryMaximum: defaultRetryMaximum,
		maxAttempts: defaultMaxAttempts,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(processor); err != nil {
			cancel()
			return nil, fmt.Errorf("configure processor: %w", err)
		}
	}
	if processor.lease <= 0 || processor.pollInterval <= 0 || processor.retryBase <= 0 || processor.retryMaximum <= 0 || processor.maxAttempts < 1 {
		cancel()
		return nil, errors.New("processor durations and retry budget must be positive")
	}
	processor.wg.Add(workers + 1)
	for index := 0; index < workers; index++ {
		go processor.runWorker(index)
	}
	go processor.monitorQueue()
	return processor, nil
}

func (p *Processor) Enqueue(ctx context.Context, update telegram.Update) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if update.UpdateID <= 0 {
		return errors.New("telegram update_id must be positive")
	}
	p.acceptMu.RLock()
	defer p.acceptMu.RUnlock()
	if !p.accepting {
		return ErrProcessorClosed
	}
	select {
	case <-p.ctx.Done():
		return ErrProcessorClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	actorID := updateActorID(update)
	policy := p.service.QueuePolicy(update)
	plaintext, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("encode Telegram update: %w", err)
	}
	payload, err := p.cipher.seal(update.UpdateID, actorID, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt Telegram update: %w", err)
	}
	inserted, err := p.store.EnqueueUpdate(ctx, domain.UpdateJob{
		UpdateID:     update.UpdateID,
		ActorID:      actorID,
		Payload:      payload,
		Supersedable: policy.Supersedable,
		Superseding:  policy.Superseding,
	})
	if err != nil {
		p.metrics.Inc("queue_enqueue_errors")
		return fmt.Errorf("persist Telegram update: %w", err)
	}
	if !inserted {
		p.metrics.Inc("duplicate_updates")
		return nil
	}
	p.service.PreemptUpdate(update)
	if policy.Superseding {
		p.cancelSuperseded(actorID, update.UpdateID)
	}
	p.metrics.Inc("jobs_enqueued")
	p.wake()
	return nil
}

func (p *Processor) Close() {
	p.closeOnce.Do(func() {
		p.acceptMu.Lock()
		p.accepting = false
		p.acceptMu.Unlock()
		p.cancel()
		p.wg.Wait()
	})
}

func (p *Processor) runWorker(index int) {
	defer p.wg.Done()
	for {
		if p.ctx.Err() != nil {
			return
		}
		token := fmt.Sprintf("%s-%d-%d", p.workerID, index, p.sequence.Add(1))
		job, err := p.store.ClaimUpdate(p.ctx, token, time.Now().UTC(), p.lease)
		if errors.Is(err, store.ErrNotFound) {
			if !p.waitForWork() {
				return
			}
			continue
		}
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			p.metrics.Inc("queue_claim_errors")
			p.logger.Error("claim update failed", "error", safeErrorCode(err))
			if !p.waitForWork() {
				return
			}
			continue
		}
		p.process(job)
	}
}

func (p *Processor) monitorQueue() {
	defer p.wg.Done()
	refresh := func() {
		ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
		defer cancel()
		pending, oldest, err := p.store.QueueStats(ctx, time.Now().UTC())
		if err != nil {
			if p.ctx.Err() == nil {
				p.metrics.Inc("queue_stats_errors")
			}
			return
		}
		p.metrics.Set("queue_pending", pending)
		p.metrics.Set("queue_oldest_seconds", int64(oldest/time.Second))
	}
	refresh()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (p *Processor) process(job domain.UpdateJob) {
	p.metrics.Inc("jobs_started")
	workCtx, cancelWork := context.WithCancelCause(p.ctx)
	unregister := p.registerActive(job, cancelWork)
	defer unregister()

	var renewalDone chan error
	err := context.Cause(workCtx)
	if err == nil {
		err = p.renewLease(workCtx, job)
	}
	if err == nil {
		renewalDone = make(chan error, 1)
		go p.maintainLease(workCtx, cancelWork, job, renewalDone)
	}

	var plaintext []byte
	if err == nil {
		plaintext, err = p.cipher.open(job.UpdateID, job.ActorID, job.Payload)
	}
	if err == nil {
		var update telegram.Update
		if err = json.Unmarshal(plaintext, &update); err == nil {
			if update.UpdateID != job.UpdateID {
				err = errors.New("queued update id mismatch")
			} else {
				handlerCtx := withSessionPersistenceContext(workCtx, p.ctx)
				handlerCtx = withUpdateLeaseGuard(handlerCtx, func(ctx context.Context) error {
					return p.renewLease(ctx, job)
				})
				err = p.service.HandleUpdate(handlerCtx, update)
			}
		}
	}
	cancelWork(nil)
	if renewalDone != nil {
		if renewalErr := <-renewalDone; renewalErr != nil {
			err = errors.Join(err, renewalErr)
		}
	}
	if errors.Is(err, store.ErrSuperseded) {
		p.metrics.Inc("jobs_superseded")
		return
	}

	now := time.Now().UTC()
	finalizeCtx, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()
	if err == nil {
		if completeErr := p.store.CompleteUpdate(finalizeCtx, job.UpdateID, job.LeaseToken, now); completeErr != nil {
			if errors.Is(completeErr, store.ErrSuperseded) {
				p.metrics.Inc("jobs_superseded")
				return
			}
			p.metrics.Inc("queue_complete_errors")
			p.logger.Error("complete update failed", "update_id", job.UpdateID, "error", safeErrorCode(completeErr))
			return
		}
		p.metrics.Inc("jobs_completed")
		return
	}

	p.metrics.Inc("jobs_failed")
	retryAt := now.Add(p.retryDelay(job.Attempts))
	dead, retryErr := p.store.RetryUpdate(
		finalizeCtx, job.UpdateID, job.LeaseToken, now, retryAt,
		safeErrorCode(err), p.maxAttempts,
	)
	if retryErr != nil {
		if errors.Is(retryErr, store.ErrSuperseded) {
			p.metrics.Inc("jobs_superseded")
			return
		}
		p.metrics.Inc("queue_retry_errors")
		p.logger.Error("release failed update failed", "update_id", job.UpdateID, "error", safeErrorCode(retryErr))
		return
	}
	if dead {
		p.metrics.Inc("jobs_dead_lettered")
		p.logger.Error("update moved to dead letter", "update_id", job.UpdateID, "attempts", job.Attempts, "error", safeErrorCode(err))
		return
	}
	p.metrics.Inc("jobs_retried")
	p.logger.Warn("update processing failed; retry scheduled", "update_id", job.UpdateID, "attempt", job.Attempts, "error", safeErrorCode(err))
	p.wake()
}

func (p *Processor) maintainLease(ctx context.Context, cancel context.CancelCauseFunc, job domain.UpdateJob, done chan<- error) {
	interval := p.lease / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			err := p.renewLease(ctx, job)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				done <- nil
				return
			}
			renewalErr := fmt.Errorf("renew update lease: %w", err)
			p.metrics.Inc("queue_lease_renew_errors")
			if errors.Is(err, store.ErrLeaseLost) {
				p.metrics.Inc("queue_leases_lost")
			}
			p.logger.Error("renew update lease failed", "update_id", job.UpdateID, "error", safeErrorCode(err))
			cancel(renewalErr)
			done <- renewalErr
			return
		}
	}
}

func (p *Processor) renewLease(ctx context.Context, job domain.UpdateJob) error {
	interval := p.lease / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	renewCtx, stop := context.WithTimeout(ctx, min(interval, maxLeaseRenewalTimeout))
	defer stop()
	started := time.Now()
	err := p.store.RenewUpdate(renewCtx, job.UpdateID, job.LeaseToken, time.Now().UTC(), p.lease)
	p.metrics.Observe("queue_lease_renewal", time.Since(started))
	if err == nil {
		p.metrics.Inc("queue_lease_renewals")
	}
	return err
}

func (p *Processor) registerActive(job domain.UpdateJob, cancel context.CancelCauseFunc) func() {
	claim := activeClaim{
		actorID: job.ActorID, updateID: job.UpdateID,
		supersedable: job.Supersedable, cancel: cancel,
	}
	p.activeMu.Lock()
	p.active[job.LeaseToken] = claim
	superseded := job.Supersedable && job.UpdateID < p.supersededThrough[job.ActorID]
	p.activeMu.Unlock()
	if superseded {
		cancel(store.ErrSuperseded)
	}
	return func() {
		p.activeMu.Lock()
		delete(p.active, job.LeaseToken)
		p.activeMu.Unlock()
	}
}

func (p *Processor) cancelSuperseded(actorID, updateID int64) {
	var cancellations []context.CancelCauseFunc
	p.activeMu.Lock()
	if updateID > p.supersededThrough[actorID] {
		p.supersededThrough[actorID] = updateID
	}
	for _, claim := range p.active {
		if claim.actorID == actorID && claim.supersedable && claim.updateID < updateID {
			cancellations = append(cancellations, claim.cancel)
		}
	}
	p.activeMu.Unlock()
	for _, cancel := range cancellations {
		cancel(store.ErrSuperseded)
	}
	if len(cancellations) > 0 {
		p.metrics.Add("queue_active_supersessions", int64(len(cancellations)))
	}
}

func (p *Processor) retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := p.retryBase
	for index := 1; index < attempt && delay < p.retryMaximum; index++ {
		if delay > p.retryMaximum/2 {
			return p.retryMaximum
		}
		delay *= 2
	}
	if delay > p.retryMaximum {
		return p.retryMaximum
	}
	return delay
}

func (p *Processor) waitForWork() bool {
	timer := time.NewTimer(p.pollInterval)
	defer timer.Stop()
	select {
	case <-p.ctx.Done():
		return false
	case <-p.notify:
		return true
	case <-timer.C:
		return true
	}
}

func (p *Processor) wake() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func updateActorID(update telegram.Update) int64 {
	switch {
	case update.CallbackQuery != nil:
		return update.CallbackQuery.From.ID
	case update.Message != nil && update.Message.From != nil:
		return update.Message.From.ID
	case update.Message != nil && update.Message.Chat.ID != 0:
		return update.Message.Chat.ID
	case update.EditedMessage != nil && update.EditedMessage.From != nil:
		return update.EditedMessage.From.ID
	case update.InlineQuery != nil:
		return update.InlineQuery.From.ID
	case update.ChosenInlineResult != nil:
		return update.ChosenInlineResult.From.ID
	case update.MyChatMember != nil:
		return update.MyChatMember.From.ID
	default:
		// Unsupported updates are still durably accepted and ignored by the
		// service. Giving each one an isolated actor avoids accidental blocking.
		return -update.UpdateID
	}
}

type queueCipher struct {
	aead cipher.AEAD
}

func newQueueCipher(callbackSecret string) (*queueCipher, error) {
	if len(callbackSecret) < 16 {
		return nil, errors.New("callback secret must contain at least 16 characters")
	}
	key := sha256.Sum256(append([]byte("witty-reply/inbox/v1\x00"), []byte(callbackSecret)...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &queueCipher{aead: aead}, nil
}

func (c *queueCipher) seal(updateID, actorID int64, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	result := make([]byte, 1+len(nonce))
	result[0] = queuePayloadVersion
	copy(result[1:], nonce)
	return c.aead.Seal(result, nonce, plaintext, queueAAD(updateID, actorID)), nil
}

func (c *queueCipher) open(updateID, actorID int64, payload []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(payload) < 1+nonceSize+c.aead.Overhead() || payload[0] != queuePayloadVersion {
		return nil, errors.New("unsupported encrypted queue payload")
	}
	nonce := payload[1 : 1+nonceSize]
	return c.aead.Open(nil, nonce, payload[1+nonceSize:], queueAAD(updateID, actorID))
}

func queueAAD(updateID, actorID int64) []byte {
	aad := make([]byte, 16)
	binary.BigEndian.PutUint64(aad[:8], uint64(updateID))
	binary.BigEndian.PutUint64(aad[8:], uint64(actorID))
	return aad
}

func randomWorkerID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
