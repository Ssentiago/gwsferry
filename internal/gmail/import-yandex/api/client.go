package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client — HTTP-клиент для Yandex 360 API с OAuth-аутентификацией.
// Обрабатывает транспортные ошибки: сетевые сбои, 5xx, rate limit (429).
// Retry-safe только для GET/HEAD/PUT/DELETE. Для POST — retry не делается.
type Client struct {
	httpClient *http.Client
	token      string
}

// NewClient создаёт HTTP-клиент с таймаутом 30 секунд.
func NewClient(oauthToken string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		token:      oauthToken,
	}
}

// Do выполняет HTTP-запрос с авторизацией и retry.
// Retry policy:
//   - 429: возвращает *RateLimitError с RetryAfter, повторных попыток НЕ делает.
//     Вызывающий код сам решает — ждать или отступить.
//   - 5xx: exponential backoff, до 3 попыток, но только для идемпотентных методов
//     (GET, HEAD, PUT, DELETE). POST/patch — без retry.
//   - Сетевые ошибки: retry с backoff.
func (c *Client) Do(req *http.Request) (*http.Response, []byte, error) {
	req.Header.Set("Authorization", "OAuth "+c.token)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	maxRetries := 3
	canRetry := req.Method == "GET" || req.Method == "HEAD" || req.Method == "PUT" || req.Method == "DELETE"
	if !canRetry {
		maxRetries = 0
	}

	start := time.Now()
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("[DEBUG] [YANDEX] Retry %d/%d for %s %s (backoff %s)", attempt, maxRetries, req.Method, req.URL.Path, backoff(attempt))
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = &TransportError{Op: req.Method + " " + req.URL.Path, Err: err}
			log.Printf("[WARN] [YANDEX] Transport error %s %s (attempt %d/%d за %s): %v",
				req.Method, req.URL.Path, attempt+1, maxRetries, time.Since(start), lastErr)
			time.Sleep(backoff(attempt))
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = &TransportError{Op: "read body", Err: readErr}
			log.Printf("[WARN] [YANDEX] Read body error %s %s (attempt %d/%d): %v",
				req.Method, req.URL.Path, attempt+1, maxRetries, readErr)
			time.Sleep(backoff(attempt))
			continue
		}

		log.Printf("[DEBUG] [YANDEX] %s %s → status=%d bodyLen=%d за %s (attempt %d/%d)",
			req.Method, req.URL.Path, resp.StatusCode, len(body), time.Since(start), attempt+1, maxRetries+1)

		if resp.StatusCode == 429 {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			log.Printf("[WARN] [YANDEX] 429 rate limit on %s %s, retryAfter=%s", req.Method, req.URL.Path, retryAfter)
			return nil, nil, &RateLimitError{RetryAfter: retryAfter}
		}

		if resp.StatusCode >= 500 {
			log.Printf("[WARN] [YANDEX] %d on %s %s (attempt %d/%d): %s", resp.StatusCode, req.Method, req.URL.Path, attempt+1, maxRetries, truncate(string(body), 200))
			lastErr = &ServerError{StatusCode: resp.StatusCode, Body: body}
			time.Sleep(backoff(attempt))
			continue
		}

		if resp.StatusCode >= 400 {
			apiErr := parseAPIError(resp.StatusCode, body)
			log.Printf("[ERROR] [YANDEX] %d on %s %s: %v", resp.StatusCode, req.Method, req.URL.Path, apiErr)
			return resp, body, apiErr
		}

		return resp, body, nil
	}

	log.Printf("[ERROR] [YANDEX] Все попытки исчерпаны %s %s (последняя ошибка: %v за %s)",
		req.Method, req.URL.Path, lastErr, time.Since(start))
	return nil, nil, lastErr
}

// Get выполняет GET-запрос.
func (c *Client) Get(url string) (*http.Response, []byte, error) {
	log.Printf("[DEBUG] [YANDEX] Get: %s", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("[ERROR] [YANDEX] Get: создание запроса: %v", err)
		return nil, nil, err
	}
	return c.Do(req)
}

// ==========================================
// ОШИБКИ
// ==========================================

// TransportError — сетевая ошибка или ошибка чтения тела.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transport %s: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// RateLimitError — 429 Too Many Requests.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit: retry after %s", e.RetryAfter)
}

// ServerError — 5xx ответ.
type ServerError struct {
	StatusCode int
	Body       []byte
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("server error %d: %s", e.StatusCode, truncate(string(e.Body), 200))
}

// APIError — Yandex API вернул ошибку в теле ответа (4xx, кроме 429).
// Поддерживает два формата:
//   - api360.yandex.net: {"code": 16, "message": "Unauthorized"}
//   - oauth.yandex.ru:   {"error": "invalid_request", "error_description": "..."}
type APIError struct {
	StatusCode int
	Code       int    // числовой gRPC-style код (api360), 0 для OAuth-формата
	ErrCode    string // строковый код (oauth), "" для gRPC-формата
	Message    string
	Raw        []byte
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("api error %d [code=%d]: %s", e.StatusCode, e.Code, e.Message)
	}
	if e.ErrCode != "" {
		return fmt.Sprintf("api error %d [%s]: %s", e.StatusCode, e.ErrCode, e.Message)
	}
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Message)
}

// ==========================================
// HELPERS
// ==========================================

func backoff(attempt int) time.Duration {
	return time.Duration(math.Pow(2, float64(attempt))) * time.Second
}

func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 60 * time.Second
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, s); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 60 * time.Second
}

func parseAPIError(statusCode int, body []byte) *APIError {
	// Формат api360.yandex.net: {"code": 16, "message": "Unauthorized"}
	var grpc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &grpc); err == nil && (grpc.Code != 0 || grpc.Message != "") {
		log.Printf("[DEBUG] [YANDEX] parseAPIError: gRPC format: status=%d code=%d message=%q", statusCode, grpc.Code, grpc.Message)
		return &APIError{
			StatusCode: statusCode,
			Code:       grpc.Code,
			Message:    grpc.Message,
			Raw:        body,
		}
	}

	// Формат oauth.yandex.ru: {"error": "invalid_request", "error_description": "..."}
	var oauth struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &oauth); err == nil && oauth.Error != "" {
		log.Printf("[DEBUG] [YANDEX] parseAPIError: OAuth format: status=%d error=%q description=%q", statusCode, oauth.Error, oauth.Description)
		return &APIError{
			StatusCode: statusCode,
			ErrCode:    oauth.Error,
			Message:    oauth.Description,
			Raw:        body,
		}
	}

	log.Printf("[DEBUG] [YANDEX] parseAPIError: raw format: status=%d body=%s", statusCode, truncate(string(body), 200))
	return &APIError{
		StatusCode: statusCode,
		Message:    truncate(string(body), 200),
		Raw:        body,
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
