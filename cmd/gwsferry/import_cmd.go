package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"gwsferry/internal/gmail/import-yandex"
	"gwsferry/internal/shared/config"
)

var (
	importWorkers int
	importYes     bool
	importTest    bool
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Импорт писем из Yandex 360 в Gmail (через IMAP)",
	Long: `Подключается к Yandex IMAP (XOAUTH2) и импортирует письма
в Gmail. Поддерживает resume, retry с backoff, автосоздание папок.

Режимы:
  Обычный    gwsferry import — импорт из всех юзеров yandex_users.json
  Тестовый   gwsferry import --test-mode — выбор Target/Source, импорт происходит`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		if importTest {
			return runTestMode(cfg)
		}

		if !importYes {
			// TODO: interactive confirm
		}

		return importyandex.RunImport(cfg)
	},
}

func init() {
	importCmd.Flags().IntVarP(&importWorkers, "workers", "w", 0, "количество msg-воркеров на юзера (по умолчанию из конфига)")
	importCmd.Flags().BoolVarP(&importYes, "yes", "y", false, "пропустить подтверждение")
	importCmd.Flags().BoolVar(&importTest, "test-mode", false, "тестовый режим: выбор Target/Source без реального импорта")
	rootCmd.AddCommand(importCmd)
}

// runTestMode — тестовый режим: выбор Target (юзер) + Source (из файла)
func runTestMode(cfg *config.Config) error {
	fmt.Println()
	pterm.DefaultSection.Println("Yandex Import — Test Mode")

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("определение пути к бинарю: %w", err)
	}
	dir := filepath.Dir(execPath)

	// 1. Загружаем юзеров из yandex_users.json для Target
	usersFile := filepath.Join(dir, "yandex_users.json")
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
	log.Printf("[INFO] [TEST] запуск импорта: source=%s", sourceEmail)
	pterm.Info.Printfln("Запуск импорта для %s...", sourceEmail)
	if err := importyandex.RunImport(cfg, sourceEmail); err != nil {
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
