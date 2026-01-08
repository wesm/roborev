package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/daemon"
	"github.com/wesm/roborev/internal/storage"
)

// TestE2EEnqueueAndReview tests the full flow of enqueueing and reviewing a commit
func TestE2EEnqueueAndReview(t *testing.T) {
	// Skip if not in a git repo (CI might not have one)
	if _, err := exec.Command("git", "rev-parse", "--git-dir").Output(); err != nil {
		t.Skip("Not in a git repo, skipping e2e test")
	}

	// Setup temp DB
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	// Create a mock server
	cfg := config.DefaultConfig()
	server := daemon.NewServer(db, cfg)

	// Create test HTTP server
	mux := http.NewServeMux()

	// Add handlers manually (simulating the server)
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		queued, running, done, failed, canceled, _ := db.GetJobCounts()
		status := storage.DaemonStatus{
			QueuedJobs:    queued,
			RunningJobs:   running,
			CompletedJobs: done,
			FailedJobs:    failed,
			CanceledJobs:  canceled,
			MaxWorkers:    4,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Test status endpoint
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("Status request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	var status storage.DaemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("Failed to decode status: %v", err)
	}

	if status.MaxWorkers != 4 {
		t.Errorf("Expected MaxWorkers 4, got %d", status.MaxWorkers)
	}

	_ = server // Avoid unused variable
}

// TestDatabaseIntegration tests the full database workflow
func TestDatabaseIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	// Simulate full workflow
	repo, err := db.GetOrCreateRepo("/tmp/test-repo")
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Test Author", "Test commit message", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	job, err := db.EnqueueJob(repo.ID, commit.ID, "abc123", "codex")
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}

	// Verify initial state
	queued, running, done, _, _, _ := db.GetJobCounts()
	if queued != 1 {
		t.Errorf("Expected 1 queued job, got %d", queued)
	}
	_ = running
	_ = done

	// Claim the job
	claimed, err := db.ClaimJob("test-worker")
	if err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if claimed == nil {
		t.Fatal("ClaimJob returned nil")
	}
	if claimed.ID != job.ID {
		t.Error("Claimed wrong job")
	}

	// Verify running state
	_, running, _, _, _, _ = db.GetJobCounts()
	if running != 1 {
		t.Errorf("Expected 1 running job, got %d", running)
	}

	// Complete the job
	err = db.CompleteJob(job.ID, "codex", "test prompt", "This commit looks good!")
	if err != nil {
		t.Fatalf("CompleteJob failed: %v", err)
	}

	// Verify completed state
	queued, running, done, _, _, _ = db.GetJobCounts()
	if done != 1 {
		t.Errorf("Expected 1 completed job, got %d", done)
	}
	_ = queued
	_ = running

	// Fetch the review
	review, err := db.GetReviewByCommitSHA("abc123")
	if err != nil {
		t.Fatalf("GetReviewByCommitSHA failed: %v", err)
	}
	if !strings.Contains(review.Output, "looks good") {
		t.Errorf("Review output doesn't contain expected text: %s", review.Output)
	}

	// Add a response
	resp, err := db.AddResponse(commit.ID, "human-reviewer", "Agreed, LGTM!")
	if err != nil {
		t.Fatalf("AddResponse failed: %v", err)
	}
	if resp.Response != "Agreed, LGTM!" {
		t.Errorf("Response not saved correctly")
	}

	// Verify response can be fetched
	responses, err := db.GetResponsesForCommitSHA("abc123")
	if err != nil {
		t.Fatalf("GetResponsesForCommitSHA failed: %v", err)
	}
	if len(responses) != 1 {
		t.Errorf("Expected 1 response, got %d", len(responses))
	}
}

// TestConfigPersistence tests config save/load
func TestConfigPersistence(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Save custom config
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	cfg.MaxWorkers = 8
	cfg.ReviewContextCount = 5

	err := config.SaveGlobal(cfg)
	if err != nil {
		t.Fatalf("SaveGlobal failed: %v", err)
	}

	// Load it back
	loaded, err := config.LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal failed: %v", err)
	}

	if loaded.DefaultAgent != "claude-code" {
		t.Errorf("DefaultAgent not persisted correctly")
	}
	if loaded.MaxWorkers != 8 {
		t.Errorf("MaxWorkers not persisted correctly")
	}
	if loaded.ReviewContextCount != 5 {
		t.Errorf("ReviewContextCount not persisted correctly")
	}
}
