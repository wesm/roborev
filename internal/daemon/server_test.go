package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/storage"
)

func TestHandleListRepos(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	server := NewServer(db, cfg)

	t.Run("empty database", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.handleListRepos(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var response map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		// repos can be nil when empty
		var reposLen int
		if repos, ok := response["repos"].([]interface{}); ok && repos != nil {
			reposLen = len(repos)
		}
		totalCount := int(response["total_count"].(float64))

		if reposLen != 0 {
			t.Errorf("Expected 0 repos, got %d", reposLen)
		}
		if totalCount != 0 {
			t.Errorf("Expected total_count 0, got %d", totalCount)
		}
	})

	// Create repos and jobs
	repo1, _ := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo1"))
	repo2, _ := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo2"))

	// Add 3 jobs to repo1
	for i := 0; i < 3; i++ {
		sha := "repo1sha" + string(rune('a'+i))
		commit, _ := db.GetOrCreateCommit(repo1.ID, sha, "Author", "Subject", time.Now())
		db.EnqueueJob(repo1.ID, commit.ID, sha, "test")
	}

	// Add 2 jobs to repo2
	for i := 0; i < 2; i++ {
		sha := "repo2sha" + string(rune('a'+i))
		commit, _ := db.GetOrCreateCommit(repo2.ID, sha, "Author", "Subject", time.Now())
		db.EnqueueJob(repo2.ID, commit.ID, sha, "test")
	}

	t.Run("repos with jobs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.handleListRepos(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var response map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}

		repos := response["repos"].([]interface{})
		totalCount := int(response["total_count"].(float64))

		if len(repos) != 2 {
			t.Errorf("Expected 2 repos, got %d", len(repos))
		}
		if totalCount != 5 {
			t.Errorf("Expected total_count 5, got %d", totalCount)
		}

		// Verify individual repo counts
		repoMap := make(map[string]int)
		for _, r := range repos {
			repoObj := r.(map[string]interface{})
			repoMap[repoObj["name"].(string)] = int(repoObj["count"].(float64))
		}

		if repoMap["repo1"] != 3 {
			t.Errorf("Expected repo1 count 3, got %d", repoMap["repo1"])
		}
		if repoMap["repo2"] != 2 {
			t.Errorf("Expected repo2 count 2, got %d", repoMap["repo2"])
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.handleListRepos(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("Expected status 405 for POST, got %d", w.Code)
		}
	})
}

func TestHandleCancelJob(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}
	defer db.Close()

	cfg := config.DefaultConfig()
	server := NewServer(db, cfg)

	// Create a repo and job
	repo, _ := db.GetOrCreateRepo(tmpDir)
	commit, _ := db.GetOrCreateCommit(repo.ID, "canceltest", "Author", "Subject", time.Now())
	job, _ := db.EnqueueJob(repo.ID, commit.ID, "canceltest", "test")

	t.Run("cancel queued job", func(t *testing.T) {
		reqBody, _ := json.Marshal(CancelJobRequest{JobID: job.ID})
		req := httptest.NewRequest(http.MethodPost, "/api/job/cancel", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, _ := db.GetJobByID(job.ID)
		if updated.Status != storage.JobStatusCanceled {
			t.Errorf("Expected status 'canceled', got '%s'", updated.Status)
		}
	})

	t.Run("cancel already canceled job fails", func(t *testing.T) {
		// Job is already canceled from previous test
		reqBody, _ := json.Marshal(CancelJobRequest{JobID: job.ID})
		req := httptest.NewRequest(http.MethodPost, "/api/job/cancel", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("Expected status 404 for already canceled job, got %d", w.Code)
		}
	})

	t.Run("cancel nonexistent job fails", func(t *testing.T) {
		reqBody, _ := json.Marshal(CancelJobRequest{JobID: 99999})
		req := httptest.NewRequest(http.MethodPost, "/api/job/cancel", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("Expected status 404 for nonexistent job, got %d", w.Code)
		}
	})

	t.Run("cancel with missing job_id fails", func(t *testing.T) {
		reqBody, _ := json.Marshal(map[string]interface{}{})
		req := httptest.NewRequest(http.MethodPost, "/api/job/cancel", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected status 400 for missing job_id, got %d", w.Code)
		}
	})

	t.Run("cancel with wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/cancel", nil)
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("Expected status 405 for GET, got %d", w.Code)
		}
	})

	t.Run("cancel running job", func(t *testing.T) {
		// Create a new job and claim it
		commit2, _ := db.GetOrCreateCommit(repo.ID, "cancelrunning", "Author", "Subject", time.Now())
		job2, _ := db.EnqueueJob(repo.ID, commit2.ID, "cancelrunning", "test")
		db.ClaimJob("worker-1")

		reqBody, _ := json.Marshal(CancelJobRequest{JobID: job2.ID})
		req := httptest.NewRequest(http.MethodPost, "/api/job/cancel", bytes.NewReader(reqBody))
		w := httptest.NewRecorder()

		server.handleCancelJob(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, _ := db.GetJobByID(job2.ID)
		if updated.Status != storage.JobStatusCanceled {
			t.Errorf("Expected status 'canceled', got '%s'", updated.Status)
		}
	})
}
