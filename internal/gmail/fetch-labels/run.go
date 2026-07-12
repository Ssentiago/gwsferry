package fetchlabels

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
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
	}

	st := store.New(labelsFile)
	doneCount, partialCount, err := st.Load()
	if err != nil {
		log.Fatalf("[ERROR] Ошибка загрузки %s: %v", labelsFile, err)
	}
	pterm.Success.Printfln("Загружено %d готовых юзеров, %d частично собранных из %s", doneCount, partialCount, labelsFile)
	log.Printf("[INFO] [OK] Загружено %d готовых юзеров, %d частично собранных из %s", doneCount, partialCount, labelsFile)

	shutdown := util.NewShutdownFlag()

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

	preFetchMsgIDs(ctx, emails, validKeys, st, n)

	var pending []string
	for _, e := range emails {
		if !st.IsUserCollected(e) {
			pending = append(pending, e)
		}
	}
	skipped := len(emails) - len(pending)

	fmt.Println()
	pterm.DefaultSection.Println("Сводка перед запуском")
	pterm.Info.Printfln("Всего юзеров:          %d", len(emails))
	pterm.Info.Printfln("Уже собрано (resume):  %d", skipped)
	pterm.Info.Printfln("Осталось в очереди:    %d", len(pending))
	pterm.Info.Printfln("Рабочих воркеров (SA): %d из %d", n, len(allKeys))
	pterm.Info.Printfln("Результат:             %s", labelsFile)
	log.Println("[INFO] -- Сводка перед запуском --")
	log.Printf("[INFO] Всего юзеров: %d, resume: %d, в очереди: %d, воркеров: %d", len(emails), skipped, len(pending), n)

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
	defer dash.Stop()
	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(emails)
		o.UsersDone = skipped
		o.UsersPending = len(pending)
	})

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
