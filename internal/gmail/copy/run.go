package copy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pterm/pterm"
	"gwsferry/internal/shared/config"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/util"
)

func Run(cfg *config.Config) {
	ws := cfg.Workspace
	logf, err := os.OpenFile(fmt.Sprintf("migration_gmail_multi_sa_%s.log", ws), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(&util.SyncFile{F: logf})
		defer logf.Close()
		log.Printf("[INFO] [RUN] лог-файл открыт: migration_gmail_multi_sa_%s.log", ws)
	}

	fmt.Println()
	pterm.DefaultSection.Println("Подготовка Gmail миграции")

	rss := getRSSMB()
	if rss > 0 {
		pterm.Info.Printfln("RSS: %.0fMB (лимит %dMB)", rss, memoryLimitMB)
		log.Printf("[DEBUG] [RUN] RSS=%.0fMB (лимит %dMB)", rss, memoryLimitMB)
	}

	usersJSONPath := "users.json"
	if execPath, err := os.Executable(); err == nil {
		usersJSONPath = filepath.Join(filepath.Dir(execPath), "users.json")
	}
	log.Printf("[INFO] [RUN] проверяю файл юзеров: %s", usersJSONPath)
	if _, err := os.Stat(usersJSONPath); err != nil {
		pterm.Error.Printfln("Файл %s не найден.", usersJSONPath)
		log.Printf("[ERROR] [RUN] файл %s не найден", usersJSONPath)
		return
	}

	type userWithUsage struct {
		Email string  `json:"Email Address [Required]"`
		Usage string  `json:"Email Usage [READ ONLY]"`
	}
	var doc struct {
		Users []userWithUsage `json:"users"`
	}
	raw, _ := os.ReadFile(usersJSONPath)
	json.Unmarshal(raw, &doc)

	var emails []string
	usageGB := map[string]float64{}
	for _, u := range doc.Users {
		if u.Email == "" {
			continue
		}
		emails = append(emails, u.Email)
		var gb float64
		fmt.Sscanf(u.Usage, "%fGB", &gb)
		usageGB[u.Email] = gb
	}

	saKeysDir := cfg.SaKeysDir
	if saKeysDir == "" {
		saKeysDir = "workers"
	}
	if execPath, err := os.Executable(); err == nil {
		saKeysDir = filepath.Join(filepath.Dir(execPath), saKeysDir)
	}
	log.Printf("[INFO] [RUN] загружаю SA ключи...")
	allKeys, err := loadServiceAccountKeys(saKeysDir)
	if err != nil {
		pterm.Error.Println(err)
		log.Printf("[ERROR] [RUN] загрузка SA ключей: %v", err)
		return
	}
	log.Printf("[INFO] [RUN] найдено %d SA ключей", len(allKeys))

	ctx := context.Background()
	log.Printf("[INFO] [RUN] проверяю SA...")
	validKeys := verifyServiceAccounts(ctx, allKeys, emails[0])
	n := len(validKeys)
	log.Printf("[INFO] [RUN] SA проверены: %d/%d валидных", n, len(allKeys))
	if n == 0 {
		pterm.Error.Println("Нет рабочих сервисных аккаунтов.")
		log.Printf("[ERROR] [RUN] нет рабочих SA")
		return
	}
	workers := cfg.Workers
	if n > workers {
		log.Printf("[WARN] [RUN] воркеров (%d) больше чем SA ключей (%d), ограничиваю до %d", n, workers, workers)
		pterm.Warning.Printfln("Воркеров (%d) больше чем SA ключей (%d). Использую %d.", n, workers, workers)
		validKeys = validKeys[:workers]
		n = workers
	}

	log.Printf("[INFO] [RUN] загружаю состояние...")
	statePath := cfg.StateFile
	if statePath == "" {
		statePath = fmt.Sprintf("migration_gmail_state_%s.json", cfg.Workspace)
	}
	if execPath, err := os.Executable(); err == nil {
		statePath = filepath.Join(filepath.Dir(execPath), statePath)
	}
	st := loadState(statePath)
	for _, e := range emails {
		if _, ok := st.Users[e]; !ok {
			st.Users[e] = "pending"
		}
	}
	saveState(st, statePath)

	// Считаем сколько уже завершено
	doneCount := 0
	for _, e := range emails {
		if st.Users[e] == "done" {
			doneCount++
		}
	}

	// Если есть завершённые — спрашиваем про перескан
	forceRescan := false
	if doneCount > 0 {
		rescanResult, err := pterm.DefaultInteractiveConfirm.
			WithDefaultValue(true).
			Show(fmt.Sprintf("Found %d completed users. Rescan all?", doneCount))
		if err != nil {
			pterm.Warning.Printfln("Input error: %v — aborting.", err)
			return
		}
		if rescanResult {
			forceRescan = true
			for _, e := range emails {
				st.Users[e] = "pending"
			}
			saveState(st, statePath)
		}
	}

	var pending []string
	for _, e := range emails {
		if st.Users[e] != "done" {
			pending = append(pending, e)
		}
	}
	skipped := len(emails) - len(pending)

	fmt.Println()
	pterm.DefaultSection.Println("Summary")
	pterm.Info.Printfln("Total users:          %d", len(emails))
	pterm.Info.Printfln("Already copied:       %d", skipped)
	pterm.Info.Printfln("Pending:              %d", len(pending))
	pterm.Info.Printfln("Workers (SA):         %d of %d", n, len(allKeys))
	pterm.Info.Printfln("Log file:             migration_gmail_multi_sa_%s.log", ws)
	pterm.Info.Printfln("State file:           %s", cfg.StateFile)
	if forceRescan {
		pterm.Warning.Println("Force rescan — all users will be reprocessed")
	}

	log.Printf("[INFO] [RUN] summary: total=%d copied=%d pending=%d workers=%d", len(emails), skipped, len(pending), n)

	log.Printf("[INFO] [RUN] ожидание подтверждения...")
	result, err := pterm.DefaultInteractiveConfirm.Show("Continue?")
	if err != nil {
		pterm.Warning.Printfln("Input error: %v — aborting.", err)
		log.Printf("[WARN] [RUN] ошибка ввода: %v, прерываю", err)
		return
	}
	if !result {
		pterm.Warning.Println("Cancelled by user.")
		log.Printf("[INFO] [RUN] отмена пользователем")
		return
	}
	log.Printf("[INFO] [RUN] подтверждено, запускаю...")

	shutdown := util.NewShutdownFlag()

	log.Printf("[INFO] [RUN] запускаю periodic state dumper (interval=%s)", stateDumpInterval)
	var dumperWg sync.WaitGroup
	dumperWg.Add(1)
	go func() {
		defer dumperWg.Done()
		ticker := time.NewTicker(stateDumpInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				saveState(st, statePath)
			case <-shutdown.Done():
				return
			}
		}
	}()

	dash := dashboard.New()
	dash.Start()
	go func() {
		<-dash.QuitCh()
		if !shutdown.IsSet() {
			shutdown.Set()
		}
		log.Println("[WARN] [SHUTDOWN] Ctrl+C: принудительный выход через 5с.")
		go func() {
			time.Sleep(5 * time.Second)
			saveState(st, statePath)
			os.Exit(0)
		}()
	}()
	dash.StartTimer()
	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(emails)
		o.UsersDone = skipped
		o.UsersPending = len(pending)
		o.MemoryLimit = memoryLimitMB
	})

	// RSS memory ticker
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rss := getRSSMB()
				dash.UpdateOverall(func(o *dashboard.OverallState) {
					o.MemoryMB = rss
				})
			case <-shutdown.Done():
				return
			}
		}
	}()

	log.Printf("[INFO] [RUN] signal handler установлен (SIGTERM/SIGWINCH)")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGWINCH:
				dash.ForwardWindowSize()
			case syscall.SIGTERM:
				if shutdown.IsSet() {
					continue
				}
				shutdown.Set()
				log.Println("[WARN] [SHUTDOWN] SIGTERM: принудительный выход через 5с.")
				go func() {
					time.Sleep(5 * time.Second)
					saveState(st, statePath)
					os.Exit(0)
				}()
			}
		}
	}()

	a := &app{
		st:       st,
		dash:     dash,
		shutdown: shutdown,
		s3client: buildS3Client(cfg),
		cfg:      cfg,
	}

	emailCh := make(chan string, len(pending))
	for _, e := range pending {
		emailCh <- e
	}
	close(emailCh)
	requeue := make(chan string, len(pending))

	log.Printf("[INFO] [RUN] запускаю %d воркеров для %d юзеров", n, len(pending))
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go a.worker(shutdown.Context(), i, validKeys[i], emailCh, requeue, &wg)
	}

	wg.Wait()
	close(requeue)

	var requeued []string
	for e := range requeue {
		requeued = append(requeued, e)
	}

	shutdown.Set()
	dumperWg.Wait()
	saveState(st, statePath)

	elapsed := time.Since(start)
	log.Printf("[INFO] [RUN] завершено за %s, requeued=%d", elapsed, len(requeued))
	dash.Stop()
	fmt.Println()
	if len(requeued) > 0 {
		pterm.Warning.Printfln("=== STOPPED in %s, %d users pending ===", elapsed, len(requeued))
	} else {
		pterm.Success.Printfln("=== GMAIL COPY COMPLETED in %s ===", elapsed)
	}
	pterm.Info.Printfln("State saved to:      %s", cfg.StateFile)
	pterm.Info.Printfln("Log file:            migration_gmail_multi_sa_%s.log", ws)

	fmt.Print("\033[0m")
	os.Stdout.Sync()
}

func loadServiceAccountKeys(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("папка %s не найдена", dir)
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
	util.SortStringsNatural(keys)
	return keys, nil
}

func verifyServiceAccounts(ctx context.Context, keys []string, testEmail string) []string {
	start := time.Now()
	fmt.Println()
	pterm.DefaultSection.Println("Pre-flight проверка сервисных аккаунтов...")
	log.Println("[INFO] [RUN] >>> Pre-flight проверка сервисных аккаунтов...")
	var valid []string
	for _, key := range keys {
		name := filepath.Base(key)
		svc, err := buildGmailClient(key, testEmail)
		if err == nil {
			_, err = execWithHardTimeout(ctx, func(cctx context.Context) (any, error) {
				return svc.Users.GetProfile("me").Context(cctx).Do()
			})
		}
		if err != nil {
			pterm.Error.Printfln("%s -> %v", name, err)
			log.Printf("[ERROR] [RUN] [FAIL] %s -> %v", name, err)
			continue
		}
		pterm.Success.Printfln("%s", name)
		log.Printf("[INFO] [RUN] [OK] %s", name)
		valid = append(valid, key)
	}
	log.Printf("[INFO] [RUN] SA проверены: %d/%d валидных (за %s)", len(valid), len(keys), time.Since(start))
	return valid
}
