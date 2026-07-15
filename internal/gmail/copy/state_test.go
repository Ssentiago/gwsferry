package copy

import (
	"path/filepath"
	"testing"
)

func TestLoadSaveState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	st := loadState(path)
	if st.Users == nil {
		t.Fatal("users map is nil")
	}

	st.Users["user@test.com"] = "done"
	st.Users["other@test.com"] = "pending"
	saveState(st, path)

	st2 := loadState(path)
	if st2.Users["user@test.com"] != "done" {
		t.Errorf("expected done, got %s", st2.Users["user@test.com"])
	}
	if st2.Users["other@test.com"] != "pending" {
		t.Errorf("expected pending, got %s", st2.Users["other@test.com"])
	}
}

func TestGetRSSMB(t *testing.T) {
	rss := getRSSMB()
	if rss < 0 {
		t.Error("RSS should not be negative")
	}
}

func TestAdaptiveBatchSize(t *testing.T) {
	abs := newAdaptiveBatchSize()
	if abs.current != batchSizeStart {
		t.Errorf("expected start=%d, got %d", batchSizeStart, abs.current)
	}

	abs.shrink()
	expected := 12
	if abs.current != expected {
		t.Errorf("after shrink: expected %d, got %d", expected, abs.current)
	}

	for i := 0; i < batchGrowthStreak; i++ {
		abs.reportCleanBatch()
	}
	if abs.current != expected+batchGrowthStep {
		t.Errorf("after growth: expected %d, got %d", expected+batchGrowthStep, abs.current)
	}
}

func TestParseGoogleErrorReason(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{"daily limit", "daily_limit"},
		{"rate limit", "rate_limit"},
		{"concurrent limit", "concurrent_limit"},
		{"other", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseGoogleErrorReason(nil)
			if result == "" {
				t.Error("expected non-empty result")
			}
		})
	}
}

func TestDecodeBase64URL(t *testing.T) {
	data, err := decodeBase64URL("SGVsbG8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "Hello" {
		t.Errorf("expected Hello, got %s", string(data))
	}

	_, err = decodeBase64URL("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}
