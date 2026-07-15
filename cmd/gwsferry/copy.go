package main

import (
	"github.com/spf13/cobra"
	"gwsferry/internal/shared/config"

	gmailcopy "gwsferry/internal/gmail/copy"
)

var copyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Скачивание .eml файлов из Gmail в S3",
	Long: `Подключается к Gmail API через сервисные аккаунты,
скачивает письма в формате .eml и загружает в S3 хранилище.
Поддерживает resume — прерванный импорт продолжается с места остановки.

Флаги переопределяют значения из конфиг-файла.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadAndResolve(config.CLIOverrides{
			Workspace: copyWorkspace,
			SaKeysDir: copySaKeysDir,
			Workers:   copyWorkers,
		})
		if err != nil {
			return err
		}
		gmailcopy.Run(cfg)
		return nil
	},
}

var (
	copyWorkspace string
	copySaKeysDir string
	copyWorkers   int
)

func init() {
	copyCmd.Flags().StringVarP(&copyWorkspace, "workspace", "W", "", "workspace prefix (обязательный)")
	copyCmd.Flags().StringVar(&copySaKeysDir, "sa-keys-dir", "", "директория с SA ключами (по умолчанию: workers)")
	copyCmd.Flags().IntVarP(&copyWorkers, "workers", "w", 0, "количество воркеров (по умолчанию: 5)")
	rootCmd.AddCommand(copyCmd)
}
