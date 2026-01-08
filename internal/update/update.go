package update

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/wesm/roborev/internal/version"
)

const (
	githubAPIURL  = "https://api.github.com/repos/wesm/roborev/releases/latest"
	cacheFileName = "update_check.json"
	cacheDuration = 1 * time.Hour
)

// Release represents a GitHub release
type Release struct {
	TagName string  `json:"tag_name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset
type Asset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// UpdateInfo contains information about an available update
type UpdateInfo struct {
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
	Size           int64
	Checksum       string // SHA256 if available
}

// cachedCheck stores the last update check result
type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

// CheckForUpdate checks if a newer version is available
// Uses a 1-hour cache to avoid hitting GitHub API too often
func CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	currentVersion := strings.TrimPrefix(version.Version, "v")

	// Check cache first (unless forced)
	if !forceCheck {
		if cached, err := loadCache(); err == nil {
			if time.Since(cached.CheckedAt) < cacheDuration {
				latestVersion := strings.TrimPrefix(cached.Version, "v")
				if !isNewer(latestVersion, currentVersion) {
					return nil, nil // Up to date (cached)
				}
				// Cache says update available, fetch fresh info
			}
		}
	}

	// Fetch latest release from GitHub
	release, err := fetchLatestRelease()
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}

	// Save to cache
	saveCache(release.TagName)

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	if !isNewer(latestVersion, currentVersion) {
		return nil, nil // Up to date
	}

	// Find the right asset for this platform
	assetName := fmt.Sprintf("roborev_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var asset *Asset
	for _, a := range release.Assets {
		if a.Name == assetName {
			asset = &a
			break
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	// Try to extract checksum from release body
	checksum := extractChecksum(release.Body, assetName)

	return &UpdateInfo{
		CurrentVersion: version.Version,
		LatestVersion:  release.TagName,
		DownloadURL:    asset.BrowserDownloadURL,
		AssetName:      asset.Name,
		Size:           asset.Size,
		Checksum:       checksum,
	}, nil
}

// PerformUpdate downloads and installs the update
func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	// 1. Download to temp file
	fmt.Printf("Downloading %s...\n", info.AssetName)
	tempDir, err := os.MkdirTemp("", "roborev-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, info.AssetName)
	checksum, err := downloadFile(info.DownloadURL, archivePath, info.Size, progressFn)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// 2. Verify checksum if available
	if info.Checksum != "" {
		fmt.Printf("Verifying checksum... ")
		if checksum != info.Checksum {
			fmt.Println("FAILED")
			return fmt.Errorf("checksum mismatch: expected %s, got %s", info.Checksum, checksum)
		}
		fmt.Println("OK")
	}

	// 3. Extract archive
	fmt.Println("Extracting...")
	extractDir := filepath.Join(tempDir, "extracted")
	if err := extractTarGz(archivePath, extractDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// 4. Find current binary locations
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}
	binDir := filepath.Dir(currentExe)

	// 5. Install new binaries
	binaries := []string{"roborev", "roborevd"}
	if runtime.GOOS == "windows" {
		binaries = []string{"roborev.exe", "roborevd.exe"}
	}

	for _, binary := range binaries {
		srcPath := filepath.Join(extractDir, binary)
		dstPath := filepath.Join(binDir, binary)
		backupPath := dstPath + ".backup"

		// Check if source exists
		if _, err := os.Stat(srcPath); os.IsNotExist(err) {
			continue // Skip if not in archive
		}

		fmt.Printf("Installing %s... ", binary)

		// Backup existing
		if _, err := os.Stat(dstPath); err == nil {
			if err := os.Rename(dstPath, backupPath); err != nil {
				return fmt.Errorf("backup %s: %w", binary, err)
			}
		}

		// Copy new binary
		if err := copyFile(srcPath, dstPath); err != nil {
			// Try to restore backup
			os.Rename(backupPath, dstPath)
			return fmt.Errorf("install %s: %w", binary, err)
		}

		// Set executable permission
		if err := os.Chmod(dstPath, 0755); err != nil {
			return fmt.Errorf("chmod %s: %w", binary, err)
		}

		// Remove backup
		os.Remove(backupPath)

		fmt.Println("OK")
	}

	return nil
}

// RestartDaemon stops and starts the daemon
func RestartDaemon() error {
	// Find roborevd and restart it
	// We do this by calling the daemon restart command
	// Since we're in a library, we'll just return instructions
	// The CLI will handle the actual restart
	return nil
}

// GetCacheDir returns the roborev cache directory
func GetCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roborev")
}

func fetchLatestRelease() (*Release, error) {
	req, err := http.NewRequest("GET", githubAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "roborev/"+version.Version)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	return &release, nil
}

func downloadFile(url, dest string, totalSize int64, progressFn func(downloaded, total int64)) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()

	// Calculate checksum while downloading
	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)

	// Download with progress
	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := writer.Write(buf[:n])
			if writeErr != nil {
				return "", writeErr
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, totalSize)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func extractTarGz(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
			if err := os.Chmod(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		}
	}

	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Close()
}

func extractChecksum(releaseBody, assetName string) string {
	// Look for checksum in release notes
	// Format: "assetname: checksum" or "checksum  assetname"
	lines := strings.Split(releaseBody, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, assetName) {
			// Try "filename: checksum" format
			re := regexp.MustCompile(`[a-f0-9]{64}`)
			if match := re.FindString(line); match != "" {
				return match
			}
		}
	}
	return ""
}

func loadCache() (*cachedCheck, error) {
	cachePath := filepath.Join(GetCacheDir(), cacheFileName)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var cached cachedCheck
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

func saveCache(version string) {
	cached := cachedCheck{
		CheckedAt: time.Now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	cachePath := filepath.Join(GetCacheDir(), cacheFileName)
	os.MkdirAll(filepath.Dir(cachePath), 0755)
	os.WriteFile(cachePath, data, 0644)
}

// isNewer returns true if v1 is newer than v2
// Assumes semver format: major.minor.patch
func isNewer(v1, v2 string) bool {
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	for i := 0; i < 3; i++ {
		var n1, n2 int
		if i < len(parts1) {
			fmt.Sscanf(parts1[i], "%d", &n1)
		}
		if i < len(parts2) {
			fmt.Sscanf(parts2[i], "%d", &n2)
		}
		if n1 > n2 {
			return true
		}
		if n1 < n2 {
			return false
		}
	}
	return false
}

// FormatSize formats bytes as human-readable string
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
