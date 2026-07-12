package fetchlabels

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pterm/pterm"
	"gwsferry/internal/gmail/gmailapi"
	"gwsferry/internal/shared/util"
)

type userRecord struct {
	Email string `json:"Email Address [Required]"`
}

func loadEmails(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Users []userRecord `json:"users"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var emails []string
	for _, u := range doc.Users {
		if u.Email != "" {
			emails = append(emails, u.Email)
		}
	}
	return emails, nil
}

func loadServiceAccountKeys(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			keys = append(keys, filepath.Join(dir, e.Name()))
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("в %s нет .json ключей", dir)
	}
	util.SortStringsNatural(keys)
	return keys, nil
}

func verifyServiceAccounts(ctx context.Context, keys []string, testEmail string) []string {
	fmt.Println()
	pterm.DefaultSection.Println("Pre-flight проверка сервисных аккаунтов...")
	log.Println("[INFO] >>> Pre-flight проверка сервисных аккаунтов...")
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
			log.Printf("[ERROR]   [FAIL] %s -> %v", name, err)
			continue
		}
		pterm.Success.Printfln("%s", name)
		log.Printf("[INFO]   [OK]   %s", name)
		valid = append(valid, key)
	}
	return valid
}
