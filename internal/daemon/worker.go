package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wesm/roborev/internal/agent"
	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/prompt"
	"github.com/wesm/roborev/internal/storage"
)

// WorkerPool manages a pool of review workers
type WorkerPool struct {
	db            *storage.DB
	cfg           *config.Config
	promptBuilder *prompt.Builder

	numWorkers    int
	activeWorkers atomic.Int32
	stopCh        chan struct{}
	wg            sync.WaitGroup

	// Track running jobs for cancellation
	runningJobs    map[int64]context.CancelFunc
	pendingCancels map[int64]bool // Jobs canceled before registered
	runningJobsMu  sync.Mutex
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(db *storage.DB, cfg *config.Config, numWorkers int) *WorkerPool {
	return &WorkerPool{
		db:             db,
		cfg:            cfg,
		promptBuilder:  prompt.NewBuilder(db),
		numWorkers:     numWorkers,
		stopCh:         make(chan struct{}),
		runningJobs:    make(map[int64]context.CancelFunc),
		pendingCancels: make(map[int64]bool),
	}
}

// Start begins the worker pool
func (wp *WorkerPool) Start() {
	log.Printf("Starting worker pool with %d workers", wp.numWorkers)

	for i := 0; i < wp.numWorkers; i++ {
		wp.wg.Add(1)
		go wp.worker(i)
	}
}

// Stop gracefully shuts down the worker pool
func (wp *WorkerPool) Stop() {
	log.Println("Stopping worker pool...")
	close(wp.stopCh)
	wp.wg.Wait()
	log.Println("Worker pool stopped")
}

// ActiveWorkers returns the number of currently active workers
func (wp *WorkerPool) ActiveWorkers() int {
	return int(wp.activeWorkers.Load())
}

// CancelJob cancels a running job by its ID, killing the subprocess.
// Returns true if the job was canceled or marked for pending cancellation.
// Returns false only if the job doesn't exist or isn't in a cancellable state.
func (wp *WorkerPool) CancelJob(jobID int64) bool {
	wp.runningJobsMu.Lock()
	cancel, ok := wp.runningJobs[jobID]
	if ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d", jobID)
		cancel()
		return true
	}
	wp.runningJobsMu.Unlock()

	// Job not registered yet - check if it's a valid job before marking pending
	// This prevents unbounded growth of pendingCancels for invalid/finished job IDs
	// Note: we release the lock before the DB call to avoid blocking other operations
	job, err := wp.db.GetJobByID(jobID)
	if err != nil {
		// DB error - but job may have registered while we were trying to read
		// Re-check runningJobs before giving up
		wp.runningJobsMu.Lock()
		if cancel, ok := wp.runningJobs[jobID]; ok {
			wp.runningJobsMu.Unlock()
			log.Printf("Canceling job %d (registered during failed DB check)", jobID)
			cancel()
			return true
		}
		wp.runningJobsMu.Unlock()
		return false
	}

	// Accept jobs that are queued, running, OR canceled-but-claimed (race condition case)
	// When db.CancelJob is called before workerPool.CancelJob, the status becomes 'canceled'
	// but the worker may not have registered yet. We detect this via WorkerID being set.
	if !wp.isJobCancellable(job) {
		return false
	}

	// Re-lock and check if job was registered while we were checking DB
	wp.runningJobsMu.Lock()
	if cancel, ok := wp.runningJobs[jobID]; ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d (registered during DB check)", jobID)
		cancel()
		return true
	}
	wp.runningJobsMu.Unlock()

	// Re-verify job is still cancellable before adding to pendingCancels
	// The job may have registered and finished during our DB lookup window
	// Do this outside the lock to avoid blocking other operations
	job, err = wp.db.GetJobByID(jobID)
	if err != nil || !wp.isJobCancellable(job) {
		// Job finished or became non-cancellable - don't add stale entry
		return false
	}

	// Final lock acquisition to set pendingCancels
	wp.runningJobsMu.Lock()

	// Final check if job registered while we did the second DB lookup
	if cancel, ok := wp.runningJobs[jobID]; ok {
		wp.runningJobsMu.Unlock()
		log.Printf("Canceling job %d (registered during second DB check)", jobID)
		cancel()
		return true
	}

	// Mark for pending cancellation
	wp.pendingCancels[jobID] = true
	wp.runningJobsMu.Unlock()
	log.Printf("Job %d not yet registered, marking for pending cancellation", jobID)
	return true
}

// isJobCancellable returns true if the job is in a state that can be canceled
func (wp *WorkerPool) isJobCancellable(job *storage.ReviewJob) bool {
	return job.Status == storage.JobStatusQueued ||
		job.Status == storage.JobStatusRunning ||
		(job.Status == storage.JobStatusCanceled && job.WorkerID != "")
}

// registerRunningJob tracks a running job for potential cancellation.
// If the job was already marked for cancellation (race condition), it
// immediately cancels it.
func (wp *WorkerPool) registerRunningJob(jobID int64, cancel context.CancelFunc) {
	wp.runningJobsMu.Lock()
	wp.runningJobs[jobID] = cancel

	// Check if this job was canceled before we registered it
	if wp.pendingCancels[jobID] {
		delete(wp.pendingCancels, jobID)
		wp.runningJobsMu.Unlock()
		log.Printf("Job %d was pending cancellation, canceling now", jobID)
		cancel()
		return
	}
	wp.runningJobsMu.Unlock()
}

// unregisterRunningJob removes a job from the running jobs map
func (wp *WorkerPool) unregisterRunningJob(jobID int64) {
	wp.runningJobsMu.Lock()
	delete(wp.runningJobs, jobID)
	delete(wp.pendingCancels, jobID) // Clean up any stale pending cancel
	wp.runningJobsMu.Unlock()
}

func (wp *WorkerPool) worker(id int) {
	defer wp.wg.Done()
	workerID := fmt.Sprintf("worker-%d", id)

	log.Printf("[%s] Started", workerID)

	for {
		select {
		case <-wp.stopCh:
			log.Printf("[%s] Shutting down", workerID)
			return
		default:
		}

		// Try to claim a job
		job, err := wp.db.ClaimJob(workerID)
		if err != nil {
			log.Printf("[%s] Error claiming job: %v", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}

		if job == nil {
			// No jobs available, wait and retry
			time.Sleep(2 * time.Second)
			continue
		}

		// Process the job
		wp.activeWorkers.Add(1)
		wp.processJob(workerID, job)
		wp.activeWorkers.Add(-1)
	}
}

// maxRetries is the number of retry attempts allowed after initial failure.
// With maxRetries=3, a job can run up to 4 times total (1 initial + 3 retries).
const maxRetries = 3

func (wp *WorkerPool) processJob(workerID string, job *storage.ReviewJob) {
	log.Printf("[%s] Processing job %d for ref %s in %s", workerID, job.ID, job.GitRef, job.RepoName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Register for cancellation tracking
	wp.registerRunningJob(job.ID, cancel)
	defer wp.unregisterRunningJob(job.ID)

	// Build the prompt
	reviewPrompt, err := wp.promptBuilder.Build(job.RepoPath, job.GitRef, job.RepoID, wp.cfg.ReviewContextCount)
	if err != nil {
		log.Printf("[%s] Error building prompt: %v", workerID, err)
		wp.failOrRetry(workerID, job.ID, fmt.Sprintf("build prompt: %v", err))
		return
	}

	// Save the prompt so it can be viewed while job is running
	if err := wp.db.SaveJobPrompt(job.ID, reviewPrompt); err != nil {
		log.Printf("[%s] Error saving prompt: %v", workerID, err)
	}

	// Get the agent (falls back to available agent if preferred not installed)
	a, err := agent.GetAvailable(job.Agent)
	if err != nil {
		log.Printf("[%s] Error getting agent: %v", workerID, err)
		wp.failOrRetry(workerID, job.ID, fmt.Sprintf("get agent: %v", err))
		return
	}

	// Use the actual agent name (may differ from requested if fallback occurred)
	agentName := a.Name()
	if agentName != job.Agent {
		log.Printf("[%s] Agent %s not available, using %s", workerID, job.Agent, agentName)
	}

	// Run the review
	log.Printf("[%s] Running %s review...", workerID, agentName)
	output, err := a.Review(ctx, job.RepoPath, job.GitRef, reviewPrompt)
	if err != nil {
		// Check if this was a cancellation
		if ctx.Err() == context.Canceled {
			log.Printf("[%s] Job %d was canceled", workerID, job.ID)
			return // Job already marked as canceled in DB, nothing more to do
		}
		log.Printf("[%s] Agent error: %v", workerID, err)
		wp.failOrRetry(workerID, job.ID, fmt.Sprintf("agent: %v", err))
		return
	}

	// Store the result (use actual agent name, not requested)
	if err := wp.db.CompleteJob(job.ID, agentName, reviewPrompt, output); err != nil {
		log.Printf("[%s] Error storing review: %v", workerID, err)
		return
	}

	log.Printf("[%s] Completed job %d", workerID, job.ID)
}

// failOrRetry attempts to retry the job, or marks it as failed if max retries reached
func (wp *WorkerPool) failOrRetry(workerID string, jobID int64, errorMsg string) {
	retried, err := wp.db.RetryJob(jobID, maxRetries)
	if err != nil {
		log.Printf("[%s] Error retrying job: %v", workerID, err)
		wp.db.FailJob(jobID, errorMsg)
		return
	}

	if retried {
		retryCount, _ := wp.db.GetJobRetryCount(jobID)
		log.Printf("[%s] Job %d queued for retry (%d/%d)", workerID, jobID, retryCount, maxRetries)
	} else {
		log.Printf("[%s] Job %d failed after %d retries", workerID, jobID, maxRetries)
		wp.db.FailJob(jobID, errorMsg)
	}
}
