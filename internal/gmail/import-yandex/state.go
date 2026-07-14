package importyandex

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ImportState — состояние миграции на диске.
// Юзеры: email → "pending"/"done"/"error".
// Письма: email → set обработанных msg_id.
// Потокобезопасен: все публичные методы защищены мьютексом.
type ImportState struct {
	mu       sync.Mutex
	Users    map[string]string          `json:"users"`    // email → status
	Messages map[string]map[string]bool `json:"messages"` // email → msg_id → done
	Errors   map[string]string          `json:"errors"`   // email → error detail
}

var stateFileMu sync.Mutex

func newImportState() *ImportState {
	return &ImportState{
		Users:    make(map[string]string),
		Messages: make(map[string]map[string]bool),
		Errors:   make(map[string]string),
	}
}

func loadImportState(path string) *ImportState {
	s := newImportState()
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[DEBUG] [STATE] загрузка из %s: файл не найден, создаю пустой state", path)
		return s
	}
	if err := json.Unmarshal(raw, s); err != nil {
		log.Printf("[WARN] [STATE] ошибка парсинга %s: %v, создаю пустой state", path, err)
		return newImportState()
	}
	if s.Users == nil {
		s.Users = make(map[string]string)
	}
	if s.Messages == nil {
		s.Messages = make(map[string]map[string]bool)
	}
	if s.Errors == nil {
		s.Errors = make(map[string]string)
	}
	doneCount := 0
	errCount := 0
	for _, status := range s.Users {
		if status == "done" {
			doneCount++
		} else if status == "error" {
			errCount++
		}
	}
	totalMsgs := 0
	for _, msgs := range s.Messages {
		totalMsgs += len(msgs)
	}
	log.Printf("[INFO] [STATE] загружен из %s: %d юзеров (%d done, %d error), %d сообщений",
		path, len(s.Users), doneCount, errCount, totalMsgs)
	return s
}

func saveImportState(s *ImportState, path string) {
	stateFileMu.Lock()
	defer stateFileMu.Unlock()

	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[ERROR] [STATE] запись %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[ERROR] [STATE] rename %s → %s: %v", tmp, path, err)
		return
	}
	log.Printf("[DEBUG] [STATE] сохранён в %s (%d bytes)", path, len(data))
}

// markUserDone помечает юзера как завершённого.
func (s *ImportState) markUserDone(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("[DEBUG] [STATE] %s: помечен как done", email)
	s.Users[email] = "done"
}

// markUserError помечает юзера как упавшего.
func (s *ImportState) markUserError(email, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("[WARN] [STATE] %s: помечен как error: %s", email, detail)
	s.Users[email] = "error"
	if s.Errors == nil {
		s.Errors = make(map[string]string)
	}
	s.Errors[email] = detail
}

// markMessageDone помечает письмо как обработанное.
func (s *ImportState) markMessageDone(email, msgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Messages[email] == nil {
		s.Messages[email] = make(map[string]bool)
	}
	s.Messages[email][msgID] = true
	log.Printf("[DEBUG] [STATE] %s: msgID=%s помечен как done (всего %d)", email, msgID, len(s.Messages[email]))
}

// isMessageDone проверяет, обработано ли письмо.
func (s *ImportState) isMessageDone(email, msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := s.Messages[email][msgID]
	if done {
		log.Printf("[DEBUG] [STATE] %s: msgID=%s уже обработан (resume skip)", email, msgID)
	}
	return done
}

// isUserDone проверяет, завершён ли юзер.
func (s *ImportState) isUserDone(email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := s.Users[email] == "done"
	if done {
		log.Printf("[DEBUG] [STATE] %s: юзер уже done", email)
	}
	return done
}

// startPeriodicDumper запускает горутину, которая дампает state на диск каждые 60 секунд.
// Возвращает функцию остановки.
func startPeriodicDumper(s *ImportState, path string) (stop func()) {
	log.Printf("[INFO] [STATE] запуск periodic dumper: interval=60s path=%s", path)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Printf("[DEBUG] [STATE] periodic dumper: сохраняю state в %s", path)
				saveImportState(s, path)
			case <-done:
				log.Printf("[DEBUG] [STATE] periodic dumper: остановлен")
				return
			}
		}
	}()
	return func() { close(done) }
}

// stateFilePath возвращает путь к файлу состояния рядом с бинарём.
func stateFilePath() string {
	execPath, err := os.Executable()
	if err != nil {
		log.Printf("[DEBUG] [STATE] не удалось определить путь к бинарю: %v, fallback import_state.json", err)
		return "import_state.json"
	}
	path := fmt.Sprintf("%s/import_state.json", filepath.Dir(execPath))
	log.Printf("[DEBUG] [STATE] stateFilePath=%s", path)
	return path
}
