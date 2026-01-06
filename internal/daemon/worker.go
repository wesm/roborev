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
}

// NewWorkerPool creates a new worker pool
func NewWorkerPool(db *storage.DB, cfg *config.Config, numWorkers int) *WorkerPool {
	return &WorkerPool{
		db:            db,
		cfg:           cfg,
		promptBuilder: prompt.NewBuilder(db),
		numWorkers:    numWorkers,
		stopCh:        make(chan struct{}),
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
