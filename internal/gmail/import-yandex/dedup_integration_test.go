package importyandex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

// TestDedupFetchExistingMsgIDs — интеграционный тест:append письмо с X-Gwsferry-MsgID,
// затем fetchExistingMsgIDs должен его найти. Очищает после себя.
func TestDedupFetchExistingMsgIDs(t *testing.T) {
	cfg := loadTestConfig(t)

	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("Yandex credentials not configured")
	}
	if cfg.Yandex.ClientID == "" || cfg.Yandex.ClientSecret == "" {
		t.Skip("Yandex client credentials not configured")
	}

	// Находим yandex_users.json рядом с бинарём
	dir, _ := os.Getwd()
	usersFile := filepath.Join(dir, "yandex_users.json")
	if _, err := os.Stat(usersFile); err != nil {
		// Пробуем в корне проекта
		for {
			usersFile = filepath.Join(dir, "yandex_users.json")
			if _, err := os.Stat(usersFile); err == nil {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				t.Skip("yandex_users.json not found")
				return
			}
			dir = parent
		}
	}
	users, err := loadTestUsers(usersFile)
	if err != nil || len(users) == 0 {
		t.Skip("yandex_users.json пуст или не найден")
	}

	testEmail := users[0]
	testMsgID := fmt.Sprintf("dedup-test-%d", time.Now().UnixNano())
	t.Logf("test: email=%s msgID=%s", testEmail, testMsgID)

	api := yandexapi.NewAPI(yandexapi.NewClient(cfg.Yandex.OAuthToken), cfg.Yandex.OrgID, cfg.Yandex.OAuthToken)

	// Получаем uid юзера
	allUsers, err := api.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	var testUID yandexapi.UserID
	for _, u := range allUsers {
		if u.Email == testEmail {
			testUID = u.ID
			break
		}
	}
	if testUID == 0 {
		t.Skipf("юзер %s не найден в API", testEmail)
	}

	token, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, testUID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}

	conn, err := ConnectAndAuth(testEmail, token.AccessToken)
	if err != nil {
		t.Fatalf("ConnectAndAuth: %v", err)
	}
	defer Close(conn)

	folder := "INBOX"
	testBody := []byte(fmt.Sprintf("From: test\r\nSubject: dedup test\r\nDate: %s\r\n\r\nBody %s\r\n",
		time.Now().Format(time.RFC1123Z), testMsgID))

	// APPEND с уникальным заголовком
	enriched := injectMsgIDHeader(testBody, testMsgID)
	err = Append(context.Background(), conn, folder, time.Now(), enriched, nil, testMsgID)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	t.Logf("APPEND OK: msgID=%s folder=%s", testMsgID, folder)

	// FETCH existing — должен найти наш msgID
	existing, err := fetchExistingMsgIDs(context.Background(), conn, folder)
	if err != nil {
		t.Fatalf("fetchExistingMsgIDs: %v", err)
	}

	if !existing[testMsgID] {
		t.Errorf("msgID %s не найден в existing (found %d)", testMsgID, len(existing))
	} else {
		t.Logf("dedup OK: msgID=%s найден в existing (total=%d)", testMsgID, len(existing))
	}

	// Очистка: ищем UID и удаляем (retry — после APPEND сервер может не отдать сразу)
	var uid uint32
	for i := 0; i < 3; i++ {
		uid = searchByMsgID(conn, folder, testMsgID)
		if uid > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if uid > 0 {
		Delete(context.Background(), conn, folder, uid)
		t.Logf("cleanup OK: uid=%d удалён", uid)
	} else {
		t.Logf("cleanup: uid для %s не найден после 3 попыток, пропускаю удаление", testMsgID)
	}
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	// Ищем gwsferry.toml рядом с корнем проекта
	dir, _ := os.Getwd()
	for {
		path := dir + "/gwsferry.toml"
		if _, err := os.Stat(path); err == nil {
			cfg, err := config.LoadFromFile(path)
			if err != nil {
				t.Skipf("gwsferry.toml load error: %v", err)
			}
			return cfg
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("gwsferry.toml not found")
	return nil
}

func loadTestUsers(path string) ([]string, error) {
	// Простой парсинг JSON без зависимостей
	raw, err := os.ReadFile(path)
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
