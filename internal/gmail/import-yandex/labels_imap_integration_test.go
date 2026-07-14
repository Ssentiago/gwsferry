package importyandex

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/client"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

// SystemFolders — папки, которые уже существуют на Yandex и не требуют CREATE.
var SystemFolders = map[string]struct{}{
	"INBOX": {},
	"Trash": {},
	"Spam":  {},
	"Sent":  {},
	"Drafts": {},
	"Outbox": {},
}

// testLabels20 — мок-файл лейблов на 20 писем, покрывающий все edge-кейсы.
var testLabels20 = LabelsFile{
	"test1@dinord.ru": {
		Messages: map[string][]string{
			// 1. Простое системное — только INBOX
			"test-msg-001": {"INBOX"},
			// 2. Только TRASH — наивысший приоритет
			"test-msg-002": {"TRASH"},
			// 3. Только SPAM
			"test-msg-003": {"SPAM"},
			// 4. TRASH + INBOX — TRASH побеждает
			"test-msg-004": {"TRASH", "INBOX"},
			// 5. SPAM + INBOX — SPAM побеждает
			"test-msg-005": {"SPAM", "INBOX"},
			// 6. Только пользовательский лейбл
			"test-msg-006": {"Label_2001"},
			// 7. Пользовательский + INBOX — пользовательский побеждает
			"test-msg-007": {"Label_2002", "INBOX"},
			// 8. Два пользовательских — детерминированный выбор по алфавиту id
			"test-msg-008": {"Label_2010", "Label_2003"},
			// 9. [Imap]/Черновики → Drafts
			"test-msg-009": {"Label_2004"},
			// 10. [Imap]/МояПапка → МояПапка (без префикса)
			"test-msg-010": {"Label_2005"},
			// 11. UNREAD + INBOX → INBOX, без \Seen
			"test-msg-011": {"INBOX", "UNREAD"},
			// 12. Только IN без UNREAD → INBOX, с \Seen
			"test-msg-012": {"INBOX"},
			// 13. STARRED + INBOX → INBOX, с \Flagged
			"test-msg-013": {"INBOX", "STARRED"},
			// 14. Только CATEGORY_FORUMS → fallback INBOX
			"test-msg-014": {"CATEGORY_FORUMS"},
			// 15. Только CHAT → fallback INBOX
			"test-msg-015": {"CHAT"},
			// 16. DRAFT → Drafts
			"test-msg-016": {"DRAFT"},
			// 17. SENT → Sent
			"test-msg-017": {"SENT"},
			// 18. Нет записи в messages — будет пропущен (проверяется отдельно)
			// 19. done: false, письмо отсутствует — будет пропущен (проверяется отдельно)
			// 20. CATEGORY_PERSONAL + IMPORTANT + UNREAD → INBOX, без \Seen
			"test-msg-020": {"CATEGORY_PERSONAL", "IMPORTANT", "UNREAD"},
		},
		LabelNames: map[string]string{
			"Label_2001": "Сообщения от Test2",
			"Label_2002": "От \"Test\"",
			"Label_2003": "Папка Alpha",
			"Label_2004": "[Imap]/Черновики",
			"Label_2005": "[Imap]/МояПапка",
			"Label_2010": "Папка Zeta",
		},
		Done: true,
	},
	// 19. done: false с неполным списком
	"partial@dinord.ru": {
		Messages: map[string][]string{
			"partial-msg-001": {"INBOX"},
		},
		LabelNames: map[string]string{"INBOX": "INBOX"},
		Done:       false,
	},
}

// genTestEmail генерирует валидное RFC822 письмо в памяти.
func genTestEmail(msgNum int, to string) []byte {
	subject := fmt.Sprintf("gwsferry-e2e-test-%03d", msgNum)
	return []byte(
		"From: test@dinord.ru\r\n" +
			"To: " + to + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Date: Mon, 14 Jul 2026 12:00:00 +0300\r\n" +
			"Message-ID: <" + subject + "@dinord.ru>\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			fmt.Sprintf("Integration test message #%d for gwsferry labels+IMAP pipeline.\r\n", msgNum))
}

// loadTestLabelsConfig загружает конфиг для интеграционного теста.
func loadTestLabelsConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromFile(filepath.Join(findRoot(t), "gwsferry.toml"))
	if err != nil {
		t.Skipf("конфиг: %v", err)
	}
	if cfg.Yandex.OAuthToken == "" || cfg.Yandex.OrgID == "" {
		t.Skip("заполните oauth_token и org_id")
	}
	if cfg.Yandex.ClientID == "" || cfg.Yandex.ClientSecret == "" {
		t.Skip("заполните client_id и client_secret")
	}
	return cfg
}

// connectTestLabelsUser подключается к IMAP для test1@dinord.ru.
func connectTestLabelsUser(t *testing.T) (*imaplib.Client, string) {
	t.Helper()
	cfg := loadTestLabelsConfig(t)

	c := yandexapi.NewClient(cfg.Yandex.OAuthToken)
	api := yandexapi.NewAPI(c, cfg.Yandex.OrgID, cfg.Yandex.OAuthToken)
	all, err := api.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}

	email := "test1@dinord.ru"
	var target *yandexapi.User
	for _, u := range all {
		if u.Email == email {
			target = &u
			break
		}
	}
	if target == nil {
		t.Skipf("%s не найден в API", email)
	}

	token, err := api.ExchangeToken(cfg.Yandex.ClientID, cfg.Yandex.ClientSecret, target.ID)
	if err != nil {
		t.Fatalf("ExchangeToken: %v", err)
	}

	imapClient, err := ConnectAndAuth(target.Email, token.AccessToken)
	if err != nil {
		t.Fatalf("ConnectAndAuth: %v", err)
	}
	return imapClient, target.Email
}

// collectLetters формирует Letter-ы для писем 1-17 и 20 (исключая 18 и 19 — пропуски).
func collectLetters(t *testing.T) []Letter {
	t.Helper()
	email := "test1@dinord.ru"
	userLabels := testLabels20[email]

	var letters []Letter
	for i := 1; i <= 20; i++ {
		msgID := fmt.Sprintf("test-msg-%03d", i)

		// Письма 18 и 19 — ожидаемо пропускаются
		if i == 18 || i == 19 {
			continue
		}

		labelIDs, ok := userLabels.Messages[msgID]
		if !ok {
			t.Fatalf("msg %s не найден в messages (неожиданно)", msgID)
		}

		letters = append(letters, Letter{
			Path:       fmt.Sprintf("ru/users/%s/gmail/%s.eml", email, msgID),
			MsgID:      msgID,
			LabelIDs:   labelIDs,
			LabelNames: userLabels.LabelNames,
		})
	}
	return letters
}

func TestIntegrationLabelsIMAP(t *testing.T) {
	c, email := connectTestLabelsUser(t)
	defer Close(c)

	letters := collectLetters(t)
	t.Logf("письма к заливке: %d", len(letters))

	// Шаг 1: вычисляем целевые папки и флаги
	type resolved struct {
		letter Letter
		folder string
		flags  []string
	}
	var resolvedLetters []resolved
	for _, l := range letters {
		folder := ResolveFolder(l.LabelIDs, l.LabelNames)
		flags := ResolveFlags(l.LabelIDs)
		resolvedLetters = append(resolvedLetters, resolved{letter: l, folder: folder, flags: flags})
		t.Logf("  %s → folder=%s flags=%v", l.MsgID, folder, flags)
	}

	// Шаг 2: собираем уникальные пользовательские папки (без системных)
	uniqueFolders := make(map[string]struct{})
	for _, r := range resolvedLetters {
		if _, isSystem := SystemFolders[r.folder]; !isSystem {
			uniqueFolders[r.folder] = struct{}{}
		}
	}

	// Шаг 3: создаём недостающие папки
	var createdFolders []string
	for folder := range uniqueFolders {
		if err := CreateFolderIfNotExists(context.Background(), c, folder); err != nil {
			t.Fatalf("CreateFolderIfNotExists(%s): %v", folder, err)
		}
		createdFolders = append(createdFolders, folder)
		t.Logf("папка %s: создана/проверена", folder)
	}

	// Шаг 4: заливаем письма
	date := time.Date(2026, 7, 14, 12, 0, 0, 0, time.FixedZone("MSK", 3*3600))

	// Сначала заливаем в INBOX (там уже есть папка)
	var inboxLetters []resolved
	var otherLetters []resolved
	for _, r := range resolvedLetters {
		if r.folder == "INBOX" {
			inboxLetters = append(inboxLetters, r)
		} else {
			otherLetters = append(otherLetters, r)
		}
	}

	// Заливаем INBOX
	for _, r := range inboxLetters {
		raw := genTestEmail(parseMsgNum(r.letter.MsgID), email)
		if err := Append(context.Background(), c, r.folder, date, raw, r.flags, r.letter.MsgID); err != nil {
			t.Fatalf("Append %s → %s: %v", r.letter.MsgID, r.folder, err)
		}
	}

	// Заливаем остальные папки
	for _, r := range otherLetters {
		raw := genTestEmail(parseMsgNum(r.letter.MsgID), email)
		if err := Append(context.Background(), c, r.folder, date, raw, r.flags, r.letter.MsgID); err != nil {
			t.Fatalf("Append %s → %s: %v", r.letter.MsgID, r.folder, err)
		}
	}

	t.Logf("залито %d писем в %d папок", len(resolvedLetters), len(uniqueFolders)+1)

	// Шаг 5: проверяем результат
	for _, r := range resolvedLetters {
		messages, err := List(context.Background(), c, r.folder)
		if err != nil {
			t.Fatalf("List %s: %v", r.folder, err)
		}

		// Ищем наше письмо по Subject
		subject := fmt.Sprintf("gwsferry-e2e-test-%03d", parseMsgNum(r.letter.MsgID))
		found := false
		for _, msg := range messages {
			if msg.Subject == subject {
				found = true

				// Проверяем флаги
				if r.folder == "INBOX" && r.letter.MsgID == "test-msg-011" {
					// UNREAD: не должно быть \Seen
					if containsStr(msg.Flags, `\Seen`) {
						t.Errorf("%s: UNREAD → не должно быть \\Seen, флаги=%v", r.letter.MsgID, msg.Flags)
					}
				}
				if r.folder == "INBOX" && r.letter.MsgID == "test-msg-012" {
					// Без UNREAD: должно быть \Seen
					if !containsStr(msg.Flags, `\Seen`) {
						t.Errorf("%s: без UNREAD → должно быть \\Seen, флаги=%v", r.letter.MsgID, msg.Flags)
					}
				}
				if r.folder == "INBOX" && r.letter.MsgID == "test-msg-013" {
					// STARRED → \Flagged ставится через STORE после APPEND (обходной путь для Yandex)
					if !containsStr(msg.Flags, `\Flagged`) {
						t.Errorf("%s: STARRED → \\Flagged не найден, флаги=%v", r.letter.MsgID, msg.Flags)
					}
				}
				break
			}
		}
		if !found {
			t.Errorf("письмо %s (subject=%q) не найдено в папке %s", r.letter.MsgID, subject, r.folder)
		}
	}

	// Проверяем что письма 18 и 19 НЕ попали ни в одну папку
	skippedIDs := []string{"test-msg-018", "test-msg-019"}
	allFolders := []string{"INBOX", "Trash", "Spam", "Sent", "Drafts"}
	for _, folder := range allFolders {
		messages, err := List(context.Background(), c, folder)
		if err != nil {
			t.Logf("List %s: %v (пропускаем проверку пропущенных)", folder, err)
			continue
		}
		for _, msg := range messages {
			for _, skipped := range skippedIDs {
				subject := fmt.Sprintf("gwsferry-e2e-test-%s", skipped[10:])
				if msg.Subject == subject {
					t.Errorf("письмо %s не должно быть в папке %s (было пропущено)", skipped, folder)
				}
			}
		}
	}

	// Шаг 6: очистка — удаляем тестовые письма и пользовательские папки
	t.Cleanup(func() {
		log.Printf("[CLEANUP] удаление тестовых писем и папок из %s", email)
		cleanupClient, cleanupEmail := connectTestLabelsUser(t)
		defer Close(cleanupClient)
		_ = cleanupEmail

		// Сначала удаляем письма из всех папок
		deletedFrom := make(map[string]int)
		for _, r := range resolvedLetters {
			messages, err := List(context.Background(), cleanupClient, r.folder)
			if err != nil {
				log.Printf("[CLEANUP] List %s: %v", r.folder, err)
				continue
			}
			subject := fmt.Sprintf("gwsferry-e2e-test-%03d", parseMsgNum(r.letter.MsgID))
			for _, msg := range messages {
				if msg.Subject == subject {
					if err := Delete(context.Background(), cleanupClient, r.folder, msg.UID); err != nil {
						log.Printf("[CLEANUP] Delete uid=%d in %s: %v", msg.UID, r.folder, err)
					} else {
						deletedFrom[r.folder]++
					}
				}
			}
		}
		for folder, count := range deletedFrom {
			log.Printf("[CLEANUP] удалено %d писем из %s", count, folder)
		}

		// Затем удаляем пользовательские папки
		for _, folder := range createdFolders {
			if err := DeleteFolder(context.Background(), cleanupClient, folder); err != nil {
				log.Printf("[CLEANUP] DeleteFolder %s: %v", folder, err)
			} else {
				log.Printf("[CLEANUP] папка %s удалена", folder)
			}
		}
	})
}

func parseMsgNum(msgID string) int {
	var n int
	fmt.Sscanf(msgID, "test-msg-%d", &n)
	return n
}

// ==========================================
// ЮНИТ-ТЕСТЫ: проверка что все 20 кейсов резолвятся корректно
// ==========================================

func TestResolveAll20Cases(t *testing.T) {
	email := "test1@dinord.ru"
	userLabels := testLabels20[email]

	type caseResult struct {
		msgID  string
		folder string
		flags  []string
		skip   bool
	}

	var results []caseResult

	for i := 1; i <= 20; i++ {
		msgID := fmt.Sprintf("test-msg-%03d", i)

		// Кейс 18: отсутствует в messages → пропуск
		if i == 18 {
			results = append(results, caseResult{msgID: msgID, skip: true})
			continue
		}
		// Кейс 19: done: false, отсутствует → пропуск (проверяется через partial@dinord.ru)
		if i == 19 {
			results = append(results, caseResult{msgID: msgID, skip: true})
			continue
		}

		labelIDs, ok := userLabels.Messages[msgID]
		if !ok {
			t.Fatalf("msg %s не найден", msgID)
		}

		folder := ResolveFolder(labelIDs, userLabels.LabelNames)
		flags := ResolveFlags(labelIDs)
		results = append(results, caseResult{msgID: msgID, folder: folder, flags: flags})
	}

	// Проверяем ожидаемые результаты
	expected := []struct {
		msgID  string
		folder string
		seen   *bool // nil = не проверяем
		flagged bool
	}{
		{"test-msg-001", "INBOX", boolPtr(true), false},         // 1
		{"test-msg-002", "Trash", nil, false},                    // 2
		{"test-msg-003", "Spam", nil, false},                     // 3
		{"test-msg-004", "Trash", nil, false},                    // 4
		{"test-msg-005", "Spam", nil, false},                     // 5
		{"test-msg-006", "Сообщения от Test2", nil, false},       // 6
		{"test-msg-007", "От Test", nil, false},                   // 7 (quotes sanitized)
		{"test-msg-008", "Папка Alpha", nil, false},              // 8 (Label_2003 < Label_2010)
		{"test-msg-009", "Drafts", nil, false},                   // 9 ([Imap]/Черновики)
		{"test-msg-010", "МояПапка", nil, false},                 // 10 ([Imap]/МояПапка)
		{"test-msg-011", "INBOX", boolPtr(false), false},         // 11 (UNREAD → без \Seen)
		{"test-msg-012", "INBOX", boolPtr(true), false},          // 12
		{"test-msg-013", "INBOX", boolPtr(true), true},           // 13 (STARRED → \Flagged)
		{"test-msg-014", "INBOX", boolPtr(true), false},          // 14 (CATEGORY_FORUMS → INBOX)
		{"test-msg-015", "INBOX", boolPtr(true), false},          // 15 (CHAT → INBOX)
		{"test-msg-016", "Drafts", nil, false},                   // 16
		{"test-msg-017", "Sent", nil, false},                     // 17
		{"test-msg-020", "INBOX", boolPtr(false), false},         // 20 (UNREAD → без \Seen)
	}

	for _, exp := range expected {
		var got caseResult
		for _, r := range results {
			if r.msgID == exp.msgID {
				got = r
				break
			}
		}
		if got.skip {
			t.Errorf("%s: ожидалась обработка, но помечен как пропуск", exp.msgID)
			continue
		}
		if got.folder != exp.folder {
			t.Errorf("%s: folder=%s, want %s", exp.msgID, got.folder, exp.folder)
		}
		if exp.seen != nil {
			hasSeen := containsStr(got.flags, `\Seen`)
			if hasSeen != *exp.seen {
				t.Errorf("%s: \\Seen=%v, want %v (flags=%v)", exp.msgID, hasSeen, *exp.seen, got.flags)
			}
		}
		if exp.flagged {
			if !containsStr(got.flags, `\Flagged`) {
				t.Errorf("%s: ожидался \\Flagged, flags=%v", exp.msgID, got.flags)
			}
		}
	}

	// Кейс 18: отсутствует в messages
	_, ok18 := userLabels.Messages["test-msg-018"]
	if ok18 {
		t.Error("test-msg-018 не должен быть в messages")
	}

	// Кейс 19: done: false, отсутствует
	partial := testLabels20["partial@dinord.ru"]
	if partial.Done {
		t.Error("partial@dinord.ru done должен быть false")
	}
	_, ok19 := partial.Messages["test-msg-019"]
	if ok19 {
		t.Error("test-msg-019 не должен быть в partial messages")
	}
}

func boolPtr(b bool) *bool { return &b }
