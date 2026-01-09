package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.ServerAddr != "127.0.0.1:7373" {
		t.Errorf("Expected ServerAddr '127.0.0.1:7373', got '%s'", cfg.ServerAddr)
	}
	if cfg.MaxWorkers != 4 {
		t.Errorf("Expected MaxWorkers 4, got %d", cfg.MaxWorkers)
	}
	if cfg.DefaultAgent != "codex" {
		t.Errorf("Expected DefaultAgent 'codex', got '%s'", cfg.DefaultAgent)
	}
}

func TestResolveAgent(t *testing.T) {
	cfg := DefaultConfig()
	tmpDir := t.TempDir()

	// Test explicit agent takes precedence
	agent := ResolveAgent("claude-code", tmpDir, cfg)
	if agent != "claude-code" {
		t.Errorf("Expected 'claude-code', got '%s'", agent)
	}

	// Test empty explicit falls back to global config
	agent = ResolveAgent("", tmpDir, cfg)
	if agent != "codex" {
		t.Errorf("Expected 'codex' (from global), got '%s'", agent)
	}

	// Test per-repo config
	repoConfig := filepath.Join(tmpDir, ".roborev.toml")
	os.WriteFile(repoConfig, []byte(`agent = "claude-code"`), 0644)

	agent = ResolveAgent("", tmpDir, cfg)
	if agent != "claude-code" {
		t.Errorf("Expected 'claude-code' (from repo config), got '%s'", agent)
	}

	// Explicit still takes precedence over repo config
	agent = ResolveAgent("codex", tmpDir, cfg)
	if agent != "codex" {
		t.Errorf("Expected 'codex' (explicit), got '%s'", agent)
	}
}

func TestSaveAndLoadGlobal(t *testing.T) {
	// Use temp home directory
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	cfg := DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	cfg.MaxWorkers = 8

	err := SaveGlobal(cfg)
	if err != nil {
		t.Fatalf("SaveGlobal failed: %v", err)
	}

	loaded, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal failed: %v", err)
	}

	if loaded.DefaultAgent != "claude-code" {
		t.Errorf("Expected DefaultAgent 'claude-code', got '%s'", loaded.DefaultAgent)
	}
	if loaded.MaxWorkers != 8 {
		t.Errorf("Expected MaxWorkers 8, got %d", loaded.MaxWorkers)
	}
}

func TestLoadRepoConfigWithGuidelines(t *testing.T) {
	tmpDir := t.TempDir()

	// Test loading config with review guidelines as multi-line string
	configContent := `
agent = "claude-code"
review_guidelines = """
We are not doing database migrations because there are no production databases yet.
Prefer composition over inheritance.
All public APIs must have documentation comments.
"""
`
	repoConfig := filepath.Join(tmpDir, ".roborev.toml")
	if err := os.WriteFile(repoConfig, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := LoadRepoConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadRepoConfig failed: %v", err)
	}

	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}

	if cfg.Agent != "claude-code" {
		t.Errorf("Expected agent 'claude-code', got '%s'", cfg.Agent)
	}

	if !strings.Contains(cfg.ReviewGuidelines, "database migrations") {
		t.Errorf("Expected guidelines to contain 'database migrations', got '%s'", cfg.ReviewGuidelines)
	}

	if !strings.Contains(cfg.ReviewGuidelines, "composition over inheritance") {
		t.Errorf("Expected guidelines to contain 'composition over inheritance'")
	}
}

func TestLoadRepoConfigNoGuidelines(t *testing.T) {
	tmpDir := t.TempDir()

	// Test loading config without review guidelines (backwards compatibility)
	configContent := `agent = "codex"`
	repoConfig := filepath.Join(tmpDir, ".roborev.toml")
	if err := os.WriteFile(repoConfig, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg, err := LoadRepoConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadRepoConfig failed: %v", err)
	}

	if cfg == nil {
		t.Fatal("Expected non-nil config")
	}

	if cfg.ReviewGuidelines != "" {
		t.Errorf("Expected empty guidelines, got '%s'", cfg.ReviewGuidelines)
	}
}

func TestLoadRepoConfigMissing(t *testing.T) {
	tmpDir := t.TempDir()

	// Test loading from directory with no config file
	cfg, err := LoadRepoConfig(tmpDir)
	if err != nil {
		t.Fatalf("LoadRepoConfig failed: %v", err)
	}

	if cfg != nil {
		t.Error("Expected nil config when file doesn't exist")
	}
}

func TestResolveJobTimeout(t *testing.T) {
	t.Run("default when no config", func(t *testing.T) {
		tmpDir := t.TempDir()
		timeout := ResolveJobTimeout(tmpDir, nil)
		if timeout != 30 {
			t.Errorf("Expected default timeout 30, got %d", timeout)
		}
	})

	t.Run("default when global config has zero", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{JobTimeoutMinutes: 0}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 30 {
			t.Errorf("Expected default timeout 30 when global is 0, got %d", timeout)
		}
	})

	t.Run("negative global config falls through to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{JobTimeoutMinutes: -10}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 30 {
			t.Errorf("Expected default timeout 30 when global is negative, got %d", timeout)
		}
	})

	t.Run("global config takes precedence over default", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfg := &Config{JobTimeoutMinutes: 45}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 45 {
			t.Errorf("Expected timeout 45 from global config, got %d", timeout)
		}
	})

	t.Run("repo config takes precedence over global", func(t *testing.T) {
		tmpDir := t.TempDir()
		repoConfig := filepath.Join(tmpDir, ".roborev.toml")
		if err := os.WriteFile(repoConfig, []byte(`job_timeout_minutes = 15`), 0644); err != nil {
			t.Fatalf("Failed to write repo config: %v", err)
		}

		cfg := &Config{JobTimeoutMinutes: 45}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 15 {
			t.Errorf("Expected timeout 15 from repo config, got %d", timeout)
		}
	})

	t.Run("repo config zero falls through to global", func(t *testing.T) {
		tmpDir := t.TempDir()
		repoConfig := filepath.Join(tmpDir, ".roborev.toml")
		if err := os.WriteFile(repoConfig, []byte(`job_timeout_minutes = 0`), 0644); err != nil {
			t.Fatalf("Failed to write repo config: %v", err)
		}

		cfg := &Config{JobTimeoutMinutes: 45}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 45 {
			t.Errorf("Expected timeout 45 from global (repo is 0), got %d", timeout)
		}
	})

	t.Run("repo config negative falls through to global", func(t *testing.T) {
		tmpDir := t.TempDir()
		repoConfig := filepath.Join(tmpDir, ".roborev.toml")
		if err := os.WriteFile(repoConfig, []byte(`job_timeout_minutes = -5`), 0644); err != nil {
			t.Fatalf("Failed to write repo config: %v", err)
		}

		cfg := &Config{JobTimeoutMinutes: 45}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 45 {
			t.Errorf("Expected timeout 45 from global (repo is negative), got %d", timeout)
		}
	})

	t.Run("repo config without timeout falls through to global", func(t *testing.T) {
		tmpDir := t.TempDir()
		repoConfig := filepath.Join(tmpDir, ".roborev.toml")
		if err := os.WriteFile(repoConfig, []byte(`agent = "codex"`), 0644); err != nil {
			t.Fatalf("Failed to write repo config: %v", err)
		}

		cfg := &Config{JobTimeoutMinutes: 60}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 60 {
			t.Errorf("Expected timeout 60 from global (repo has no timeout), got %d", timeout)
		}
	})

	t.Run("malformed repo config falls through to global", func(t *testing.T) {
		tmpDir := t.TempDir()
		repoConfig := filepath.Join(tmpDir, ".roborev.toml")
		if err := os.WriteFile(repoConfig, []byte(`this is not valid toml {{{`), 0644); err != nil {
			t.Fatalf("Failed to write repo config: %v", err)
		}

		cfg := &Config{JobTimeoutMinutes: 45}
		timeout := ResolveJobTimeout(tmpDir, cfg)
		if timeout != 45 {
			t.Errorf("Expected timeout 45 from global (repo config malformed), got %d", timeout)
		}
	})
}

