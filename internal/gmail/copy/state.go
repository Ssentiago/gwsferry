package copy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
)

type migrationState struct {
	Users  map[string]string `json:"users"`
	Errors map[string]string `json:"errors,omitempty"`
}

var stateFileMu sync.Mutex

func loadState() *migrationState {
	log.Printf("[DEBUG] [STATE] загрузка из %s...", stateFile)
	s := &migrationState{Users: make(map[string]string)}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		log.Printf("[DEBUG] [STATE] файл %s не найден, создаю пустой state", stateFile)
		return s
	}
	json.Unmarshal(raw, s)
	if s.Users == nil {
		s.Users = make(map[string]string)
	}
	if s.Errors == nil {
		s.Errors = make(map[string]string)
	}
	doneCount, errCount := 0, 0
	for _, status := range s.Users {
		if status == "done" {
			doneCount++
		} else if status == "error" {
			errCount++
		}
	}
	log.Printf("[INFO] [STATE] загружен из %s: %d юзеров (%d done, %d error)", stateFile, len(s.Users), doneCount, errCount)
	return s
}

func saveState(s *migrationState) {
	stateFileMu.Lock()
	defer stateFileMu.Unlock()
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := stateFile + ".tmp"
	os.WriteFile(tmp, data, 0644)
	os.Rename(tmp, stateFile)
	log.Printf("[DEBUG] [STATE] сохранён в %s (%d bytes)", stateFile, len(data))
}

func setUserStatus(s *migrationState, email, status, errorDetail string) {
	s.Users[email] = status
	if errorDetail != "" {
		if s.Errors == nil {
			s.Errors = make(map[string]string)
		}
		s.Errors[email] = errorDetail
	}
	log.Printf("[DEBUG] [STATE] %s: статус → %s", email, status)
	saveState(s)
}

func getRSSMB() float64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range splitLines(string(raw)) {
		if len(line) > 6 && line[:6] == "VmRSS:" {
			var kb int
			fmt.Sscanf(line[6:], "%d", &kb)
			return float64(kb) / 1024
		}
	}
	return 0
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
