// Package store отвечает за персистентное хранение собранных Gmail
// labelIds на диске и за resume-логику НА УРОВНЕ КОНКРЕТНОГО msg_id.
//
// Структура записи юзера в файле: {"messages": {msg_id: [labelIds]},
// "label_names": {labelId: name}}. Каждый msg_id пишется
// в "messages" сразу по получении ответа от API (см. SaveMsgLabels),
// не дожидаясь завершения всего ящика - периодический дамп на диск
// сохраняет и частично собранных юзеров.
//
// Единственный источник истины для "собран/не собран" — сравнение
// количества локальных msg_id с количеством из Google (msgIndex).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// User - запись одного почтового ящика в результирующем файле.
type User struct {
	Messages   map[string][]string `json:"messages"`
	LabelNames map[string]string   `json:"label_names"`
}

// Store - потокобезопасное хранилище результата с персистентностью на
// диск. Все публичные методы безопасны для вызова из многих горутин
// одновременно.
type Store struct {
	mu       sync.Mutex // защищает data + msgIndex
	fileMu   sync.Mutex // сериализует запись файла
	path     string
	data     map[string]*User
	msgIndex map[string][]string // pre-fetched msg_id для каждого юзера
}

func New(path string) *Store {
	return &Store{path: path, data: make(map[string]*User)}
}

// Load читает уже собранные лейблы с диска.
func (s *Store) Load() (count int, err error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("чтение %s: %w", s.path, err)
	}

	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawTop); err != nil {
		return 0, fmt.Errorf("парсинг %s: %w", s.path, err)
	}

	data := make(map[string]*User, len(rawTop))
	for email, rawUser := range rawTop {
		var u struct {
			Messages   map[string][]string `json:"messages"`
			LabelNames map[string]string   `json:"label_names"`
		}
		if err := json.Unmarshal(rawUser, &u); err != nil {
			return 0, fmt.Errorf("парсинг записи для %s: %w", email, err)
		}
		if u.Messages == nil {
			u.Messages = make(map[string][]string)
		}
		if u.LabelNames == nil {
			u.LabelNames = make(map[string]string)
		}
		data[email] = &User{
			Messages:   u.Messages,
			LabelNames: u.LabelNames,
		}
	}

	s.mu.Lock()
	s.data = data
	s.mu.Unlock()

	return len(data), nil
}

// deepCopy - полное копирование map[string]*User включая вложенные
// map'ы, чтобы снапшот для записи на диск не делил память с живыми
// структурами, которые продолжают мутироваться другими горутинами.
func deepCopy(src map[string]*User) map[string]*User {
	dst := make(map[string]*User, len(src))
	for email, u := range src {
		msgs := make(map[string][]string, len(u.Messages))
		for mid, labels := range u.Messages {
			labelsCopy := make([]string, len(labels))
			copy(labelsCopy, labels)
			msgs[mid] = labelsCopy
		}
		names := make(map[string]string, len(u.LabelNames))
		for k, v := range u.LabelNames {
			names[k] = v
		}
		dst[email] = &User{Messages: msgs, LabelNames: names}
	}
	return dst
}

// Save делает atomic-дамп всего накопленного результата на диск.
func (s *Store) Save() error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()

	s.mu.Lock()
	snapshot := deepCopy(s.data)
	s.mu.Unlock()

	tmpPath := s.path + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("создание %s: %w", tmpPath, err)
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snapshot); err != nil {
		f.Close()
		return fmt.Errorf("сериализация: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, s.path, err)
	}
	return nil
}

// CachedLabels возвращает уже собранные для юзера msg_id -> labelIds.
func (s *Store) CachedLabels(email string) map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data[email]
	if !ok {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(u.Messages))
	for k, v := range u.Messages {
		out[k] = v
	}
	return out
}

// CachedLabelNames возвращает label_names для юзера.
func (s *Store) CachedLabelNames(email string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.data[email]
	if !ok {
		return map[string]string{}
	}
	out := make(map[string]string, len(u.LabelNames))
	for k, v := range u.LabelNames {
		out[k] = v
	}
	return out
}

func (s *Store) getOrCreate(email string) *User {
	u, ok := s.data[email]
	if !ok {
		u = &User{Messages: map[string][]string{}, LabelNames: map[string]string{}}
		s.data[email] = u
	}
	return u
}

// SaveMsgLabels пишет labelIds ОДНОГО письма в общий результат сразу по
// получении - не ждёт финализации юзера.
func (s *Store) SaveMsgLabels(email, msgID string, labelIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(email)
	u.Messages[msgID] = labelIDs
}

// FinalizeUser сохраняет label_names для юзера.
func (s *Store) FinalizeUser(email string, labelNames map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(email)
	u.LabelNames = labelNames
}

// ==========================================
// MSG INDEX — pre-fetched msg_id
// ==========================================

// SetMsgIndex задаёт ожидаемый список msg_id для юзера.
func (s *Store) SetMsgIndex(email string, msgIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msgIndex == nil {
		s.msgIndex = make(map[string][]string)
	}
	ids := make([]string, len(msgIDs))
	copy(ids, msgIDs)
	s.msgIndex[email] = ids
}

// SaveMsgIndex сохраняет индекс msg_id во временный файл.
func (s *Store) SaveMsgIndex(path string) error {
	s.mu.Lock()
	idx := make(map[string][]string, len(s.msgIndex))
	for k, v := range s.msgIndex {
		ids := make([]string, len(v))
		copy(ids, v)
		idx[k] = ids
	}
	s.mu.Unlock()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(idx); err != nil {
		return err
	}
	return f.Sync()
}

// LoadMsgIndex загружает индекс msg_id из временного файла.
func (s *Store) LoadMsgIndex(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var idx map[string][]string
	if err := json.Unmarshal(raw, &idx); err != nil {
		return err
	}
	s.mu.Lock()
	s.msgIndex = idx
	s.mu.Unlock()
	return nil
}

// IsUserCollected проверяет, что ВСЕ msg_id из Google есть локально.
// Каждый ID из msgIndex должен присутствовать в Messages.
// Лишние локальные ID не мешают.
func (s *Store) IsUserCollected(email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	expected, hasIndex := s.msgIndex[email]
	if !hasIndex {
		return false
	}

	u, hasData := s.data[email]
	if !hasData {
		return false
	}

	for _, id := range expected {
		if _, ok := u.Messages[id]; !ok {
			return false
		}
	}
	return true
}

// ExpectedMsgIDs возвращает список ожидаемых msg_id для юзера.
func (s *Store) ExpectedMsgIDs(email string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgIndex[email]
}

// UserStats возвращает статистику для юзера: Google count vs Local count.
// Если msgIndex не загружен — Google=0.
func (s *Store) UserStats(email string) (google int, local int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	google = len(s.msgIndex[email])
	if u, ok := s.data[email]; ok {
		local = len(u.Messages)
	}
	return google, local
}
