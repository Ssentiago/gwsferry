package importyandex

import (
	"testing"
)

func TestInjectMsgIDHeader(t *testing.T) {
	raw := []byte("From: test\r\nSubject: hello\r\n\r\nBody here")
	result := injectMsgIDHeader(raw, "msg-123")
	if len(result) <= len(raw) {
		t.Errorf("expected header to be injected, got %d bytes (original %d)", len(result), len(raw))
	}
}

func TestContainsStr(t *testing.T) {
	if !containsStr([]string{"a", "b", "c"}, "b") {
		t.Error("expected true")
	}
	if containsStr([]string{"a", "b"}, "x") {
		t.Error("expected false")
	}
}

func TestFilterOut(t *testing.T) {
	got := filterOut([]string{`\Seen`, `\Flagged`}, `\Flagged`)
	if len(got) != 1 || got[0] != `\Seen` {
		t.Errorf("got %v, want [\\Seen]", got)
	}
}
