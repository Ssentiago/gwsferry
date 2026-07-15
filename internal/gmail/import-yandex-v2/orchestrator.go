package importyandex

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	yandexapi "gwsferry/internal/gmail/import-yandex-v2/api"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/etatracker"
)

// OrchestratorParams — параметры для одного импорта source → target.
type OrchestratorParams struct {
	SourceUser   yandexapi.User // чьи письма в S3
	TargetUser   yandexapi.User // в чей IMAP заливаем
	Labels       LabelsFile
	S3           S3Reader
	API          *yandexapi.API
	ClientID     string
	ClientSecret string
}

// MsgWorkers — parallelism per user. Константа.
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

func dash_eta(dash *dashboard.Dashboard, key, eta string) {
	if dash != nil {
		dash.UpdateWorker(key, "", "", eta)
	}
}

// RunUserImport — импорт писем из SOURCE в TARGET для одного пользователя.
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

	// Signal handler — сохраняем state при Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("[WARN] [USER] получен сигнал %v, сохраняю состояние...", sig)
		saveImportState(st, statePath)
		log.Printf("[WARN] [USER] состояние сохранено, выхожу")
		os.Exit(0)
	}()

	log.Printf("[INFO] [USER] ======== ИМПОРТ %s → %s ========", params.SourceUser.Email, params.TargetUser.Email)

	dashUpdate(dash, workerKey, "загрузка лейблов", "working")

	// 1. BuildLetters — загружаем письма из S3 по SOURCE email
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
	log.Printf("[INFO] [USER] к обработке %d/%d (resume skip %d)", len(pending), len(letters), report.Skipped-len(warnings))

	if len(pending) == 0 {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, "уже импортировано", "done")
		return
	}

	dashUpdate(dash, workerKey, fmt.Sprintf("0/%d", len(pending)), "working")

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
	preFetchWorker := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, &SharedToken{}, nil)
	existingIDs, err = preFetchWorker.PreFetchExistingIDs(ctx, uniqueFolders)
	preFetchWorker.Close()
	if err != nil {
		log.Printf("[WARN] [USER] pre-fetch failed (попытка 1/2): %v, retry...", err)
		preFetchWorker2 := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, &SharedToken{}, nil)
		existingIDs, err = preFetchWorker2.PreFetchExistingIDs(ctx, uniqueFolders)
		preFetchWorker2.Close()
		if err != nil {
			log.Printf("[WARN] [USER] pre-fetch failed (попытка 2/2): %v, без дедупа", err)
			existingIDs = nil
		}
	}

	// Фильтруем уже существующие в TARGET
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
	}
	pending = filtered

	if len(pending) == 0 {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, "уже в TARGET", "done")
		return
	}

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
	eta.Record(0) // инициализация

	for i := 0; i < MsgWorkers; i++ {
		go func() {
			worker := NewImapWorker(params.TargetUser, params.API, params.ClientID, params.ClientSecret, sharedToken, createdFolders)
			runMessagesGoroutine(ctx, params.S3, worker, taskChan, msgReportChan)
			worker.Close()
		}()
	}

	for mr := range msgReportChan {
		if mr.Err != nil {
			report.Failed++
		} else {
			report.Processed++
			st.markMessageDone(params.SourceUser.Email, mr.MsgID)
		}
		eta.Record(1)
		remaining := len(pending) - report.Processed - report.Failed
		etaStr := etatracker.FormatETA(eta.EstimateSeconds(remaining))
		dashUpdate(dash, workerKey,
			fmt.Sprintf("%d/%d", report.Processed+report.Failed, len(pending)),
			"working")
		dash_eta(dash, workerKey, etaStr)
	}

	if report.Failed > 0 {
		st.markUserError(params.SourceUser.Email, fmt.Sprintf("%d failed", report.Failed))
		dashUpdate(dash, workerKey, fmt.Sprintf("%d ok, %d failed", report.Processed, report.Failed), "error")
	} else {
		st.markUserDone(params.SourceUser.Email)
		dashUpdate(dash, workerKey, fmt.Sprintf("%d писем", report.Processed), "done")
	}

	log.Printf("[INFO] [USER] ======== ГОТОВО %s → %s: %d ok, %d failed, %d skipped, %s ========",
		params.SourceUser.Email, params.TargetUser.Email,
		report.Processed, report.Failed, report.Skipped, time.Since(start))

	// Сохраняем state
	if statePath != "" {
		saveImportState(st, statePath)
	}
}
