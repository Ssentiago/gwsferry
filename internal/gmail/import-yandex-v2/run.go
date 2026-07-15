package importyandex

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	yandexapi "gwsferry/internal/gmail/import-yandex-v2/api"
	"gwsferry/internal/gmail/import-yandex-v2/users"
	"gwsferry/internal/shared/config"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/util"
)

// RunImport — точка входа.
// sourceEmail и targetEmail пустые → обычный режим (из файла, source=target для каждого юзера).
// sourceEmail и targetEmail заданы → test mode (source ≠ target).
func RunImport(cfg *config.Config, sourceEmail, targetEmail string) error {
	logPath := "yandex_import.log"
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[WARN] [RUN] не удалось открыть лог-файл %s: %v", logPath, err)
	} else {
		log.SetOutput(&util.SyncFile{F: logf})
		defer logf.Close()
	}

	fmt.Println()
	pterm.DefaultSection.Println("Yandex → IMAP импорт")

	if cfg.ImapHost != "" {
		SetImapHost(cfg.ImapHost)
	}
	log.Printf("[INFO] [RUN] IMAP target host: %s", imapHost)

	isTestMode := sourceEmail != "" && targetEmail != ""

	// 1. API клиент
	api := yandexapi.NewAPI(
		yandexapi.NewClient(cfg.Yandex.OAuthToken),
		cfg.Yandex.OrgID,
		cfg.Yandex.OAuthToken,
	)

	// 2. Юзеры
	var sourceUser yandexapi.User
	var targetUser yandexapi.User
	var allUsers []yandexapi.User

	if isTestMode {
		log.Printf("[INFO] [RUN] test mode: source=%s target=%s", sourceEmail, targetEmail)
		allUsers, err = api.ListUsers()
		if err != nil {
			return fmt.Errorf("загрузка юзеров из Yandex API: %w", err)
		}
		for _, u := range allUsers {
			if u.Email == sourceEmail {
				sourceUser = u
			}
			if u.Email == targetEmail {
				targetUser = u
			}
		}
		if sourceUser.Email == "" {
			return fmt.Errorf("source %s не найден в Yandex API", sourceEmail)
		}
		if targetUser.Email == "" {
			return fmt.Errorf("target %s не найден в Yandex API", targetEmail)
		}
		log.Printf("[INFO] [RUN] source: %s (uid=%d), target: %s (uid=%d)",
			sourceUser.Email, sourceUser.ID, targetUser.Email, targetUser.ID)
	} else {
		usersFile := "yandex_users.json"
		if execPath, err := os.Executable(); err == nil {
			usersFile = filepath.Join(filepath.Dir(execPath), "yandex_users.json")
		}
		log.Printf("[INFO] [RUN] проверяю файл юзеров: %s", usersFile)
		if _, err := os.Stat(usersFile); os.IsNotExist(err) {
			pterm.Error.Printfln("Файл %s не найден.", usersFile)
			return fmt.Errorf("файл %s не найден", usersFile)
		}

		log.Printf("[INFO] [RUN] загружаю юзеров из %s + Yandex API...", usersFile)
		allUsers, err = users.LoadUsers(api, usersFile)
		if err != nil {
			pterm.Error.Printfln("Ошибка загрузки юзеров: %v", err)
			return err
		}
	}
	log.Printf("[INFO] [RUN] загружено %d юзеров", len(allUsers))

	// 3. Лейблы
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
		return fmt.Errorf("файл %s не найден", labelsFile)
	}

	labels, err := ParseLabelsFile(labelsFile)
	if err != nil {
		pterm.Error.Printfln("Ошибка загрузки лейблов: %v", err)
		return err
	}
	log.Printf("[INFO] [RUN] лейблы загружены: %d юзеров", len(labels))

	// 4. S3
	s3Client, err := NewS3Client(cfg)
	if err != nil {
		pterm.Error.Printfln("Ошибка S3: %v", err)
		return err
	}

	// 5. State
	statePath := cfg.StateFile
	if statePath == "" {
		statePath = "import_state.json"
	}
	if execPath, err := os.Executable(); err == nil {
		statePath = filepath.Join(filepath.Dir(execPath), statePath)
	}
	st := loadImportState(statePath)

	// 6. Задачи
	type importTask struct {
		Source yandexapi.User
		Target yandexapi.User
	}

	var tasks []importTask
	if isTestMode {
		tasks = []importTask{{Source: sourceUser, Target: targetUser}}
	} else {
		for _, u := range allUsers {
			if st.isUserDone(u.Email) {
				log.Printf("[DEBUG] [RUN] resume: %s уже импортирован, пропуск", u.Email)
				continue
			}
			tasks = append(tasks, importTask{Source: u, Target: u})
		}
	}

	if len(tasks) == 0 {
		pterm.Success.Println("Все юзеры уже импортированы. Nothing to do.")
		return nil
	}

	// 7. Summary
	fmt.Println()
	pterm.DefaultSection.Println("Summary")
	if isTestMode {
		pterm.Info.Printfln("Mode:                 TEST")
		pterm.Info.Printfln("Source (S3):          %s", sourceUser.Email)
		pterm.Info.Printfln("Target (IMAP):        %s", targetUser.Email)
	} else {
		pterm.Info.Printfln("Mode:                 NORMAL")
		pterm.Info.Printfln("Total users:          %d", len(allUsers))
		pterm.Info.Printfln("Pending:              %d", len(tasks))
	}
	pterm.Info.Printfln("Msg workers:          %d", MsgWorkers)
	pterm.Info.Printfln("IMAP host:            %s", imapHost)
	pterm.Info.Printfln("Log file:             %s", logPath)
	pterm.Info.Printfln("State file:           %s", statePath)
	pterm.Info.Printfln("Labels file:          %s", labelsFile)

	// 8. Confirm
	result, err := pterm.DefaultInteractiveConfirm.Show("Continue?")
	if err != nil || !result {
		pterm.Warning.Println("Cancelled.")
		return nil
	}

	// 9. Dashboard
	dash := dashboard.New()
	dash.Start()
	dash.StartTimer()
	defer dash.Stop()

	if isTestMode {
		dash.SetModeInfo(fmt.Sprintf("TEST MODE  |  %s → %s", sourceUser.Email, targetUser.Email))
	}

	dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersTotal = len(tasks)
		o.UsersPending = len(tasks)
	})

	for _, t := range tasks {
		key := t.Source.Email
		if isTestMode {
			key = fmt.Sprintf("%s → %s", t.Source.Email, t.Target.Email)
		}
		dash.UpdateWorker(key, "в очереди", "idle", "")
	}

	// 10. Запуск
	for _, t := range tasks {
		key := t.Source.Email
		if isTestMode {
			key = fmt.Sprintf("%s → %s", t.Source.Email, t.Target.Email)
		}

		params := OrchestratorParams{
			SourceUser:   t.Source,
			TargetUser:   t.Target,
			Labels:       labels,
			S3:           s3Client,
			API:          api,
			ClientID:     cfg.Yandex.ClientID,
			ClientSecret: cfg.Yandex.ClientSecret,
		}

		log.Printf("[INFO] [RUN] запуск: source=%s target=%s", t.Source.Email, t.Target.Email)
		RunUserImport(context.Background(), params, st, statePath, dash, key)
	}

	pterm.Success.Println("Импорт завершён.")
	fmt.Print("\033[0m")
	os.Stdout.Sync()
	return nil
}
