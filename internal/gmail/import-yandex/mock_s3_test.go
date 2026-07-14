package importyandex

import (
	"context"
	"io"
	"sync"
	"time"
)

// MockS3Client — in-memory S3 для тестов, реализует S3Reader.
// Поддерживает искусственную задержку стриминга для тестирования конкурентности.
type MockS3Client struct {
	mu       sync.RWMutex
	emails   map[string][]EmailMeta // email → []EmailMeta
	bodies   map[string][]byte      // key → raw .eml
	throttle time.Duration          // задержка на каждые 64KB
}

func NewMockS3(throttle time.Duration) *MockS3Client {
	return &MockS3Client{
		emails:   make(map[string][]EmailMeta),
		bodies:   make(map[string][]byte),
		throttle: throttle,
	}
}

// AddEmail добавляет письмо в мок.
func (m *MockS3Client) AddEmail(email, msgID string, raw []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := "ru/users/" + email + "/gmail/" + msgID + ".eml"
	m.emails[email] = append(m.emails[email], EmailMeta{
		MessageID: msgID,
		Key:       key,
		Size:      int64(len(raw)),
	})
	m.bodies[key] = raw
}

func (m *MockS3Client) ListEmails(_ context.Context, email string) ([]EmailMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.emails[email], nil
}

func (m *MockS3Client) GetEmail(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	data, ok := m.bodies[key]
	m.mu.RUnlock()

	if !ok {
		return nil, nil
	}

	if m.throttle <= 0 {
		return data, nil
	}

	// Throttled read — отдаём данные порциями с задержкой
	return m.throttledRead(data), nil
}

func (m *MockS3Client) throttledRead(data []byte) []byte {
	const chunkSize = 64 * 1024 // 64KB
	result := make([]byte, len(data))
	copy(result, data)

	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		// Имитируем задержку чтения
		time.Sleep(m.throttle)
		_ = result[i:end] // "читаем" порцию
	}
	return data
}

// ==========================================
// МОК API
// ==========================================

// MockAPI — мок Yandex API для тестирования параллелизма.
type MockAPI struct {
	mu           sync.Mutex
	refreshCount int
	refreshDelay time.Duration
}

func NewMockAPI(refreshDelay time.Duration) *MockAPI {
	return &MockAPI{refreshDelay: refreshDelay}
}

func (m *MockAPI) RefreshCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshCount
}

// ==========================================
// МОК IMAP (упрощённый)
// ==========================================

// MockImapUser — мок ImapUser для тестирования пулов без реального IMAP.
type MockImapUser struct {
	mu           sync.Mutex
	appendCount  int
	failEmails   map[string]struct{} // emails которые должны упасть
	appendDelay  time.Duration
}

func NewMockImapUser() *MockImapUser {
	return &MockImapUser{
		failEmails: make(map[string]struct{}),
	}
}

func (m *MockImapUser) SetFail(msgID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failEmails[msgID] = struct{}{}
}

func (m *MockImapUser) AppendCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendCount
}

// ==========================================
// ThrottledReader — io.Reader с искусственной задержкой
// ==========================================

type ThrottledReader struct {
	data      []byte
	pos       int
	chunkSize int
	delay     time.Duration
}

func NewThrottledReader(data []byte, chunkSize int, delay time.Duration) *ThrottledReader {
	return &ThrottledReader{
		data:      data,
		chunkSize: chunkSize,
		delay:     delay,
	}
}

func (r *ThrottledReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	n := r.chunkSize
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	if n > len(p) {
		n = len(p)
	}

	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n

	if r.delay > 0 {
		time.Sleep(r.delay)
	}

	return n, nil
}
