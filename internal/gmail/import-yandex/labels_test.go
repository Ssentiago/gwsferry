package importyandex

import (
	"testing"
)

// ==========================================
// FIXTURE — реальные данные из migration_labels.json
// ==========================================

var testLabelsJSON = []byte(`{
  "admin@dinord.ru": {
    "messages": {
      "18d4537586ea84b4": ["INBOX", "UNREAD"],
      "18d4537597f69bde": ["INBOX", "Label_1"],
      "18d58fca13787008": ["TRASH"],
      "18d58fca1a2b3c4d": ["SPAM", "Label_1083172799092281365"],
      "18d58fca1e4f5g6h": ["Label_1", "Label_1083172799092281365"]
    },
    "label_names": {
      "INBOX": "INBOX",
      "UNREAD": "UNREAD",
      "Label_1": "[Imap]/Черновики",
      "Label_1083172799092281365": "Сообщения от Test2"
    },
    "done": true
  },
  "partial@dinord.ru": {
    "messages": {
      "aaa": ["INBOX"]
    },
    "label_names": {
      "INBOX": "INBOX"
    },
    "done": false
  }
}`)

// ==========================================
// ПАРСИНГ
// ==========================================

func TestParseLabelsJSON(t *testing.T) {
	file, err := ParseLabelsJSON(testLabelsJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(file) != 2 {
		t.Fatalf("got %d users, want 2", len(file))
	}

	// admin@dinord.ru
	admin, ok := file["admin@dinord.ru"]
	if !ok {
		t.Fatal("admin@dinord.ru not found")
	}
	if !admin.Done {
		t.Error("admin done should be true")
	}
	if len(admin.Messages) != 5 {
		t.Errorf("admin messages: %d, want 5", len(admin.Messages))
	}
	if len(admin.LabelNames) != 4 {
		t.Errorf("admin label_names: %d, want 4", len(admin.LabelNames))
	}

	// Проверяем конкретные лейблы
	labels, ok := admin.Messages["18d4537586ea84b4"]
	if !ok || len(labels) != 2 || labels[0] != "INBOX" || labels[1] != "UNREAD" {
		t.Errorf("msg 18d4537586ea84b4 labels: %v", labels)
	}

	// Проверяем label_names
	name, ok := admin.LabelNames["Label_1"]
	if !ok || name != "[Imap]/Черновики" {
		t.Errorf("Label_1 name: %q", name)
	}

	// partial@dinord.ru (done: false)
	partial, ok := file["partial@dinord.ru"]
	if !ok {
		t.Fatal("partial@dinord.ru not found")
	}
	if partial.Done {
		t.Error("partial done should be false")
	}
}

func TestParseLabelsJSON_Invalid(t *testing.T) {
	_, err := ParseLabelsJSON([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseLabelsJSON_Empty(t *testing.T) {
	file, err := ParseLabelsJSON([]byte(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(file) != 0 {
		t.Errorf("got %d users, want 0", len(file))
	}
}

// ==========================================
// СОПОСТАВЛЕНИЕ (MATCH)
// ==========================================

func TestMatchLetters_Found(t *testing.T) {
	labels, _ := ParseLabelsJSON(testLabelsJSON)
	emails := []EmailMeta{
		{MessageID: "18d4537586ea84b4", Key: "ru/users/admin@dinord.ru/gmail/18d4537586ea84b4.eml"},
		{MessageID: "18d58fca13787008", Key: "ru/users/admin@dinord.ru/gmail/18d58fca13787008.eml"},
	}

	letters, warnings := MatchLetters("admin@dinord.ru", emails, labels)

	if len(letters) != 2 {
		t.Fatalf("got %d letters, want 2", len(letters))
	}
	if len(warnings) != 0 {
		t.Errorf("got %d warnings, want 0", len(warnings))
	}

	// Первое письмо: INBOX + UNREAD
	if letters[0].MsgID != "18d4537586ea84b4" {
		t.Errorf("first letter msgID: %s", letters[0].MsgID)
	}
	if len(letters[0].LabelIDs) != 2 || letters[0].LabelIDs[0] != "INBOX" {
		t.Errorf("first letter labels: %v", letters[0].LabelIDs)
	}

	// Второе письмо: TRASH
	if letters[1].MsgID != "18d58fca13787008" {
		t.Errorf("second letter msgID: %s", letters[1].MsgID)
	}
	if len(letters[1].LabelIDs) != 1 || letters[1].LabelIDs[0] != "TRASH" {
		t.Errorf("second letter labels: %v", letters[1].LabelIDs)
	}
}

func TestMatchLetters_MissingLabels(t *testing.T) {
	labels, _ := ParseLabelsJSON(testLabelsJSON)
	emails := []EmailMeta{
		{MessageID: "18d4537586ea84b4", Key: "ru/users/admin@dinord.ru/gmail/18d4537586ea84b4.eml"},
		{MessageID: "nonexistent", Key: "ru/users/admin@dinord.ru/gmail/nonexistent.eml"},
	}

	letters, warnings := MatchLetters("admin@dinord.ru", emails, labels)

	if len(letters) != 1 {
		t.Fatalf("got %d letters, want 1", len(letters))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warnings))
	}
	if letters[0].MsgID != "18d4537586ea84b4" {
		t.Errorf("letter msgID: %s", letters[0].MsgID)
	}
	// Warning должен содержать путь к письму
	if warnings[0] == "" {
		t.Error("warning should not be empty")
	}
}

func TestMatchLetters_DoneFalse(t *testing.T) {
	labels, _ := ParseLabelsJSON(testLabelsJSON)
	emails := []EmailMeta{
		{MessageID: "aaa", Key: "ru/users/partial@dinord.ru/gmail/aaa.eml"},
		{MessageID: "bbb", Key: "ru/users/partial@dinord.ru/gmail/bbb.eml"},
	}

	letters, warnings := MatchLetters("partial@dinord.ru", emails, labels)

	// "aaa" есть в messages → попадает в letters
	if len(letters) != 1 {
		t.Fatalf("got %d letters, want 1", len(letters))
	}
	// "bbb" нет в messages → warning (done: false, неполный файл)
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warnings))
	}
}

func TestMatchLetters_UnknownEmail(t *testing.T) {
	labels, _ := ParseLabelsJSON(testLabelsJSON)
	emails := []EmailMeta{
		{MessageID: "123", Key: "ru/users/nobody@test.com/gmail/123.eml"},
	}

	letters, warnings := MatchLetters("nobody@test.com", emails, labels)

	if len(letters) != 0 {
		t.Errorf("got %d letters, want 0", len(letters))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warnings))
	}
}

func TestMatchLetters_EmptyEmails(t *testing.T) {
	labels, _ := ParseLabelsJSON(testLabelsJSON)
	letters, warnings := MatchLetters("admin@dinord.ru", nil, labels)

	if len(letters) != 0 {
		t.Errorf("got %d letters, want 0", len(letters))
	}
	if len(warnings) != 0 {
		t.Errorf("got %d warnings, want 0", len(warnings))
	}
}

// ==========================================
// SYSTEM FOLDERS TABLE
// ==========================================

func TestSystemFolders_Order(t *testing.T) {
	if len(systemFolders) == 0 {
		t.Fatal("systemFolders is empty")
	}

	// TRASH must be first
	if systemFolders[0][0] != "TRASH" {
		t.Errorf("first priority: %s, want TRASH", systemFolders[0][0])
	}

	// Verify order: TRASH < SPAM < SENT < DRAFT
	type entry struct{ id, folder string }
	var order []entry
	for _, r := range systemFolders {
		order = append(order, entry{r[0], r[1]})
	}
	if order[0].id != "TRASH" || order[1].id != "SPAM" || order[2].id != "SENT" || order[3].id != "DRAFT" {
		t.Errorf("priority order wrong: %v", order)
	}
}

// ==========================================
// RESOLVE FOLDER
// ==========================================

func TestResolveFolder_TRASH_wins(t *testing.T) {
	got := ResolveFolder([]string{"INBOX", "TRASH"}, nil)
	if got != "Trash" {
		t.Errorf("got %q, want Trash", got)
	}
}

func TestResolveFolder_SPAM_wins(t *testing.T) {
	got := ResolveFolder([]string{"INBOX", "SPAM"}, nil)
	if got != "Spam" {
		t.Errorf("got %q, want Spam", got)
	}
}

func TestResolveFolder_SENT_wins(t *testing.T) {
	got := ResolveFolder([]string{"INBOX", "SENT"}, nil)
	if got != "Sent" {
		t.Errorf("got %q, want Sent", got)
	}
}

func TestResolveFolder_DRAFT_wins(t *testing.T) {
	got := ResolveFolder([]string{"INBOX", "DRAFT"}, nil)
	if got != "Drafts" {
		t.Errorf("got %q, want Drafts", got)
	}
}

func TestResolveFolder_unread_fallback(t *testing.T) {
	// UNREAD — флаг, не папка → fallback INBOX
	got := ResolveFolder([]string{"UNREAD"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX", got)
	}
}

func TestResolveFolder_empty_fallback(t *testing.T) {
	got := ResolveFolder(nil, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX", got)
	}
}

func TestResolveFolder_category_FORUMS_to_INBOX(t *testing.T) {
	got := ResolveFolder([]string{"CATEGORY_FORUMS", "INBOX"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX (CATEGORY_FORUMS is not a folder)", got)
	}
}

func TestResolveFolder_category_PROMOTIONS_to_INBOX(t *testing.T) {
	got := ResolveFolder([]string{"CATEGORY_PROMOTIONS"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX (CATEGORY_PROMOTIONS is not a folder)", got)
	}
}

func TestResolveFolder_category_with_CUSTOM(t *testing.T) {
	// CATEGORY_* + реальный пользовательский лейбл → пользовательский побеждает
	names := map[string]string{"Label_1": "МояПапка"}
	got := ResolveFolder([]string{"CATEGORY_UPDATES", "Label_1"}, names)
	if got != "МояПапка" {
		t.Errorf("got %q, want МояПапка (custom wins over category)", got)
	}
}

func TestResolveFolder_all_categories_to_INBOX(t *testing.T) {
	categories := []string{
		"CATEGORY_PERSONAL", "CATEGORY_SOCIAL", "CATEGORY_PROMOTIONS",
		"CATEGORY_UPDATES", "CATEGORY_FORUMS", "CATEGORY_PURCHASES",
	}
	for _, cat := range categories {
		got := ResolveFolder([]string{cat}, nil)
		if got != "INBOX" {
			t.Errorf("ResolveFolder([%s]) = %q, want INBOX", cat, got)
		}
	}
}

func TestResolveFolder_UNREAD_to_INBOX(t *testing.T) {
	got := ResolveFolder([]string{"UNREAD", "INBOX"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX (UNREAD is a flag, not a folder)", got)
	}
}

func TestResolveFolder_UNREAD_alone(t *testing.T) {
	got := ResolveFolder([]string{"UNREAD"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX", got)
	}
}

func TestResolveFolder_IMPORTANT_to_INBOX(t *testing.T) {
	got := ResolveFolder([]string{"IMPORTANT", "INBOX"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX (IMPORTANT is a flag, not a folder)", got)
	}
}

func TestResolveFolder_STARRED_to_INBOX(t *testing.T) {
	got := ResolveFolder([]string{"STARRED", "INBOX"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX (STARRED is a flag, not a folder)", got)
	}
}

func TestResolveFolder_flags_and_category_combined(t *testing.T) {
	// UNREAD + CATEGORY_FORUMS + INBOX → всё флаги/категории → INBOX
	got := ResolveFolder([]string{"UNREAD", "CATEGORY_FORUMS", "INBOX"}, nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX", got)
	}
}

func TestResolveFolder_flag_beats_custom(t *testing.T) {
	// UNREAD + реальный пользовательский лейбл → custom побеждает
	names := map[string]string{"Label_1": "МояПапка"}
	got := ResolveFolder([]string{"UNREAD", "Label_1"}, names)
	if got != "МояПапка" {
		t.Errorf("got %q, want МояПапка", got)
	}
}

func TestResolveFolder_custom_wins_over_INBOX(t *testing.T) {
	names := map[string]string{"Label_1": "Мои письма"}
	got := ResolveFolder([]string{"INBOX", "Label_1"}, names)
	if got != "Мои письма" {
		t.Errorf("got %q, want Мои письма", got)
	}
}

func TestResolveFolder_custom_deterministic(t *testing.T) {
	// Два пользовательских лейбла — берём первый по алфавиту id
	names := map[string]string{
		"Label_100": "Папка B",
		"Label_1":   "Папка A",
	}
	got := ResolveFolder([]string{"Label_100", "Label_1"}, names)
	if got != "Папка A" {
		t.Errorf("got %q, want Папка A (Label_1 < Label_100)", got)
	}
}

// ==========================================
// RESOLVE USER LABEL
// ==========================================

func TestResolveUserLabel_custom_label(t *testing.T) {
	names := map[string]string{"Label_1": "Сообщения от Test2"}
	got := resolveUserLabel("Label_1", names)
	if got != "Сообщения от Test2" {
		t.Errorf("got %q", got)
	}
}

func TestResolveUserLabel_imap_prefix_chernoviki(t *testing.T) {
	names := map[string]string{"Label_1": "[Imap]/Черновики"}
	got := resolveUserLabel("Label_1", names)
	if got != "Drafts" {
		t.Errorf("got %q, want Drafts", got)
	}
}

func TestResolveUserLabel_imap_prefix_ukhodennye(t *testing.T) {
	names := map[string]string{"Label_2": "[Imap]/Удалённые"}
	got := resolveUserLabel("Label_2", names)
	if got != "Trash" {
		t.Errorf("got %q, want Trash", got)
	}
}

func TestResolveUserLabel_imap_prefix_unknown(t *testing.T) {
	names := map[string]string{"Label_3": "[Imap]/МояПапка"}
	got := resolveUserLabel("Label_3", names)
	if got != "МояПапка" {
		t.Errorf("got %q, want МояПапка", got)
	}
}

func TestResolveUserLabel_unknown_label_fallback(t *testing.T) {
	got := resolveUserLabel("unknown_id", nil)
	if got != "INBOX" {
		t.Errorf("got %q, want INBOX", got)
	}
}

// ==========================================
// RESOLVE FLAGS
// ==========================================

func containsFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

func TestResolveFlags_UNREAD_absent(t *testing.T) {
	flags := ResolveFlags([]string{"INBOX"})
	if !containsFlag(flags, `\Seen`) {
		t.Errorf("expected \\Seen when UNREAD absent, got %v", flags)
	}
}

func TestResolveFlags_UNREAD_present(t *testing.T) {
	flags := ResolveFlags([]string{"INBOX", "UNREAD"})
	if containsFlag(flags, `\Seen`) {
		t.Errorf("should not have \\Seen when UNREAD present, got %v", flags)
	}
}

func TestResolveFlags_STARRED_present(t *testing.T) {
	flags := ResolveFlags([]string{"INBOX", "STARRED"})
	if !containsFlag(flags, `\Flagged`) {
		t.Errorf("expected \\Flagged when STARRED present, got %v", flags)
	}
}

func TestResolveFlags_STARRED_absent(t *testing.T) {
	flags := ResolveFlags([]string{"INBOX"})
	if containsFlag(flags, `\Flagged`) {
		t.Errorf("should not have \\Flagged when STARRED absent, got %v", flags)
	}
}

func TestResolveFlags_empty(t *testing.T) {
	flags := ResolveFlags(nil)
	if !containsFlag(flags, `\Seen`) {
		t.Errorf("empty labels should default to \\Seen, got %v", flags)
	}
	if containsFlag(flags, `\Flagged`) {
		t.Errorf("empty labels should not have \\Flagged, got %v", flags)
	}
}

func TestResolveFlags_UNREAD_and_STARRED(t *testing.T) {
	flags := ResolveFlags([]string{"UNREAD", "STARRED"})
	if containsFlag(flags, `\Seen`) {
		t.Errorf("should not have \\Seen when UNREAD present, got %v", flags)
	}
	if !containsFlag(flags, `\Flagged`) {
		t.Errorf("expected \\Flagged when STARRED present, got %v", flags)
	}
}
