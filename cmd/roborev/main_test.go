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

	"github.com/wesm/roborev/internal/daemon"
	"github.com/wesm/roborev/internal/storage"
	"github.com/wesm/roborev/internal/version"
)

func TestEnqueueCmdPositionalArg(t *testing.T) {
	// Override HOME to prevent reading real daemon.json
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	// Create a temp git repo with a known commit
	tmpDir := t.TempDir()

	// Initialize git repo
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(),
			"HOME="+tmpHome,
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "Test")

	// Create two commits so we can distinguish them
	if err := os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file1.txt")
	runGit("commit", "-m", "first commit")

	// Get first commit SHA
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = tmpDir
	firstSHABytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get first commit SHA: %v", err)
	}
	firstSHA := string(firstSHABytes[:len(firstSHABytes)-1]) // trim newline

	// Create second commit (this becomes HEAD)
	if err := os.WriteFile(filepath.Join(tmpDir, "file2.txt"), []byte("second"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "file2.txt")
	runGit("commit", "-m", "second commit")

	// Second commit is now HEAD (we don't need to track its SHA since CLI sends "HEAD" unresolved)

	// Track what SHA was sent to the server
	var receivedSHA string

	// Create mock server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/enqueue" {
			var req struct {
				GitRef string `json:"git_ref"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			receivedSHA = req.GitRef

			job := storage.ReviewJob{ID: 1, GitRef: req.GitRef, Agent: "test"}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(job)
			return
		}
		// Status endpoint for ensureDaemon check
		if r.URL.Path == "/api/status" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{})
			return
		}
	}))
	defer ts.Close()

	// Write fake daemon.json pointing to our mock server
	roborevDir := filepath.Join(tmpHome, ".roborev")
	os.MkdirAll(roborevDir, 0755)
	// Extract host:port from ts.URL (strip http://)
	mockAddr := ts.URL[7:] // remove "http://"
	daemonInfo := daemon.RuntimeInfo{Addr: mockAddr, PID: os.Getpid(), Version: version.Version}
	data, _ := json.Marshal(daemonInfo)
	os.WriteFile(filepath.Join(roborevDir, "daemon.json"), data, 0644)

	// Test: positional arg should be used instead of HEAD
	t.Run("positional arg overrides default HEAD", func(t *testing.T) {
		receivedSHA = ""
		serverAddr = ts.URL

		shortFirstSHA := firstSHA[:7]
		cmd := enqueueCmd()
		cmd.SetArgs([]string{"--repo", tmpDir, shortFirstSHA}) // Use short SHA as positional arg
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}

		// Should have received the first commit SHA (short form as entered), not HEAD
		if receivedSHA != shortFirstSHA {
			t.Errorf("Expected SHA %s, got %s", shortFirstSHA, receivedSHA)
		}
		if receivedSHA == "HEAD" {
			t.Error("Received HEAD instead of positional arg - bug not fixed!")
		}
	})

	// Test: --sha flag still works
	t.Run("sha flag works", func(t *testing.T) {
		receivedSHA = ""
		serverAddr = ts.URL

		shortFirstSHA := firstSHA[:7]
		cmd := enqueueCmd()
		cmd.SetArgs([]string{"--repo", tmpDir, "--sha", shortFirstSHA})
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}

		if receivedSHA != shortFirstSHA {
			t.Errorf("Expected SHA %s, got %s", shortFirstSHA, receivedSHA)
		}
	})

	// Test: default to HEAD when no arg provided
	t.Run("defaults to HEAD", func(t *testing.T) {
		receivedSHA = ""
		serverAddr = ts.URL

		cmd := enqueueCmd()
		cmd.SetArgs([]string{"--repo", tmpDir})
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("enqueue failed: %v", err)
		}

		// When no arg provided, CLI sends "HEAD" which gets resolved server-side
		if receivedSHA != "HEAD" {
			t.Errorf("Expected HEAD, got %s", receivedSHA)
		}
	})
}

func TestUninstallHookCmd(t *testing.T) {
	// Helper to create a git repo with an optional hook
	setupRepo := func(t *testing.T, hookContent string) (repoPath string, hookPath string) {
		tmpDir := t.TempDir()

		// Initialize git repo
		cmd := exec.Command("git", "init")
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v\n%s", err, out)
		}

		hookPath = filepath.Join(tmpDir, ".git", "hooks", "post-commit")

		if hookContent != "" {
			if err := os.MkdirAll(filepath.Dir(hookPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
				t.Fatal(err)
			}
		}

		return tmpDir, hookPath
	}

	t.Run("hook missing", func(t *testing.T) {
		repoPath, hookPath := setupRepo(t, "")

		// Change to repo dir for the command
		origDir, _ := os.Getwd()
		os.Chdir(repoPath)
		defer os.Chdir(origDir)

		cmd := uninstallHookCmd()
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("uninstall-hook failed: %v", err)
		}

		// Hook should still not exist
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Error("Hook file should not exist")
		}
	})

	t.Run("hook without roborev", func(t *testing.T) {
		hookContent := "#!/bin/bash\necho 'other hook'\n"
		repoPath, hookPath := setupRepo(t, hookContent)

		origDir, _ := os.Getwd()
		os.Chdir(repoPath)
		defer os.Chdir(origDir)

		cmd := uninstallHookCmd()
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("uninstall-hook failed: %v", err)
		}

		// Hook should be unchanged
		content, err := os.ReadFile(hookPath)
		if err != nil {
			t.Fatalf("Failed to read hook: %v", err)
		}
		if string(content) != hookContent {
			t.Errorf("Hook content changed: got %q, want %q", string(content), hookContent)
		}
	})

	t.Run("hook with roborev only - removes file", func(t *testing.T) {
		hookContent := "#!/bin/bash\n# RoboRev auto-commit hook\nroborev enqueue\n"
		repoPath, hookPath := setupRepo(t, hookContent)

		origDir, _ := os.Getwd()
		os.Chdir(repoPath)
		defer os.Chdir(origDir)

		cmd := uninstallHookCmd()
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("uninstall-hook failed: %v", err)
		}

		// Hook should be removed entirely
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Error("Hook file should have been removed")
		}
	})

	t.Run("hook with roborev and other commands - preserves others", func(t *testing.T) {
		hookContent := "#!/bin/bash\necho 'before'\nroborev enqueue\necho 'after'\n"
		repoPath, hookPath := setupRepo(t, hookContent)

		origDir, _ := os.Getwd()
		os.Chdir(repoPath)
		defer os.Chdir(origDir)

		cmd := uninstallHookCmd()
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("uninstall-hook failed: %v", err)
		}

		// Hook should exist with roborev line removed
		content, err := os.ReadFile(hookPath)
		if err != nil {
			t.Fatalf("Failed to read hook: %v", err)
		}

		contentStr := string(content)
		if strings.Contains(strings.ToLower(contentStr), "roborev") {
			t.Error("Hook should not contain roborev")
		}
		if !strings.Contains(contentStr, "echo 'before'") {
			t.Error("Hook should still contain 'echo before'")
		}
		if !strings.Contains(contentStr, "echo 'after'") {
			t.Error("Hook should still contain 'echo after'")
		}
	})

	t.Run("hook with capitalized RoboRev", func(t *testing.T) {
		hookContent := "#!/bin/bash\n# RoboRev hook\nRoboRev enqueue\n"
		repoPath, hookPath := setupRepo(t, hookContent)

		origDir, _ := os.Getwd()
		os.Chdir(repoPath)
		defer os.Chdir(origDir)

		cmd := uninstallHookCmd()
		err := cmd.Execute()
		if err != nil {
			t.Fatalf("uninstall-hook failed: %v", err)
		}

		// Hook should be removed (only had RoboRev content)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Error("Hook file should have been removed")
		}
	})
}
