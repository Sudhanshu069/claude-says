package audio

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Uses Err-results only so player.Play (afplay) is never invoked; exercises the
// ordered drain, epoch fence, and quiesce paths for deadlock/ordering.
func TestQueueSmoke(t *testing.T) {
	q := NewQueue(&AfplayPlayer{dir: t.TempDir()}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	// Reserve in order, deliver out of order, all as skip (Err set).
	q.Reserve(ItemID{Epoch: 0, Seq: 1})
	q.Reserve(ItemID{Epoch: 0, Seq: 2})
	q.Reserve(ItemID{Epoch: 0, Seq: 3})
	// Stale epoch result must be dropped (would otherwise fill seq 2 forever).
	q.Deliver(SynthResult{ID: ItemID{Epoch: 99, Seq: 2}, Err: errors.New("stale")})
	q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 3}, Err: errors.New("x")})
	q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 1}, Err: errors.New("x")})
	q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 2}, Err: errors.New("x")})

	qctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer qcancel()
	if err := q.Quiesce(qctx); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}

	// After switch, an old-epoch result must still be dropped and quiesce idle.
	q.Switch(1)
	q.Reserve(ItemID{Epoch: 0, Seq: 1}) // stale reserve ignored
	q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 1}, Err: errors.New("x")})
	qctx2, qcancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer qcancel2()
	if err := q.Quiesce(qctx2); err != nil {
		t.Fatalf("post-switch quiesce failed: %v", err)
	}

	// Senders must be total after Run exits. Wait deterministically for Run to
	// close q.done rather than sleeping a fixed interval (which flaked under load).
	cancel()
	waitClosed(t, q.done, 3*time.Second, "Run goroutine did not exit after ctx cancel")
	q.Reserve(ItemID{Epoch: 1, Seq: 5}) // must not block
	q.Deliver(SynthResult{ID: ItemID{Epoch: 1, Seq: 5}})
	q.Pause()
	if err := q.Quiesce(context.Background()); err != nil {
		t.Fatalf("quiesce after stop should be nil: %v", err)
	}
}
