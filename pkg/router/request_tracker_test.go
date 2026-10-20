package router

import (
	"context"
	"testing"
	"time"
)

func TestRequestTrackerStartAndFinish(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	id := tr.Begin("gemma-4-12b", "worker-a", "chat")
	if id == "" {
		t.Fatal("Start returned empty id")
	}

	// In-flight record present and uncompleted.
	tr.mu.Lock()
	if len(tr.active) != 1 {
		t.Fatalf("active = %d, want 1", len(tr.active))
	}
	rec := tr.active[id]
	if rec.Model != "gemma-4-12b" || rec.WorkerID != "worker-a" || rec.Kind != "chat" || rec.Status != StatusInflight {
		t.Errorf("active record = %+v", rec)
	}
	tr.mu.Unlock()

	// Finish as done.
	tr.Finish(id, StatusDone)
	tr.mu.Lock()
	if len(tr.active) != 0 {
		t.Fatalf("active after finish = %d, want 0", len(tr.active))
	}
	if len(tr.completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(tr.completed))
	}
	finished := tr.completed[0]
	if finished.Status != StatusDone {
		t.Errorf("finished status = %q, want %q", finished.Status, StatusDone)
	}
	if finished.DurationMs < 0 {
		t.Errorf("duration_ms = %d, want >= 0", finished.DurationMs)
	}
	if finished.FinishedAt.IsZero() {
		t.Error("finished_at is zero")
	}
	tr.mu.Unlock()
}

func TestRequestTrackerSetStatus(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	id := tr.Begin("m", "w", "completion")
	tr.SetStatus(id, StatusStreaming)
	tr.mu.Lock()
	if tr.active[id].Status != StatusStreaming {
		t.Errorf("status = %q, want %q", tr.active[id].Status, StatusStreaming)
	}
	tr.mu.Unlock()

	// Unknown id is a no-op.
	tr.SetStatus("does-not-exist", StatusDone)
}

func TestRequestTrackerFinishUnknownID(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	tr.Finish("nope", StatusDone) // must not panic
	if len(tr.active) != 0 || len(tr.completed) != 0 {
		t.Error("finish on unknown id mutated state")
	}
}

func TestRequestTrackerSnapshotOrdering(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	// Two in-flight + two completed; snapshot must be newest-first.
	a := tr.Begin("m1", "w1", "chat")
	time.Sleep(2 * time.Millisecond)
	tr.Finish(a, StatusDone)
	b := tr.Begin("m2", "w2", "chat")
	time.Sleep(2 * time.Millisecond)
	c := tr.Begin("m3", "w3", "chat")
	tr.Finish(c, StatusError)

	sn := tr.Snapshot()
	if len(sn) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(sn))
	}
	// Newest-first: c (most recent) first, then b (in-flight), then a.
	if sn[0].RequestID != c {
		t.Errorf("snapshot[0] = %s, want %s", sn[0].RequestID, c)
	}
	if sn[1].RequestID != b {
		t.Errorf("snapshot[1] = %s, want %s", sn[1].RequestID, b)
	}
	if sn[2].RequestID != a {
		t.Errorf("snapshot[2] = %s, want %s", sn[2].RequestID, a)
	}
}

func TestRequestTrackerSnapshotDeterministic(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	// Two records started within the same nanosecond window: the tie-break on
	// request_id must keep the order stable across polls.
	tr.Begin("m", "w", "chat")
	tr.Begin("m", "w", "chat")
	first := tr.Snapshot()
	for range 20 {
		again := tr.Snapshot()
		if len(first) != len(again) {
			t.Fatalf("len changed: %d vs %d", len(first), len(again))
		}
		for i := range first {
			if first[i].RequestID != again[i].RequestID {
				t.Fatalf("order shuffled at index %d: %v vs %v", i, first[i].RequestID, again[i].RequestID)
			}
		}
	}
}

func TestRequestTrackerBoundedCompleted(t *testing.T) {
	tr := NewRequestTracker(3, 1*time.Hour)
	for i := 0; i < 10; i++ {
		id := tr.Begin("m", "w", "chat")
		tr.Finish(id, StatusDone)
	}
	tr.mu.Lock()
	if len(tr.completed) > 3 {
		t.Errorf("completed len = %d, want <= 3 (bounded)", len(tr.completed))
	}
	tr.mu.Unlock()
	sn := tr.Snapshot()
	// Only the 3 most recent completed records survive; the 10 in-flight
	// starts were all finished, so the snapshot holds exactly the buffer.
	if len(sn) != 3 {
		t.Errorf("snapshot len = %d, want 3", len(sn))
	}
}

func TestRequestTrackerTTLPrune(t *testing.T) {
	tr := NewRequestTracker(100, 50*time.Millisecond)
	id := tr.Begin("m", "w", "chat")
	tr.Finish(id, StatusDone)
	if len(tr.Snapshot()) != 1 {
		t.Fatal("expected 1 record before TTL")
	}
	time.Sleep(120 * time.Millisecond)
	if len(tr.Snapshot()) != 0 {
		t.Fatal("expected 0 records after TTL")
	}
}

func TestRequestTrackerStartStop(t *testing.T) {
	tr := NewRequestTracker(0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	tr.Start(ctx)
	id := tr.Begin("m", "w", "chat")
	tr.SetStatus(id, StatusStreaming)
	tr.Finish(id, StatusDone)
	if len(tr.Snapshot()) != 1 {
		t.Fatal("expected 1 completed record")
	}
	cancel()
	tr.Stop() // must not panic on a closed pruneCh
}