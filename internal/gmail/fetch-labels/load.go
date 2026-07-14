package fetchlabels

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pterm/pterm"
	"gwsferry/internal/gmail/gmailapi"
	"gwsferry/internal/shared/util"
)

type userRecord struct {
	Email string `json:"Email Address [Required]"`
}

func loadEmails(path string) ([]string, error) {
	start := time.Now()
	log.Printf("[DEBUG] [LOAD] чтение %s...", path)
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[ERROR] [LOAD] чтение %s: %v", path, err)
		return nil, err
	}
	var doc struct {
		Users []userRecord `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("[ERROR] [LOAD] парсинг %s: %v", path, err)
		return nil, err
	}
	var emails []string
	for _, u := range doc.Users {
		if u.Email != "" {
			emails = append(emails, u.Email)
		}
	}
	log.Printf("[INFO] [LOAD] загружено %d emails из %s (за %s)", len(emails), path, time.Since(start))
	return emails, nil
}

func loadServiceAccountKeys(dir string) ([]string, error) {
	start := time.Now()
	log.Printf("[DEBUG] [LOAD] чтение ключей из %s...", dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[ERROR] [LOAD] чтение директории %s: %v", dir, err)
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			keys = append(keys, filepath.Join(dir, e.Name()))
		}
	}
	if len(keys) == 0 {
		log.Printf("[ERROR] [LOAD] нет .json ключей в %s", dir)
		return nil, fmt.Errorf("в %s нет .json ключей", dir)
	}
	util.SortStringsNatural(keys)
	log.Printf("[INFO] [LOAD] найдено %d ключей в %s (за %s)", len(keys), dir, time.Since(start))
	return keys, nil
}

func verifyServiceAccounts(ctx context.Context, keys []string, testEmail string) []string {
	start := time.Now()
	fmt.Println()
	pterm.DefaultSection.Println("Pre-flight проверка сервисных аккаунтов...")
	log.Println("[INFO] [LOAD] >>> Pre-flight проверка сервисных аккаунтов...")
	var valid []string
	for _, key := range keys {
		name := filepath.Base(key)
		svc, err := gmailapi.BuildClient(ctx, key, testEmail)
		if err == nil {
			_, err = gmailapi.ExecWithHardTimeout(ctx, gmailapi.HardTimeout, func(cctx context.Context) (any, error) {
				return svc.Users.GetProfile("me").Context(cctx).Do()
			})
		}
		if err != nil {
			pterm.Error.Printfln("%s -> %v", name, err)
			log.Printf("[ERROR] [LOAD] [FAIL] %s -> %v", name, err)
			continue
		}
		pterm.Success.Printfln("%s", name)
		log.Printf("[INFO] [LOAD] [OK] %s", name)
		valid = append(valid, key)
	}
	log.Printf("[INFO] [LOAD] SA проверены: %d/%d валидных (за %s)", len(valid), len(keys), time.Since(start))
	return valid
}
