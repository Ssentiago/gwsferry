// Package gmailapi инкапсулирует все обращения к Gmail API: сборку
// клиента через сервисный аккаунт (domain-wide delegation), листинг
// message id, получение label_names и батч-получение labelIds по
// format=metadata (тело письма не тянется - дешевле по units-квоте,
// чем полная миграция с format=raw).
package gmailapi

import (
	"context"
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

const (
	scope          = "https://mail.google.com/"
	HardTimeout    = 45 * time.Second
	ListMaxRetries = 5
)

// ErrorReason - нормализованная причина ошибки Google API.
// Тело ответа Google для 429/403
// содержит errors[0].reason/message, по которым различаются три разных
// вида "лимита": суточная квота (фатально для юзера здесь и сейчас),
// concurrent-limit (короткий backoff, не расходует retry-раунды) и
// обычный rate-limit (полноценный экспоненциальный backoff).
type ErrorReason int

const (
	ReasonOther ErrorReason = iota
	ReasonDailyLimit
	ReasonRateLimit
	ReasonConcurrentLimit
)

// ParseGoogleErrorReason разбирает googleapi.Error и определяет причину.
func ParseGoogleErrorReason(err error) ErrorReason {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return ReasonOther
	}
	var reason, message string
	if len(gerr.Errors) > 0 {
		reason = strings.ToLower(gerr.Errors[0].Reason)
		message = strings.ToLower(gerr.Errors[0].Message)
	}
	if reason == "" {
		message = strings.ToLower(gerr.Message)
	}
	switch reason {
	case "dailylimitexceeded":
		return ReasonDailyLimit
	case "ratelimitexceeded", "userratelimitexceeded", "quotaexceeded":
		if strings.Contains(message, "concurrent") {
			return ReasonConcurrentLimit
		}
		return ReasonRateLimit
	}
	return ReasonOther
}

// DailyQuotaExceededError - фатально для текущего юзера прямо сейчас;
// вызывающий код должен вернуть юзера в очередь целиком, сохранив уже
// собранный прогресс (PendingIDs).
type DailyQuotaExceededError struct {
	PendingIDs []string
	Err        error
}

func (e *DailyQuotaExceededError) Error() string {
	return fmt.Sprintf("dailyLimitExceeded: %v", e.Err)
}
func (e *DailyQuotaExceededError) Unwrap() error { return e.Err }

// BuildClient собирает Gmail-клиент для конкретного юзера через
// domain-wide delegation (Subject = email юзера, от имени которого
// действует сервисный аккаунт).
func BuildClient(ctx context.Context, saKeyPath, email string) (*gmail.Service, error) {
	keyData, err := os.ReadFile(saKeyPath)
	if err != nil {
		return nil, fmt.Errorf("чтение ключа %s: %w", saKeyPath, err)
	}
	cfg, err := google.JWTConfigFromJSON(keyData, scope)
	if err != nil {
		return nil, fmt.Errorf("парсинг ключа %s: %w", saKeyPath, err)
	}
	cfg.Subject = email

	tokenSource := cfg.TokenSource(ctx)
	svc, err := gmail.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("создание gmail service: %w", err)
	}
	return svc, nil
}

// ExecWithHardTimeout выполняет f с жёстким таймаутом. Контекст с
// таймаутом прокидывается в google-api-go-client, который корректно
// уважает ctx.Done() и в HTTP-запросах, и в токен-рефреш. Логирует
// время выполнения на DEBUG уровне.
func ExecWithHardTimeout[T any](ctx context.Context, timeout time.Duration, f func(ctx context.Context) (T, error)) (T, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
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

// FetchLabelNames - один лёгкий вызов на юзера: users.labels.list
// отдаёт {id: "Label_15", name: "Клиенты", ...} для каждого лейбла
// конкретного ящика. labelId - внутренний id, у разных юзеров под
// одинаковым числом может быть разное имя, без этой карты сырые
// labelIds в messages нерасшифровываемы.
func FetchLabelNames(ctx context.Context, svc *gmail.Service, email string) (map[string]string, error) {
	var lastErr error
	for attempt := 0; attempt < ListMaxRetries; attempt++ {
		resp, err := ExecWithHardTimeout(ctx, HardTimeout, func(cctx context.Context) (*gmail.ListLabelsResponse, error) {
			return svc.Users.Labels.List("me").Context(cctx).Do()
		})
		if err == nil {
			out := make(map[string]string, len(resp.Labels))
			for _, l := range resp.Labels {
				out[l.Id] = l.Name
			}
			return out, nil
		}
		lastErr = err
		if !isRetryableStatus(err) && !isNetworkError(err) {
			return nil, err
		}
		time.Sleep(backoffDelay(attempt))
	}
	return nil, fmt.Errorf("labels.list для %s не удался после %d попыток: %w", email, ListMaxRetries, lastErr)
}

// ListAllMessageIDs - полный список msg_id ящика, постранично.
func ListAllMessageIDs(ctx context.Context, svc *gmail.Service, email string, onPage func(collected int, page int)) ([]string, error) {
	var ids []string
	pageToken := ""
	page := 0

	for {
		page++
		var resp *gmail.ListMessagesResponse
		var lastErr error
		ok := false

		for attempt := 0; attempt < ListMaxRetries; attempt++ {
			r, err := ExecWithHardTimeout(ctx, HardTimeout, func(cctx context.Context) (*gmail.ListMessagesResponse, error) {
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
			if !isRetryableStatus(err) && !isNetworkError(err) {
				return nil, err
			}
			time.Sleep(backoffDelay(attempt))
		}
		if !ok {
			return nil, fmt.Errorf("листинг писем %s не удался после %d попыток: %w", email, ListMaxRetries, lastErr)
		}

		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
		}
		if onPage != nil {
			onPage(len(ids), page)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return ids, nil
}

// BatchLabelsResult - итог одного батч-запроса.
type BatchLabelsResult struct {
	Written             int
	RetryIDs            []string
	ConcurrentLimitOnly bool
}

// FetchLabelsBatch получает labelIds для набора msg_id через format=metadata
// (тело письма НЕ тянется). Каждый msg_id обрабатывается отдельным
// запросом с ограниченной конкурентностью (batchConcurrency).
//
// Каждый успешно полученный msg_id сразу пишется в персистентный
// результат через onSuccess(msgID, labelIDs) - НЕ ждёт финализации
// юзера, что и даёт гранулярность resume по msg_id.
func FetchLabelsBatch(
	ctx context.Context,
	svc *gmail.Service,
	msgIDs []string,
	onSuccess func(msgID string, labelIDs []string),
) (BatchLabelsResult, error) {
	type outcome struct {
		msgID    string
		labelIDs []string
		err      error
	}

	sem := make(chan struct{}, batchConcurrency)
	results := make(chan outcome, len(msgIDs))

	for _, id := range msgIDs {
		id := id
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			resp, err := ExecWithHardTimeout(ctx, HardTimeout, func(cctx context.Context) (*gmail.Message, error) {
				return svc.Users.Messages.Get("me", id).Format("metadata").MetadataHeaders().Context(cctx).Do()
			})
			if err != nil {
				results <- outcome{msgID: id, err: err}
				return
			}
			results <- outcome{msgID: id, labelIDs: resp.LabelIds}
		}()
	}

	var (
		written            int
		retryIDs           []string
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
				switch ParseGoogleErrorReason(o.err) {
				case ReasonDailyLimit:
					dailyQuotaHit = true
					dailyPending = append(dailyPending, o.msgID)
				case ReasonConcurrentLimit:
					concurrentLimitHit = true
					retryIDs = append(retryIDs, o.msgID)
				default:
					rateLimitHit = true
					retryIDs = append(retryIDs, o.msgID)
				}
			} else if errors.As(o.err, &gerr) && (gerr.Code == 500 || gerr.Code == 503) {
				retryIDs = append(retryIDs, o.msgID)
			} else if isNetworkError(o.err) {
				retryIDs = append(retryIDs, o.msgID)
			}
			// прочие фатальные ошибки для конкретного msg_id молча не
			// ретраятся - вызывающий код увидит их через непокрытый remainder.
			continue
		}
		onSuccess(o.msgID, o.labelIDs)
		written++
	}

	if dailyQuotaHit {
		return BatchLabelsResult{Written: written}, &DailyQuotaExceededError{
			PendingIDs: append(dailyPending, retryIDs...),
			Err:        errors.New("dailyLimitExceeded (per-message in batch)"),
		}
	}

	return BatchLabelsResult{
		Written:             written,
		RetryIDs:            retryIDs,
		ConcurrentLimitOnly: concurrentLimitHit && !rateLimitHit,
	}, nil
}

// batchConcurrency - сколько msg.Get летит параллельно внутри одного
// "батча". Держим консервативно ниже типичного per-user concurrent
// request limit Gmail API, чтобы не провоцировать concurrent_limit
// чаще, чем это делал настоящий batch-эндпоинт.
const batchConcurrency = 10

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
	// context deadline exceeded и типичные сетевые обрывы - без строгой
	// типизации, ловим широкий набор транспортных ошибок.
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
