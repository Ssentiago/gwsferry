package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gwsferry/internal/shared/config"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Генерация конфигурационного файла gwsferry.toml",
	Long: `Создаёт gwsferry.toml рядом с бинарём с настройками по умолчанию.
Заполните [s3] и [yandex] секции перед использованием.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		execPath, _ := os.Executable()
		configPath := filepath.Join(filepath.Dir(execPath), "gwsferry.toml")

		if _, err := os.Stat(configPath); err == nil {
			fmt.Printf("Конфиг уже существует: %s\n", configPath)
			fmt.Println("Отредактируйте его вручную.")
			return nil
		}

		if err := os.WriteFile(configPath, []byte(config.GenerateDefaultConfig()), 0644); err != nil {
			return fmt.Errorf("создание конфига: %w", err)
		}

		fmt.Printf("Конфиг создан: %s\n", configPath)
		fmt.Println("Заполните [s3] и [yandex] секции.")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
