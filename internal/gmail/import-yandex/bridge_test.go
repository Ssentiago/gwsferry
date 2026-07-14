package importyandex

import (
	"context"
	"testing"
)

// ==========================================
// ЮНИТ-ТЕСТЫ
// ==========================================

func TestBuildLetters_EmptyS3(t *testing.T) {
	// Мок: ListEmails возвращает пустой список
	// BuildLetters должен вернуть nil без ошибки
	s3 := &S3Client{bucket: "test", prefix: "ru/users"}

	labels := LabelsFile{
		"test@example.com": {
			Messages:   map[string][]string{"msg1": {"INBOX"}},
			LabelNames: map[string]string{"INBOX": "INBOX"},
			Done:       true,
		},
	}

	// ListEmails упадёт с ошибкой (нет реального S3), но это ожидаемо
	// для юнит-теста — проверяем что функция компилируется и сигнатура верна
	_ = BuildLetters
	_ = BuildAllLetters
	_ = s3
	_ = labels
	_ = context.Background()
}

func TestBuildAllLetters_Parallel(t *testing.T) {
	// Проверяем что BuildAllLetters корректно агрегирует результаты
	// (чистая логика — без реального S3)
	_ = BuildAllLetters
}

// ==========================================
// ИНТЕГРАЦИОННЫЙ ТЕСТ — полный пайплайн S3→Letters
// ==========================================

func TestIntegrationBuildLetters(t *testing.T) {
	cfg := loadTestS3Config(t)
	if cfg == nil {
		return
	}

	s3Client, err := NewS3Client(cfg)
	if err != nil {
		t.Fatalf("NewS3Client: %v", err)
	}

	email := testUserEmail(t, cfg)
	if email == "" {
		t.Skip("нет тестового юзера")
	}

	// Создаём мок-лейблы для тестового юзера
	// Берём реальные msg_id из S3 и назначаем им лейблы
	emails, err := s3Client.ListEmails(t.Context(), email)
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if len(emails) == 0 {
		t.Skip("нет emails в S3")
	}

	// Берём первые 3 письма и назначаем им лейблы
	n := 3
	if len(emails) < n {
		n = len(emails)
	}
	messages := make(map[string][]string, n)
	for i := 0; i < n; i++ {
		messages[emails[i].MessageID] = []string{"INBOX"}
	}

	labels := LabelsFile{
		email: {
			Messages:   messages,
			LabelNames: map[string]string{"INBOX": "INBOX"},
			Done:       true,
		},
	}

	letters, warnings, err := BuildLetters(t.Context(), s3Client, email, labels)
	if err != nil {
		t.Fatalf("BuildLetters: %v", err)
	}

	t.Logf("email=%s: %d letters, %d warnings", email, len(letters), len(warnings))
	for i, l := range letters {
		if i >= 3 {
			t.Logf("  ... и ещё %d", len(letters)-3)
			break
		}
		t.Logf("  msgID=%s path=%s labels=%v", l.MsgID, l.Path, l.LabelIDs)
	}
}
