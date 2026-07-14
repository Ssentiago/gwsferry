package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gmailcopy "gwsferry/internal/gmail/copy"
	gmailfetch "gwsferry/internal/gmail/fetch-labels"
	"gwsferry/internal/gmail/import-yandex"
	"gwsferry/internal/shared/config"

	"github.com/pterm/pterm"
)

var testMode bool

func main() {
	flag.BoolVar(&testMode, "test-mode", false, "включить тестовый режим (Yandex Import Test Mode)")
	flag.Parse()

	pterm.EnableStyling()
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()

	// Загружаем конфиг
	cfg, err := config.Load()
	if err != nil {
		pterm.Error.Printfln("Ошибка загрузки конфига: %v", err)
		pterm.Info.Println("Создайте gwsferry.toml рядом с бинарём (см. gwsferry --init)")
		return
	}

	// Проверяем S3 конфиг
	if cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
		pterm.Warning.Println("S3 Access Key / Secret Key не заданы в конфиге.")
		pterm.Info.Println("Заполните [s3] секцию в gwsferry.toml")
	}

	for {
		action := showMainMenu()
		switch action {
		case "gmail-fetch-labels":
			gmailfetch.Run()
		case "gmail-copy":
			gmailcopy.Run()
		case "yandex-import":
			if err := importyandex.RunImport(cfg); err != nil {
				pterm.Error.Printfln("Ошибка: %v", err)
			}
		case "yandex-import-test":
			if err := runTestMode(cfg); err != nil {
				pterm.Error.Printfln("Ошибка: %v", err)
			}
		case "drive":
			pterm.Warning.Println("Drive — в разработке.")
		case "init":
			initConfig()
		case "exit":
			return
		}
		fmt.Println()
	}
}

func showMainMenu() string {
	mainChoice, _ := pterm.DefaultInteractiveSelect.
		WithDefaultText("gwsferry — Google Workspace Ferry").
		WithOptions([]string{"Gmail", "Drive", "Setup Config", "Exit"}).
		Show()

	switch mainChoice {
	case "Drive":
		return "drive"
	case "Setup Config":
		return "init"
	case "Exit":
		return "exit"
	}

	gmailOptions := []string{
		"Export label IDs to JSON",
		"Download emails (raw .eml) to S3",
		"Yandex (Import)",
	}
	if testMode {
		gmailOptions = append(gmailOptions, "Yandex (Import) Test Mode")
	}
	gmailOptions = append(gmailOptions, "< Back")

	gmailChoice, _ := pterm.DefaultInteractiveSelect.
		WithDefaultText("Gmail").
		WithOptions(gmailOptions).
		Show()

	switch gmailChoice {
	case "Export label IDs to JSON":
		return "gmail-fetch-labels"
	case "Download emails (raw .eml) to S3":
		return "gmail-copy"
	case "Yandex (Import)":
		return "yandex-import"
	case "Yandex (Import) Test Mode":
		return "yandex-import-test"
	default:
		return "back"
	}
}

// runTestMode — тестовый режим: выбор Target (юзер) + Source (S3 письма)
func runTestMode(cfg *config.Config) error {
	fmt.Println()
	pterm.DefaultSection.Println("Yandex Import — Test Mode")

	// Определяем директорию рядом с бинарём
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

	// 3. Загружаем юзеров из файла для Source
	fmt.Println()
	pterm.Info.Printfln("Загрузка данных из файла юзеров...")
	allEmails := make(map[string]int)
	for _, u := range users {
		if u == target {
			continue
		}
		// Пока просто считаем юзеров как кандидатов
		// Количество писем покажем позже при реальном импорте
		allEmails[u] = 0
	}
	log.Printf("[INFO] [TEST] файл: %d кандидатов для Source", len(allEmails))

	if len(allEmails) == 0 {
		pterm.Error.Println("Нет других юзеров в файле.")
		return fmt.Errorf("нет кандидатов для Source")
	}

	// Сортируем по email
	type sourceInfo struct {
		email string
		count int
	}
	var sources []sourceInfo
	for email := range allEmails {
		sources = append(sources, sourceInfo{email: email, count: 0})
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].email < sources[j].email
	})

	// Показываем таблицу Sources
	fmt.Println()
	pterm.DefaultSection.Println("Sources (откуда качать)")
	tableData := [][]string{{"EMAIL", "ПИСЕМ"}}
	for _, s := range sources {
		tableData = append(tableData, []string{
			s.email,
			fmt.Sprintf("%d", s.count),
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

	// 4. Выбор Source (юзер для импорта)
	var sourceOptions []string
	for _, s := range sources {
		sourceOptions = append(sourceOptions, fmt.Sprintf("%s (%d писем)", s.email, s.count))
	}

	pterm.Info.Printfln("Выберите Source (откуда импортировать):")
	sourceChoice, err := pterm.DefaultInteractiveSelect.
		WithDefaultText("Source email").
		WithOptions(sourceOptions).
		WithFilter(true).
		Show()
	if err != nil {
		return fmt.Errorf("выбор source: %w", err)
	}

	// Извлекаем email из "email (N писем)"
	sourceEmail := strings.SplitN(sourceChoice, " ", 2)[0]
	log.Printf("[INFO] [TEST] source selected: %s", sourceEmail)

	// 5. Итог
	fmt.Println()
	pterm.DefaultSection.Println("Test Mode Summary")
	pterm.Info.Printfln("Target:   %s (получатель)", target)
	pterm.Info.Printfln("Source:   %s (откуда письма)", sourceEmail)
	pterm.Info.Printfln("Режим:   тестовый — только.preview, без реального импорта")

	fmt.Println()
	pterm.Success.Println("Test Mode: всё готово к запуску. Реальный импорт — через основной режим.")
	log.Printf("[INFO] [TEST] test mode completed: target=%s source=%s", target, sourceEmail)

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

func initConfig() {
	execPath, _ := os.Executable()
	configPath := execPath[:len(execPath)-len(filepath.Base(execPath))] + "gwsferry.toml"

	if _, err := os.Stat(configPath); err == nil {
		pterm.Warning.Printfln("Конфиг уже существует: %s", configPath)
		pterm.Info.Println("Отредактируйте его вручную.")
		return
	}

	if err := os.WriteFile(configPath, []byte(config.GenerateDefaultConfig()), 0644); err != nil {
		pterm.Error.Printfln("Ошибка создания конфига: %v", err)
		return
	}

	pterm.Success.Printfln("Конфиг создан: %s", configPath)
	pterm.Info.Println("Заполните [s3] секцию с Access Key и Secret Key.")
}
