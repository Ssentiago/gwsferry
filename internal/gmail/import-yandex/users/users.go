package users

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
)

// LoadUsers загружает список юзеров из JSON-файла, загружает полный список
// из Yandex API, фильтрует и возвращает только тех, чей email есть в файле.
// filePath — путь к yandex_users.json рядом с бинарём.
func LoadUsers(api *yandexapi.API, filePath string) ([]yandexapi.User, error) {
	start := time.Now()
	log.Printf("[INFO] [USERS] загрузка юзеров из %s", filePath)
	allowed, err := loadEmails(filePath)
	if err != nil {
		log.Printf("[ERROR] [USERS] ошибка чтения emails из файла за %s: %v", time.Since(start), err)
		return nil, fmt.Errorf("load allowed emails: %w", err)
	}
	if len(allowed) == 0 {
		log.Printf("[ERROR] [USERS] нет emails в файле %s", filePath)
		return nil, fmt.Errorf("no allowed emails in %s", filePath)
	}
	log.Printf("[INFO] [USERS] загружено %d уникальных emails из файла за %s", len(allowed), time.Since(start))

	apiStart := time.Now()
	log.Printf("[INFO] [USERS] запрашиваю список юзеров из Yandex API...")
	all, err := api.ListUsers()
	if err != nil {
		log.Printf("[ERROR] [USERS] ошибка ListUsers API за %s: %v", time.Since(apiStart), err)
		return nil, fmt.Errorf("list yandex users: %w", err)
	}
	log.Printf("[INFO] [USERS] Yandex API вернул %d юзеров (enabled+active) за %s", len(all), time.Since(apiStart))

	allowedSet := make(map[string]struct{}, len(allowed))
	for _, email := range allowed {
		allowedSet[email] = struct{}{}
	}

	var matched []yandexapi.User
	var unmatched []string
	for _, u := range all {
		if _, ok := allowedSet[u.Email]; ok {
			matched = append(matched, u)
			log.Printf("[DEBUG] [USERS] match: %s (uid=%d)", u.Email, u.ID)
		} else {
			unmatched = append(unmatched, u.Email)
		}
	}

	log.Printf("[INFO] [USERS] совпадение: %d/%d (из API), %d не совпали: %v (total %s)",
		len(matched), len(all), len(unmatched), unmatched, time.Since(start))

	if len(matched) == 0 {
		log.Printf("[WARN] [USERS] ни один email из файла не найден в Yandex API!")
	}

	return matched, nil
}

func loadEmails(filePath string) ([]string, error) {
	log.Printf("[DEBUG] [USERS] чтение файла %s", filePath)
	raw, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("[ERROR] [USERS] чтение %s: %v", filePath, err)
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}
	log.Printf("[DEBUG] [USERS] файл прочитан: %d bytes", len(raw))

	// Пробуем формат 1: {"users": ["email1", "email2"]}
	var asArray struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(raw, &asArray); err == nil && len(asArray.Users) > 0 {
		return dedup(asArray.Users), nil
	}

	// Пробуем формат 2: {"users": {"email1": {...}, "email2": {...}}}
	var asObject struct {
		Users map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil && len(asObject.Users) > 0 {
		var emails []string
		for email := range asObject.Users {
			emails = append(emails, email)
		}
		return dedup(emails), nil
	}

	// Пробуем формат 3: [{"email": "..."}, ...] (массив объектов)
	var asSlice []struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &asSlice); err == nil && len(asSlice) > 0 {
		var emails []string
		for _, u := range asSlice {
			if u.Email != "" {
				emails = append(emails, u.Email)
			}
		}
		return dedup(emails), nil
	}

	return nil, fmt.Errorf("не удалось распарсить %s: неизвестный формат", filePath)
}

func dedup(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	var result []string
	for _, item := range items {
		if _, ok := seen[item]; !ok {
			seen[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

// UserFilePath возвращает путь к yandex_users.json рядом с бинарём.
func UserFilePath() string {
	execPath, err := os.Executable()
	if err != nil {
		return "yandex_users.json"
	}
	return filepath.Join(filepath.Dir(execPath), "yandex_users.json")
}
