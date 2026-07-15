package main

import (
	"github.com/spf13/cobra"
	"gwsferry/internal/shared/config"

	gmailfetch "gwsferry/internal/gmail/fetch-labels"
)

var fetchLabelsCmd = &cobra.Command{
	Use:   "fetch-labels",
	Short: "Сбор label IDs из Gmail в JSON",
	Long: `Подключается к Gmail API через сервисные аккаунты,
собирает label IDs для всех писем каждого пользователя
и сохраняет в migration_labels.json.

Флаги переопределяют значения из конфиг-файла.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadAndResolve(config.CLIOverrides{
			Workspace: fetchWorkspace,
			SaKeysDir: fetchSaKeysDir,
			Workers:   fetchWorkers,
		})
		if err != nil {
			return err
		}
		gmailfetch.Run(cfg)
		return nil
	},
}

var (
	fetchWorkspace string
	fetchSaKeysDir string
	fetchWorkers   int
)

func init() {
	fetchLabelsCmd.Flags().StringVarP(&fetchWorkspace, "workspace", "W", "", "workspace prefix (обязательный)")
	fetchLabelsCmd.Flags().StringVar(&fetchSaKeysDir, "sa-keys-dir", "", "директория с SA ключами (по умолчанию: workers)")
	fetchLabelsCmd.Flags().IntVarP(&fetchWorkers, "workers", "w", 0, "количество воркеров (по умолчанию: 5)")
	rootCmd.AddCommand(fetchLabelsCmd)
}
