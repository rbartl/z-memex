package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ExtractionConfig bounds the background work that follows a store. Both entity
// extraction and cross-connection call the LLM, and cross-connection alone can
// make one call per semantic neighbour. A local Ollama serves those one at a
// time, so storing a handful of memories at once used to fan out a goroutine
// each and leave the model server thrashing — stalling the embedding calls that
// foreground stores and recalls depend on. The queue keeps that work to Workers
// jobs at a time.
type ExtractionConfig struct {
	// Workers is how many extraction jobs run concurrently.
	Workers int
}

const (
	defaultExtractionWorkers = 1
	extractionJobTimeout     = 5 * time.Minute
)

// withDefaults fills unset (zero or negative) fields. Callers pass a partially
// filled config straight from the environment, so this is the single place the
// defaults live.
func (c ExtractionConfig) withDefaults() ExtractionConfig {
	if c.Workers <= 0 {
		c.Workers = defaultExtractionWorkers
	}
	return c
}

// ExtractionStats is a snapshot of the background extraction backlog, surfaced
// in the UI so a slow burst looks like progress rather than a hang.
type ExtractionStats struct {
	// Running is how many jobs are currently executing.
	Running int `json:"running"`
	// Pending is how many jobs are waiting for a worker.
	Pending int `json:"pending"`
	// Workers is the configured concurrency.
	Workers int `json:"workers"`
}

// Total is the whole outstanding backlog, running jobs included.
func (s ExtractionStats) Total() int { return s.Running + s.Pending }

type extractionJob struct {
	memory *Memory
	vec    []float32
}

// extractionQueue is an unbounded FIFO drained by a fixed set of workers. It is
// deliberately unbounded: a job holds the only chance a memory has to gain its
// graph edges, so a burst of stores waits its turn rather than being discarded.
// Backlog is visible in the "pending" field of the queue's log lines.
type extractionQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending []extractionJob
	active  int
	closed  bool
	workers int
	wg      sync.WaitGroup
}

// newExtractionQueue starts cfg.Workers workers, each running run for one job
// at a time.
func newExtractionQueue(cfg ExtractionConfig, run func(context.Context, extractionJob)) *extractionQueue {
	cfg = cfg.withDefaults()
	q := &extractionQueue{workers: cfg.Workers}
	q.cond = sync.NewCond(&q.mu)

	q.wg.Add(cfg.Workers)
	for i := 0; i < cfg.Workers; i++ {
		go func() {
			defer q.wg.Done()
			for {
				job, ok := q.next()
				if !ok {
					return
				}
				// The timeout starts when the job runs, not when it was queued:
				// a job that waited behind a backlog still gets a full budget.
				ctx, cancel := context.WithTimeout(context.Background(), extractionJobTimeout)
				run(ctx, job)
				cancel()
				q.finish()
			}
		}()
	}
	return q
}

// next blocks until a job is available, returning ok=false once the queue is
// closed and drained.
func (q *extractionQueue) next() (extractionJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.pending) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.pending) == 0 {
		return extractionJob{}, false
	}

	job := q.pending[0]
	q.active++
	q.pending = q.pending[1:]
	if len(q.pending) == 0 {
		// Drop the reference to the spent backing array rather than holding a
		// burst's worth of it for the life of the process.
		q.pending = nil
	}
	return job, true
}

// finish marks the job a worker just ran as no longer in flight.
func (q *extractionQueue) finish() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.active--
}

// stats reports the current backlog.
func (q *extractionQueue) stats() ExtractionStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return ExtractionStats{Running: q.active, Pending: len(q.pending), Workers: q.workers}
}

// enqueue appends a job and returns immediately. Storing a memory never waits
// on the extraction backlog, and never loses its place in it.
func (q *extractionQueue) enqueue(job extractionJob) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		slog.Warn("extraction queue closed, dropping job", "memory_id", job.memory.ID)
		return
	}

	q.pending = append(q.pending, job)
	slog.Debug("extraction job queued", "memory_id", job.memory.ID, "pending", len(q.pending))
	q.cond.Signal()
}

// stop refuses new jobs and waits for the queued and in-flight ones to finish,
// returning ctx's error if they outlast it.
func (q *extractionQueue) stop(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.cond.Broadcast()
	}
	q.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
