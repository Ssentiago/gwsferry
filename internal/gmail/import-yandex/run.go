package importyandex

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	yandexapi "gwsferry/internal/gmail/import-yandex/api"
	"gwsferry/internal/gmail/import-yandex/users"
	"gwsferry/internal/shared/config"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/util"
)

// RunImport — точка входа: загрузка, summary, confirm, dashboard, запуск.
// Если emails переданы — импортирует только их (test mode).
// Если emails пустой — читает из yandex_users.json.
// modeInfo — строка для отображения в дашборде (например "TEST MODE | target → source").
func RunImport(cfg *config.Config, emails ...string) error {
	return RunImportWithMode(cfg, "", emails...)
}

func RunImportWithMode(cfg *config.Config, modeInfo string, emails ...string) error {
	// Лог-файл
	logPath := "yandex_import.log"
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[WARN] [RUN] не удалось открыть лог-файл %s: %v, логи идут только в stdout", logPath, err)
	} else {
		log.SetOutput(&util.SyncFile{F: logf})
		defer logf.Close()
		log.Printf("[INFO] [RUN] лог-файл открыт: %s", logPath)
	}

	fmt.Println()
	pterm.DefaultSection.Println("Yandex → IMAP импорт")

	// Устанавливаем IMAP хост из конфига (TARGET: Yandex)
	if cfg.ImapHost != "" {
		SetImapHost(cfg.ImapHost)
	}
	log.Printf("[INFO] [RUN] IMAP target host: %s", imapHost)

	// 1. Юзеры
	log.Printf("[INFO] [RUN] создаю Yandex API клиент (orgID=%s, token len=%d)", cfg.Yandex.OrgID, len(cfg.Yandex.OAuthToken))
	api := yandexapi.NewAPI(
		yandexapi.NewClient(cfg.Yandex.OAuthToken),
		cfg.Yandex.OrgID,
		cfg.Yandex.OAuthToken,
	)

	var userList []yandexapi.User
	if len(emails) > 0 {
		// Test mode: импортируем только переданные email'ы
		// Находим полные объекты User (с ID) через Yandex API
		log.Printf("[INFO] [RUN] test mode: импорт %d юзеров из параметров", len(emails))
		allUsers, err := api.ListUsers()
		if err != nil {
			return fmt.Errorf("загрузка юзеров из Yandex API: %w", err)
		}
		emailSet := make(map[string]bool, len(emails))
		for _, e := range emails {
			emailSet[e] = true
		}
		for _, u := range allUsers {
			if emailSet[u.Email] {
				userList = append(userList, u)
			}
		}
		if len(userList) == 0 {
			return fmt.Errorf("ни один из переданных email не найден в Yandex API")
		}
	} else {
		// Обычный режим: читаем из файла рядом с бинарём
		usersFile := "yandex_users.json"
		if execPath, err := os.Executable(); err == nil {
			usersFile = filepath.Join(filepath.Dir(execPath), "yandex_users.json")
		}
		log.Printf("[INFO] [RUN] проверяю файл юзеров: %s", usersFile)
		if _, err := os.Stat(usersFile); os.IsNotExist(err) {
			pterm.Error.Printfln("Файл %s не найден.", usersFile)
			log.Printf("[ERROR] [RUN] файл юзеров не найден: %s", usersFile)
			return fmt.Errorf("файл %s не найден", usersFile)
		}
		log.Printf("[INFO] [RUN] файл юзеров найден, размер OK")

		log.Printf("[INFO] [RUN] загружаю юзеров из %s + Yandex API...", usersFile)
		userList, err = users.LoadUsers(api, usersFile)
		if err != nil {
			pterm.Error.Printfln("Ошибка загрузки юзеров: %v", err)
			log.Printf("[ERROR] [RUN] ошибка загрузки юзеров: %v", err)
			return err
		}
	}
	log.Printf("[INFO] [RUN] загружено %d юзеров", len(userList))
	for i, u := range userList {
		log.Printf("[DEBUG] [RUN] юзер[%d]: email=%s uid=%d", i, u.Email, u.ID)
	}

	// 2. Лейблы
	labelsFile := cfg.LabelsFile
	if labelsFile == "" {
		labelsFile = fmt.Sprintf("migration_labels_%s.json", cfg.Workspace)
	}
	if execPath, err := os.Executable(); err == nil {
		labelsFile = filepath.Join(filepath.Dir(execPath), labelsFile)
	}
	log.Printf("[INFO] [RUN] проверяю файл лейблов: %s", labelsFile)
	if _, err := os.Stat(labelsFile); os.IsNotExist(err) {
		pterm.Error.Printfln("Файл %s не найден.", labelsFile)
		log.Printf("[ERROR] [RUN] файл лейблов не найден: %s", labelsFile)
		return fmt.Errorf("файл %s не найден", labelsFile)
	}
	log.Printf("[INFO] [RUN] файл лейблов найден")

	labels, err := ParseLabelsFile(labelsFile)
	if err != nil {
		pterm.Error.Printfln("Ошибка загрузки лейблов: %v", err)
		log.Printf("[ERROR] [RUN] ошибка парсинга лейблов: %v", err)
		return err
	}
	log.Printf("[INFO] [RUN] лейблы загружены: %d юзеров в файле", len(labels))
	for email, ul := range labels {
		log.Printf("[DEBUG] [RUN] лейблы: email=%s messages=%d labelNames=%d done=%v",
			email, len(ul.Messages), len(ul.LabelNames), ul.Done)
	}

	// 3. S3
	log.Printf("[INFO] [RUN] создаю S3 клиент...")
	s3Client, err := NewS3Client(cfg)
	if err != nil {
		pterm.Error.Printfln("Ошибка S3: %v", err)
		log.Printf("[ERROR] [RUN] ошибка создания S3 клиента: %v", err)
		return err
	}
	log.Printf("[INFO] [RUN] S3 клиент создан OK")

	// 4. Resume — проверяем сколько уже обработано
	statePath := cfg.StateFile
	if statePath == "" {
		statePath = "import_state.json"
	}
	if execPath, err := os.Executable(); err == nil {
		statePath = filepath.Join(filepath.Dir(execPath), statePath)
	}
	log.Printf("[INFO] [RUN] загружаю состояние из %s...", statePath)
	st := loadImportState(statePath)
	doneCount := 0
	for _, u := range userList {
		if st.isUserDone(u.Email) {
			doneCount++
			log.Printf("[DEBUG] [RUN] resume: %s уже импортирован", u.Email)
		}
	}
	log.Printf("[INFO] [RUN] resume: %d/%d юзеров уже обработаны", doneCount, len(userList))

	// ==============================
	// SUMMARY
	// ==============================
	fmt.Println()
	pterm.DefaultSection.Println("Summary")
	pterm.Info.Printfln("Total users:          %d", len(userList))
	pterm.Info.Printfln("Already imported:     %d", doneCount)
	pterm.Info.Printfln("Pending:              %d", len(userList)-doneCount)
	pterm.Info.Printfln("User workers:         %d", cfg.Yandex.UserWorkers)
	pterm.Info.Printfln("Msg workers/user:     %d", MsgWorkers)
	pterm.Info.Printfln("IMAP target:          %s", imapHost)
	pterm.Info.Printfln("Log file:             %s", logPath)
	pterm.Info.Printfln("State file:           %s", statePath)
	pterm.Info.Printfln("Labels file:          %s", labelsFile)

	log.Printf("[INFO] [RUN] summary: total=%d done=%d pending=%d userWorkers=%d msgWorkers=%d",
		len(userList), doneCount, len(userList)-doneCount, cfg.Yandex.UserWorkers, MsgWorkers)

	// ==============================
	// CONFIRM
	// ==============================
	log.Printf("[INFO] [RUN] ожидание подтверждения пользователя...")
	result, err := pterm.DefaultInteractiveConfirm.Show("Continue?")
	if err != nil {
		pterm.Warning.Printfln("Input error: %v — aborting.", err)
		log.Printf("[WARN] [RUN] ошибка ввода подтверждения: %v, прерываю", err)
		return nil
	}
	if !result {
		pterm.Warning.Println("Cancelled by user.")
		log.Printf("[INFO] [RUN] импорт отменён пользователем")
		return nil
	}
	log.Printf("[INFO] [RUN] подтверждено, запускаю импорт...")

	// ==============================
	// DASHBOARD
	// ==============================
	log.Printf("[INFO] [RUN] запускаю dashboard...")
	dash := dashboard.New()
	dash.Start()
	dash.StartTimer()
	if modeInfo != "" {
		dash.SetModeInfo(modeInfo)
	}
	defer dash.Stop()

	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(userList)
		o.UsersDone = doneCount
		o.UsersPending = len(userList) - doneCount
	})

	// Регистрируем всех юзеров в дашборде сразу — таблица видна с старта
	for _, u := range userList {
		if st.isUserDone(u.Email) {
			dash.UpdateWorker(u.Email, "импортировано", "done", "")
		} else {
			dash.UpdateWorker(u.Email, "в очереди", "idle", "")
		}
	}
	log.Printf("[INFO] [RUN] dashboard запущен, %d юзеров зарегистрировано", len(userList))

	// ==============================
	// ЗАПУСК
	// ==============================
	params := OrchestratorParams{
		Users:        userList,
		Labels:       labels,
		S3:           s3Client,
		API:          api,
		ClientID:     cfg.Yandex.ClientID,
		ClientSecret: cfg.Yandex.ClientSecret,
	}

	log.Printf("[INFO] [RUN] запускаю оркестратор (users=%d, clientID=%s)", len(userList), cfg.Yandex.ClientID)
	Run(context.Background(), params, cfg, dash)

	pterm.Success.Println("Импорт завершён.")
	pterm.Info.Printfln("State: %s", statePath)
	pterm.Info.Printfln("Log:   %s", logPath)
	log.Printf("[INFO] [RUN] импорт завершён успешно")

	fmt.Print("\033[0m")
	os.Stdout.Sync()
	return nil
}
