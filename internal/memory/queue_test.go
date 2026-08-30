package memory

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The default config is the whole point of the queue: a local model server
// handles one generate call at a time, so extraction must not fan out.
func TestExtractionQueueRunsOneJobAtATime(t *testing.T) {
	var mu sync.Mutex
	var active, maxActive int
	done := make(chan struct{}, 5)

	q := newExtractionQueue(ExtractionConfig{}, func(_ context.Context, _ extractionJob) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		active--
		mu.Unlock()
		done <- struct{}{}
	})

	for i := 0; i < 5; i++ {
		q.enqueue(extractionJob{memory: &Memory{}})
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Errorf("max concurrent jobs = %d, want 1", maxActive)
	}
}

func TestExtractionQueueHonoursWorkerCount(t *testing.T) {
	const workers = 3
	started := make(chan struct{}, workers)
	release := make(chan struct{})

	q := newExtractionQueue(ExtractionConfig{Workers: workers}, func(_ context.Context, _ extractionJob) {
		started <- struct{}{}
		<-release
	})

	for i := 0; i < workers; i++ {
		q.enqueue(extractionJob{memory: &Memory{}})
	}
	// All three must be in flight at once, or this blocks until the test fails.
	for i := 0; i < workers; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d jobs started concurrently", i, workers)
		}
	}
	close(release)
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// The queue is unbounded on purpose: a backlog waits, it is never discarded,
// and enqueueing never blocks the store that produced the job.
func TestExtractionQueueKeepsEveryJobWhileBusy(t *testing.T) {
	const jobs = 200

	firstStarted := make(chan struct{})
	release := make(chan struct{})

	var mu sync.Mutex
	var ran int

	q := newExtractionQueue(ExtractionConfig{Workers: 1}, func(_ context.Context, _ extractionJob) {
		mu.Lock()
		ran++
		first := ran == 1
		mu.Unlock()
		if first {
			close(firstStarted)
			<-release
		}
	})

	q.enqueue(extractionJob{memory: &Memory{}})
	<-firstStarted // the single worker is now blocked; everything else must pile up

	for i := 1; i < jobs; i++ {
		q.enqueue(extractionJob{memory: &Memory{}})
	}

	q.mu.Lock()
	pending := len(q.pending)
	q.mu.Unlock()
	if pending != jobs-1 {
		t.Errorf("pending = %d, want %d — the backlog must grow, not drop", pending, jobs-1)
	}

	close(release)
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if ran != jobs {
		t.Errorf("jobs run = %d, want %d", ran, jobs)
	}
}

// Jobs run in the order they were stored.
func TestExtractionQueueIsFIFO(t *testing.T) {
	const jobs = 20

	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	done := make(chan struct{})

	q := newExtractionQueue(ExtractionConfig{Workers: 1}, func(_ context.Context, job extractionJob) {
		<-release
		mu.Lock()
		order = append(order, job.memory.Content)
		n := len(order)
		mu.Unlock()
		if n == jobs {
			close(done)
		}
	})

	want := make([]string, jobs)
	for i := 0; i < jobs; i++ {
		want[i] = strconv.Itoa(i)
		q.enqueue(extractionJob{memory: &Memory{Content: want[i]}})
	}
	close(release)
	<-done
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, want) {
		t.Errorf("run order = %v, want %v", order, want)
	}
}

func TestExtractionQueueEnqueueAfterStop(t *testing.T) {
	q := newExtractionQueue(ExtractionConfig{}, func(_ context.Context, _ extractionJob) {})
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := q.stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	q.enqueue(extractionJob{memory: &Memory{}}) // must not panic on the closed channel
}

func TestExtractionConfigDefaults(t *testing.T) {
	got := ExtractionConfig{Workers: -1}.withDefaults()
	if got.Workers != defaultExtractionWorkers {
		t.Errorf("withDefaults() = %+v, want workers=%d", got, defaultExtractionWorkers)
	}
}
