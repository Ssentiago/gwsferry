package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	gmailcopy "gwsferry/internal/gmail/copy"
	gmailfetch "gwsferry/internal/gmail/fetch-labels"
	"gwsferry/internal/gmail/import-yandex"
	"gwsferry/internal/shared/config"
)

func main() {
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

	gmailChoice, _ := pterm.DefaultInteractiveSelect.
		WithDefaultText("Gmail").
		WithOptions([]string{
			"Export label IDs to JSON",
			"Download emails (raw .eml) to S3",
			"Yandex (Import)",
			"< Back",
		}).
		Show()

	switch gmailChoice {
	case "Export label IDs to JSON":
		return "gmail-fetch-labels"
	case "Download emails (raw .eml) to S3":
		return "gmail-copy"
	case "Yandex (Import)":
		return "yandex-import"
	default:
		return "back"
	}
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
