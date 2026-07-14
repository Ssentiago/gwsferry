package importyandex

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

// ==========================================
// ГЕНЕРАЦИЯ СИНТЕТИЧЕСКИХ ДАННЫХ
// ==========================================

const (
	numSyntheticUsers = 3
	emailsPerUser     = 25
	throttlePerMB     = 10 * time.Millisecond // задержка на каждый MB стриминга
)

type syntheticUser struct {
	email string
	id    int64
}

type syntheticLetter struct {
	user    syntheticUser
	msgNum  int
	sizeMB  int
	date    time.Time
	labelIDs []string
	folder  string
	flags   []string
	status  string // "ok", "skipped", "error"
	errMsg  string
}

func genSyntheticUsers() []syntheticUser {
	users := make([]syntheticUser, numSyntheticUsers)
	for i := range users {
		users[i] = syntheticUser{
			email: fmt.Sprintf("synthetic-user-%02d@dinord.ru", i+1),
			id:    int64(9000000 + i),
		}
	}
	return users
}

func genSyntheticLabels(users []syntheticUser) LabelsFile {
	labels := LabelsFile{}
	for _, u := range users {
		messages := make(map[string][]string, emailsPerUser)
		for i := 1; i <= emailsPerUser; i++ {
			msgID := fmt.Sprintf("synth-%s-%02d", u.email[:15], i)
			// Распределение лейблов по кейсам
			switch i % 7 {
			case 0:
				messages[msgID] = []string{"INBOX"}
			case 1:
				messages[msgID] = []string{"TRASH"}
			case 2:
				messages[msgID] = []string{"SPAM"}
			case 3:
				messages[msgID] = []string{"INBOX", "UNREAD"}
			case 4:
				messages[msgID] = []string{"INBOX", "STARRED"}
			case 5:
				messages[msgID] = []string{"DRAFT"}
			case 6:
				messages[msgID] = []string{"SENT"}
			}
		}
		labels[u.email] = &UserLabels{
			Messages:   messages,
			LabelNames: map[string]string{"INBOX": "INBOX"},
			Done:       true,
		}
	}
	return labels
}

func genRandomDate(rng *rand.Rand) time.Time {
	// Случайная дата за последние 10 лет
	now := time.Now()
	sec := rng.Int63n(10 * 365 * 24 * 3600)
	return now.Add(-time.Duration(sec) * time.Second)
}

func genSyntheticEmail(user syntheticUser, msgNum int, rng *rand.Rand) ([]byte, string, time.Time) {
	date := genRandomDate(rng)
	msgID := fmt.Sprintf("synth-%s-%02d", user.email[:15], msgNum)
	subject := fmt.Sprintf("SYNTH-%s-%02d-%dMB", user.email[:15], msgNum, msgNum)
	sizeMB := msgNum // 1, 2, 3, ... 25 MB

	// Заголовки
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", user.email)
	fmt.Fprintf(&b, "To: test1@dinord.ru\r\n")
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", date.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@dinord.ru>\r\n", msgID)
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n")
	fmt.Fprintf(&b, "\r\n")

	header := b.String()

	// Тело нужного размера
	bodySize := sizeMB*1024*1024 - len(header)
	if bodySize < 0 {
		bodySize = 100
	}
	body := make([]byte, bodySize)
	for i := range body {
		body[i] = byte('A' + (i % 26))
	}

	raw := append([]byte(header), body...)
	return raw, msgID, date
}

// ==========================================
// ИНТЕГРАЦИОННЫЙ ТЕСТ
// ==========================================

func TestIntegrationParallelMigration(t *testing.T) {
	if os.Getenv("YANDEX_INTEGRATION_TEST") == "" {
		t.Skip("set YANDEX_INTEGRATION_TEST=1")
	}

	cfg := loadTestLabelsConfig(t)
	if cfg == nil {
		return
	}

	rng := rand.New(rand.NewSource(42)) // детерминированный seed

	// Генерируем синтетических юзеров
	users := genSyntheticUsers()
	labels := genSyntheticUsersLabels(users)

	// Создаём мок-S3 с throttle
	s3Mock := NewMockS3(throttlePerMB)

	// Генерируем письма и заполняем мок-S3
	// Все письма хранятся под реальным юзером test1@dinord.ru
	// (оркестратор ищет по его email)
	var allLetters []syntheticLetter
	for _, u := range users {
		for i := 1; i <= emailsPerUser; i++ {
			raw, msgID, date := genSyntheticEmail(u, i, rng)
			s3Mock.AddEmail("test1@dinord.ru", msgID, raw)

			userLabels := labels[u.email]
			letterLabels := userLabels.Messages[msgID]
			folder := ResolveFolder(letterLabels, userLabels.LabelNames)
			flags := ResolveFlags(letterLabels)

			allLetters = append(allLetters, syntheticLetter{
				user:     u,
				msgNum:   i,
				sizeMB:   i,
				date:     date,
				labelIDs: letterLabels,
				folder:   folder,
				flags:    flags,
				status:   "pending",
			})
		}
	}

	t.Logf("сгенерировано: %d юзеров × %d писем = %d писем", numSyntheticUsers, emailsPerUser, len(allLetters))

	// Подключаемся к реальному IMAP
	imapClient, _ := connectTestUser(t)
	defer Close(imapClient)

	// Создаём API для ExchangeToken
	c := yandexapi.NewClient(cfg.Yandex.OAuthToken)
	api := yandexapi.NewAPI(c, cfg.Yandex.OrgID, cfg.Yandex.OAuthToken)

	// Собираем уникальные папки
	uniqueFolders := make(map[string]struct{})
	for _, l := range allLetters {
		if _, isSystem := SystemFolders[l.folder]; !isSystem {
			uniqueFolders[l.folder] = struct{}{}
		}
	}

	// Создаём папки
	for folder := range uniqueFolders {
		if err := CreateFolderIfNotExists(context.Background(), imapClient, folder); err != nil {
			t.Fatalf("CreateFolderIfNotExists(%s): %v", folder, err)
		}
		t.Logf("папка %s: создана", folder)
	}

	// Запускаем оркестратор
	// Все письма заливаются через реального test1@dinord.ru (его токен/UID)
	// Синтетические юзеры — только для генерации From/Subject/labels
	realUserYandex := yandexapi.User{
		Email: "test1@dinord.ru",
		ID:    1130000072441884, // реальный UID
	}

	// Лейблы маппим на реального юзера
	realLabels := LabelsFile{
		"test1@dinord.ru": labels["synthetic-user-01@dinord.ru"],
	}
	// Объединяем лейблы всех синтетических юзеров под одним ключом real user
	allMessages := make(map[string][]string)
	allNames := make(map[string]string)
	for _, u := range users {
		for k, v := range labels[u.email].Messages {
			allMessages[k] = v
		}
		for k, v := range labels[u.email].LabelNames {
			allNames[k] = v
		}
	}
	realLabels["test1@dinord.ru"] = &UserLabels{
		Messages:   allMessages,
		LabelNames: allNames,
		Done:       true,
	}

	params := OrchestratorParams{
		Users:        []yandexapi.User{realUserYandex},
		Labels:       realLabels,
		S3:           s3Mock,
		API:          api,
		ClientID:     cfg.Yandex.ClientID,
		ClientSecret: cfg.Yandex.ClientSecret,
	}

	// Конфиг: 1 юзер (real test1@dinord.ru), 5 параллельных append
	testCfg := &config.Config{
		Yandex: config.YandexConfig{
			UserWorkers: 1,
		},
	}

	start := time.Now()
	t.Logf("ЗАПУСК: %d юзеров × %d воркеров, %d писем × %d воркеров",
		testCfg.Yandex.UserWorkers, MsgWorkers,
		emailsPerUser, MsgWorkers)

	Run(context.Background(), params, testCfg, nil)

	elapsed := time.Since(start)
	t.Logf("ЗАВЕРШЕНО за %s", elapsed)

	// Проверяем результат — считаем письма в ящике
	totalFound := 0
	folders := []string{"INBOX", "Trash", "Spam", "Drafts", "Sent"}
	for _, folder := range folders {
		msgs, err := List(context.Background(), imapClient, folder)
		if err != nil {
			t.Logf("List %s: %v", folder, err)
			continue
		}
		synthCount := 0
		for _, m := range msgs {
			if strings.HasPrefix(m.Subject, "SYNTH-") {
				synthCount++
			}
		}
		if synthCount > 0 {
			t.Logf("папка %s: %d синтетических писем", folder, synthCount)
			totalFound += synthCount
		}
	}

	t.Logf("ИТОГО в ящике: %d/%d синтетических писем", totalFound, len(allLetters))

	// Генерируем отчёт
	reportPath := filepath.Join(t.TempDir(), "concurrency_test_report.txt")
	generateReport(t, reportPath, allLetters, elapsed)

	// Cleanup — удаляем синтетические письма
	t.Cleanup(func() {
		cleanupClient, _ := connectTestUser(t)
		defer Close(cleanupClient)
		for _, folder := range folders {
			msgs, err := List(context.Background(), cleanupClient, folder)
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if strings.HasPrefix(m.Subject, "SYNTH-") {
					_ = Delete(context.Background(), cleanupClient, folder, m.UID)
				}
			}
		}
		for folder := range uniqueFolders {
			_ = DeleteFolder(context.Background(), cleanupClient, folder)
		}
		log.Printf("[CLEANUP] синтетические письма и папки удалены")
	})
}

func genSyntheticUsersLabels(users []syntheticUser) LabelsFile {
	labels := LabelsFile{}
	for _, u := range users {
		messages := make(map[string][]string, emailsPerUser)
		for i := 1; i <= emailsPerUser; i++ {
			msgID := fmt.Sprintf("synth-%s-%02d", u.email[:15], i)
			switch i % 7 {
			case 0:
				messages[msgID] = []string{"INBOX"}
			case 1:
				messages[msgID] = []string{"TRASH"}
			case 2:
				messages[msgID] = []string{"SPAM"}
			case 3:
				messages[msgID] = []string{"INBOX", "UNREAD"}
			case 4:
				messages[msgID] = []string{"INBOX", "STARRED"}
			case 5:
				messages[msgID] = []string{"DRAFT"}
			case 6:
				messages[msgID] = []string{"SENT"}
			}
		}
		labels[u.email] = &UserLabels{
			Messages:   messages,
			LabelNames: map[string]string{"INBOX": "INBOX"},
			Done:       true,
		}
	}
	return labels
}

// ==========================================
// ГЕНЕРАЦИЯ ОТЧЁТА
// ==========================================

func generateReport(t *testing.T, path string, letters []syntheticLetter, elapsed time.Duration) {
	t.Helper()

	var b strings.Builder
	fmt.Fprintf(&b, "=== CONCURRENCY TEST REPORT ===\n")
	fmt.Fprintf(&b, "Date: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "Elapsed: %s\n", elapsed)
	fmt.Fprintf(&b, "Users: %d, Emails per user: %d, Total: %d\n", numSyntheticUsers, emailsPerUser, len(letters))
	fmt.Fprintf(&b, "Pool: user_workers=%d, msg_workers=%d\n\n", 3, MsgWorkers)

	fmt.Fprintf(&b, "=== LETTERS ===\n")
	fmt.Fprintf(&b, "%-30s %-5s %-6s %-12s %-20s %-10s %-10s %s\n",
		"FROM", "NUM", "MB", "DATE", "FOLDER", "FLAGS", "STATUS", "ERROR")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 140))

	for _, l := range letters {
		fmt.Fprintf(&b, "%-30s %-5d %-6d %-12s %-20s %-10s %-10s %s\n",
			l.user.email, l.msgNum, l.sizeMB,
			l.date.Format("2006-01-02"),
			l.folder, strings.Join(l.flags, ","), l.status, l.errMsg)
	}

	fmt.Fprintf(&b, "\n=== SUMMARY ===\n")
	ok, skipped, failed := 0, 0, 0
	for _, l := range letters {
		switch l.status {
		case "ok":
			ok++
		case "skipped":
			skipped++
		case "error":
			failed++
		}
	}
	fmt.Fprintf(&b, "Total: %d, OK: %d, Skipped: %d, Failed: %d\n", len(letters), ok, skipped, failed)
	fmt.Fprintf(&b, "Elapsed: %s\n", elapsed)
	if ok > 0 {
		perEmail := elapsed / time.Duration(ok)
		fmt.Fprintf(&b, "Per email (avg): %s\n", perEmail)
	}

	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Logf("не удалось записать отчёт: %v", err)
	} else {
		t.Logf("отчёт: %s", path)
	}
}
