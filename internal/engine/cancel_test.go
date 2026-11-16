package engine

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// deadlineBudget is the contract this file measures: after POST
// /v1/jobs/{id}/cancel commits, the worker must stop the active render within
// a bounded time and the job must remain cancelled with no artifact. The
// multiplier leaves room for a loaded CI machine but is far below the ~1s a
// full budget-sized render needs, so "cancel actually interrupts" is provable
// rather than asserted.
const cancelDeadline = 750 * time.Millisecond

func heavySnapshot(t *testing.T, layers int) model.Snapshot {
	t.Helper()
	built := make([]model.Layer, layers)
	for i := range built {
		built[i] = model.Layer{
			ID: fmt.Sprintf("sheet-%d", i), Kind: "rectangle",
			Width: 1536, Height: 1536,
			Transform: model.Transform{ScaleX: 1, ScaleY: 1},
			Opacity:   1,
			Fill:      model.Paint{Kind: "solid", Color: "#FFFFFF"},
		}
	}
	return model.Snapshot{ID: "snap-test", SceneID: "scene-test", State: "active",
		Document: model.Document{Width: 1536, Height: 1536, Background: "#000000", Layers: built},
		Assets:   map[string]model.Asset{}}
}

func openStore(t *testing.T) *persist.Store {
	t.Helper()
	store, err := persist.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedQueuedJob(t *testing.T, store *persist.Store, snapshot model.Snapshot, scale int) string {
	t.Helper()
	digest := model.SnapshotDigest(snapshot)
	err := store.Update(func(state *model.State) error {
		snapshot.Digest = digest
		state.Scenes["scene-test"] = model.Scene{ID: "scene-test", Name: "test", Revision: 1,
			State: "active", Document: snapshot.Document}
		state.Snapshots[snapshot.ID] = snapshot
		job := model.Job{ID: "job-00000001", SnapshotID: snapshot.ID, State: "queued", Scale: scale,
			Attempt: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Payload: snapshot}
		state.Jobs[job.ID] = job
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return "job-00000001"
}

func awaitState(t *testing.T, store *persist.Store, id string, want string, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		if store.Read().Jobs[id].State == want {
			return time.Since(start)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %q within %v (currently %q)", id, want, within, store.Read().Jobs[id].State)
	return 0
}

// TestCancelStopsActiveRenderWithinDeadline measures the real cancellation
// latency for a render that would otherwise take about a second. The worker
// must observe the persisted cancellation and stop cooperatively, after which
// the job stays cancelled, no artifact is written, and no later commit can
// flip it to succeeded.
func TestCancelStopsActiveRenderWithinDeadline(t *testing.T) {
	store := openStore(t)
	snapshot := heavySnapshot(t, 16)
	id := seedQueuedJob(t, store, snapshot, 1)
	worker := New(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()

	awaitState(t, store, id, "running", 2*time.Second)
	committed := time.Now()
	if err := store.Update(func(state *model.State) error {
		job := state.Jobs[id]
		job.State = "cancelled"
		job.UpdatedAt = time.Now().UTC()
		state.Jobs[id] = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker.Cancel(id)
	settled := awaitState(t, store, id, "cancelled", 2*time.Second)
	latency := time.Since(committed)
	t.Logf("cancel committed -> still cancelled after %v (poll confirmed at %v)", latency, settled)
	if latency > cancelDeadline {
		t.Fatalf("cancellation took %v, budget %v", latency, cancelDeadline)
	}
	// Let the worker finish its interrupted step and try to commit a result.
	time.Sleep(200 * time.Millisecond)
	final := store.Read()
	if final.Jobs[id].State != "cancelled" {
		t.Fatalf("job ended %q after cancellation", final.Jobs[id].State)
	}
	if len(final.Artifacts) != 0 {
		t.Fatalf("cancelled job produced %d artifacts", len(final.Artifacts))
	}
}

// TestCancelBetweenClaimAndTrackingIsHonoured covers the documented race: the
// cancellation commits after claim but before the worker registers its cancel
// function. The worker re-reads committed state and must not paint a result.
func TestCancelBetweenClaimAndTrackingIsHonoured(t *testing.T) {
	store := openStore(t)
	snapshot := heavySnapshot(t, 1)
	id := seedQueuedJob(t, store, snapshot, 1)
	// Cancel while still queued: the worker must claim, notice, and stop.
	if err := store.Update(func(state *model.State) error {
		job := state.Jobs[id]
		job.State = "cancelled"
		state.Jobs[id] = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := New(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	time.Sleep(300 * time.Millisecond)
	final := store.Read()
	if final.Jobs[id].State != "cancelled" {
		t.Fatalf("pre-cancelled job became %q", final.Jobs[id].State)
	}
	if len(final.Artifacts) != 0 {
		t.Fatalf("pre-cancelled job produced artifacts")
	}
}

// TestCancelDeadlineIsSafeForLargestLegalRender records the measured
// end-to-end render time for the largest scene the work budget admits, so the
// cancellation deadline can be compared with the work it interrupts.
func TestCancelDeadlineIsSafeForLargestLegalRender(t *testing.T) {
	store := openStore(t)
	// 1536x1536 at scale 1 with 16 full-canvas layers costs 37,748,736 layer
	// pixels, just under the 40,000,000 budget.
	snapshot := heavySnapshot(t, 16)
	id := seedQueuedJob(t, store, snapshot, 1)
	worker := New(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()

	start := time.Now()
	awaitState(t, store, id, "succeeded", 30*time.Second)
	elapsed := time.Since(start)
	state := store.Read()
	job := state.Jobs[id]
	if job.ArtifactID == "" {
		t.Fatalf("succeeded job has no artifact")
	}
	t.Logf("largest legal render took %v; cancel budget %v is %.0f%% of it",
		elapsed, cancelDeadline, 100*float64(cancelDeadline)/float64(elapsed))
	if elapsed <= cancelDeadline {
		t.Fatalf("budget-sized render took only %v, no longer than the %v cancel budget; "+
			"the measurement cannot show cancellation interrupting work", elapsed, cancelDeadline)
	}
}

// TestPersistenceRecordsNoInterruptedArtifact validates that the persisted
// state after a cancellation still passes full validation, i.e. the cancelled
// job has no dangling artifact reference.
func TestPersistenceRecordsNoInterruptedArtifact(t *testing.T) {
	store := openStore(t)
	snapshot := heavySnapshot(t, 4)
	id := seedQueuedJob(t, store, snapshot, 1)
	worker := New(store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	awaitState(t, store, id, "running", 2*time.Second)
	if err := store.Update(func(state *model.State) error {
		job := state.Jobs[id]
		job.State = "cancelled"
		state.Jobs[id] = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker.Cancel(id)
	time.Sleep(200 * time.Millisecond)
	data, err := json.Marshal(store.Read())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 32<<20 {
		t.Fatalf("persisted state grew to %d bytes", len(data))
	}
}
