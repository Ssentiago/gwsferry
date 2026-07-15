package importyandex

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/etatracker"
	"gwsferry/internal/shared/util"
)

// OrchestratorParams — параметры для одного импорта source → target.
type OrchestratorParams struct {
	SourceUser   yandexapi.User
	TargetUser   yandexapi.User
	Labels       LabelsFile
	S3           S3Reader
	API          *yandexapi.API
	ClientID     string
	ClientSecret string
}

// MsgWorkers — parallelism per user.
const MsgWorkers = 25

// UserReport — агрегированный отчёт по импорту.
type UserReport struct {
	Source    string
	Target    string
	Processed int
	Failed    int
	Skipped   int
	Duration  time.Duration
}

func dashUpdate(dash *dashboard.Dashboard, key, task, status string) {
	if dash != nil {
		dash.UpdateWorker(key, task, status, "")
	}
}

// RunUserImport — импорт писем из SOURCE в TARGET для одного пользователя.
// Использует ShutdownFlag для graceful shutdown по SIGINT/SIGTERM.
func RunUserImport(
	ctx context.Context,
	params OrchestratorParams,
	st *ImportState,
	statePath string,
	dash *dashboard.Dashboard,
	workerKey string,
) {
	start := time.Now()
	report := UserReport{Source: params.SourceUser.Email, Target: params.TargetUser.Email}

	// Shutdown coordination
	shutdown := util.NewShutdownFlag()
	ctx = shutdown.Context()

	// Listen for Ctrl+C from Bubble Tea (edinstvennyy put' v raw mode)
	go func() {
		<-dash.QuitCh()
		if !shutdown.IsSet() {
			shutdown.Set()
		}
		log.Println("[WARN] [SHUTDOWN] Ctrl+C: принудительный выход через 5с.")
		go func() {
			time.Sleep(5 * time.Second)
			saveImportState(st, statePath)
			os.Exit(0)
		}()
	}()

	log.Printf("[INFO] [USER] ======== ИМПОРТ %s → %s ========", params.SourceUser.Email, params.TargetUser.Email)

	dashUpdate(dash, workerKey, "загрузка лейблов", "working")

	// 1. BuildLetters
	log.Printf("[INFO] [USER] загружаю письма из S3 (source=%s)...", params.SourceUser.Email)
	letters, warnings, err := BuildLetters(ctx, params.S3, params.SourceUser.Email, params.Labels)
	if err != nil {
		log.Printf("[ERROR] [USER] BuildLetters failed: %v", err)
		st.markUserError(params.SourceUser.Email, err.Error())
		dashUpdate(dash, workerKey, "ошибка", "error")
		report.Duration = time.Since(start)
		return
	}
	report.Skipped = len(warnings)
	log.Printf("[INFO] [USER] BuildLetters: %d писем, %d warnings", len(letters), len(warnings))

	if len(letters) == 0 {
		log.Printf("[INFO] [USER] 0 писем, done")
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, "0 писем", "done")
		return
	}

	// 2. Фильтруем по resume
	var pending []Letter
	for _, l := range letters {
		if st.isMessageDone(params.SourceUser.Email, l.MsgID) {
			report.Skipped++
			continue
		}
		pending = append(pending, l)
	}
	totalLetters := len(letters)
	stateSkipped := report.Skipped - len(warnings)
	log.Printf("[INFO] [USER] к обработке %d/%d (resume skip %d)", len(pending), totalLetters, stateSkipped)

	if len(pending) == 0 {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d", totalLetters, totalLetters), "done")
		return
	}

	dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d", stateSkipped, totalLetters), "working")

	// 3. Dedup — проверяем TARGET IMAP
	folderSet := make(map[string]struct{})
	for _, l := range pending {
		folderSet[ResolveFolder(l.LabelIDs, l.LabelNames)] = struct{}{}
	}
	uniqueFolders := make([]string, 0, len(folderSet))
	for f := range folderSet {
		uniqueFolders = append(uniqueFolders, f)
	}
	log.Printf("[INFO] [USER] pre-fetch dedup в TARGET (%s) для %d папок", params.TargetUser.Email, len(uniqueFolders))

	var existingIDs map[string]bool
	preFetchWorker := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, &SharedToken{}, nil, nil)
	existingIDs, err = preFetchWorker.PreFetchExistingIDs(ctx, uniqueFolders)
	preFetchWorker.Close()
	if err != nil {
		log.Printf("[WARN] [USER] pre-fetch failed (1/2): %v, retry...", err)
		w2 := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, &SharedToken{}, nil, nil)
		existingIDs, err = w2.PreFetchExistingIDs(ctx, uniqueFolders)
		w2.Close()
		if err != nil {
			log.Printf("[WARN] [USER] pre-fetch failed (2/2): %v, без дедупа", err)
			existingIDs = nil
		}
	}

	var filtered []Letter
	skippedByServer := 0
	for _, l := range pending {
		if existingIDs != nil && existingIDs[l.MsgID] {
			skippedByServer++
			continue
		}
		filtered = append(filtered, l)
	}
	if skippedByServer > 0 {
		log.Printf("[INFO] [USER] пропущено %d (уже в TARGET), к обработке %d", skippedByServer, len(filtered))
		report.Skipped += skippedByServer
		stateSkipped += skippedByServer
	}
	pending = filtered

	if len(pending) == 0 {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d", totalLetters, totalLetters), "done")
		return
	}

	dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d", stateSkipped, totalLetters), "working")

	// 4. Append — заливаем в TARGET IMAP
	taskChan := make(chan LetterTask, len(pending))
	for _, l := range pending {
		taskChan <- LetterTask{Letter: l}
	}
	close(taskChan)

	sharedToken := &SharedToken{}
	createdFolders := &sync.Map{}
	msgReportChan := make(chan MessageReport, len(pending))

	eta := etatracker.New(0.3)
	eta.Record(0)

	var msgWg sync.WaitGroup
	msgWg.Add(MsgWorkers)
	for i := 0; i < MsgWorkers; i++ {
		go func() {
			defer msgWg.Done()
			worker := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, sharedToken, createdFolders,
				func(status string) {
					dashUpdate(dash, workerKey, status, "working")
				})
			runMessagesGoroutine(ctx, params.S3, worker, taskChan, msgReportChan)
			worker.Close()
		}()
	}

	go func() {
		msgWg.Wait()
		close(msgReportChan)
	}()

	for {
		select {
		case <-ctx.Done():
			goto done
		case mr, ok := <-msgReportChan:
			if !ok {
				goto done
			}
			if mr.Err != nil {
				report.Failed++
			} else {
				report.Processed++
				st.markMessageDone(params.SourceUser.Email, mr.MsgID)
			}
			eta.Record(1)
			done := stateSkipped + report.Processed + report.Failed
			remaining := totalLetters - done
			etaStr := etatracker.FormatETA(eta.EstimateSeconds(remaining))
			dash.UpdateWorker(workerKey,
				fmt.Sprintf("%d/%d", done, totalLetters),
				"working", etaStr)
		}
	}
done:

	if ctx.Err() != nil {
		// Shutdown: помечаем юзера как pending для следующего запуска
		st.markMessageDone(params.SourceUser.Email, "") // no-op marker
		dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d (interrupted)", stateSkipped+report.Processed+report.Failed, totalLetters), "working")
		log.Printf("[WARN] [USER] ======== SHUTDOWN %s → %s: %d ok, %d failed, %d skipped ========",
			params.SourceUser.Email, params.TargetUser.Email,
			report.Processed, report.Failed, report.Skipped)
	} else if report.Failed > 0 {
		st.markUserError(params.SourceUser.Email, fmt.Sprintf("%d failed", report.Failed))
		dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d ok, %d failed", stateSkipped+report.Processed, totalLetters, report.Failed), "error")
	} else {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, fmt.Sprintf("%d/%d", totalLetters, totalLetters), "done")
	}

	log.Printf("[INFO] [USER] ======== ГОТОВО %s → %s: %d ok, %d failed, %d skipped, %s ========",
		params.SourceUser.Email, params.TargetUser.Email,
		report.Processed, report.Failed, report.Skipped, time.Since(start))

	if statePath != "" {
		saveImportState(st, statePath)
	}
}
