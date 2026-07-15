package main

import (
	"github.com/spf13/cobra"
	gmailcopy "gwsferry/internal/gmail/copy"
)

var copyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Скачивание .eml файлов из Gmail в S3",
	Long: `Подключается к Gmail API через сервисные аккаунты,
скачивает письма в формате .eml и загружает в S3 хранилище.
Поддерживает resume — прерванный импорт продолжается с места остановки.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		gmailcopy.Run()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(copyCmd)
}
