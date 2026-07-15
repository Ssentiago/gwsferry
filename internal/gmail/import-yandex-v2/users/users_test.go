package users

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

// ==========================================
// loadEmails (парсинг JSON-файла)
// ==========================================

func TestLoadEmails_Valid(t *testing.T) {
	dir := t.TempDir()
 path := filepath.Join(dir, "yandex_users.json")
	os.WriteFile(path, []byte(`{"users":["a@test.com","b@test.com"]}`), 0644)

	emails, err := loadEmails(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emails) != 2 || emails[0] != "a@test.com" || emails[1] != "b@test.com" {
		t.Errorf("got %v, want [a@test.com b@test.com]", emails)
	}
}

func TestLoadEmails_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yandex_users.json")
	os.WriteFile(path, []byte(`{"users":["a@test.com","a@test.com","b@test.com"]}`), 0644)

	emails, err := loadEmails(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emails) != 2 {
		t.Errorf("got %d emails, want 2 (deduped)", len(emails))
	}
}

func TestLoadEmails_EmptyList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yandex_users.json")
	os.WriteFile(path, []byte(`{"users":[]}`), 0644)

	emails, err := loadEmails(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emails) != 0 {
		t.Errorf("got %d emails, want 0", len(emails))
	}
}

func TestLoadEmails_FileNotFound(t *testing.T) {
	_, err := loadEmails("/nonexistent/path/file.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadEmails_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yandex_users.json")
	os.WriteFile(path, []byte(`not json`), 0644)

	_, err := loadEmails(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadEmails_WrongStructure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yandex_users.json")
	os.WriteFile(path, []byte(`{"wrong_key":["a@test.com"]}`), 0644)

	emails, err := loadEmails(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emails) != 0 {
		t.Errorf("got %d emails, want 0 (wrong key)", len(emails))
	}
}

// ==========================================
// FILTERING (LoadUsers через in-memory API)
// ==========================================

func filterUsers(all []yandexapi.User, allowedEmails []string) []yandexapi.User {
	allowedSet := make(map[string]struct{}, len(allowedEmails))
	for _, email := range allowedEmails {
		allowedSet[email] = struct{}{}
	}
	var matched []yandexapi.User
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			matched = append(matched, u)
		}
	}
	return matched
}

func TestFilterUsers_Match(t *testing.T) {
	all := []yandexapi.User{
		{Email: "a@test.com", ID: 1},
		{Email: "b@test.com", ID: 2},
		{Email: "c@test.com", ID: 3},
	}
	allowed := []string{"a@test.com", "c@test.com"}

	matched := filterUsers(all, allowed)
	if len(matched) != 2 {
		t.Fatalf("got %d, want 2", len(matched))
	}
	if matched[0].Email != "a@test.com" || matched[1].Email != "c@test.com" {
		t.Errorf("got %v", matched)
	}
}

func TestFilterUsers_NoMatch(t *testing.T) {
	all := []yandexapi.User{
		{Email: "a@test.com", ID: 1},
	}
	allowed := []string{"nobody@test.com"}

	matched := filterUsers(all, allowed)
	if len(matched) != 0 {
		t.Errorf("got %d, want 0", len(matched))
	}
}

func TestFilterUsers_EmptyAllowed(t *testing.T) {
	all := []yandexapi.User{
		{Email: "a@test.com", ID: 1},
	}
	matched := filterUsers(all, []string{})
	if len(matched) != 0 {
		t.Errorf("got %d, want 0", len(matched))
	}
}

func TestFilterUsers_CaseSensitive(t *testing.T) {
	all := []yandexapi.User{
		{Email: "User@Test.com", ID: 1},
	}
	allowed := []string{"user@test.com"}

	matched := filterUsers(all, allowed)
	if len(matched) != 0 {
		t.Errorf("email matching should be case-sensitive, got %d matches", len(matched))
	}
}

// ==========================================
// INTEGRATION (реальный API, из gwsferry.toml)
// ==========================================

func findProjectRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "gwsferry.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("gwsferry.toml не найден в родительских директориях")
		}
		dir = parent
	}
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfgPath := filepath.Join(findProjectRoot(t), "gwsferry.toml")
	cfg, err := config.LoadFromFile(cfgPath)
	if err != nil {
		t.Skipf("не удалось загрузить конфиг: %v", err)
	}
	return cfg
}

func TestIntegrationListUsers(t *testing.T) {
	cfg := loadTestConfig(t)
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id в gwsferry.toml")
	}

	c := yandexapi.NewClient(cfg.Yandex.OAuthToken)
	api := yandexapi.NewAPI(c, cfg.Yandex.OrgID, cfg.Yandex.OAuthToken)
	users, err := api.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	t.Logf("got %d users", len(users))
	for _, u := range users[:min(3, len(users))] {
		t.Logf("  id=%d email=%s", u.ID, u.Email)
	}
}

func TestIntegrationExchangeToken(t *testing.T) {
	cfg := loadTestConfig(t)
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id в gwsferry.toml")
	}
	if cfg.Yandex.ClientID == "" || cfg.Yandex.ClientSecret == "" {
		t.Skip("заполните client_id и client_secret в gwsferry.toml для ExchangeToken")
	}

	// Загружаем список юзеров из yandex_users.json
	usersFile := filepath.Join(findProjectRoot(t), "yandex_users.json")
	if _, err := os.Stat(usersFile); os.IsNotExist(err) {
		t.Skipf("создайте %s со списком юзеров", usersFile)
	}

	allowed, err := loadEmails(usersFile)
	if err != nil {
		t.Fatalf("loadEmails: %v", err)
	}
	if len(allowed) == 0 {
		t.Skip("yandex_users.json пуст")
	}

	// Получаем полный список из API и находим первого совпавшего
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

	var target *yandexapi.User
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			target = &u
			break
		}
	}
	if target == nil {
		t.Skip("ни один юзер из yandex_users.json не найден в API")
	}

	t.Logf("ExchangeToken для %s (id=%d)", target.Email, target.ID)
	exchanged, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, target.ID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}
	t.Logf("got token expires_in=%d", exchanged.ExpiresIn)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
