package dashboard

import "strings"

const maxLogLines = 12

type WorkerState struct {
	Task       string
	Status     string
	ETA        string
	RetryRound string
	BatchSize  string
	DeadQuota  bool
}

type OverallState struct {
	UsersTotal   int
	UsersDone    int
	UsersError   int
	UsersPending int
	MemoryMB     float64
	MemoryLimit  int
}

type logLine struct {
	level  string
	text   string
	worker string // worker key (e.g. "sa0") or "" for general
}

func isWorkerTagged(msg string) bool {
	if !strings.HasPrefix(msg, "[sa") {
		return false
	}
	return strings.Index(msg, "]") > 0
}

// extractWorkerKey извлекает ключ воркера из лог-строки вида "[sa0] ..."
func extractWorkerKey(msg string) string {
	if !isWorkerTagged(msg) {
		return ""
	}
	end := strings.Index(msg, "]")
	if end < 0 {
		return ""
	}
	return msg[1:end]
}

// workerLogsByKey возвращает логи для конкретного воркера
func workerLogsByKey(logs []logLine, key string) []logLine {
	var result []logLine
	for _, l := range logs {
		if l.worker == key {
			result = append(result, l)
		}
	}
	return result
}
