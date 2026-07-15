package importyandex

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/config"
	"gwsferry/internal/shared/dashboard"
)

// OrchestratorParams — всё что нужно для запуска импорта.
type OrchestratorParams struct {
	Users        []yandexapi.User
	Labels       LabelsFile
	S3           S3Reader
	API          *yandexapi.API
	ClientID     string
	ClientSecret string
}

// MsgWorkers — hardcoded parallelism per user. Not configurable.
const MsgWorkers = 25

// Run запускает пайплайн импорта с resume и dashboard.
func Run(ctx context.Context, params OrchestratorParams, cfg *config.Config, dash *dashboard.Dashboard) {
	userWorkers := cfg.Yandex.UserWorkers
	msgWorkers := MsgWorkers
	statePath := cfg.StateFile
	if statePath == "" {
		statePath = "import_state.json"
	}
	if execPath, err := os.Executable(); err == nil {
		statePath = filepath.Join(filepath.Dir(execPath), statePath)
	}

	log.Printf("[INFO] [ORCH] запуск оркестратора: userWorkers=%d msgWorkers=%d statePath=%s",
		userWorkers, msgWorkers, statePath)

	// Загружаем состояние
	log.Printf("[INFO] [ORCH] загружаю состояние из %s...", statePath)
	st := loadImportState(statePath)
	log.Printf("[INFO] [ORCH] состояние загружено: %d юзеров в state, %d юзеров с ошибками",
		len(st.Users), len(st.Errors))
	for email, status := range st.Users {
		log.Printf("[DEBUG] [ORCH] state: %s → %s", email, status)
	}

	// Фильтруем завершённых
	var pending []yandexapi.User
	for _, u := range params.Users {
		if !st.isUserDone(u.Email) {
			pending = append(pending, u)
			log.Printf("[DEBUG] [ORCH] pending: %s (uid=%d)", u.Email, u.ID)
		} else {
			log.Printf("[DEBUG] [ORCH] skip (done): %s", u.Email)
		}
	}

	log.Printf("[INFO] [ORCH] фильтрация: %d всего, %d pending, %d done",
		len(params.Users), len(pending), len(params.Users)-len(pending))

	if len(pending) == 0 {
		log.Printf("[INFO] [ORCH] все юзеры уже обработаны, выхожу")
		return
	}

	// Обновляем dashboard
	if dash != nil {
		dash.UpdateOverall(func(o *dashboard.OverallState) {
			o.UsersTotal = len(params.Users)
			o.UsersDone = len(params.Users) - len(pending)
			o.UsersPending = len(pending)
		})
	}

	// Очередь
	userChan := make(chan yandexapi.User, len(pending))
	for _, u := range pending {
		userChan <- u
		log.Printf("[DEBUG] [ORCH] добавлен в очередь: %s (uid=%d)", u.Email, u.ID)
	}
	close(userChan)
	log.Printf("[INFO] [ORCH] очередь создана: %d юзеров, channel cap=%d", len(pending), len(pending))

	reportChan := make(chan UserReport, len(pending))

	// Пул UserGoroutine
	log.Printf("[INFO] [ORCH] запускаю %d user-воркеров...", userWorkers)
	var wg sync.WaitGroup
	for i := 0; i < userWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			log.Printf("[DEBUG] [ORCH] user-воркер %d запущен", workerID)
			runUserGoroutine(ctx, userChan, reportChan, st, params, msgWorkers, dash)
			log.Printf("[DEBUG] [ORCH] user-воркер %d завершён", workerID)
		}(i)
	}

	go func() {
		wg.Wait()
		log.Printf("[INFO] [ORCH] все user-воркеры завершены, закрываю reportChan")
		close(reportChan)
	}()

	// Периодический дамп state
	log.Printf("[INFO] [ORCH] запускаю периодический дамп state каждые 60с в %s", statePath)
	stopDumper := startPeriodicDumper(st, statePath)
	defer stopDumper()

	// SIGINT/SIGTERM handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[WARN] [ORCH] получен сигнал %v, сохраняю состояние...", sig)
		saveImportState(st, statePath)
		log.Printf("[WARN] [ORCH] состояние сохранено, выхожу")
		os.Exit(0)
	}()

	// Чтение отчётов
	log.Printf("[INFO] [ORCH] жду отчёты от юзеров...")
	var totalProcessed, totalFailed, totalSkipped int
	for report := range reportChan {
		totalProcessed += report.Processed
		totalFailed += report.Failed
		totalSkipped += report.Skipped

		log.Printf("[INFO] [ORCH] отчёт от %s: processed=%d failed=%d skipped=%d duration=%s",
			report.Email, report.Processed, report.Failed, report.Skipped, report.Duration)

		// Обновляем state
		if report.Failed > 0 {
			st.markUserError(report.Email, "failed")
			log.Printf("[WARN] [ORCH] %s: помечен как error (failed=%d)", report.Email, report.Failed)
		} else {
			st.markUserDone(report.Email)
			log.Printf("[INFO] [ORCH] %s: помечен как done", report.Email)
		}
		saveImportState(st, statePath)
		log.Printf("[DEBUG] [ORCH] state сохранён после %s", report.Email)

		// Обновляем dashboard
		if dash != nil {
			dash.UpdateOverall(func(o *dashboard.OverallState) {
				o.UsersDone++
				if report.Failed > 0 {
					o.UsersError++
					o.UsersPending--
				} else {
					o.UsersPending--
				}
			})
			dash.Log(levelFromReport(report), formatReport(report))
		}
	}

	// Финальное сохранение
	saveImportState(st, statePath)

	log.Printf("[INFO] [ORCH] завершено: %d обработано, %d ошибок, %d пропущено (из %d)",
		totalProcessed, totalFailed, totalSkipped, len(params.Users))
}

func formatReport(r UserReport) string {
	if r.Failed > 0 {
		return fmt.Sprintf("%s: %d ok, %d failed (%s)", r.Email, r.Processed, r.Failed, r.Duration)
	}
	return fmt.Sprintf("%s: %d писем (%s)", r.Email, r.Processed, r.Duration)
}

func levelFromReport(r UserReport) string {
	if r.Failed > 0 {
		return "ERROR"
	}
	if r.Skipped > 0 {
		return "WARN"
	}
	return "INFO"
}
