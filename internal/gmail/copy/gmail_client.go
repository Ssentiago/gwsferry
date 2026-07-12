package copy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

type dailyQuotaExceeded struct {
	PendingIDs []string
}

func (e *dailyQuotaExceeded) Error() string {
	return fmt.Sprintf("dailyLimitExceeded: %d pending ids", len(e.PendingIDs))
}

func buildGmailClient(saKeyPath, email string) (*gmail.Service, error) {
	log.Printf("[DEBUG] [GMAIL] Создание клиента: key=%s email=%s", saKeyPath, email)

	keyData, err := os.ReadFile(saKeyPath)
	if err != nil {
		log.Printf("[ERROR] [GMAIL] Чтение ключа %s: %v", saKeyPath, err)
		return nil, fmt.Errorf("чтение ключа %s: %w", saKeyPath, err)
	}
	cfg, err := google.JWTConfigFromJSON(keyData, "https://mail.google.com/")
	if err != nil {
		log.Printf("[ERROR] [GMAIL] Парсинг ключа %s: %v", saKeyPath, err)
		return nil, fmt.Errorf("парсинг ключа %s: %w", saKeyPath, err)
	}
	cfg.Subject = email
	tokenSource := cfg.TokenSource(context.Background())
	svc, err := gmail.NewService(context.Background(), option.WithTokenSource(tokenSource))
	if err != nil {
		log.Printf("[ERROR] [GMAIL] Создание сервиса: %v", err)
		return nil, err
	}
	log.Printf("[DEBUG] [GMAIL] Клиент создан: email=%s", email)
	return svc, nil
}

func execWithHardTimeout[T any](ctx context.Context, f func(ctx context.Context) (T, error)) (T, error) {
	cctx, cancel := context.WithTimeout(ctx, hardTimeout)
	defer cancel()
	start := time.Now()
	result, err := f(cctx)
	elapsed := time.Since(start).Seconds()
	if err != nil {
		log.Printf("[DEBUG] [NET-ERR] за %.2fs -> %v", elapsed, err)
	} else {
		log.Printf("[DEBUG] [NET-OK] за %.2fs", elapsed)
	}
	return result, err
}

func listAllMessageIDs(ctx context.Context, svc *gmail.Service, email string, onPage func(collected, page int)) ([]string, error) {
	log.Printf("[DEBUG] [LIST] Старт листинга для %s", email)
	var ids []string
	pageToken := ""
	page := 0

	for {
		page++
		var resp *gmail.ListMessagesResponse
		var lastErr error
		ok := false

		for attempt := 0; attempt < listMaxRetries; attempt++ {
			log.Printf("[DEBUG] [LIST] %s: страница %d, попытка %d/%d", email, page, attempt+1, listMaxRetries)
			r, err := execWithHardTimeout(ctx, func(cctx context.Context) (*gmail.ListMessagesResponse, error) {
				call := svc.Users.Messages.List("me").MaxResults(500).Fields("messages/id,nextPageToken").Context(cctx)
				if pageToken != "" {
					call = call.PageToken(pageToken)
				}
				return call.Do()
			})
			if err == nil {
				resp = r
				ok = true
				break
			}
			lastErr = err
			log.Printf("[WARN] [LIST] %s: страница %d попытка %d/%d не удалась: %v", email, page, attempt+1, listMaxRetries, err)
			if !isRetryableStatus(err) && !isNetworkError(err) {
				return nil, err
			}
			delay := backoffDelay(attempt)
			log.Printf("[DEBUG] [LIST] %s: retry backoff %s", email, delay)
			time.Sleep(delay)
		}
		if !ok {
			log.Printf("[ERROR] [LIST] %s: листинг не удался после %d попыток: %v", email, listMaxRetries, lastErr)
			return nil, fmt.Errorf("листинг писем %s не удался после %d попыток: %w", email, listMaxRetries, lastErr)
		}

		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
		}
		log.Printf("[DEBUG] [LIST] %s: страница %d, +%d ID, итого %d", email, page, len(resp.Messages), len(ids))
		if onPage != nil {
			onPage(len(ids), page)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	log.Printf("[DEBUG] [LIST] %s: завершено, %d message IDs", email, len(ids))
	return ids, nil
}

func fetchMessagesBatch(
	ctx context.Context,
	svc *gmail.Service,
	msgIDs []string,
	onSuccess func(msgID string, rawBytes []byte) error,
) (written int, retryIDs []string, concurrentLimitOnly bool, err error) {
	log.Printf("[DEBUG] [BATCH] Старт батча: %d msg_ids", len(msgIDs))
	batchStart := time.Now()

	type outcome struct {
		msgID    string
		rawBytes []byte
		err      error
	}

	sem := make(chan struct{}, 10)
	results := make(chan outcome, len(msgIDs))

	for _, id := range msgIDs {
		id := id
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			resp, err := execWithHardTimeout(ctx, func(cctx context.Context) (*gmail.Message, error) {
				return svc.Users.Messages.Get("me", id).Format("raw").Context(cctx).Do()
			})
			if err != nil {
				results <- outcome{msgID: id, err: err}
				return
			}
			rawBytes, err := decodeBase64URL(resp.Raw)
			results <- outcome{msgID: id, rawBytes: rawBytes, err: err}
		}()
	}

	var (
		dailyQuotaHit      bool
		concurrentLimitHit bool
		rateLimitHit       bool
		dailyPending       []string
	)

	for i := 0; i < len(msgIDs); i++ {
		o := <-results
		if o.err != nil {
			var gerr *googleapi.Error
			if errors.As(o.err, &gerr) && (gerr.Code == 429 || gerr.Code == 403) {
				reason := parseGoogleErrorReason(o.err)
				log.Printf("[WARN] [BATCH] msg=%s status=%d reason=%s: %v", o.msgID, gerr.Code, reason, o.err)
				switch reason {
				case "daily_limit":
					dailyQuotaHit = true
					dailyPending = append(dailyPending, o.msgID)
				case "concurrent_limit":
					concurrentLimitHit = true
					retryIDs = append(retryIDs, o.msgID)
				default:
					rateLimitHit = true
					retryIDs = append(retryIDs, o.msgID)
				}
			} else if errors.As(o.err, &gerr) && (gerr.Code == 500 || gerr.Code == 503) {
				log.Printf("[WARN] [BATCH] msg=%s HTTP %d: %v", o.msgID, gerr.Code, o.err)
				retryIDs = append(retryIDs, o.msgID)
			} else if isNetworkError(o.err) {
				log.Printf("[WARN] [BATCH] msg=%s сетевая ошибка: %v", o.msgID, o.err)
				retryIDs = append(retryIDs, o.msgID)
			} else {
				log.Printf("[ERROR] [BATCH] msg=%s неизвестная ошибка: %v", o.msgID, o.err)
			}
			continue
		}
		log.Printf("[DEBUG] [BATCH] msg=%s скачано %d bytes", o.msgID, len(o.rawBytes))
		if writeErr := onSuccess(o.msgID, o.rawBytes); writeErr != nil {
			log.Printf("[ERROR] [BATCH] msg=%s ошибка записи в S3: %v", o.msgID, writeErr)
			retryIDs = append(retryIDs, o.msgID)
			continue
		}
		written++
	}

	elapsed := time.Since(batchStart).Seconds()
	log.Printf("[DEBUG] [BATCH] завершено за %.2fs: written=%d retry=%d daily_quota=%v concurrent=%v rate=%v",
		elapsed, written, len(retryIDs), dailyQuotaHit, concurrentLimitHit, rateLimitHit)

	if dailyQuotaHit {
		return 0, append(dailyPending, retryIDs...), false, &dailyQuotaExceeded{PendingIDs: append(dailyPending, retryIDs...)}
	}

	return written, retryIDs, concurrentLimitHit && !rateLimitHit, nil
}

func parseGoogleErrorReason(err error) string {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return "other"
	}
	reason, message := "", ""
	if len(gerr.Errors) > 0 {
		reason = strings.ToLower(gerr.Errors[0].Reason)
		message = strings.ToLower(gerr.Errors[0].Message)
	}
	if reason == "" {
		reason = strings.ToLower(gerr.Message)
	}
	switch reason {
	case "dailylimitexceeded":
		return "daily_limit"
	case "ratelimitexceeded", "userratelimitexceeded", "quotaexceeded":
		if strings.Contains(message, "concurrent") {
			return "concurrent_limit"
		}
		return "rate_limit"
	}
	return "other"
}

func isRetryableStatus(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case 429, 403, 500, 503:
			return true
		}
	}
	return false
}

func isNetworkError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"timeout", "connection reset", "eof", "broken pipe", "no such host"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func backoffDelay(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func decodeBase64URL(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("пустой raw payload")
	}
	// Добить padding если отсутствует — Gmail может присылать по-разному.
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(s)
}
