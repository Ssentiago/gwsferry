package importyandex

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/dashboard"
)

// UserReport — агрегированный отчёт по одному юзеру.
type UserReport struct {
	Email     string
	Processed int
	Failed    int
	Skipped   int
	Duration  time.Duration
}

// dashUpdate — обёртка: обновляет dashboard, если он не nil.
func dashUpdate(dash *dashboard.Dashboard, key, task, status string) {
	if dash != nil {
		dash.UpdateWorker(key, task, status, "")
	}
}

// runUserGoroutine — берёт юзера из канала, грузит письма, запускает пул MessagesGoroutine.
func runUserGoroutine(
	ctx context.Context,
	userChan <-chan yandexapi.User,
	reportChan chan<- UserReport,
	st *ImportState,
	params OrchestratorParams,
	msgWorkers int,
	dash *dashboard.Dashboard,
) {
	for user := range userChan {
		start := time.Now()
		report := UserReport{Email: user.Email}
		workerKey := user.Email

		log.Printf("[INFO] [USER] ======== НАЧАЛО обработки %s (uid=%d) ========", user.Email, user.ID)

		dashUpdate(dash, workerKey, "загрузка лейблов", "working")

		// Загружаем письма из S3 + лейблы
		log.Printf("[INFO] [USER] %s: загружаю письма из S3 и матчу с лейблами...", user.Email)
		letters, warnings, err := BuildLetters(ctx, params.S3, user.Email, params.Labels)
		if err != nil {
			log.Printf("[ERROR] [USER] %s: BuildLetters failed: %v", user.Email, err)
			st.markUserError(user.Email, err.Error())
			dashUpdate(dash, workerKey, "ошибка", "error")
			report.Duration = time.Since(start)
			reportChan <- report
			continue
		}

		report.Skipped = len(warnings)
		if len(warnings) > 0 {
			for _, w := range warnings {
				log.Printf("[WARN] [USER] %s: %s", user.Email, w)
			}
		}

		log.Printf("[INFO] [USER] %s: BuildLetters вернул %d писем, %d warnings", user.Email, len(letters), len(warnings))

		if len(letters) == 0 {
			log.Printf("[INFO] [USER] %s: 0 писем с лейблами, пропуск пользователя", user.Email)
			st.markUserDone(user.Email)
			dashUpdate(dash, workerKey, "пропуск (0 писем)", "done")
			report.Duration = time.Since(start)
			reportChan <- report
			continue
		}

		// Фильтруем уже обработанные (resume)
		log.Printf("[INFO] [USER] %s: фильтрую по resume (isMessageDone)...", user.Email)
		var pending []Letter
		for _, l := range letters {
			if st.isMessageDone(user.Email, l.MsgID) {
				report.Skipped++
				log.Printf("[DEBUG] [USER] %s: resume skip msgID=%s path=%s", user.Email, l.MsgID, l.Path)
				continue
			}
			pending = append(pending, l)
		}

		if len(pending) == 0 {
			log.Printf("[INFO] [USER] %s: все %d писем уже обработаны (resume), помечаю done", user.Email, len(letters))
			st.markUserDone(user.Email)
			dashUpdate(dash, workerKey, "уже импортирован", "done")
			report.Duration = time.Since(start)
			reportChan <- report
			continue
		}

		log.Printf("[INFO] [USER] %s: к обработке %d/%d писем (resume пропущено %d), запускаю %d горутин",
			user.Email, len(pending), len(letters), report.Skipped-len(warnings), msgWorkers)

		for i, l := range pending {
			if i < 5 || i == len(pending)-1 {
				log.Printf("[DEBUG] [USER] %s: pending[%d/%d] msgID=%s path=%s labels=%v",
					user.Email, i, len(pending), l.MsgID, l.Path, l.LabelIDs)
			} else if i == 5 {
				log.Printf("[DEBUG] [USER] %s: ... (%d писем пропущено в логе) ...", user.Email, len(pending)-6)
			}
		}

		dashUpdate(dash, workerKey, fmt.Sprintf("0/%d", len(pending)), "working")

		// Канал с письмами — pipeline: скачивание + заливка идут параллельно
		// Буфер = len(pending), иначе deadlock: goroutines ещё не стартовали,
		// а запись в полный канал блокируется
		taskChan := make(chan LetterTask, len(pending))
		for _, l := range pending {
			taskChan <- LetterTask{Letter: l}
		}
		close(taskChan)
		log.Printf("[INFO] [USER] %s: taskChan создан и заполнен: %d задач", user.Email, len(pending))

		// Пул MessagesGoroutine — каждая горутина со своим ImapWorker и соединением
		// Общий токен и папки на уровне user — шарятся между всеми msg-воркерами
		sharedToken := &SharedToken{}
		createdFolders := &sync.Map{}
		msgReportChan := make(chan MessageReport, len(pending))
		var msgWg sync.WaitGroup
		for i := 0; i < msgWorkers; i++ {
			msgWg.Add(1)
			go func(workerID int) {
				defer msgWg.Done()
				worker := NewImapWorker(user, params.API, params.ClientID, params.ClientSecret, sharedToken, createdFolders)
				log.Printf("[DEBUG] [USER] %s: msg-воркер %d запущен (ImapWorker)", user.Email, workerID)
				runMessagesGoroutine(ctx, params.S3, worker, taskChan, msgReportChan)
				worker.Close()
				log.Printf("[DEBUG] [USER] %s: msg-воркер %d завершён", user.Email, workerID)
			}(i)
		}

		go func() {
			msgWg.Wait()
			log.Printf("[DEBUG] [USER] %s: все msg-воркеры завершены, закрываю msgReportChan", user.Email)
			close(msgReportChan)
		}()

		// Агрегация + dashboard
		log.Printf("[INFO] [USER] %s: жду результаты от msg-воркеров...", user.Email)
		for mr := range msgReportChan {
			switch {
			case mr.Err != nil:
				report.Failed++
				log.Printf("[ERROR] [USER] %s msgID=%s FAILED: %v (total failed=%d)", user.Email, mr.MsgID, mr.Err, report.Failed)
			default:
				report.Processed++
				st.markMessageDone(user.Email, mr.MsgID)
				log.Printf("[INFO] [USER] %s msgID=%s OK (total processed=%d)", user.Email, mr.MsgID, report.Processed)
			}
			dashUpdate(dash, workerKey,
				fmt.Sprintf("%d/%d", report.Processed+report.Failed, len(pending)),
				"working")
		}

		report.Duration = time.Since(start)

		if report.Failed > 0 {
			dashUpdate(dash, workerKey, fmt.Sprintf("%d ok, %d failed", report.Processed, report.Failed), "error")
			log.Printf("[WARN] [USER] ======== ЗАВЕРШЕНО С ОШИБКАМИ %s: %d ok, %d failed, %d skipped, duration=%s ========",
				user.Email, report.Processed, report.Failed, report.Skipped, report.Duration)
		} else {
			dashUpdate(dash, workerKey, fmt.Sprintf("%d писем", report.Processed), "done")
			log.Printf("[INFO] [USER] ======== ЗАВЕРШЕНО OK %s: %d processed, %d skipped, duration=%s ========",
				user.Email, report.Processed, report.Skipped, report.Duration)
		}

		reportChan <- report
	}
}
