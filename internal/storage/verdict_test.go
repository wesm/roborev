package storage

import "testing"

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "no issues found",
			output: "No issues found. This commit adds a new feature.",
			want:   "P",
		},
		{
			name:   "no issues found lowercase",
			output: "no issues found. The code looks good.",
			want:   "P",
		},
		{
			name:   "no issues",
			output: "I found no issues with this commit.",
			want:   "P",
		},
		{
			name:   "no findings",
			output: "No findings to report.",
			want:   "P",
		},
		{
			name:   "has findings",
			output: "Medium - Bug in line 42\nThe code has issues.",
			want:   "F",
		},
		{
			name:   "empty output",
			output: "",
			want:   "F",
		},
		{
			name:   "ambiguous",
			output: "The commit looks mostly fine but could use some cleanup.",
			want:   "F",
		},
		{
			name:   "mixed case no issues found",
			output: "NO ISSUES FOUND. Great work!",
			want:   "P",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVerdict(tt.output)
			if got != tt.want {
				t.Errorf("parseVerdict() = %q, want %q", got, tt.want)
			}
		})
	}
}
