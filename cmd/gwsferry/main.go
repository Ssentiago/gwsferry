package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/pterm/pterm"
	"gmail-labels/internal/dashboard"
	"gmail-labels/internal/gmailapi"
	"gmail-labels/internal/store"
)

// syncFile — обёртка, вызывает Sync() после каждой записи для lnav/tail -f.
type syncFile struct{ f *os.File }

func (s *syncFile) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	if err != nil {
		return n, err
	}
	s.f.Sync()
	return n, nil
}

func cprint(color func(format string, a ...any) *pterm.Style, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println(color("").Sprint(msg))
	log.Printf("[INFO] %s", msg)
}

// ==========================================
// КОНФИГУРАЦИЯ
// ==========================================
const (
	workspacePrefix = "ru"
	usersJSONPath   = "users.json"
	saKeysDir       = "workers"

	labelsDumpInterval = 60 * time.Second

	maxRetries           = 5
	workerStartStagger   = 3 * time.Second
	maxConcurrentWorkers = 15

	batchSizeMax      = 80
	batchSizeMin      = 10
	batchSizeStart    = 40
	batchShrinkFactor = 0.5
	batchGrowthStreak = 8
	batchGrowthStep   = 10

	interBatchDelay = 200 * time.Millisecond

	rateLimitBackoffBase          = 60 * time.Second
	concurrentLimitBackoffBase    = 3 * time.Second
	concurrentLimitBackoffMaxRnds = 6

	stuckThreadsShutdownThreshold = 8
	etaWindowSize                 = 10
)

var labelsFile = fmt.Sprintf("migration_labels_%s.json", workspacePrefix)

type shutdownFlag struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newShutdownFlag() *shutdownFlag {
	ctx, cancel := context.WithCancel(context.Background())
	return &shutdownFlag{ctx: ctx, cancel: cancel}
}
func (s *shutdownFlag) IsSet() bool           { return s.ctx.Err() != nil }
func (s *shutdownFlag) Set()                  { s.cancel() }
func (s *shutdownFlag) Done() <-chan struct{} { return s.ctx.Done() }

// ==========================================
// ADAPTIVE BATCH SIZE
// ==========================================
type adaptiveBatchSize struct {
	current     int
	cleanStreak int
}

func newAdaptiveBatchSize() *adaptiveBatchSize {
	return &adaptiveBatchSize{current: batchSizeStart}
}

func (a *adaptiveBatchSize) shrink() int {
	a.current = int(float64(a.current) * batchShrinkFactor)
	if a.current < batchSizeMin {
		a.current = batchSizeMin
	}
	a.cleanStreak = 0
	return a.current
}

func (a *adaptiveBatchSize) reportCleanBatch() {
	a.cleanStreak++
	if a.cleanStreak >= batchGrowthStreak && a.current < batchSizeMax {
		a.current += batchGrowthStep
		if a.current > batchSizeMax {
			a.current = batchSizeMax
		}
		a.cleanStreak = 0
	}
}

// ==========================================
// ETA TRACKER
// ==========================================
type etaPoint struct {
	t         time.Time
	collected int
}

type etaTracker struct {
	points []etaPoint
}

func (e *etaTracker) record(collected int) {
	e.points = append(e.points, etaPoint{t: time.Now(), collected: collected})
	if len(e.points) > etaWindowSize {
		e.points = e.points[1:]
	}
}

func (e *etaTracker) estimateSeconds(remaining int) float64 {
	if len(e.points) < 2 || remaining <= 0 {
		return -1
	}
	first, last := e.points[0], e.points[len(e.points)-1]
	elapsed := last.t.Sub(first.t).Seconds()
	collectedInWindow := last.collected - first.collected
	if elapsed <= 0 || collectedInWindow <= 0 {
		return -1
	}
	speed := float64(collectedInWindow) / elapsed
	return float64(remaining) / speed
}

func formatETA(seconds float64) string {
	if seconds < 0 {
		return "--:--"
	}
	s := int(seconds)
	if s < 3600 {
		return fmt.Sprintf("%02d:%02d", s/60, s%60)
	}
	h := s / 3600
	m := (s % 3600) / 60
	return fmt.Sprintf("%d:%02d:00", h, m)
}

// ==========================================
// USERS.JSON
// ==========================================
type userRecord struct {
	Email string `json:"Email Address [Required]"`
}

func loadEmails(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Users []userRecord `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var emails []string
	for _, u := range doc.Users {
		if u.Email != "" {
			emails = append(emails, u.Email)
		}
	}
	return emails, nil
}

// ==========================================
// SERVICE ACCOUNT KEYS
// ==========================================
func loadServiceAccountKeys(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			keys = append(keys, filepath.Join(dir, e.Name()))
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("в %s нет .json ключей", dir)
	}
	// Natural sort: sa1, sa2, ..., sa9, sa10, sa11 (не sa1, sa10, sa11, sa2).
	sort.Slice(keys, func(i, j int) bool {
		return naturalLess(filepath.Base(keys[i]), filepath.Base(keys[j]))
	})
	return keys, nil
}

// naturalLess сравнивает строки с учётом числовых блоков:
// "sa2" < "sa10", "key_1" < "key_2" < "key_10".
func naturalLess(a, b string) bool {
	for a != "" || b != "" {
		if a == "" {
			return true
		}
		if b == "" {
			return false
		}
		ai := a[0]
		bi := b[0]
		da := ai >= '0' && ai <= '9'
		db := bi >= '0' && bi <= '9'
		switch {
		case da && !db:
			return true
		case !da && db:
			return false
		case da && db:
			na, restA := readNumber(a)
			nb, restB := readNumber(b)
			if na != nb {
				return na < nb
			}
			a, b = restA, restB
		default:
			if ai != bi {
				return ai < bi
			}
			a = a[1:]
			b = b[1:]
		}
	}
	return false
}

func readNumber(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n := 0
	for _, c := range s[:i] {
		n = n*10 + int(c-'0')
	}
	return n, s[i:]
}

func verifyServiceAccounts(ctx context.Context, keys []string, testEmail string) []string {
	fmt.Println()
	pterm.DefaultSection.Println("Pre-flight проверка сервисных аккаунтов...")
	log.Println("[INFO] >>> Pre-flight проверка сервисных аккаунтов...")
	var valid []string
	for _, key := range keys {
		name := filepath.Base(key)
		svc, err := gmailapi.BuildClient(ctx, key, testEmail)
		if err == nil {
			_, err = gmailapi.ExecWithHardTimeout(ctx, gmailapi.HardTimeout, func(cctx context.Context) (any, error) {
				return svc.Users.GetProfile("me").Context(cctx).Do()
			})
		}
		if err != nil {
			pterm.Error.Printfln("%s -> %v", name, err)
			log.Printf("[ERROR]   [FAIL] %s -> %v", name, err)
			continue
		}
		pterm.Success.Printfln("%s", name)
		log.Printf("[INFO]   [OK]   %s", name)
		valid = append(valid, key)
	}
	return valid
}

// ==========================================
// ВОРКЕР
// ==========================================
type app struct {
	st       *store.Store
	dash     *dashboard.Dashboard
	shutdown *shutdownFlag
}

func (a *app) bumpDone() {
	a.dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersDone++
		if o.UsersPending > 0 {
			o.UsersPending--
		}
	})
}
func (a *app) bumpError() {
	a.dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersError++
		if o.UsersPending > 0 {
			o.UsersPending--
		}
	})
}

func (a *app) worker(ctx context.Context, idx int, saKeyPath string, emailCh chan string, requeue chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	workerKey := fmt.Sprintf("sa%d", idx)

	stagger := time.Duration(idx) * workerStartStagger
	if stagger > 0 {
		a.dash.UpdateWorker(workerKey, "IDLE", fmt.Sprintf("старт через %s", stagger), "")
		select {
		case <-time.After(stagger):
		case <-a.shutdown.Done():
			return
		}
	}
	a.dash.UpdateWorker(workerKey, "IDLE", "подключен", "")

	defer func() {
		a.dash.UpdateWorker(workerKey, "FINISH", "done", "")
	}()

	for {
		if a.shutdown.IsSet() {
			return
		}
		var email string
		select {
		case e, ok := <-emailCh:
			if !ok {
				return
			}
			email = e
		default:
			return
		}

		shortEmail := strings.Split(email, "@")[0]

		if a.st.IsUserCollected(email) {
			a.dash.Log("INFO", fmt.Sprintf("[%s] %s: уже собран, пропуск", workerKey, shortEmail))
			a.bumpDone()
			continue
		}

		a.dash.UpdateWorker(workerKey, shortEmail, "получение labelIds...", "")
		svc, err := gmailapi.BuildClient(ctx, saKeyPath, email)
		if err != nil {
			a.dash.Log("ERROR", fmt.Sprintf("[%s] Клиент для %s не собран: %v", workerKey, email, err))
			a.bumpError()
			continue
		}

		// Берём msg_id из pre-fetched индекса — не тратим API на повторный листинг.
		msgIDs := a.st.ExpectedMsgIDs(email)
		if len(msgIDs) == 0 {
			// Фallback: если индекс пуст (старый запуск без pre-fetch).
			a.dash.UpdateWorker(workerKey, shortEmail, "сбор индексов писем...", "")
			msgIDs, err = gmailapi.ListAllMessageIDs(ctx, svc, email, func(collected, page int) {
				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("индексация: %d писем, стр. %d", collected, page), "")
			})
			if err != nil {
				a.dash.Log("ERROR", fmt.Sprintf("[%s] Листинг %s не удался: %v", workerKey, email, err))
				a.bumpError()
				continue
			}
		}

		total := len(msgIDs)
		if total == 0 {
			names, err := gmailapi.FetchLabelNames(ctx, svc, email)
			if err != nil {
				a.dash.Log("ERROR", fmt.Sprintf("[%s] %s: labels.list не удался (%v), юзер НЕ помечен как done", workerKey, shortEmail, err))
				a.bumpError()
				continue
			}
			a.st.FinalizeUser(email, names)
			a.bumpDone()
			continue
		}

		cached := a.st.CachedLabels(email)
		var pending []string
		for _, id := range msgIDs {
			if _, ok := cached[id]; !ok {
				pending = append(pending, id)
			}
		}
		if len(cached) > 0 {
			a.dash.Log("INFO", fmt.Sprintf("[%s] %s: resume, в кэше %d/%d, осталось %d", workerKey, shortEmail, len(cached), total, len(pending)))
		}

		collected := len(cached)
		retryRound := 0
		concurrentRetryRound := 0
		adaptive := newAdaptiveBatchSize()
		eta := &etaTracker{}
		eta.record(collected)

		fatalQuota := false
		var errorLog []string

		for len(pending) > 0 && retryRound < maxRetries {
			if a.shutdown.IsSet() {
				break
			}
			var nextPending []string
			roundConcurrentOnly := true
			anyBatches := false

			for i := 0; i < len(pending); {
				if a.shutdown.IsSet() {
					nextPending = append(nextPending, pending[i:]...)
					break
				}
				chunkSize := adaptive.current
				end := i + chunkSize
				if end > len(pending) {
					end = len(pending)
				}
				chunk := pending[i:end]
				i = end
				anyBatches = true

				res, err := gmailapi.FetchLabelsBatch(ctx, svc, chunk, func(msgID string, labelIDs []string) {
					a.st.SaveMsgLabels(email, msgID, labelIDs)
				})
				if err != nil {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ошибка: %v", workerKey, shortEmail, len(chunk), err)
					if dq, ok := asDailyQuota(err); ok {
						leftover := dq.PendingIDs
						nextPending = append(append([]string{}, leftover...), pending[i:]...)
						fatalQuota = true
						break
					}
					errorLog = append(errorLog, fmt.Sprintf("batch error: %v", err))
					nextPending = append(nextPending, chunk...)
					continue
				}

				collected += res.Written
				if len(res.RetryIDs) > 0 {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ok=%d retry=%d", workerKey, shortEmail, len(chunk), res.Written, len(res.RetryIDs))
					nextPending = append(nextPending, res.RetryIDs...)
					if !res.ConcurrentLimitOnly {
						roundConcurrentOnly = false
					}
					adaptive.shrink()
				} else {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ok=%d", workerKey, shortEmail, len(chunk), res.Written)
					adaptive.reportCleanBatch()
				}

				eta.record(collected)
				remaining := total - collected
				etaStr := formatETA(eta.estimateSeconds(remaining))
				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("%d/%d", collected, total), etaStr)

				if !a.shutdown.IsSet() {
					time.Sleep(interBatchDelay)
				}
			}

			if fatalQuota {
				break
			}
			pending = nextPending
			if len(pending) == 0 || a.shutdown.IsSet() {
				break
			}

			var delay time.Duration
			if anyBatches && roundConcurrentOnly {
				concurrentRetryRound++
				if concurrentRetryRound > concurrentLimitBackoffMaxRnds {
					retryRound++
					delay = rateLimitBackoffBase * time.Duration(1<<(retryRound-1))
				} else {
					delay = concurrentLimitBackoffBase
				}
			} else {
				retryRound++
				if retryRound >= maxRetries {
					break
				}
				delay = rateLimitBackoffBase * time.Duration(1<<(retryRound-1))
			}

			a.dash.Log("WARN", fmt.Sprintf("[%s] %s: %d на retry, пауза %s", workerKey, shortEmail, len(pending), delay))
			time.Sleep(delay)
		}

		if fatalQuota {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Суточная квота исчерпана на %s, возвращаем в пул (%d/%d уже собрано)", workerKey, email, collected, total))
			a.dash.UpdateWorker(workerKey, "DAILY QUOTA", "DEAD", "")
			requeue <- email
			return
		}

		if a.shutdown.IsSet() {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Остановка: %s собрано %d/%d, докачает недостающее при следующем запуске", workerKey, shortEmail, collected, total))
			return
		}

		if len(pending) > 0 {
			errorLog = append(errorLog, fmt.Sprintf("%d msg_id не удалось собрать после исчерпания retry", len(pending)))
		}

		if len(errorLog) > 0 {
			for _, line := range errorLog {
				log.Printf("[WARN] [%s] %s: %s", workerKey, email, line)
			}
			a.dash.Log("ERROR", fmt.Sprintf("[%s] %s: завершён с ошибками (%d), юзер НЕ помечен как done", workerKey, shortEmail, len(errorLog)))
			a.bumpError()
			continue
		}

		names, err := gmailapi.FetchLabelNames(ctx, svc, email)
		if err != nil {
			a.dash.Log("ERROR", fmt.Sprintf("[%s] %s: labels.list не удался (%v), юзер НЕ помечен как done", workerKey, shortEmail, err))
			a.bumpError()
			continue
		}
		a.st.FinalizeUser(email, names)
		a.bumpDone()
	}
}

func asDailyQuota(err error) (*gmailapi.DailyQuotaExceededError, bool) {
	var dq *gmailapi.DailyQuotaExceededError
	if errors.As(err, &dq) {
		return dq, true
	}
	return nil, false
}

// ==========================================
// MAIN
// ==========================================
func main() {
	pterm.EnableStyling()
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()

	// === Main Menu ===
	for {
		action := showMainMenu()
		switch action {
		case "gmail-fetch-labels":
			runFetchLabels()
		case "gmail-fetch-bodies":
			pterm.Warning.Println("Fetch message bodies — в разработке.")
		case "gmail-verify":
			pterm.Warning.Println("Verify (Gmail vs S3) — в разработке.")
		case "drive":
			pterm.Warning.Println("Drive — в разработке.")
		case "exit":
			return
		}
		fmt.Println()
	}
}

func showMainMenu() string {
	// Главное меню.
	var mainChoice string
	prompt := &survey.Select{
		Message: "gwsferry — Google Workspace Ferry",
		Options: []string{"Gmail", "Drive", "Выход"},
	}
	survey.AskOne(prompt, &mainChoice)

	switch mainChoice {
	case "Drive":
		return "drive"
	case "Выход":
		return "exit"
	}

	// Подменю Gmail.
	var gmailChoice string
	gmailPrompt := &survey.Select{
		Message: "Gmail",
		Options: []string{
			"Fetch labelIds → JSON",
			"Fetch message bodies (raw) → S3",
			"Verify (Gmail vs S3 reconciliation)",
			"← Назад",
		},
	}
	survey.AskOne(gmailPrompt, &gmailChoice)

	switch gmailChoice {
	case "Fetch labelIds → JSON":
		return "gmail-fetch-labels"
	case "Fetch message bodies (raw) → S3":
		return "gmail-fetch-bodies"
	case "Verify (Gmail vs S3 reconciliation)":
		return "gmail-verify"
	default:
		return "back"
	}
}

func runFetchLabels() {

	logf, err := os.OpenFile(fmt.Sprintf("gmail_labels_fetch_%s.log", workspacePrefix), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(&syncFile{f: logf})
		defer logf.Close()
	}

	st := store.New(labelsFile)
	doneCount, partialCount, err := st.Load()
	if err != nil {
		log.Fatalf("[ERROR] Ошибка загрузки %s: %v", labelsFile, err)
	}
	pterm.Success.Printfln("Загружено %d готовых юзеров, %d частично собранных из %s", doneCount, partialCount, labelsFile)
	log.Printf("[INFO] [OK] Загружено %d готовых юзеров, %d частично собранных из %s", doneCount, partialCount, labelsFile)

	shutdown := newShutdownFlag()

	log.Println("[INFO] === СБОР GMAIL LABELIDS ===")

	if _, err := os.Stat(usersJSONPath); err != nil {
		log.Fatalf("[ERROR] Файл %s не найден.", usersJSONPath)
	}
	emails, err := loadEmails(usersJSONPath)
	if err != nil || len(emails) == 0 {
		log.Fatalln("[ERROR] Нет пользователей в users.json.")
	}

	allKeys, err := loadServiceAccountKeys(saKeysDir)
	if err != nil {
		log.Fatalf("[ERROR] %v", err)
	}

	ctx := context.Background()
	validKeys := verifyServiceAccounts(ctx, allKeys, emails[0])
	n := len(validKeys)
	if n == 0 {
		pterm.Error.Println("Нет рабочих сервисных аккаунтов.")
		log.Fatalln("[ERROR] Нет рабочих сервисных аккаунтов.")
	}
	if n > maxConcurrentWorkers {
		validKeys = validKeys[:maxConcurrentWorkers]
		n = maxConcurrentWorkers
	}

	// === Pre-fetch msg_id с live-таблицей ===
	tmpMsgIdx, err := os.CreateTemp("", "msg_ids_*.json")
	if err != nil {
		log.Fatalf("[ERROR] Создание temp-файла: %v", err)
	}
	tmpMsgIdxPath := tmpMsgIdx.Name()
	tmpMsgIdx.Close()
	os.Remove(tmpMsgIdxPath)

	// Pre-fetch с live-таблицей по воркерам (как при сборке).
	type fetchResult struct {
		email  string
		msgIDs []string
		err    error
	}

	fetchResults := make(chan fetchResult, len(emails))
	fetchTotal := len(emails)

	// Состояние воркеров для live-таблицы.
	type fetchWorkerState struct {
		task      string
		done      int
		errors    int
		collected int // сколько msg_id собрано для текущего юзера
		page      int // текущая страница
	}
	fetchWorkers := make([]fetchWorkerState, n)
	fetchDoneTotal := 0
	fetchMu := sync.Mutex{}

	// Канал задач для воркеров.
	fetchEmailCh := make(chan string, len(emails))
	for _, e := range emails {
		fetchEmailCh <- e
	}
	close(fetchEmailCh)

	// Запускаем pre-fetch воркеров.
	var fetchWg sync.WaitGroup
	for i := 0; i < n; i++ {
		fetchWg.Add(1)
		go func(idx int) {
			defer fetchWg.Done()

			for e := range fetchEmailCh {
				shortEmail := strings.Split(e, "@")[0]
				fetchMu.Lock()
				fetchWorkers[idx].task = shortEmail
				fetchWorkers[idx].collected = 0
				fetchWorkers[idx].page = 0
				fetchMu.Unlock()

				svc, err := gmailapi.BuildClient(ctx, validKeys[0], e)
				if err != nil {
					fetchMu.Lock()
					fetchWorkers[idx].errors++
					fetchDoneTotal++
					fetchMu.Unlock()
					fetchResults <- fetchResult{email: e, err: err}
					continue
				}
				ids, err := gmailapi.ListAllMessageIDs(ctx, svc, e, func(collected, page int) {
					fetchMu.Lock()
					fetchWorkers[idx].collected = collected
					fetchWorkers[idx].page = page
					fetchMu.Unlock()
				})
				fetchMu.Lock()
				fetchWorkers[idx].done++
				fetchDoneTotal++
				fetchMu.Unlock()
				fetchResults <- fetchResult{email: e, msgIDs: ids, err: err}
			}

			fetchMu.Lock()
			fetchWorkers[idx].task = "done"
			fetchMu.Unlock()
		}(i)
	}

	// Live-таблица pre-fetch — по воркерам, обновляется каждые 200мс.
	prefetchArea, _ := pterm.DefaultArea.WithRemoveWhenDone().Start()
	fetchDoneCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-fetchDoneCh:
				return
			case <-ticker.C:
				fetchMu.Lock()
				workers := make([]fetchWorkerState, n)
				copy(workers, fetchWorkers)
				doneTotal := fetchDoneTotal
				fetchMu.Unlock()

				pct := 0
				if fetchTotal > 0 {
					pct = doneTotal * 100 / fetchTotal
				}

				cols := []string{"WORKER", "ЗАДАЧА", "СТАТУС"}
				var rows [][]string
				for i, w := range workers {
					task := "—"
					if w.task != "" && w.task != "done" {
						task = w.task
					}
					status := "—"
					if w.page > 0 {
						status = fmt.Sprintf("%d msg_id, стр. %d", w.collected, w.page)
					}
					rows = append(rows, []string{
						fmt.Sprintf("sa%d", i),
						task,
						status,
					})
				}

				tbl, _ := pterm.DefaultTable.
					WithHasHeader().
					WithBoxed().
					WithHeaderRowSeparator("═").
					WithRowSeparator("─").
					WithSeparator("│").
					WithStyle(pterm.NewStyle(pterm.FgLightWhite)).
					WithHeaderStyle(pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold)).
					WithData(append([][]string{cols}, rows...)).
					Srender()

				header := pterm.DefaultHeader.
					WithBackgroundStyle(pterm.NewStyle(pterm.BgLightBlue)).
					WithTextStyle(pterm.NewStyle(pterm.FgBlack, pterm.Bold)).
					Sprintf("Pre-fetch msg_id [%d%%] %d/%d", pct, doneTotal, fetchTotal)

				prefetchArea.Update(header + "\n\n" + tbl)
			}
		}
	}()

	// Собираем результаты.
	for done := 0; done < fetchTotal; done++ {
		r := <-fetchResults
		fetchMu.Lock()
		if r.err != nil {
			log.Printf("[WARN] Pre-fetch %s: %v", r.email, r.err)
		} else {
			st.SetMsgIndex(r.email, r.msgIDs)
		}
		fetchMu.Unlock()
	}
	fetchWg.Wait()
	// Даём последнему тику обновить таблицу.
	time.Sleep(300 * time.Millisecond)
	close(fetchDoneCh)
	prefetchArea.Stop()
	// Гарантированная очистка.
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()

	if err := st.SaveMsgIndex(tmpMsgIdxPath); err != nil {
		log.Printf("[WARN] Сохранение msg_index: %v", err)
	}

	// Определяем очередь на основе collected vs expected.
	var pending []string
	for _, e := range emails {
		if !st.IsUserCollected(e) {
			pending = append(pending, e)
		}
	}
	skipped := len(emails) - len(pending)

	// === Сводка перед запуском ===
	fmt.Println()
	pterm.DefaultSection.Println("Сводка перед запуском")
	pterm.Info.Printfln("Всего юзеров:          %d", len(emails))
	pterm.Info.Printfln("Уже собрано (resume):  %d", skipped)
	pterm.Info.Printfln("Осталось в очереди:    %d", len(pending))
	pterm.Info.Printfln("Рабочих воркеров (SA): %d из %d", n, len(allKeys))
	pterm.Info.Printfln("Результат:             %s", labelsFile)
	log.Println("[INFO] -- Сводка перед запуском --")
	log.Printf("[INFO] Всего юзеров: %d, resume: %d, в очереди: %d, воркеров: %d", len(emails), skipped, len(pending), n)

	ans, _ := pterm.DefaultInteractiveTextInput.
		WithDefaultText("[?] Начать сбор labelIds? [Y/n]: ").
		Show()
	if strings.ToLower(strings.TrimSpace(ans)) != "" &&
		strings.ToLower(strings.TrimSpace(ans)) != "y" &&
		strings.ToLower(strings.TrimSpace(ans)) != "yes" &&
		strings.ToLower(strings.TrimSpace(ans)) != "д" &&
		strings.ToLower(strings.TrimSpace(ans)) != "да" {
		pterm.Warning.Println("Отмена пользователем.")
		log.Println("[INFO] Отмена пользователем.")
		return
	}

	// === Запуск дампера (только при активной сборке) ===
	var dumperWg sync.WaitGroup
	dumperWg.Add(1)
	go func() {
		defer dumperWg.Done()
		ticker := time.NewTicker(labelsDumpInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := st.Save(); err != nil {
					log.Printf("[WARN] [DUMP] ошибка дампа: %v", err)
				} else {
					log.Printf("[DEBUG] [DUMP] labels файл дамплен на диск")
				}
			case <-shutdown.Done():
				return
			}
		}
	}()

	// === Запуск TUI ===
	dash := dashboard.New()
	dash.Start()
	defer dash.Stop()
	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(emails)
		o.UsersDone = skipped
		o.UsersPending = len(pending)
	})

	// Обработка сигналов — непрерывный цикл.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGWINCH:
				dash.ForceRedraw()
			case syscall.SIGINT, syscall.SIGTERM:
				if shutdown.IsSet() {
					continue
				}
				shutdown.Set()
				log.Println("[WARN] [SHUTDOWN] Получен сигнал остановки - дописываем и сохраняем.")
				go func() {
					time.Sleep(5 * time.Second)
					if err := st.Save(); err != nil {
						log.Printf("[WARN] [DUMP] экстренный дамп: %v", err)
					}
					log.Println("[WARN] [SHUTDOWN] Экстренный дамп выполнен.")
					os.Exit(0)
				}()
			}
		}
	}()

	a := &app{st: st, dash: dash, shutdown: shutdown}

	emailCh := make(chan string, len(pending))
	for _, e := range pending {
		emailCh <- e
	}
	close(emailCh)
	requeue := make(chan string, len(pending))

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go a.worker(ctx, i, validKeys[i], emailCh, requeue, &wg)
	}

	wg.Wait()
	close(requeue)

	// === Post-flight ===
	var requeued []string
	for e := range requeue {
		requeued = append(requeued, e)
	}
	if len(requeued) > 0 {
		log.Printf("[WARN] [QUOTA] %d юзеров исчерпали суточную квоту, доберутся при следующем запуске: %v", len(requeued), requeued)
	}

	shutdown.Set()
	dumperWg.Wait()

	elapsed := time.Since(start)
	if err := st.Save(); err != nil {
		log.Printf("[WARN] [DUMP] финальный дамп не удался: %v", err)
	}

	fmt.Println()
	if len(requeued) > 0 {
		pterm.Warning.Printfln("=== ЗАВЕРШЕНО за %s, %d юзеров ждут следующего запуска (суточная квота) ===", elapsed, len(requeued))
		log.Printf("[INFO] === ЗАВЕРШЕНО за %s ===", elapsed)
	} else {
		pterm.Success.Printfln("=== СБОР LABELIDS ЗАВЕРШЁН за %s ===", elapsed)
		log.Printf("[INFO] === СБОР LABELIDS ЗАВЕРШЁН за %s ===", elapsed)
	}
}
