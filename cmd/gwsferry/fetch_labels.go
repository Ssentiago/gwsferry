package main

import (
	"github.com/spf13/cobra"
	gmailfetch "gwsferry/internal/gmail/fetch-labels"
)

var fetchLabelsCmd = &cobra.Command{
	Use:   "fetch-labels",
	Short: "Сбор label IDs из Gmail в JSON",
	Long: `Подключается к Gmail API через сервисные аккаунты,
собирает label IDs для всех писем каждого пользователя
и сохраняет в migration_labels.json.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		gmailfetch.Run()
		return nil
	},
}

func init() {
	rootCmd.AddCommand(fetchLabelsCmd)
}
