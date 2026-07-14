package importyandex

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/client"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

func readEmails(filePath string) ([]string, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var file struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	return file.Users, nil
}

func findRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "gwsferry.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("gwsferry.toml не найден")
		}
		dir = parent
	}
}

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromFile(filepath.Join(findRoot(t), "gwsferry.toml"))
	if err != nil {
		t.Skipf("конфиг: %v", err)
	}
	return cfg
}

func TestIntegrationIMAPConnect(t *testing.T) {
	cfg := loadCfg(t)
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id")
	}

	// Загружаем юзеров из yandex_users.json + API
	usersFile := filepath.Join(findRoot(t), "yandex_users.json")
	allowed, err := readEmails(usersFile)
	if err != nil || len(allowed) == 0 {
		t.Skip("yandex_users.json пуст или не найден")
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

	var target *yandexapi.User
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			target = &u
			break
		}
	}
	if target == nil {
		t.Skip("нет юзеров из yandex_users.json в API")
	}

	// Обмениваем uid на exchange-токен
	token, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, target.ID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}

	// Подключаемся к IMAP
	imapClient, err := ConnectAndAuth(target.Email, token.AccessToken)
	if err != nil {
		t.Fatalf("ConnectAndAuth: %v", err)
	}
	defer Close(imapClient)

	t.Logf("IMAP подключён: %s", target.Email)

	// List папок через Select
	state, err := imapClient.Select("INBOX", true)
	if err != nil {
		t.Fatalf("Select INBOX: %v", err)
	}
	t.Logf("INBOX: %d messages, uidvalidity=%d", state.Messages, state.UidValidity)
}

func TestIntegrationIMAPListMessages(t *testing.T) {
	cfg := loadCfg(t)
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id")
	}

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

	var target *yandexapi.User
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			target = &u
			break
		}
	}
	if target == nil {
		t.Skip("нет совпадений")
	}

	token, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, target.ID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}

	imapClient, err := ConnectAndAuth(target.Email, token.AccessToken)
	if err != nil {
		t.Fatalf("ConnectAndAuth: %v", err)
	}
	defer Close(imapClient)

	messages, err := List(context.Background(), imapClient, "INBOX")
	if err != nil {
		t.Fatalf("List INBOX: %v", err)
	}

	t.Logf("INBOX: %d messages", len(messages))
	for i, msg := range messages {
		if i >= 3 {
			t.Logf("  ... и ещё %d", len(messages)-3)
			break
		}
		t.Logf("  uid=%d from=%q subject=%q date=%s", msg.UID, msg.From, msg.Subject, msg.InternalDate.Format(time.RFC3339))
	}
}

// connectTestUser — хелпер: загружает конфиг, находит юзера из yandex_users.json,
// обменивает токен, подключается к IMAP. Вызывающий код делает defer Close(c).
func connectTestUser(t *testing.T) (*imaplib.Client, string) {
	t.Helper()
	cfg := loadCfg(t)
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id")
	}
	if cfg.Yandex.ClientID == "" || cfg.Yandex.ClientSecret == "" {
		t.Skip("заполните client_id и client_secret")
	}

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
	var target *yandexapi.User
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			target = &u
			break
		}
	}
	if target == nil {
		t.Skip("нет юзеров из yandex_users.json в API")
	}

	token, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, target.ID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}

	imapClient, err := ConnectAndAuth(target.Email, token.AccessToken)
	if err != nil {
		t.Fatalf("ConnectAndAuth: %v", err)
	}

	t.Logf("подключён: %s", target.Email)
	return imapClient, target.Email
}

func TestIntegrationIMAPAppend(t *testing.T) {
	c, email := connectTestUser(t)
	defer Close(c)

	raw := []byte("From: test@dinord.ru\r\n" +
		"To: " + email + "\r\n" +
		"Subject: gwsferry-integration-test\r\n" +
		"Date: Mon, 14 Jul 2026 12:00:00 +0300\r\n" +
		"Message-ID: <test-gwsferry-001@dinord.ru>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"This is an integration test message for gwsferry IMAP client.\r\n")

	date := time.Date(2026, 7, 14, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	if err := Append(context.Background(), c, "INBOX", date, raw, nil, "integration-test"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	t.Logf("письмо (%d bytes) добавлено в INBOX %s — проверьте почту", len(raw), email)
}

func TestIntegrationIMAPDelete(t *testing.T) {
	c, _ := connectTestUser(t)
	defer Close(c)

	messages, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(messages) == 0 {
		t.Skip("INBOX пуст, сначала запустите TestIntegrationIMAPAppend")
	}

	// Ищем наше тестовое письмо по Subject
	var target *MessageMeta
	for i := range messages {
		if messages[i].Subject == "gwsferry-integration-test" {
			target = &messages[i]
			break
		}
	}
	if target == nil {
		t.Skip("тестовое письмо не найдено в INBOX, сначала запустите TestIntegrationIMAPAppend")
	}

	t.Logf("удаляю uid=%d subject=%q", target.UID, target.Subject)
	if err := Delete(context.Background(), c, "INBOX", target.UID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	t.Logf("письмо удалено")
}

func TestIntegrationIMAPCreateFolder(t *testing.T) {
	c, email := connectTestUser(t)
	defer Close(c)

	// Вычисляем целевые папки для тестовых комбинаций лейблов
	testLetters := []Letter{
		{MsgID: "test1", LabelIDs: []string{"TRASH"}},
		{MsgID: "test2", LabelIDs: []string{"SPAM"}},
		{MsgID: "test3", LabelIDs: []string{"SENT"}},
		{MsgID: "test4", LabelIDs: []string{"DRAFT"}},
		{MsgID: "test5", LabelIDs: []string{"Label_999"}, Path: "test"},
	}

	labelNames := map[string]string{"Label_999": "gwsferry-test-folder"}

	// Собираем уникальные папки
	uniqueFolders := make(map[string]struct{})
	for _, l := range testLetters {
		folder := ResolveFolder(l.LabelIDs, labelNames)
		uniqueFolders[folder] = struct{}{}
		t.Logf("msg=%s labels=%v → folder=%s", l.MsgID, l.LabelIDs, folder)
	}

	// Создаём недостающие папки
	var customFolders []string
	for folder := range uniqueFolders {
		if _, isSystem := SystemFolders[folder]; isSystem {
			continue
		}
		if err := CreateFolderIfNotExists(context.Background(), c, folder); err != nil {
			t.Fatalf("CreateFolderIfNotExists(%s): %v", folder, err)
		}
		customFolders = append(customFolders, folder)
		t.Logf("папка %s: создана", folder)
	}

	t.Logf("все папки созданы для %s — проверьте Yandex Почту", email)

	// Cleanup
	t.Cleanup(func() {
		cleanupClient, _ := connectTestUser(t)
		defer Close(cleanupClient)
		for _, folder := range customFolders {
			if err := DeleteFolder(context.Background(), cleanupClient, folder); err != nil {
				log.Printf("[CLEANUP] DeleteFolder %s: %v", folder, err)
			} else {
				log.Printf("[CLEANUP] папка %s удалена", folder)
			}
		}
	})
}
