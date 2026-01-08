package update

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeTarPath(t *testing.T) {
	destDir := "/tmp/extract"

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"normal file", "roborev", false},
		{"nested file", "bin/roborev", false},
		{"absolute path", "/etc/passwd", true},
		{"path traversal with ..", "../../../etc/passwd", true},
		{"path traversal mid-path", "foo/../../../etc/passwd", true},
		{"hidden traversal", "foo/bar/../../..", true},
		{"dot only", ".", false},
		{"double dot only", "..", true},
		{"empty path", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sanitizeTarPath(destDir, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("sanitizeTarPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestExtractTarGzPathTraversal(t *testing.T) {
	// Create a malicious tar.gz with path traversal
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "malicious.tar.gz")
	extractDir := filepath.Join(tmpDir, "extract")
	outsideFile := filepath.Join(tmpDir, "pwned")

	// Create archive with path traversal attempt
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)

	// Add a malicious entry that tries to escape
	header := &tar.Header{
		Name: "../pwned",
		Mode: 0644,
		Size: 5,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("owned")); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gzw.Close()
	f.Close()

	// Extract should fail
	err = extractTarGz(archivePath, extractDir)
	if err == nil {
		t.Error("extractTarGz should fail with path traversal attempt")
	}

	// Verify the file wasn't created outside
	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Error("Malicious file was created outside extract dir")
	}
}

func TestExtractTarGzSymlinkSkipped(t *testing.T) {
	// Create a tar.gz with a symlink
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "symlink.tar.gz")
	extractDir := filepath.Join(tmpDir, "extract")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)

	// Add a symlink entry
	header := &tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}

	// Add a normal file
	header = &tar.Header{
		Name: "normal.txt",
		Mode: 0644,
		Size: 4,
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gzw.Close()
	f.Close()

	// Extract should succeed (symlinks are skipped)
	if err := extractTarGz(archivePath, extractDir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	// Normal file should exist
	if _, err := os.Stat(filepath.Join(extractDir, "normal.txt")); err != nil {
		t.Error("Normal file should have been extracted")
	}

	// Symlink should not exist
	if _, err := os.Lstat(filepath.Join(extractDir, "evil-link")); !os.IsNotExist(err) {
		t.Error("Symlink should have been skipped")
	}
}

func TestExtractChecksum(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		assetName string
		want      string
	}{
		{
			name:      "standard sha256sum format",
			body:      "abc123def456789012345678901234567890123456789012345678901234abcd  roborev_darwin_arm64.tar.gz",
			assetName: "roborev_darwin_arm64.tar.gz",
			want:      "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:      "uppercase checksum",
			body:      "ABC123DEF456789012345678901234567890123456789012345678901234ABCD  roborev_linux_amd64.tar.gz",
			assetName: "roborev_linux_amd64.tar.gz",
			want:      "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:      "mixed case checksum",
			body:      "AbC123DeF456789012345678901234567890123456789012345678901234aBcD  roborev_darwin_amd64.tar.gz",
			assetName: "roborev_darwin_amd64.tar.gz",
			want:      "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:      "colon format",
			body:      "roborev_darwin_arm64.tar.gz: abc123def456789012345678901234567890123456789012345678901234abcd",
			assetName: "roborev_darwin_arm64.tar.gz",
			want:      "abc123def456789012345678901234567890123456789012345678901234abcd",
		},
		{
			name:      "multiline with target in middle",
			body:      "abc123def456789012345678901234567890123456789012345678901234aaaa  roborev_linux_amd64.tar.gz\nabc123def456789012345678901234567890123456789012345678901234bbbb  roborev_darwin_arm64.tar.gz\nabc123def456789012345678901234567890123456789012345678901234cccc  roborev_darwin_amd64.tar.gz",
			assetName: "roborev_darwin_arm64.tar.gz",
			want:      "abc123def456789012345678901234567890123456789012345678901234bbbb",
		},
		{
			name:      "no match",
			body:      "abc123def456789012345678901234567890123456789012345678901234abcd  roborev_linux_amd64.tar.gz",
			assetName: "roborev_darwin_arm64.tar.gz",
			want:      "",
		},
		{
			name:      "empty body",
			body:      "",
			assetName: "roborev_darwin_arm64.tar.gz",
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChecksum(tt.body, tt.assetName)
			if got != tt.want {
				t.Errorf("extractChecksum() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		v1, v2 string
		want   bool
	}{
		{"1.0.0", "0.9.0", true},
		{"1.1.0", "1.0.0", true},
		{"1.0.1", "1.0.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"0.9.0", "1.0.0", false},
		{"v1.0.0", "v0.9.0", true},
		{"v1.0.0", "0.9.0", true},
		{"1.0.0", "v0.9.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.v1+"_vs_"+tt.v2, func(t *testing.T) {
			got := isNewer(tt.v1, tt.v2)
			if got != tt.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tt.v1, tt.v2, got, tt.want)
			}
		})
	}
}
