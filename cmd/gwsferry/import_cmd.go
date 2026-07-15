package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"gwsferry/internal/gmail/import-yandex-v2"
	"gwsferry/internal/shared/config"
)

var (
	importYes  bool
	importTest bool
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Импорт писем из Yandex 360 в Gmail (через IMAP)",
	Long: `Подключается к Yandex IMAP (XOAUTH2) и импортирует письма
в Gmail. Поддерживает resume, retry с backoff, автосоздание папок.

Режимы:
  Обычный    gwsferry import — импорт из всех юзеров yandex_users.json
  Тестовый   gwsferry import --test-mode — выбор Target/Source, импорт происходит

Флаги переопределяют значения из конфиг-файла.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadAndResolve(config.CLIOverrides{
			LabelsFile: importLabelsFile,
			StateFile:  importStateFile,
			Workers:    importWorkers,
		})
		if err != nil {
			return err
		}

		if importTest {
			return runTestMode(cfg)
		}

		if !importYes {
			// TODO: interactive confirm
		}

		return importyandex.RunImport(cfg, "", "")
	},
}

var (
	importLabelsFile string
	importStateFile  string
	importWorkers    int
)

func init() {
	importCmd.Flags().StringVar(&importLabelsFile, "labels-file", "", "путь к migration_labels.json (по умолчанию: migration_labels.json)")
	importCmd.Flags().StringVar(&importStateFile, "state-file", "", "путь к файлу состояния (по умолчанию: import_state.json)")
	importCmd.Flags().IntVarP(&importWorkers, "workers", "w", 0, "количество user-воркеров (по умолчанию: 10)")
	importCmd.Flags().BoolVarP(&importYes, "yes", "y", false, "пропустить подтверждение перед импортом")
	importCmd.Flags().BoolVar(&importTest, "test-mode", false, "тестовый режим: интерактивный выбор Target/Source, затем реальный импорт")
	rootCmd.AddCommand(importCmd)
}

// runTestMode — тестовый режим: выбор Target (юзер) + Source (из файла)
func runTestMode(cfg *config.Config) error {
	fmt.Println()
	pterm.DefaultSection.Println("Yandex Import — Test Mode")

	// 1. Загружаем юзеров для Target
	usersFile := "yandex_users.json"
	if execPath, err := os.Executable(); err == nil {
		usersFile = filepath.Join(filepath.Dir(execPath), "yandex_users.json")
	}
	users, err := loadUsersForSelect(usersFile)
	if err != nil {
		return fmt.Errorf("загрузка юзеров: %w", err)
	}
	if len(users) == 0 {
		return fmt.Errorf("нет юзеров в %s", usersFile)
	}

	// 2. Выбор Target (юзер)
	pterm.Info.Printfln("Выберите Target (получатель):")
	target, err := pterm.DefaultInteractiveSelect.
		WithDefaultText("Target email").
		WithOptions(users).
		WithFilter(true).
		Show()
	if err != nil {
		return fmt.Errorf("выбор target: %w", err)
	}
	log.Printf("[INFO] [TEST] target selected: %s", target)

	// 3. Юзеры из файла для Source
	fmt.Println()
	pterm.Info.Printfln("Загрузка данных из файла юзеров...")
	var sources []string
	for _, u := range users {
		if u != target {
			sources = append(sources, u)
		}
	}
	log.Printf("[INFO] [TEST] файл: %d кандидатов для Source", len(sources))

	if len(sources) == 0 {
		pterm.Error.Println("Нет других юзеров в файле.")
		return fmt.Errorf("нет кандидатов для Source")
	}

	// 4. Выбор Source
	pterm.Info.Printfln("Выберите Source (откуда импортировать):")
	sourceEmail, err := pterm.DefaultInteractiveSelect.
		WithDefaultText("Source email").
		WithOptions(sources).
		WithFilter(true).
		Show()
	if err != nil {
		return fmt.Errorf("выбор source: %w", err)
	}
	log.Printf("[INFO] [TEST] source selected: %s", sourceEmail)

	// 5. Итог
	fmt.Println()
	pterm.DefaultSection.Println("Test Mode Summary")
	pterm.Info.Printfln("Target:   %s (получатель)", target)
	pterm.Info.Printfln("Source:   %s (откуда письма)", sourceEmail)
	pterm.Info.Printfln("Режим:   тестовый — выбор Target/Source, реальный импорт")

	// Подтверждение
	result, err := pterm.DefaultInteractiveConfirm.Show("Запустить импорт?")
	if err != nil {
		return fmt.Errorf("подтверждение: %w", err)
	}
	if !result {
		pterm.Warning.Println("Отмена.")
		return nil
	}

	// Запускаем реальный импорт для выбранного Source
	log.Printf("[INFO] [TEST] запуск импорта: source=%s target=%s", sourceEmail, target)
	pterm.Info.Printfln("Запуск импорта: %s → %s", sourceEmail, target)
	if err := importyandex.RunImport(cfg, sourceEmail, target); err != nil {
		return fmt.Errorf("импорт: %w", err)
	}

	pterm.Success.Println("Импорт завершён.")
	return nil
}

// loadUsersForSelect загружает email-ы из JSON файла.
func loadUsersForSelect(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	return file.Users, nil
}
