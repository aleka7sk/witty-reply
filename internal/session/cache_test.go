package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBeginMakesPreviousLeaseObsoleteAndCancelsIt(t *testing.T) {
	cache, err := New[string](time.Minute, WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	oldLease, err := cache.Begin(context.Background(), 42, "old")
	if err != nil {
		t.Fatal(err)
	}
	newLease, err := cache.Begin(context.Background(), 42, "new")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-oldLease.Context.Done():
	default:
		t.Fatal("old lease context was not cancelled")
	}
	if cache.IsCurrent(oldLease) || !cache.IsCurrent(newLease) {
		t.Fatal("last-write-wins invariant failed")
	}
	if err := cache.Commit(oldLease, "late result"); !errors.Is(err, ErrObsolete) {
		t.Fatalf("late commit error = %v, want ErrObsolete", err)
	}
	snapshot, ok := cache.Get(42)
	if !ok || snapshot.Value != "new" {
		t.Fatalf("new value overwritten: %+v, %v", snapshot, ok)
	}
}

func TestTTLExpiryCancelsLease(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cache, err := New[int](time.Minute, WithClock(func() time.Time { return now }), WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lease, err := cache.Begin(context.Background(), 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if removed := cache.Sweep(); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	select {
	case <-lease.Context.Done():
	default:
		t.Fatal("expired lease was not cancelled")
	}
	if _, ok := cache.Peek(7); ok {
		t.Fatal("expired entry remained visible")
	}
}

func TestConcurrentBeginLeavesExactlyOneCurrentLease(t *testing.T) {
	cache, err := New[int](time.Minute, WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	const workers = 64
	leases := make([]Lease, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			lease, beginErr := cache.Begin(context.Background(), 99, index)
			if beginErr != nil {
				t.Errorf("Begin: %v", beginErr)
				return
			}
			leases[index] = lease
		}(i)
	}
	wg.Wait()

	current := 0
	for _, lease := range leases {
		if cache.IsCurrent(lease) {
			current++
		}
	}
	if current != 1 || cache.Len() != 1 {
		t.Fatalf("current leases = %d, cache entries = %d", current, cache.Len())
	}
}

func TestReparentPersistsValueAfterWorkEnds(t *testing.T) {
	cache, err := New[string](time.Minute, WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	workCtx, stopWork := context.WithCancel(context.Background())
	lease, err := cache.Begin(workCtx, 42, "result")
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := cache.Reparent(lease, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stopWork()
	if snapshot, ok := cache.Get(42); !ok || snapshot.Value != "result" || !cache.IsCurrent(persistent) {
		t.Fatalf("reparented session = %+v, current=%v", snapshot, ok && cache.IsCurrent(persistent))
	}
}

func TestReparentCannotResurrectConcurrentSupersession(t *testing.T) {
	cache, err := New[string](time.Minute, WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	for iteration := 0; iteration < 500; iteration++ {
		oldLease, beginErr := cache.Begin(context.Background(), 42, "old")
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_, reparentErr := cache.Reparent(oldLease, context.Background())
			if reparentErr != nil && !errors.Is(reparentErr, ErrObsolete) {
				t.Errorf("Reparent: %v", reparentErr)
			}
		}()
		go func() {
			defer workers.Done()
			<-start
			if _, beginErr := cache.Begin(context.Background(), 42, "new"); beginErr != nil {
				t.Errorf("Begin replacement: %v", beginErr)
			}
		}()
		close(start)
		workers.Wait()
		snapshot, ok := cache.Get(42)
		if !ok || snapshot.Value != "new" {
			t.Fatalf("iteration %d resurrected stale state: %+v, %v", iteration, snapshot, ok)
		}
	}
}

func TestParentCancellationRemovesEntry(t *testing.T) {
	cache, err := New[int](time.Minute, WithoutJanitor())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := cache.Begin(ctx, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-lease.Context.Done():
	case <-time.After(time.Second):
		t.Fatal("lease context was not cancelled")
	}
	if cache.IsCurrent(lease) {
		t.Fatal("cancelled lease remained current")
	}
}
