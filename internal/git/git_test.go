package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetHooksPath(t *testing.T) {
	// Create a temp git repo
	tmpDir := t.TempDir()

	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	t.Run("default hooks path", func(t *testing.T) {
		hooksPath, err := GetHooksPath(tmpDir)
		if err != nil {
			t.Fatalf("GetHooksPath failed: %v", err)
		}

		// Should be absolute
		if !filepath.IsAbs(hooksPath) {
			t.Errorf("hooks path should be absolute, got: %s", hooksPath)
		}

		// Should end with .git/hooks (or .git\hooks on Windows)
		expectedSuffix := filepath.Join(".git", "hooks")
		if !strings.HasSuffix(hooksPath, expectedSuffix) {
			t.Errorf("hooks path should end with %s, got: %s", expectedSuffix, hooksPath)
		}

		// Should be under tmpDir
		if !strings.HasPrefix(hooksPath, tmpDir) {
			t.Errorf("hooks path should be under %s, got: %s", tmpDir, hooksPath)
		}
	})

	t.Run("custom core.hooksPath absolute", func(t *testing.T) {
		// Create a custom hooks directory
		customHooksDir := filepath.Join(tmpDir, "my-hooks")
		if err := os.MkdirAll(customHooksDir, 0755); err != nil {
			t.Fatal(err)
		}

		// Set core.hooksPath to absolute path
		cmd := exec.Command("git", "config", "core.hooksPath", customHooksDir)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config failed: %v\n%s", err, out)
		}

		hooksPath, err := GetHooksPath(tmpDir)
		if err != nil {
			t.Fatalf("GetHooksPath failed: %v", err)
		}

		// Should return the custom absolute path
		if hooksPath != customHooksDir {
			t.Errorf("expected %s, got %s", customHooksDir, hooksPath)
		}

		// Reset for other tests
		cmd = exec.Command("git", "config", "--unset", "core.hooksPath")
		cmd.Dir = tmpDir
		cmd.Run() // ignore error if not set
	})

	t.Run("custom core.hooksPath relative", func(t *testing.T) {
		// Set core.hooksPath to relative path
		cmd := exec.Command("git", "config", "core.hooksPath", "custom-hooks")
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config failed: %v\n%s", err, out)
		}

		hooksPath, err := GetHooksPath(tmpDir)
		if err != nil {
			t.Fatalf("GetHooksPath failed: %v", err)
		}

		// Should be made absolute
		if !filepath.IsAbs(hooksPath) {
			t.Errorf("hooks path should be absolute, got: %s", hooksPath)
		}

		// Should resolve to tmpDir/custom-hooks
		expected := filepath.Join(tmpDir, "custom-hooks")
		if hooksPath != expected {
			t.Errorf("expected %s, got %s", expected, hooksPath)
		}
	})
}
