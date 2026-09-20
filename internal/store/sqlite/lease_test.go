package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func leaseStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestLeaseIsHeldByOneOwnerUntilItExpires(t *testing.T) {
	s, now := leaseStore(t)
	ctx := context.Background()

	ok, err := s.AcquireLease(ctx, "sweeper", "replica-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	// A second replica must not run the same job at the same time.
	if ok, err := s.AcquireLease(ctx, "sweeper", "replica-b", time.Minute); err != nil || ok {
		t.Fatalf("replica-b took a held lease: ok=%v err=%v", ok, err)
	}
	// The holder renews without contention, which is what keeps one replica
	// doing a periodic job instead of it bouncing between them.
	if ok, err := s.AcquireLease(ctx, "sweeper", "replica-a", time.Minute); err != nil || !ok {
		t.Fatalf("holder could not renew: ok=%v err=%v", ok, err)
	}
	// A different job is independent.
	if ok, err := s.AcquireLease(ctx, "escalator", "replica-b", time.Minute); err != nil || !ok {
		t.Fatalf("second job blocked by the first: ok=%v err=%v", ok, err)
	}

	// The holder dies; after the TTL the lease is reclaimable, with no
	// election and nobody to notice the death.
	*now = now.Add(61 * time.Second)
	if ok, err := s.AcquireLease(ctx, "sweeper", "replica-b", time.Minute); err != nil || !ok {
		t.Fatalf("expired lease not reclaimed: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.AcquireLease(ctx, "sweeper", "replica-a", time.Minute); ok {
		t.Error("the dead holder took its lease back from the new owner")
	}
}

func TestReleaseLeaseOnlyByItsOwner(t *testing.T) {
	s, _ := leaseStore(t)
	ctx := context.Background()
	if _, err := s.AcquireLease(ctx, "fan-in", "replica-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Releasing somebody else's lease is a no-op, not a theft.
	if err := s.ReleaseLease(ctx, "fan-in", "replica-b"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AcquireLease(ctx, "fan-in", "replica-b", time.Minute); ok {
		t.Fatal("replica-b released and took a lease it did not hold")
	}
	// The owner releasing hands it over immediately, which is what a graceful
	// shutdown does so the job does not pause for a whole TTL.
	if err := s.ReleaseLease(ctx, "fan-in", "replica-a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireLease(ctx, "fan-in", "replica-b", time.Minute); err != nil || !ok {
		t.Fatalf("released lease not available: ok=%v err=%v", ok, err)
	}
}

func TestLeaseRejectsEmptyNameOwnerOrTTL(t *testing.T) {
	s, _ := leaseStore(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, owner string }{{"", "a"}, {"x", ""}} {
		if _, err := s.AcquireLease(ctx, tc.name, tc.owner, time.Minute); err == nil {
			t.Errorf("accepted name=%q owner=%q", tc.name, tc.owner)
		}
	}
	if _, err := s.AcquireLease(ctx, "x", "a", 0); err == nil {
		t.Error("accepted a zero ttl")
	}
}
