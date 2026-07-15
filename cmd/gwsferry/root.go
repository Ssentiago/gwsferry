package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"gwsferry/internal/shared/config"
)

var rootCmd = &cobra.Command{
	Use:   "gwsferry",
	Short: "Google Workspace Ferry — миграция почты",
	Long: `Инструмент для миграции данных Google Workspace.

Модули:
  fetch-labels  Сбор label IDs из Gmail в JSON
  copy          Скачивание .eml файлов из Gmail в S3
  import        Импорт писем из Yandex 360 в Gmail (через IMAP)
  init          Генерация конфигурационного файла gwsferry.toml

Конфигурация (приоритет: флаги > конфиг-файл > дефолты):
  --config      Путь к конфигу (по умолчанию gwsferry.toml рядом с бинарём)`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&config.ConfigPath, "config", "", "путь к конфиг-файлу (по умолчанию: gwsferry.toml рядом с бинарём)")
}
