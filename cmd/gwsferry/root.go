package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "gwsferry",
	Short: "Google Workspace Ferry — миграция почты",
	Long: `Инструмент для миграции данных Google Workspace.

Модули:
  fetch-labels  Сбор label IDs из Gmail в JSON
  copy          Скачивание .eml файлов из Gmail в S3
  import        Импорт писем из Yandex 360 в Gmail (через IMAP)
  init          Генерация конфигурационного файла gwsferry.toml`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
