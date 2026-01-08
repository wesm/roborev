package daemon

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/storage"
)

func TestWorkerPoolE2E(t *testing.T) {
	// Setup temp DB
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	// Setup config with test agent
	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 2

	// Create a repo and commit
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	commit, err := db.GetOrCreateCommit(repo.ID, "testsha123", "Test Author", "Test commit", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	// Enqueue a job with test agent
	job, err := db.EnqueueJob(repo.ID, commit.ID, "testsha123", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}

	// Create and start worker pool
	pool := NewWorkerPool(db, cfg, 1)
	pool.Start()

	// Wait for job to complete (with timeout)
	deadline := time.Now().Add(10 * time.Second)
	var finalJob *storage.ReviewJob
	for time.Now().Before(deadline) {
		finalJob, err = db.GetJobByID(job.ID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}
		if finalJob.Status == storage.JobStatusDone || finalJob.Status == storage.JobStatusFailed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Stop worker pool
	pool.Stop()

	// Verify job completed (might fail if git repo not available, that's ok)
	if finalJob.Status != storage.JobStatusDone && finalJob.Status != storage.JobStatusFailed {
		t.Errorf("Job should be done or failed, got %s", finalJob.Status)
	}

	// If done, verify review was stored
	if finalJob.Status == storage.JobStatusDone {
		review, err := db.GetReviewByCommitSHA("testsha123")
		if err != nil {
			t.Fatalf("GetReviewByCommitSHA failed: %v", err)
		}
		if review.Agent != "test" {
			t.Errorf("Expected agent 'test', got '%s'", review.Agent)
		}
		if review.Output == "" {
			t.Error("Review output should not be empty")
		}
	}
}

func TestWorkerPoolConcurrency(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 4

	repo, _ := db.GetOrCreateRepo(tmpDir)

	// Create multiple jobs
	for i := 0; i < 5; i++ {
		sha := "concurrentsha" + string(rune('0'+i))
		commit, _ := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
		db.EnqueueJob(repo.ID, commit.ID, sha, "test")
	}

	pool := NewWorkerPool(db, cfg, 4)
	pool.Start()

	// Wait briefly and check active workers
	time.Sleep(500 * time.Millisecond)
	activeWorkers := pool.ActiveWorkers()

	pool.Stop()

	// Should have had some workers active (exact number depends on timing)
	t.Logf("Peak active workers: %d", activeWorkers)
}

func TestWorkerPoolCancelRunningJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 1

	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "cancelsha", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "cancelsha", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}

	pool := NewWorkerPool(db, cfg, 1)
	pool.Start()
	defer pool.Stop()

	// Wait for job to be claimed (status becomes running)
	deadline := time.Now().Add(5 * time.Second)
	reachedRunning := false
	for time.Now().Before(deadline) {
		j, err := db.GetJobByID(job.ID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}
		if j.Status == storage.JobStatusRunning {
			reachedRunning = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !reachedRunning {
		t.Fatal("Timeout: job never reached 'running' state")
	}

	// Cancel the job via DB and worker pool
	if err := db.CancelJob(job.ID); err != nil {
		t.Fatalf("CancelJob failed: %v", err)
	}
	pool.CancelJob(job.ID)

	// Wait for worker to react to cancellation
	time.Sleep(500 * time.Millisecond)

	// Verify job status is canceled
	finalJob, err := db.GetJobByID(job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if finalJob.Status != storage.JobStatusCanceled {
		t.Errorf("Expected status 'canceled', got '%s'", finalJob.Status)
	}

	// Verify no review was stored
	_, err = db.GetReviewByJobID(job.ID)
	if err == nil {
		t.Error("Expected no review for canceled job, but found one")
	}
}

func TestWorkerPoolPendingCancellation(t *testing.T) {
	// Test the race condition fix: cancel arrives before job is registered
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	// Create a real job in 'running' state (simulating claimed but not yet registered)
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "pending-cancel", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "pending-cancel", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	// Manually claim the job to put it in 'running' state
	_, err = db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}

	// Don't start the pool - we want to test pending cancellation manually

	// Mark the job as pending cancellation before it's registered
	// CancelJob now returns true for pending cancellations of valid jobs
	if !pool.CancelJob(job.ID) {
		t.Error("CancelJob should return true for valid running job")
	}

	// Verify it's in pending cancels
	pool.runningJobsMu.Lock()
	if !pool.pendingCancels[job.ID] {
		t.Errorf("Job %d should be in pendingCancels", job.ID)
	}
	pool.runningJobsMu.Unlock()

	// Now register the job - should immediately cancel
	canceled := false
	pool.registerRunningJob(job.ID, func() { canceled = true })

	if !canceled {
		t.Error("Job should have been canceled immediately on registration")
	}

	// Verify it's been removed from pending cancels
	pool.runningJobsMu.Lock()
	if pool.pendingCancels[job.ID] {
		t.Errorf("Job %d should have been removed from pendingCancels", job.ID)
	}
	pool.runningJobsMu.Unlock()
}

func TestWorkerPoolPendingCancellationAfterDBCancel(t *testing.T) {
	// Test the real API path: db.CancelJob is called first (sets status to 'canceled'),
	// then workerPool.CancelJob is called while worker hasn't registered yet.
	// This simulates the race condition in handleCancelJob.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	// Create and claim a job (simulating worker claimed but not yet registered)
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "api-cancel-race", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "api-cancel-race", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	claimed, err := db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("Expected to claim job %d, got %d", job.ID, claimed.ID)
	}

	// Simulate the API path: db.CancelJob is called first
	if err := db.CancelJob(job.ID); err != nil {
		t.Fatalf("db.CancelJob failed: %v", err)
	}

	// Verify status is now 'canceled' but WorkerID is still set
	jobAfterDBCancel, err := db.GetJobByID(job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if jobAfterDBCancel.Status != storage.JobStatusCanceled {
		t.Fatalf("Expected status 'canceled', got '%s'", jobAfterDBCancel.Status)
	}
	if jobAfterDBCancel.WorkerID == "" {
		t.Fatal("Expected WorkerID to be set after claim")
	}

	// Now call workerPool.CancelJob (simulating second part of handleCancelJob)
	// This should still work because job has WorkerID set (was claimed)
	if !pool.CancelJob(job.ID) {
		t.Error("CancelJob should return true for canceled-but-claimed job")
	}

	// Verify it's in pending cancels
	pool.runningJobsMu.Lock()
	inPending := pool.pendingCancels[job.ID]
	pool.runningJobsMu.Unlock()
	if !inPending {
		t.Errorf("Job %d should be in pendingCancels", job.ID)
	}

	// Now register the job - should immediately cancel
	canceled := false
	pool.registerRunningJob(job.ID, func() { canceled = true })

	if !canceled {
		t.Error("Job should have been canceled immediately on registration")
	}
}

func TestWorkerPoolCancelInvalidJob(t *testing.T) {
	// Test that CancelJob returns false for non-existent jobs
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	// CancelJob for non-existent job should return false
	if pool.CancelJob(99999) {
		t.Error("CancelJob should return false for non-existent job")
	}

	// Verify it's NOT in pending cancels (prevents unbounded growth)
	pool.runningJobsMu.Lock()
	if pool.pendingCancels[99999] {
		t.Error("Non-existent job should not be added to pendingCancels")
	}
	pool.runningJobsMu.Unlock()
}

func TestWorkerPoolCancelJobFinishedDuringWindow(t *testing.T) {
	// Test that CancelJob doesn't add stale pendingCancels when job finishes
	// during the DB lookup window (simulated by completing job before CancelJob)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	// Create and claim a job
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "finish-window", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "finish-window", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	_, err = db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}

	// Complete the job (simulates job finishing during DB lookup window)
	if err := db.CompleteJob(job.ID, "test", "prompt", "output"); err != nil {
		t.Fatalf("CompleteJob failed: %v", err)
	}

	// Verify job is now done
	completedJob, err := db.GetJobByID(job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if completedJob.Status != storage.JobStatusDone {
		t.Fatalf("Expected status 'done', got '%s'", completedJob.Status)
	}

	// CancelJob should return false because job is done
	if pool.CancelJob(job.ID) {
		t.Error("CancelJob should return false for completed job")
	}

	// Verify pendingCancels is empty (no stale entry)
	pool.runningJobsMu.Lock()
	if pool.pendingCancels[job.ID] {
		t.Error("Completed job should not be added to pendingCancels")
	}
	pool.runningJobsMu.Unlock()
}

func TestWorkerPoolCancelJobRegisteredDuringCheck(t *testing.T) {
	// Test that a job registered during DB checks gets canceled
	// This simulates the race where job registers after initial runningJobs check
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	// Create and claim a job
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "register-during", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "register-during", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	_, err = db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}

	// Pre-register the job with a cancel function
	// This simulates the job registering between CancelJob's checks
	canceled := false
	pool.registerRunningJob(job.ID, func() { canceled = true })

	// Now CancelJob should find it in runningJobs and cancel
	if !pool.CancelJob(job.ID) {
		t.Error("CancelJob should return true for registered job")
	}

	if !canceled {
		t.Error("Job should have been canceled")
	}

	// Verify it's not in pendingCancels (was handled via runningJobs)
	pool.runningJobsMu.Lock()
	if pool.pendingCancels[job.ID] {
		t.Error("Registered job should not be in pendingCancels")
	}
	pool.runningJobsMu.Unlock()
}

func TestWorkerPoolCancelJobConcurrentRegister(t *testing.T) {
	// Test concurrent registration during CancelJob
	// This exercises the race condition where a job registers during DB lookup
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	// Run multiple iterations to increase chance of hitting race
	for i := 0; i < 10; i++ {
		sha := fmt.Sprintf("concurrent-race-%d", i)
		commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}
		job, err := db.EnqueueJob(repo.ID, commit.ID, sha, "test")
		if err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}
		_, err = db.ClaimJob("test-worker")
		if err != nil {
			t.Fatalf("ClaimJob failed: %v", err)
		}

		var canceled int32
		cancelFunc := func() { atomic.AddInt32(&canceled, 1) }

		// Start CancelJob in goroutine
		cancelDone := make(chan bool)
		go func() {
			cancelDone <- pool.CancelJob(job.ID)
		}()

		// Concurrently register the job (simulates worker starting)
		pool.registerRunningJob(job.ID, cancelFunc)

		// Wait for CancelJob to complete
		result := <-cancelDone

		// Job should have been canceled (either via runningJobs or pendingCancels)
		if !result {
			t.Errorf("Iteration %d: CancelJob should return true", i)
		}
		if atomic.LoadInt32(&canceled) == 0 {
			t.Errorf("Iteration %d: Job should have been canceled", i)
		}

		// Clean up for next iteration
		pool.unregisterRunningJob(job.ID)
	}
}

func TestWorkerPoolCancelJobFinalCheckDeadlockSafe(t *testing.T) {
	// Test that cancel() is called without holding the lock (no deadlock)
	// This verifies the fix for the "final check" path
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	pool := NewWorkerPool(db, cfg, 1)

	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "deadlock-test", "Author", "Subject", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(repo.ID, commit.ID, "deadlock-test", "test")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	_, err = db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}

	// Create a cancel function that tries to call unregisterRunningJob
	// If cancel() is called while holding the lock, this would deadlock
	canceled := false
	cancelFunc := func() {
		canceled = true
		// This would deadlock if cancel() was called while holding runningJobsMu
		pool.unregisterRunningJob(job.ID)
	}

	// Register the job
	pool.registerRunningJob(job.ID, cancelFunc)

	// CancelJob should complete without deadlock
	done := make(chan bool)
	go func() {
		pool.CancelJob(job.ID)
		done <- true
	}()

	select {
	case <-done:
		// Success - no deadlock
	case <-time.After(2 * time.Second):
		t.Fatal("CancelJob deadlocked - cancel() called while holding lock")
	}

	if !canceled {
		t.Error("Job should have been canceled")
	}
}
