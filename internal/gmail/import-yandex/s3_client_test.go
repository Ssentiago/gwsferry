package importyandex

import (
	"path/filepath"
	"testing"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

func loadTestS3Config(t *testing.T) *config.Config {
	t.Helper()
	cfg := loadCfg(t)
	if cfg.S3.Bucket == "" {
		t.Skip("заполните bucket в gwsferry.toml")
	}
	return cfg
}

func testUserEmail(t *testing.T, cfg *config.Config) string {
	t.Helper()
	usersFile := filepath.Join(findRoot(t), "yandex_users.json")
	allowed, err := readEmails(usersFile)
	if err != nil || len(allowed) == 0 {
		t.Skip("yandex_users.json пуст")
	}

	c := yandexapi.NewClient(cfg.Yandex.OAuthToken)
	api := yandexapi.NewAPI(c, cfg.Yandex.OrgID, cfg.Yandex.OAuthToken)
	all, err := api.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	allowedSet := make(map[string]struct{}, len(allowed))
	for _, email := range allowed {
		allowedSet[email] = struct{}{}
	}
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			return u.Email
		}
	}
	t.Skip("нет юзеров из yandex_users.json в API")
	return ""
}

func TestExtractEmailID(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"ru/users/test@dinord.ru/gmail/abc-def.eml", "abc-def"},
		{"ru/users/alice@dinord.ru/gmail/12345.eml", "12345"},
		{"ru/users/bob@dinord.ru/gmail/.eml", ""},
		{"ru/users/charlie@dinord.ru/gmail/noext", "noext"},
		{"some/prefix/file.eml", "file"},
	}
	for _, tt := range tests {
		got := extractEmailID(tt.key)
		if got != tt.want {
			t.Errorf("extractEmailID(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestUserPrefix(t *testing.T) {
	s := &S3Client{prefix: "ru/user/gmail"}
	got := s.UserPrefix("test1@dinord.ru")
	want := "ru/users/test1@dinord.ru/gmail/"
	if got != want {
		t.Errorf("UserPrefix = %q, want %q", got, want)
	}
}

// ==========================================
// INTEGRATION (реальный S3)
// ==========================================

func TestIntegrationS3ListEmails(t *testing.T) {
	cfg := loadTestS3Config(t)
	if cfg == nil {
		return
	}

	client, err := NewS3Client(cfg)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}

	email := testUserEmail(t, cfg)
	if email == "" {
		t.Skip("нет тестового юзера")
	}

	emails, err := client.ListEmails(t.Context(), email)
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	t.Logf("email=%s: %d emails в S3", email, len(emails))
	for i, e := range emails {
		if i >= 3 {
			t.Logf("  ... и ещё %d", len(emails)-3)
			break
		}
		t.Logf("  id=%s size=%d key=%s", e.MessageID, e.Size, e.Key)
	}
}

func TestIntegrationS3GetEmail(t *testing.T) {
	cfg := loadTestS3Config(t)
	if cfg == nil {
		return
	}

	client, err := NewS3Client(cfg)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}

	email := testUserEmail(t, cfg)
	if email == "" {
		t.Skip("нет тестового юзера")
	}

	emails, err := client.ListEmails(t.Context(), email)
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if len(emails) == 0 {
		t.Skip("нет emails в S3 для тестового юзера")
	}

	data, err := client.GetEmail(t.Context(), emails[0].Key)
	if err != nil {
		t.Fatalf("GetEmail: %v", err)
	}
	t.Logf("key=%s size=%d bytes", emails[0].Key, len(data))
}
