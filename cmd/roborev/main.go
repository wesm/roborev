package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/roborev/internal/config"
	"github.com/wesm/roborev/internal/daemon"
	"github.com/wesm/roborev/internal/git"
	"github.com/wesm/roborev/internal/storage"
	"github.com/wesm/roborev/internal/update"
	"github.com/wesm/roborev/internal/version"
)

var (
	serverAddr string
	verbose    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "roborev",
		Short: "Automatic code review for git commits",
		Long:  "RoboRev automatically reviews git commits using AI agents (Codex, Claude Code, Gemini, Copilot, OpenCode)",
	}

	rootCmd.PersistentFlags().StringVar(&serverAddr, "server", "http://127.0.0.1:7373", "daemon server address")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")

	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(enqueueCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(showCmd())
	rootCmd.AddCommand(respondCmd())
	rootCmd.AddCommand(addressCmd())
	rootCmd.AddCommand(installHookCmd())
	rootCmd.AddCommand(uninstallHookCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(tuiCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// getDaemonAddr returns the daemon address from runtime file or default
func getDaemonAddr() string {
	if info, err := daemon.ReadRuntime(); err == nil {
		return fmt.Sprintf("http://%s", info.Addr)
	}
	return serverAddr
}

// ensureDaemon checks if daemon is running, starts it if not
// If daemon is running but has different version, restart it
func ensureDaemon() error {
	client := &http.Client{Timeout: 500 * time.Millisecond}

	// First check runtime file for daemon address and version
	if info, err := daemon.ReadRuntime(); err == nil {
		addr := fmt.Sprintf("http://%s/api/status", info.Addr)
		resp, err := client.Get(addr)
		if err == nil {
			resp.Body.Close()

			// Check version match
			if info.Version != version.Version {
				if verbose {
					fmt.Printf("Daemon version mismatch (daemon: %s, cli: %s), restarting...\n", info.Version, version.Version)
				}
				return restartDaemon()
			}

			serverAddr = fmt.Sprintf("http://%s", info.Addr)
			return nil
		}
	}

	// Try default address
	resp, err := client.Get(serverAddr + "/api/status")
	if err == nil {
		resp.Body.Close()
		return nil
	}

	// Start daemon in background
	return startDaemon()
}

// startDaemon starts a new daemon process
func startDaemon() error {
	if verbose {
		fmt.Println("Starting daemon...")
	}

	roborevdPath, err := exec.LookPath("roborevd")
	if err != nil {
		exe, _ := os.Executable()
		roborevdPath = filepath.Join(filepath.Dir(exe), "roborevd")
	}

	cmd := exec.Command(roborevdPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Wait for daemon to be ready and update serverAddr from runtime file
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if info, err := daemon.ReadRuntime(); err == nil {
			addr := fmt.Sprintf("http://%s", info.Addr)
			resp, err := client.Get(addr + "/api/status")
			if err == nil {
				resp.Body.Close()
				serverAddr = addr
				return nil
			}
		}
	}

	return fmt.Errorf("daemon failed to start")
}

// stopDaemon stops the running daemon using PID from daemon.json
func stopDaemon() error {
	info, err := daemon.ReadRuntime()
	if err == nil && info.PID > 0 {
		// Kill by specific PID
		if runtime.GOOS == "windows" {
			exec.Command("taskkill", "/PID", fmt.Sprintf("%d", info.PID), "/F").Run()
		} else {
			// Send SIGTERM first for graceful shutdown
			exec.Command("kill", "-TERM", fmt.Sprintf("%d", info.PID)).Run()
			time.Sleep(500 * time.Millisecond)
			// Then SIGKILL to ensure it's dead
			exec.Command("kill", "-KILL", fmt.Sprintf("%d", info.PID)).Run()
		}
		// Clean up runtime file
		daemon.RemoveRuntime()
	} else {
		// Fallback to pkill if no PID file (shouldn't happen normally)
		if runtime.GOOS == "windows" {
			exec.Command("taskkill", "/IM", "roborevd.exe", "/F").Run()
		} else {
			exec.Command("pkill", "-TERM", "-x", "roborevd").Run()
			time.Sleep(500 * time.Millisecond)
			exec.Command("pkill", "-KILL", "-x", "roborevd").Run()
		}
	}
	time.Sleep(500 * time.Millisecond)
	return nil
}

// restartDaemon stops the running daemon and starts a new one
func restartDaemon() error {
	stopDaemon()

	// Checkpoint WAL to ensure clean state for new daemon
	// Retry a few times in case daemon hasn't fully released the DB
	if dbPath := storage.DefaultDBPath(); dbPath != "" {
		var lastErr error
		for i := 0; i < 3; i++ {
			db, err := storage.Open(dbPath)
			if err != nil {
				lastErr = err
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				lastErr = err
				db.Close()
				time.Sleep(200 * time.Millisecond)
				continue
			}
			db.Close()
			lastErr = nil
			break
		}
		if lastErr != nil && verbose {
			fmt.Printf("Warning: WAL checkpoint failed: %v\n", lastErr)
		}
	}

	return startDaemon()
}

func initCmd() *cobra.Command {
	var agent string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize roborev in current repository",
		Long: `Initialize roborev with a single command:
  - Creates ~/.roborev/ global config directory
  - Creates .roborev.toml in repo (if --agent specified)
  - Installs post-commit hook
  - Starts the daemon`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Initializing roborev...")

			// 1. Ensure we're in a git repo
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository - run this from inside a git repo")
			}

			// 2. Create config directory and default config
			home, _ := os.UserHomeDir()
			configDir := filepath.Join(home, ".roborev")
			if err := os.MkdirAll(configDir, 0755); err != nil {
				return fmt.Errorf("create config dir: %w", err)
			}

			configPath := filepath.Join(configDir, "config.toml")
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				cfg := config.DefaultConfig()
				if agent != "" {
					cfg.DefaultAgent = agent
				}
				if err := config.SaveGlobal(cfg); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("  Created config at %s\n", configPath)
			} else {
				fmt.Printf("  Config already exists at %s\n", configPath)
			}

			// 3. Create per-repo config if agent specified
			repoConfigPath := filepath.Join(root, ".roborev.toml")
			if agent != "" {
				if _, err := os.Stat(repoConfigPath); os.IsNotExist(err) {
					repoConfig := fmt.Sprintf("# RoboRev per-repo configuration\nagent = %q\n", agent)
					if err := os.WriteFile(repoConfigPath, []byte(repoConfig), 0644); err != nil {
						return fmt.Errorf("create repo config: %w", err)
					}
					fmt.Printf("  Created %s\n", repoConfigPath)
				}
			}

			// 4. Install post-commit hook
			hookPath := filepath.Join(root, ".git", "hooks", "post-commit")
			hookContent := `#!/bin/sh
# RoboRev post-commit hook - auto-reviews every commit
roborev enqueue --sha HEAD 2>/dev/null &
`
			// Check for existing hook
			if existing, err := os.ReadFile(hookPath); err == nil {
				if !strings.Contains(string(existing), "roborev") {
					// Append to existing hook
					hookContent = string(existing) + "\n" + hookContent
				} else {
					fmt.Println("  Hook already installed")
					goto startDaemon
				}
			}

			if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
				return fmt.Errorf("install hook: %w", err)
			}
			fmt.Printf("  Installed post-commit hook\n")

		startDaemon:
			// 5. Start daemon
			if err := ensureDaemon(); err != nil {
				fmt.Printf("  Warning: %v\n", err)
				fmt.Println("  Run 'roborev daemon start' to start manually")
			} else {
				fmt.Println("  Daemon is running")
			}

			// 5. Success message
			fmt.Println()
			fmt.Println("Ready! Every commit will now be automatically reviewed.")
			fmt.Println()
			fmt.Println("Commands:")
			fmt.Println("  roborev status      - view queue and daemon status")
			fmt.Println("  roborev show HEAD   - view review for a commit")
			fmt.Println("  roborev tui         - interactive terminal UI")

			return nil
		},
	}

	cmd.Flags().StringVar(&agent, "agent", "", "default agent (codex, claude-code, gemini, copilot, opencode)")

	return cmd
}

func daemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the roborev daemon",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDaemon(); err != nil {
				return err
			}
			fmt.Println("Daemon started")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			stopDaemon()
			fmt.Println("Daemon stopped")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			stopDaemon()
			if err := ensureDaemon(); err != nil {
				return err
			}
			fmt.Println("Daemon restarted")
			return nil
		},
	})

	return cmd
}

func enqueueCmd() *cobra.Command {
	var (
		repoPath string
		sha      string
		agent    string
	)

	cmd := &cobra.Command{
		Use:   "enqueue [commit] or enqueue [start] [end]",
		Short: "Enqueue a commit or commit range for review",
		Long: `Enqueue a commit or commit range for review.

Examples:
  roborev enqueue              # Review HEAD
  roborev enqueue abc123       # Review specific commit
  roborev enqueue abc123 def456  # Review range from abc123 to def456 (inclusive)
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return err
			}

			// Default to current directory
			if repoPath == "" {
				repoPath = "."
			}

			// Get repo root
			root, err := git.GetRepoRoot(repoPath)
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			var gitRef string
			if len(args) >= 2 {
				// Range: START END -> START^..END (inclusive)
				gitRef = args[0] + "^.." + args[1]
			} else if len(args) == 1 {
				// Single commit
				gitRef = args[0]
			} else {
				// Default to HEAD
				gitRef = sha
			}

			// Make request - server will validate and resolve refs
			reqBody, _ := json.Marshal(map[string]string{
				"repo_path": root,
				"git_ref":   gitRef,
				"agent":     agent,
			})

			resp, err := http.Post(serverAddr+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("enqueue failed: %s", body)
			}

			var job storage.ReviewJob
			json.Unmarshal(body, &job)

			fmt.Printf("Enqueued job %d for %s (agent: %s)\n", job.ID, shortRef(job.GitRef), job.Agent)
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "path to git repository (default: current directory)")
	cmd.Flags().StringVar(&sha, "sha", "HEAD", "commit SHA to review (used when no positional args)")
	cmd.Flags().StringVar(&agent, "agent", "", "agent to use (codex, claude-code, gemini, copilot, opencode)")

	return cmd
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon and queue status",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running (and restart if version mismatch)
			if err := ensureDaemon(); err != nil {
				fmt.Println("Daemon: not running")
				fmt.Println()
				fmt.Println("Start with: roborev daemon start")
				return nil
			}

			addr := getDaemonAddr()
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(addr + "/api/status")
			if err != nil {
				fmt.Println("Daemon: not running")
				fmt.Println()
				fmt.Println("Start with: roborev daemon start")
				return nil
			}
			defer resp.Body.Close()

			var status storage.DaemonStatus
			if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			fmt.Println("Daemon: running")
			fmt.Printf("Workers: %d/%d active\n", status.ActiveWorkers, status.MaxWorkers)
			fmt.Printf("Jobs:    %d queued, %d running, %d completed, %d failed\n",
				status.QueuedJobs, status.RunningJobs, status.CompletedJobs, status.FailedJobs)
			fmt.Println()

			// Get recent jobs
			resp, err = client.Get(addr + "/api/jobs?limit=10")
			if err != nil {
				return nil
			}
			defer resp.Body.Close()

			var jobsResp struct {
				Jobs []storage.ReviewJob `json:"jobs"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&jobsResp); err != nil {
				return nil
			}

			if len(jobsResp.Jobs) > 0 {
				fmt.Println("Recent Jobs:")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "  ID\tSHA\tRepo\tAgent\tStatus\tTime\n")
				for _, j := range jobsResp.Jobs {
					elapsed := ""
					if j.StartedAt != nil {
						if j.FinishedAt != nil {
							elapsed = j.FinishedAt.Sub(*j.StartedAt).Round(time.Second).String()
						} else {
							elapsed = time.Since(*j.StartedAt).Round(time.Second).String() + "..."
						}
					}
					fmt.Fprintf(w, "  %d\t%s\t%s\t%s\t%s\t%s\n",
						j.ID, shortRef(j.GitRef), j.RepoName, j.Agent, j.Status, elapsed)
				}
				w.Flush()
			}

			return nil
		},
	}
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [sha]",
		Short: "Show review for a commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running (and restart if version mismatch)
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			sha := "HEAD"
			if len(args) > 0 {
				sha = args[0]
			}

			// Resolve SHA if in a git repo
			if root, err := git.GetRepoRoot("."); err == nil {
				if resolved, err := git.ResolveSHA(root, sha); err == nil {
					sha = resolved
				}
			}

			addr := getDaemonAddr()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(addr + "/api/review?sha=" + sha)
			if err != nil {
				return fmt.Errorf("failed to connect to daemon (is it running?)")
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("no review found for commit %s", shortSHA(sha))
			}

			var review storage.Review
			if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			fmt.Printf("Review for %s (by %s)\n", shortSHA(sha), review.Agent)
			fmt.Println(strings.Repeat("-", 60))
			fmt.Println(review.Output)

			return nil
		},
	}
}

func respondCmd() *cobra.Command {
	var (
		responder string
		message   string
	)

	cmd := &cobra.Command{
		Use:   "respond [sha]",
		Short: "Add a response to a review",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sha := args[0]

			// Resolve SHA
			if root, err := git.GetRepoRoot("."); err == nil {
				if resolved, err := git.ResolveSHA(root, sha); err == nil {
					sha = resolved
				}
			}

			// If no message provided, open editor
			if message == "" {
				editor := os.Getenv("EDITOR")
				if editor == "" {
					editor = "vim"
				}

				tmpfile, err := os.CreateTemp("", "roborev-response-*.md")
				if err != nil {
					return fmt.Errorf("create temp file: %w", err)
				}
				tmpfile.Close()
				defer os.Remove(tmpfile.Name())

				editorCmd := exec.Command(editor, tmpfile.Name())
				editorCmd.Stdin = os.Stdin
				editorCmd.Stdout = os.Stdout
				editorCmd.Stderr = os.Stderr
				if err := editorCmd.Run(); err != nil {
					return fmt.Errorf("editor failed: %w", err)
				}

				content, err := os.ReadFile(tmpfile.Name())
				if err != nil {
					return fmt.Errorf("read response: %w", err)
				}
				message = strings.TrimSpace(string(content))
			}

			if message == "" {
				return fmt.Errorf("empty response, aborting")
			}

			if responder == "" {
				responder = os.Getenv("USER")
				if responder == "" {
					responder = "anonymous"
				}
			}

			reqBody, _ := json.Marshal(map[string]string{
				"sha":       sha,
				"responder": responder,
				"response":  message,
			})

			addr := getDaemonAddr()
			resp, err := http.Post(addr+"/api/respond", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("failed to add response: %s", body)
			}

			fmt.Println("Response added successfully")
			return nil
		},
	}

	cmd.Flags().StringVar(&responder, "responder", "", "responder name (default: $USER)")
	cmd.Flags().StringVarP(&message, "message", "m", "", "response message (opens editor if not provided)")

	return cmd
}

func addressCmd() *cobra.Command {
	var unaddress bool

	cmd := &cobra.Command{
		Use:   "address <review_id>",
		Short: "Mark a review as addressed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			reviewID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || reviewID <= 0 {
				return fmt.Errorf("invalid review_id: %s", args[0])
			}

			addressed := !unaddress
			reqBody, _ := json.Marshal(map[string]interface{}{
				"review_id": reviewID,
				"addressed": addressed,
			})

			addr := getDaemonAddr()
			resp, err := http.Post(addr+"/api/review/address", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("failed to mark review: %s", body)
			}

			if addressed {
				fmt.Printf("Review %d marked as addressed\n", reviewID)
			} else {
				fmt.Printf("Review %d marked as unaddressed\n", reviewID)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&unaddress, "unaddress", false, "mark as unaddressed instead")

	return cmd
}

func installHookCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "install-hook",
		Short: "Install post-commit hook in current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			hookPath := filepath.Join(root, ".git", "hooks", "post-commit")

			// Check if hook already exists
			if _, err := os.Stat(hookPath); err == nil && !force {
				return fmt.Errorf("hook already exists at %s (use --force to overwrite)", hookPath)
			}

			hookContent := `#!/bin/sh
# RoboRev post-commit hook - auto-reviews every commit
roborev enqueue --sha HEAD 2>/dev/null &
`

			if err := os.WriteFile(hookPath, []byte(hookContent), 0755); err != nil {
				return fmt.Errorf("write hook: %w", err)
			}

			fmt.Printf("Installed post-commit hook at %s\n", hookPath)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing hook")

	return cmd
}

func uninstallHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-hook",
		Short: "Remove post-commit hook from current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			hookPath := filepath.Join(root, ".git", "hooks", "post-commit")

			// Check if hook exists
			content, err := os.ReadFile(hookPath)
			if os.IsNotExist(err) {
				fmt.Println("No post-commit hook found")
				return nil
			} else if err != nil {
				return fmt.Errorf("read hook: %w", err)
			}

			// Check if it contains roborev (case-insensitive)
			hookStr := string(content)
			if !strings.Contains(strings.ToLower(hookStr), "roborev") {
				fmt.Println("Post-commit hook does not contain roborev")
				return nil
			}

			// Remove roborev lines from the hook
			lines := strings.Split(hookStr, "\n")
			var newLines []string
			for _, line := range lines {
				// Skip roborev-related lines (case-insensitive)
				if strings.Contains(strings.ToLower(line), "roborev") {
					continue
				}
				newLines = append(newLines, line)
			}

			// Check if anything remains (besides shebang and empty lines)
			hasContent := false
			for _, line := range newLines {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" && !strings.HasPrefix(trimmed, "#!") {
					hasContent = true
					break
				}
			}

			if hasContent {
				// Write back the hook without roborev lines
				newContent := strings.Join(newLines, "\n")
				if err := os.WriteFile(hookPath, []byte(newContent), 0755); err != nil {
					return fmt.Errorf("write hook: %w", err)
				}
				fmt.Printf("Removed roborev from post-commit hook at %s\n", hookPath)
			} else {
				// Remove the hook entirely
				if err := os.Remove(hookPath); err != nil {
					return fmt.Errorf("remove hook: %w", err)
				}
				fmt.Printf("Removed post-commit hook at %s\n", hookPath)
			}

			return nil
		},
	}
}

func updateCmd() *cobra.Command {
	var checkOnly bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update roborev to the latest version",
		Long: `Check for and install roborev updates.

Shows exactly what will be downloaded and where it will be installed.
Requires confirmation before making changes (use --yes to skip).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Checking for updates...")

			info, err := update.CheckForUpdate(true) // Force check, ignore cache
			if err != nil {
				return fmt.Errorf("check for updates: %w", err)
			}

			if info == nil {
				fmt.Printf("Already running latest version (%s)\n", version.Version)
				return nil
			}

			fmt.Printf("\n  Current version: %s\n", info.CurrentVersion)
			fmt.Printf("  Latest version:  %s\n", info.LatestVersion)
			fmt.Println("\nUpdate available!")
			fmt.Println("\nDownload:")
			fmt.Printf("  URL:  %s\n", info.DownloadURL)
			fmt.Printf("  Size: %s\n", update.FormatSize(info.Size))
			if info.Checksum != "" {
				fmt.Printf("  SHA256: %s\n", info.Checksum)
			}

			// Show install location
			currentExe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("find executable: %w", err)
			}
			currentExe, _ = filepath.EvalSymlinks(currentExe)
			binDir := filepath.Dir(currentExe)

			fmt.Println("\nInstall location:")
			fmt.Printf("  %s\n", binDir)

			if checkOnly {
				return nil
			}

			// Confirm
			if !yes {
				fmt.Print("\nProceed with update? [y/N] ")
				var response string
				fmt.Scanln(&response)
				if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
					fmt.Println("Update cancelled")
					return nil
				}
			}

			fmt.Println()

			// Progress display
			var lastPercent int
			progressFn := func(downloaded, total int64) {
				if total > 0 {
					percent := int(downloaded * 100 / total)
					if percent != lastPercent {
						fmt.Printf("\rDownloading... %d%% (%s / %s)",
							percent, update.FormatSize(downloaded), update.FormatSize(total))
						lastPercent = percent
					}
				}
			}

			// Perform update
			if err := update.PerformUpdate(info, progressFn); err != nil {
				return fmt.Errorf("update failed: %w", err)
			}

			fmt.Printf("\nUpdated to %s\n", info.LatestVersion)

			// Restart daemon if running
			if daemonInfo, err := daemon.ReadRuntime(); err == nil && daemonInfo != nil {
				fmt.Print("Restarting daemon... ")
				// Stop old daemon with timeout
				stopURL := fmt.Sprintf("http://%s/api/shutdown", daemonInfo.Addr)
				client := &http.Client{Timeout: 5 * time.Second}
				if resp, err := client.Post(stopURL, "application/json", nil); err != nil {
					fmt.Printf("warning: failed to stop daemon: %v\n", err)
				} else {
					resp.Body.Close()
				}
				time.Sleep(500 * time.Millisecond)

				// Start new daemon
				daemonPath := filepath.Join(binDir, "roborevd")
				if runtime.GOOS == "windows" {
					daemonPath += ".exe"
				}
				startCmd := exec.Command(daemonPath)
				if err := startCmd.Start(); err != nil {
					fmt.Printf("warning: failed to start daemon: %v\n", err)
				} else {
					fmt.Println("OK")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for updates, don't install")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")

	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show roborev version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("roborev %s\n", version.Version)
		},
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortRef(ref string) string {
	// For ranges like "abc123..def456", show as "abc123..def456" (up to 17 chars)
	// For single SHAs, truncate to 7 chars
	if strings.Contains(ref, "..") {
		if len(ref) > 17 {
			return ref[:17]
		}
		return ref
	}
	return shortSHA(ref)
}
