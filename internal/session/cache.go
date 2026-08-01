// Package session contains short-lived, in-memory conversation state. It is
// intentionally independent of Telegram and storage implementations.
package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidTTL = errors.New("session TTL must be positive")
	ErrObsolete   = errors.New("session lease is obsolete")
	ErrClosed     = errors.New("session cache is closed")
)

type options struct {
	now             func() time.Time
	cleanupInterval time.Duration
	janitor         bool
}

type Option func(*options)

// WithClock is primarily useful for deterministic expiry tests.
func WithClock(now func() time.Time) Option {
	return func(cfg *options) {
		if now != nil {
			cfg.now = now
		}
	}
}

func WithCleanupInterval(interval time.Duration) Option {
	return func(cfg *options) {
		if interval > 0 {
			cfg.cleanupInterval = interval
		}
	}
}

func WithoutJanitor() Option {
	return func(cfg *options) { cfg.janitor = false }
}

type entry[T any] struct {
	value     T
	version   uint64
	expiresAt time.Time
	ctx       context.Context
	cancel    context.CancelFunc
}

// Lease identifies one generation of a user's session. Starting a new lease
// for the same user cancels and makes all previous leases obsolete.
type Lease struct {
	UserID  int64
	Version uint64
	Context context.Context

	cacheID uint64
}

type Snapshot[T any] struct {
	Value     T
	Version   uint64
	ExpiresAt time.Time
}

type Cache[T any] struct {
	mu       sync.Mutex
	entries  map[int64]entry[T]
	ttl      time.Duration
	now      func() time.Time
	cacheID  uint64
	sequence uint64
	closed   bool
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

var cacheSequence atomic.Uint64

func New[T any](ttl time.Duration, opts ...Option) (*Cache[T], error) {
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}
	cfg := options{
		now:             time.Now,
		cleanupInterval: ttl / 2,
		janitor:         true,
	}
	for _, option := range opts {
		if option != nil {
			option(&cfg)
		}
	}
	if cfg.cleanupInterval <= 0 {
		cfg.cleanupInterval = time.Second
	}
	cache := &Cache[T]{
		entries: make(map[int64]entry[T]),
		ttl:     ttl,
		now:     cfg.now,
		cacheID: cacheSequence.Add(1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if cfg.janitor {
		go cache.runJanitor(cfg.cleanupInterval)
	} else {
		close(cache.done)
	}
	return cache, nil
}

// Begin installs value as the only current session for userID. The returned
// context is cancelled when a newer input wins, the session expires, Cancel is
// called, or the parent context is cancelled.
func (c *Cache[T]) Begin(parent context.Context, userID int64, value T) (Lease, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	now := c.now()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return Lease{}, ErrClosed
	}
	if previous, exists := c.entries[userID]; exists {
		previous.cancel()
	}
	c.sequence++
	version := c.sequence
	c.entries[userID] = entry[T]{
		value:     value,
		version:   version,
		expiresAt: now.Add(c.ttl),
		ctx:       ctx,
		cancel:    cancel,
	}
	c.mu.Unlock()

	lease := Lease{UserID: userID, Version: version, Context: ctx, cacheID: c.cacheID}
	context.AfterFunc(ctx, func() {
		c.cancelVersion(userID, version)
	})
	return lease, nil
}

// Get returns the current value and renews its sliding TTL.
func (c *Cache[T]) Get(userID int64) (Snapshot[T], bool) {
	return c.lookup(userID, true)
}

// Peek returns the current value without extending its TTL.
func (c *Cache[T]) Peek(userID int64) (Snapshot[T], bool) {
	return c.lookup(userID, false)
}

func (c *Cache[T]) lookup(userID int64, touch bool) (Snapshot[T], bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.entries[userID]
	if !ok || c.closed {
		return Snapshot[T]{}, false
	}
	if current.ctx.Err() != nil || !now.Before(current.expiresAt) {
		delete(c.entries, userID)
		current.cancel()
		return Snapshot[T]{}, false
	}
	if touch {
		current.expiresAt = now.Add(c.ttl)
		c.entries[userID] = current
	}
	return Snapshot[T]{Value: current.value, Version: current.version, ExpiresAt: current.expiresAt}, true
}

// Commit atomically replaces the session value only if lease is still current.
// A late provider response therefore cannot overwrite a newer user request.
func (c *Cache[T]) Commit(lease Lease, value T) error {
	return c.Update(lease, func(current *T) error {
		*current = value
		return nil
	})
}

// Reparent atomically preserves the current value under a new parent context.
// It is useful when a short-lived work context must end while the resulting
// interaction remains available. A concurrent Begin or Cancel always wins
// over a stale lease, so reparenting cannot resurrect superseded state.
func (c *Cache[T]) Reparent(lease Lease, parent context.Context) (Lease, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	now := c.now()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return Lease{}, ErrClosed
	}
	current, ok := c.entries[lease.UserID]
	if lease.cacheID != c.cacheID || lease.Context == nil || lease.Context.Err() != nil ||
		!ok || current.version != lease.Version || current.ctx.Err() != nil || !now.Before(current.expiresAt) {
		var staleCancel context.CancelFunc
		if ok && current.version == lease.Version && (!now.Before(current.expiresAt) || current.ctx.Err() != nil) {
			delete(c.entries, lease.UserID)
			staleCancel = current.cancel
		}
		c.mu.Unlock()
		cancel()
		if staleCancel != nil {
			staleCancel()
		}
		return Lease{}, ErrObsolete
	}
	previousCancel := current.cancel
	c.sequence++
	version := c.sequence
	current.version = version
	current.expiresAt = now.Add(c.ttl)
	current.ctx = ctx
	current.cancel = cancel
	c.entries[lease.UserID] = current
	c.mu.Unlock()

	newLease := Lease{UserID: lease.UserID, Version: version, Context: ctx, cacheID: c.cacheID}
	context.AfterFunc(ctx, func() {
		c.cancelVersion(lease.UserID, version)
	})
	previousCancel()
	return newLease, nil
}

// Update invokes mutate while holding the cache lock. mutate must be small and
// must not call methods on this cache.
func (c *Cache[T]) Update(lease Lease, mutate func(*T) error) error {
	if mutate == nil {
		return nil
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if lease.cacheID != c.cacheID || lease.Context == nil || lease.Context.Err() != nil {
		return ErrObsolete
	}
	current, ok := c.entries[lease.UserID]
	if !ok || current.version != lease.Version || !now.Before(current.expiresAt) {
		if ok && current.version == lease.Version {
			delete(c.entries, lease.UserID)
			current.cancel()
		}
		return ErrObsolete
	}
	if err := mutate(&current.value); err != nil {
		return err
	}
	current.expiresAt = now.Add(c.ttl)
	c.entries[lease.UserID] = current
	return nil
}

func (c *Cache[T]) IsCurrent(lease Lease) bool {
	if lease.cacheID != c.cacheID || lease.Context == nil || lease.Context.Err() != nil {
		return false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.entries[lease.UserID]
	if !ok || c.closed || current.version != lease.Version {
		return false
	}
	if !now.Before(current.expiresAt) {
		delete(c.entries, lease.UserID)
		current.cancel()
		return false
	}
	return true
}

func (c *Cache[T]) Touch(lease Lease) error {
	return c.Update(lease, func(*T) error { return nil })
}

// Cancel invalidates the current session and cancels in-flight work.
func (c *Cache[T]) Cancel(userID int64) bool {
	c.mu.Lock()
	current, ok := c.entries[userID]
	if ok {
		delete(c.entries, userID)
	}
	c.mu.Unlock()
	if ok {
		current.cancel()
	}
	return ok
}

func (c *Cache[T]) Delete(userID int64) bool { return c.Cancel(userID) }

// Sweep removes expired sessions immediately. The janitor calls it
// periodically, while tests and shutdown code may call it directly.
func (c *Cache[T]) Sweep() int {
	now := c.now()
	c.mu.Lock()
	expired := make([]context.CancelFunc, 0)
	for userID, current := range c.entries {
		if current.ctx.Err() != nil || !now.Before(current.expiresAt) {
			delete(c.entries, userID)
			expired = append(expired, current.cancel)
		}
	}
	c.mu.Unlock()
	for _, cancel := range expired {
		cancel()
	}
	return len(expired)
}

func (c *Cache[T]) Len() int {
	c.Sweep()
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache[T]) Close() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		cancels := make([]context.CancelFunc, 0, len(c.entries))
		for userID, current := range c.entries {
			delete(c.entries, userID)
			cancels = append(cancels, current.cancel)
		}
		c.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		close(c.stop)
		<-c.done
	})
}

func (c *Cache[T]) runJanitor(interval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.Sweep()
		case <-c.stop:
			return
		}
	}
}

func (c *Cache[T]) cancelVersion(userID int64, version uint64) {
	c.mu.Lock()
	current, ok := c.entries[userID]
	if ok && current.version == version {
		delete(c.entries, userID)
	}
	c.mu.Unlock()
}
