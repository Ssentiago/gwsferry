package fetchlabels

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pterm/pterm"
	"gwsferry/internal/gmail/fetch-labels/store"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/util"
)

func Run() {
	logf, err := os.OpenFile(fmt.Sprintf("gmail_labels_fetch_%s.log", workspacePrefix), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(&util.SyncFile{F: logf})
		defer logf.Close()
		log.Printf("[INFO] [RUN] лог-файл открыт: gmail_labels_fetch_%s.log", workspacePrefix)
	}

	log.Printf("[INFO] [RUN] загружаю store из %s...", labelsFile)
	st := store.New(labelsFile)
	loadedCount, err := st.Load()
	if err != nil {
		log.Fatalf("[ERROR] [RUN] Ошибка загрузки %s: %v", labelsFile, err)
	}
	pterm.Success.Printfln("[OK] Loaded %d users from %s", loadedCount, labelsFile)

	shutdown := util.NewShutdownFlag()

	log.Printf("[INFO] [RUN] проверяю файл юзеров: %s", usersJSONPath)
	if _, err := os.Stat(usersJSONPath); err != nil {
		log.Fatalf("[ERROR] [RUN] Файл %s не найден.", usersJSONPath)
	}

	log.Printf("[INFO] [RUN] загружаю юзеров из %s...", usersJSONPath)
	emails, err := loadEmails(usersJSONPath)
	if err != nil || len(emails) == 0 {
		pterm.Error.Printfln("Failed to load users.json")
		log.Fatalln("[ERROR] [RUN] Нет пользователей в users.json.")
	}
	pterm.Success.Printfln("Loaded %d users from %s", len(emails), usersJSONPath)
	log.Printf("[INFO] [RUN] загружено %d юзеров", len(emails))

	log.Printf("[INFO] [RUN] загружаю SA ключи из %s...", saKeysDir)
	allKeys, err := loadServiceAccountKeys(saKeysDir)
	if err != nil {
		log.Fatalf("[ERROR] [RUN] %v", err)
	}
	log.Printf("[INFO] [RUN] найдено %d SA ключей", len(allKeys))

	ctx := context.Background()
	log.Printf("[INFO] [RUN] проверяю SA на работоспособность...")
	validKeys := verifyServiceAccounts(ctx, allKeys, emails[0])
	n := len(validKeys)
	log.Printf("[INFO] [RUN] SA проверены: %d/%d валидных", n, len(allKeys))
	if n == 0 {
		pterm.Error.Println("Нет рабочих сервисных аккаунтов.")
		log.Fatalln("[ERROR] [RUN] Нет рабочих сервисных аккаунтов.")
	}
	if n > maxConcurrentWorkers {
		validKeys = validKeys[:maxConcurrentWorkers]
		n = maxConcurrentWorkers
	}

	log.Printf("[INFO] [RUN] pre-fetch msg_ids для %d юзеров, %d воркеров...", len(emails), n)
	preFetchStart := time.Now()
	preFetchMsgIDs(ctx, emails, validKeys, st, n)
	log.Printf("[INFO] [RUN] pre-fetch завершён за %s", time.Since(preFetchStart))

	var pending []string
	collectedCount := 0
	for _, e := range emails {
		if st.IsUserCollected(e) {
			collectedCount++
		} else {
			pending = append(pending, e)
		}
	}
	log.Printf("[INFO] [RUN] collected=%d pending=%d", collectedCount, len(pending))

	fmt.Println()
	pterm.DefaultSection.Println("Summary")

	tableData := [][]string{{"USER", "GOOGLE", "LOCAL", "STATUS"}}
	for _, e := range emails {
		google, local := st.UserStats(e)
		status := pterm.Yellow("pending")
		if st.IsUserCollected(e) {
			status = pterm.Green("collected")
		} else if google == 0 {
			status = pterm.Red("no data")
		}
		tableData = append(tableData, []string{
			e,
			fmt.Sprintf("%d", google),
			fmt.Sprintf("%d", local),
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
		WithData(tableData).
		Srender()
	fmt.Println(tbl)

	pterm.Info.Printfln("Workers (SA):  %d of %d", n, len(allKeys))
	pterm.Info.Printfln("Collected:     %d", collectedCount)
	pterm.Info.Printfln("Pending:       %d", len(pending))
	pterm.Info.Printfln("Log file:      gmail_labels_fetch_%s.log", workspacePrefix)
	pterm.Info.Printfln("Result file:   %s", labelsFile)

	log.Printf("[INFO] [RUN] summary: total=%d collected=%d pending=%d workers=%d", len(emails), collectedCount, len(pending), n)

	if len(pending) == 0 {
		pterm.Success.Println("All users collected. Nothing to do.")
		log.Printf("[INFO] [RUN] все юзеры уже собраны, ничего не делать")
		return
	}

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
	fmt.Println()

	log.Printf("[INFO] [RUN] запускаю periodic dumper (interval=%s)", labelsDumpInterval)
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

	dash := dashboard.New()
	dash.Start()
	dash.StartTimer()
	defer dash.Stop()
	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(emails)
		o.UsersDone = collectedCount
		o.UsersPending = len(pending)
	})

	log.Printf("[INFO] [RUN] signal handler установлен (SIGINT/SIGTERM/SIGWINCH)")
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

	var consumed atomic.Int32
	tasksDone := make(chan struct{})
	go func() {
		for int(consumed.Load()) < len(pending) {
			time.Sleep(100 * time.Millisecond)
		}
		close(tasksDone)
	}()

	requeue := make(chan string, len(pending))

	log.Printf("[INFO] [RUN] запускаю %d воркеров для %d юзеров", n, len(pending))
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go a.worker(ctx, i, validKeys[i], emailCh, tasksDone, &consumed, requeue, &wg)
	}

	wg.Wait()
	close(requeue)

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
		log.Printf("[WARN] [DUMP] final dump failed: %v", err)
	}

	log.Printf("[INFO] [RUN] завершено за %s, requeued=%d", elapsed, len(requeued))

	fmt.Println()
	if len(requeued) > 0 {
		pterm.Warning.Printfln("=== STOPPED in %s, %d users pending (daily quota) ===", elapsed, len(requeued))
	} else {
		pterm.Success.Printfln("=== LABEL IDS EXPORT COMPLETED in %s ===", elapsed)
	}
	pterm.Info.Printfln("Result saved to:     %s", labelsFile)
	pterm.Info.Printfln("Log file:            gmail_labels_fetch_%s.log", workspacePrefix)

	fmt.Print("\033[0m")
	os.Stdout.Sync()
}
