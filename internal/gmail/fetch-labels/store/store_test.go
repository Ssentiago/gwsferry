package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NewFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")

	data := `{
		"user1@test.com": {
			"messages": {"msg1": ["Label_1"], "msg2": ["Label_2"]},
			"label_names": {"Label_1": "Inbox"},
			"done": true
		},
		"user2@test.com": {
			"messages": {"msg3": ["Label_3"]},
			"label_names": {},
			"done": false
		}
	}`
	os.WriteFile(path, []byte(data), 0644)

	st := New(path)
	done, partial, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if done != 1 {
		t.Errorf("done=%d, want 1", done)
	}
	if partial != 1 {
		t.Errorf("partial=%d, want 1", partial)
	}

	// Проверяем данные.
	cached := st.CachedLabels("user1@test.com")
	if len(cached) != 2 {
		t.Errorf("user1 messages=%d, want 2", len(cached))
	}
	if cached["msg1"][0] != "Label_1" {
		t.Errorf("msg1 label=%v, want [Label_1]", cached["msg1"])
	}

	names := st.CachedLabelNames("user1@test.com")
	if names["Label_1"] != "Inbox" {
		t.Errorf("label name=%v, want Inbox", names["Label_1"])
	}
}

func TestLoad_NoFile(t *testing.T) {
	st := New("/nonexistent/labels.json")
	done, partial, err := st.Load()
	if err != nil {
		t.Fatalf("Load nonexistent: %v", err)
	}
	if done != 0 || partial != 0 {
		t.Errorf("done=%d partial=%d, want 0,0", done, partial)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")

	st := New(path)
	st.SaveMsgLabels("user@test.com", "msg1", []string{"Label_1"})
	st.SaveMsgLabels("user@test.com", "msg2", []string{"Label_2", "Label_3"})
	st.FinalizeUser("user@test.com", map[string]string{"Label_1": "Inbox"})

	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Загружаем заново.
	st2 := New(path)
	done, partial, err := st2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// FinalizeUser не ставит done=true — готовность через IsUserCollected.
	if done != 0 {
		t.Errorf("done=%d, want 0", done)
	}
	if partial != 1 {
		t.Errorf("partial=%d, want 1", partial)
	}

	cached := st2.CachedLabels("user@test.com")
	if len(cached) != 2 {
		t.Errorf("messages=%d, want 2", len(cached))
	}
}

func TestIsUserCollected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")

	st := New(path)

	// С индексом — сравнение collected vs expected.
	st.SaveMsgLabels("user@test.com", "msg1", []string{"Label_1"})
	st.SetMsgIndex("user@test.com", []string{"msg1", "msg2"})
	if st.IsUserCollected("user@test.com") {
		t.Error("expected not collected (1/2)")
	}

	st.SaveMsgLabels("user@test.com", "msg2", []string{"Label_2"})
	if !st.IsUserCollected("user@test.com") {
		t.Error("expected collected (2/2)")
	}

	// Нет индекса — fallback на done.
	st2 := New(path)
	st2.mu.Lock()
	st2.data["fallback@test.com"] = &User{
		Messages:   map[string][]string{"msg1": {"L1"}},
		LabelNames: map[string]string{},
		Done:       true,
	}
	st2.mu.Unlock()
	if !st2.IsUserCollected("fallback@test.com") {
		t.Error("expected collected (done=true fallback)")
	}
}

func TestSaveMsgIndexAndLoad(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "msg_index.json")

	st := New(filepath.Join(dir, "labels.json"))
	st.SetMsgIndex("user@test.com", []string{"msg1", "msg2", "msg3"})
	st.SetMsgIndex("other@test.com", []string{"msgA"})

	if err := st.SaveMsgIndex(idxPath); err != nil {
		t.Fatalf("SaveMsgIndex: %v", err)
	}

	st2 := New(filepath.Join(dir, "labels.json"))
	if err := st2.LoadMsgIndex(idxPath); err != nil {
		t.Fatalf("LoadMsgIndex: %v", err)
	}

	ids := st2.ExpectedMsgIDs("user@test.com")
	if len(ids) != 3 {
		t.Errorf("expected 3 msg_ids, got %d", len(ids))
	}

	ids = st2.ExpectedMsgIDs("unknown@test.com")
	if len(ids) != 0 {
		t.Errorf("expected 0 for unknown, got %d", len(ids))
	}
}

func TestConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")

	st := New(path)
	st.SaveMsgLabels("user@test.com", "init", []string{"L0"})

	// Параллельная запись.
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				st.SaveMsgLabels("user@test.com", "msg", []string{"Label"})
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}

	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}
