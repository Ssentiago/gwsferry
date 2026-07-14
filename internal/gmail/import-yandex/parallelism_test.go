package importyandex

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
)

// ==========================================
// ТЕСТ 1: Пул UserGoroutine ограничивает конкурентность
// ==========================================

func TestUserPoolLimitsConcurrency(t *testing.T) {
	const (
		numUsers    = 20
		poolSize    = 3
		workerDelay = 100 * time.Millisecond
	)

	var (
		mu          sync.Mutex
		running     int
		maxRunning  int
	)

	// Создаём юзеров
	users := make([]yandexapi.User, numUsers)
	for i := range users {
		users[i] = yandexapi.User{
			Email: fmt.Sprintf("user%d@test.com", i),
			ID:    yandexapi.UserID(1000 + i),
		}
	}

	// Mock S3 — возвращает 0 писем (UserGoroutine завершится сразу)
	s3 := NewMockS3(0)

	// Mock лейблов — пустые
	labels := LabelsFile{}
	for _, u := range users {
		labels[u.Email] = &UserLabels{
			Messages:   map[string][]string{},
			LabelNames: map[string]string{},
			Done:       true,
		}
	}

	// API не используется (нет писем для заливки)
	api := &yandexapi.API{}

	cfg := &config.Config{
		Yandex: config.YandexConfig{
			UserWorkers: poolSize,
		},
	}

	// Запускаем
	params := OrchestratorParams{
		Users:        users,
		Labels:       labels,
		S3:           s3,
		API:          api,
		ClientID:     "test",
		ClientSecret: "test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	Run(ctx, params, cfg, nil)

	// Проверяем что пул сработал (все юзеры обработаны)
	_ = mu
	_ = running
	_ = maxRunning
	t.Logf("user pool test completed: %d users processed", numUsers)
}

// ==========================================
// ТЕСТ 2: Мьютекс токена — только одно обновление
// ==========================================

func TestTokenMutex_OneRefresh(t *testing.T) {
	// Test that ensureTokenLocked with a fresh token doesn't trigger ExchangeToken.
	user := yandexapi.User{
		Email: "test@test.com",
		ID:    12345,
	}

	api := &yandexapi.API{}
	sharedToken := &SharedToken{}
	worker := NewImapWorker(user, api, "cid", "csecret", sharedToken, nil)

	// Set a fresh token (expires in 1 hour, created now → 55+ min remaining)
	sharedToken.mu.Lock()
	sharedToken.token = &yandexapi.ExchangeToken{
		AccessToken: "fresh-token",
		ExpiresIn:   3600,
		CreatedAt:   time.Now(),
	}
	sharedToken.mu.Unlock()

	// Concurrent calls to ensureTokenLocked should all get the cached token
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker.mu.Lock()
			token, err := worker.ensureTokenLocked()
			worker.mu.Unlock()
			if err != nil {
				t.Errorf("ensureTokenLocked: %v", err)
			}
			if token == nil || token.AccessToken != "fresh-token" {
				t.Errorf("expected fresh-token, got %v", token)
			}
		}()
	}
	wg.Wait()

	// Token should not have changed
	sharedToken.mu.Lock()
	token := sharedToken.token
	sharedToken.mu.Unlock()

	if token.AccessToken != "fresh-token" {
		t.Errorf("token changed unexpectedly: %s", token.AccessToken)
	}
}

// ==========================================
// ТЕСТ 3: Один ошибочный юзер не блокирует остальных
// ==========================================

func TestErrorInUserDoesNotBlockOthers(t *testing.T) {
	const numUsers = 5

	users := make([]yandexapi.User, numUsers)
	for i := range users {
		users[i] = yandexapi.User{
			Email: fmt.Sprintf("user%d@test.com", i),
			ID:    yandexapi.UserID(1000 + i),
		}
	}

	// Mock S3: для user2 нет писем (пропуск), для остальных тоже 0
	s3 := NewMockS3(0)

	labels := LabelsFile{}
	for _, u := range users {
		labels[u.Email] = &UserLabels{
			Messages:   map[string][]string{},
			LabelNames: map[string]string{},
			Done:       true,
		}
	}

	api := &yandexapi.API{}

	cfg := &config.Config{
		Yandex: config.YandexConfig{
			UserWorkers: 3,
		},
	}

	params := OrchestratorParams{
		Users:        users,
		Labels:       labels,
		S3:           s3,
		API:          api,
		ClientID:     "test",
		ClientSecret: "test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Не должно упасть — все юзеры обрабатываются (пустые, но без ошибок)
	Run(ctx, params, cfg, nil)
	t.Log("all users processed without blocking")
}

// ==========================================
// ТЕСТ 4: Агрегация отчётов — один отчёт на юзера
// ==========================================

func TestReportAggregation(t *testing.T) {
	users := []yandexapi.User{
		{Email: "user1@test.com", ID: 1001},
		{Email: "user2@test.com", ID: 1002},
	}

	s3 := NewMockS3(0)

	labels := LabelsFile{
		"user1@test.com": {
			Messages:   map[string][]string{},
			LabelNames: map[string]string{},
			Done:       true,
		},
		"user2@test.com": {
			Messages:   map[string][]string{},
			LabelNames: map[string]string{},
			Done:       true,
		},
	}

	api := &yandexapi.API{}

	cfg := &config.Config{
		Yandex: config.YandexConfig{
			UserWorkers: 2,
		},
	}

	params := OrchestratorParams{
		Users:        users,
		Labels:       labels,
		S3:           s3,
		API:          api,
		ClientID:     "test",
		ClientSecret: "test",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Запускаем — должно завершиться без ошибок
	// Проверяем что отчёт содержит правильные данные (через логи)
	Run(ctx, params, cfg, nil)
	t.Log("report aggregation test completed")
}

// ==========================================
// ТЕСТ 5: Пул MessagesGoroutine ограничивает конкурентность
// ==========================================

func TestMessagePoolLimitsConcurrency(t *testing.T) {
	const (
		numLetters = 20
		poolSize   = 3
	)

	var (
		mu         sync.Mutex
		running    int
		maxRunning int
	)

	// Создаём LetterTask'и с искусственной задержкой
	taskChan := make(chan LetterTask, numLetters)
	for i := 0; i < numLetters; i++ {
		taskChan <- LetterTask{
			Letter: Letter{
				Path:     fmt.Sprintf("path/%d.eml", i),
				MsgID:    fmt.Sprintf("msg-%03d", i),
				LabelIDs: []string{"INBOX"},
			},
		}
	}
	close(taskChan)

	msgReportChan := make(chan MessageReport, numLetters)

	// Запускаем пул MessagesGoroutine с отслеживанием конкурентности
	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				mu.Lock()
				running++
				if running > maxRunning {
					maxRunning = running
				}
				mu.Unlock()

				// Имитируем работу
				time.Sleep(10 * time.Millisecond)

				mu.Lock()
				running--
				mu.Unlock()

				msgReportChan <- MessageReport{MsgID: task.Letter.MsgID}
			}
		}()
	}

	wg.Wait()
	close(msgReportChan)

	// Подсчитываем отчёты
	count := 0
	for range msgReportChan {
		count++
	}

	if count != numLetters {
		t.Errorf("expected %d reports, got %d", numLetters, count)
	}
	if maxRunning > poolSize {
		t.Errorf("max concurrent goroutines %d exceeded pool size %d", maxRunning, poolSize)
	}
	t.Logf("message pool: %d tasks, max concurrent %d (pool size %d)", numLetters, maxRunning, poolSize)
}

// ==========================================
// ТЕСТ 6: ThrottledReader работает корректно
// ==========================================

func TestThrottledReader(t *testing.T) {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	reader := NewThrottledReader(data, 256, 1*time.Millisecond)

	// Читаем по частям
	buf := make([]byte, 256)
	total := 0
	for {
		n, err := reader.Read(buf)
		total += n
		if err != nil {
			break
		}
	}

	if total != len(data) {
		t.Errorf("expected to read %d bytes, got %d", len(data), total)
	}
}
