package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiBase   = "https://api360.yandex.net"
	oauthBase = "https://oauth.yandex.ru"
)

// ==========================================
// ДОМЕННЫЕ ОШИБКИ
// ==========================================

type NotAnOwnerError struct{}

func (e *NotAnOwnerError) Error() string { return "not an owner" }

type InsufficientScopeError struct{}

func (e *InsufficientScopeError) Error() string { return "insufficient scope" }

type InvalidTokenError struct{}

func (e *InvalidTokenError) Error() string { return "invalid or expired token" }

// ==========================================
// USER — результат загрузки юзера
// ==========================================

// UserID — числовой uid Yandex.
// API отдаёт id как строку ("1130000072441891"), UnmarshalJSON
// принимает и строку, и число.
type UserID int64

func (id *UserID) UnmarshalJSON(data []byte) error {
	// строка: "1130000072441891"
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("unmarshal UserID string: %w", err)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("parse UserID %q: %w", s, err)
		}
		*id = UserID(n)
		return nil
	}
	// число: 1130000072441891
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("unmarshal UserID number: %w", err)
	}
	*id = UserID(n)
	return nil
}

func (id UserID) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(id))
}

type User struct {
	Email        string
	ID           UserID
	ExchangeToken *ExchangeToken // nil пока токен не получен
}

// ==========================================
// EXCHANGE TOKEN
// ==========================================

type ExchangeToken struct {
	AccessToken string
	ExpiresIn   int // секунды
	CreatedAt   time.Time
}

func (t *ExchangeToken) Expired() bool {
	return time.Since(t.CreatedAt) >= time.Duration(t.ExpiresIn)*time.Second
}

func (t *ExchangeToken) ExpiresSoon() bool {
	remaining := time.Duration(t.ExpiresIn)*time.Second - time.Since(t.CreatedAt)
	return remaining < 1*time.Minute
}

// ==========================================
// API LAYER
// ==========================================

type API struct {
	client      *Client
	orgID       string
	ownerToken  string // OAuth-токен владельца организации
	baseURL     string // по умолчанию apiBase, переопределяется в тестах
	oauthBaseURL string // по умолчанию oauthBase, переопределяется в тестах
}

func NewAPI(client *Client, orgID, ownerToken string) *API {
	return &API{
		client:      client,
		orgID:       orgID,
		ownerToken:  ownerToken,
		baseURL:     apiBase,
		oauthBaseURL: oauthBase,
	}
}

// ListUsers загружает всех сотрудников организации постранично.
// Возвращает только enabled, не-dismissed юзеров.
func (a *API) ListUsers() ([]User, error) {
	var all []User
	page := 1
	start := time.Now()

	for {
		log.Printf("[DEBUG] [YANDEX] ListUsers: запрос страницы %d orgID=%s", page, a.orgID)

		url := fmt.Sprintf("%s/directory/v1/org/%s/users?page=%d", a.baseURL, a.orgID, page)
		_, body, err := a.client.Get(url)
		if err != nil {
			log.Printf("[ERROR] [YANDEX] ListUsers: ошибка запроса страницы %d: %v", page, err)
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				log.Printf("[ERROR] [YANDEX] ListUsers: APIError на странице %d: status=%d code=%d message=%q",
					page, apiErr.StatusCode, apiErr.Code, apiErr.Message)
				return nil, a.classifyAPIError(apiErr)
			}
			return nil, fmt.Errorf("list users page %d: %w", page, err)
		}

		var parsed struct {
			Users []struct {
				ID          UserID `json:"id"`
				Email       string `json:"email"`
				IsEnabled   bool   `json:"isEnabled"`
				IsDismissed bool   `json:"isDismissed"`
			} `json:"users"`
			Page  int `json:"page"`
			Pages int `json:"pages"`
			Total int `json:"total"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			log.Printf("[ERROR] [YANDEX] ListUsers: парсинг страницы %d: %v", page, err)
			return nil, fmt.Errorf("parse users page %d: %w", page, err)
		}

		before := len(all)
		for _, u := range parsed.Users {
			if u.IsEnabled && !u.IsDismissed {
				all = append(all, User{Email: u.Email, ID: u.ID})
			}
		}
		added := len(all) - before

		log.Printf("[DEBUG] [YANDEX] ListUsers: страница %d/%d: всего %d, enabled+active +%d, skipped %d (isDismissed или !isEnabled)",
			page, parsed.Pages, len(parsed.Users), added, len(parsed.Users)-added)

		if page >= parsed.Pages {
			break
		}
		page++
	}

	log.Printf("[INFO] [YANDEX] ListUsers завершено: %d enabled users за %s (%d страниц)", len(all), time.Since(start), page)
	return all, nil
}

// ExchangeToken получает временный токен для конкретного юзера (для IMAP XOAUTH2).
// clientID/clientSecret — от зарегистрированного сервисного приложения.
// subjectToken — uid юзера.
func (a *API) ExchangeToken(clientID, clientSecret string, subjectToken UserID) (*ExchangeToken, error) {
	start := time.Now()
	log.Printf("[DEBUG] [YANDEX] ExchangeToken: uid=%d clientID=%s oauthURL=%s/token",
		subjectToken, clientID, a.oauthBaseURL)

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("subject_token", strconv.FormatInt(int64(subjectToken), 10))
	form.Set("subject_token_type", "urn:yandex:params:oauth:token-type:uid")

	req, err := http.NewRequest("POST", a.oauthBaseURL+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		log.Printf("[ERROR] [YANDEX] ExchangeToken: создание запроса: %v", err)
		return nil, fmt.Errorf("exchange token create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.httpClient.Do(req)
	if err != nil {
		log.Printf("[ERROR] [YANDEX] ExchangeToken: транспортная ошибка за %s: %v", time.Since(start), err)
		return nil, fmt.Errorf("exchange token transport: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[ERROR] [YANDEX] ExchangeToken: чтение тела ответа: %v", err)
		return nil, fmt.Errorf("exchange token read body: %w", err)
	}

	log.Printf("[DEBUG] [YANDEX] ExchangeToken: ответ status=%d за %s bodyLen=%d", resp.StatusCode, time.Since(start), len(body))

	if resp.StatusCode == 429 {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		log.Printf("[WARN] [YANDEX] ExchangeToken: 429 rate limit, retryAfter=%s", retryAfter)
		return nil, &RateLimitError{RetryAfter: retryAfter}
	}

	// ExchangeToken использует OAuth client credentials (form-urlencoded), а не
	// OAuth Bearer как остальные методы API. Формат ошибок — OAuth JSON
	// (error/error_description), без gRPC-кодов. Доменная классификация
	// (NotAnOwner/InvalidToken/InsufficientScope) неприменима к token-exchange:
	// возвращаем сырой *APIError.
	if resp.StatusCode >= 400 {
		apiErr := parseAPIError(resp.StatusCode, body)
		log.Printf("[ERROR] [YANDEX] ExchangeToken: ошибка %d: %v", resp.StatusCode, apiErr)
		return nil, apiErr
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		log.Printf("[ERROR] [YANDEX] ExchangeToken: парсинг ответа: %v body=%s", err, truncate(string(body), 200))
		return nil, fmt.Errorf("parse exchange token response: %w", err)
	}

	if parsed.ExpiresIn == 0 {
		log.Printf("[DEBUG] [YANDEX] ExchangeToken: expiresIn=0, дефолт 3600")
		parsed.ExpiresIn = 3600
	}

	token := &ExchangeToken{
		AccessToken: parsed.AccessToken,
		ExpiresIn:   parsed.ExpiresIn,
		CreatedAt:   time.Now(),
	}

	log.Printf("[INFO] [YANDEX] ExchangeToken: uid=%d OK за %s expiresIn=%ds tokenType=%s",
		subjectToken, time.Since(start), parsed.ExpiresIn, parsed.TokenType)
	return token, nil
}

// classifyAPIError переводит APIError в доменную ошибку.
// gRPC code 7 (PERMISSION_DENIED) — зонтик: разные ситуации под одним кодом,
// различаем по message.
func (a *API) classifyAPIError(apiErr *APIError) error {
	switch {
	case apiErr.Code == 16:
		log.Printf("[ERROR] [YANDEX] classifyAPIError: code=16 → InvalidTokenError (status=%d message=%q)",
			apiErr.StatusCode, apiErr.Message)
		return &InvalidTokenError{}
	case apiErr.Code == 7 && strings.Contains(strings.ToLower(apiErr.Message), "not an owner"):
		log.Printf("[ERROR] [YANDEX] classifyAPIError: code=7 'not an owner' → NotAnOwnerError (status=%d message=%q)",
			apiErr.StatusCode, apiErr.Message)
		return &NotAnOwnerError{}
	case apiErr.Code == 7 && strings.Contains(strings.ToLower(apiErr.Message), "no required scope"):
		log.Printf("[ERROR] [YANDEX] classifyAPIError: code=7 'no required scope' → InsufficientScopeError (status=%d message=%q)",
			apiErr.StatusCode, apiErr.Message)
		return &InsufficientScopeError{}
	default:
		log.Printf("[ERROR] [YANDEX] classifyAPIError: неизвестная ошибка code=%d status=%d message=%q",
			apiErr.Code, apiErr.StatusCode, apiErr.Message)
		return apiErr
	}
}
