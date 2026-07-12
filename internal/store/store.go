// Package store отвечает за персистентное хранение собранных Gmail
// labelIds на диске и за resume-логику НА УРОВНЕ КОНКРЕТНОГО msg_id.
//
// Структура записи юзера в файле: {"messages": {msg_id: [labelIds]},
// "label_names": {labelId: name}, "done": bool}. Каждый msg_id пишется
// в "messages" сразу по получении ответа от API (см. SaveMsgLabels),
// не дожидаясь завершения всего ящика - периодический дамп на диск
// сохраняет и частично собранных юзеров.
//
// Конкурентная гонка: если снапшотить map и мутировать вложенные map'ы
// (User.Messages) из других горутин ПОКА снапшот ещё сериализуется в
// json.Marshal - data race, которую -race поймает. Поэтому снапшот
// делается под мьютексом С ГЛУБОКИМ КОПИРОВАНИЕМ вложенных map'ов
// (deepCopy). Запись файла сериализована отдельным мьютексом (fileMu),
// чтобы периодический дампер и финальный дамп в main() не писали в
// один и тот же .tmp-файл одновременно.
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
	Done       bool                `json:"done"`
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

// Load читает уже собранные лейблы с диска. Формат файла:
// {email: {"messages": {msg_id: [labelIds]}, "label_names": {id: name}, "done": bool}}.
func (s *Store) Load() (doneCount, partialCount int, err error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("чтение %s: %w", s.path, err)
	}

	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawTop); err != nil {
		return 0, 0, fmt.Errorf("парсинг %s: %w", s.path, err)
	}

	data := make(map[string]*User, len(rawTop))
	for email, rawUser := range rawTop {
		var u struct {
			Messages   map[string][]string `json:"messages"`
			LabelNames map[string]string   `json:"label_names"`
			Done       bool                `json:"done"`
		}
		if err := json.Unmarshal(rawUser, &u); err != nil {
			return 0, 0, fmt.Errorf("парсинг записи для %s: %w", email, err)
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
			Done:       u.Done,
		}
	}

	s.mu.Lock()
	s.data = data
	s.mu.Unlock()

	for _, u := range data {
		if u.Done {
			doneCount++
		} else {
			partialCount++
		}
	}
	return doneCount, partialCount, nil
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
		dst[email] = &User{Messages: msgs, LabelNames: names, Done: u.Done}
	}
	return dst
}

// Save делает atomic-дамп всего накопленного результата на диск: пишет
// во временный файл, fsync, затем rename поверх целевого файла. rename
// на одной файловой системе атомарен - читатель никогда не увидит
// частично записанный файл, что бы ни случилось (падение процесса,
// конкурентный Save).
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

// CachedLabelNames возвращает label_names для юзера, если они уже были
// собраны в прошлом прогоне.
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
// получении - не ждёт финализации юзера. Это и даёт гранулярность
// resume по msg_id: даже если процесс упадёт посреди батча, всё, что
// уже долетело до этой функции, переживёт следующий периодический дамп
// на диск.
func (s *Store) SaveMsgLabels(email, msgID string, labelIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.getOrCreate(email)
	u.Messages[msgID] = labelIDs
}

// FinalizeUser сохраняет label_names для юзера. Не выставляет done=true —
// готовность определяется динамически через IsUserCollected.
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

// IsUserCollected проверяет, собраны ли ВСЕ ожидаемые msg_id для юзера.
// Сравнивает количество собранных с количеством ожидаемых из msgIndex.
// Если msgIndex пуст — fallback на старый done-флаг.
func (s *Store) IsUserCollected(email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	expected, hasIndex := s.msgIndex[email]
	u, hasData := s.data[email]

	// Если индекс есть — сравниваем количество.
	if hasIndex && hasData {
		return len(u.Messages) >= len(expected)
	}

	// Fallback: старый done-флаг.
	return hasData && u.Done
}

// ExpectedMsgIDs возвращает список ожидаемых msg_id для юзера.
func (s *Store) ExpectedMsgIDs(email string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgIndex[email]
}
