package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ==========================================
// HELPERS
// ==========================================

func newTestClient(handler http.Handler) (*Client, *httptest.Server) {
	ts := httptest.NewServer(handler)
	c := &Client{
		httpClient: ts.Client(),
		token:      "test-token",
	}
	return c, ts
}

func newTestAPI(handler http.Handler) (*API, *httptest.Server) {
	ts := httptest.NewServer(handler)
	c := &Client{
		httpClient: ts.Client(),
		token:      "test-token",
	}
	api := &API{
		client:       c,
		orgID:        "test-org",
		baseURL:      ts.URL,
		oauthBaseURL: ts.URL,
	}
	return api, ts
}

func mustUsers(t *testing.T, api *API) []User {
	t.Helper()
	users, err := api.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	return users
}

func userByEmail(users []User, email string) (User, bool) {
	for _, u := range users {
		if u.Email == email {
			return u, true
		}
	}
	return User{}, false
}

// ==========================================
// USER ID PARSING
// ==========================================

func TestUserIDFromString(t *testing.T) {
	input := `{"id":"1130000072441891","email":"test@example.com"}`
	var u struct {
		ID    UserID `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal([]byte(input), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.ID != 1130000072441891 {
		t.Errorf("ID = %d, want 1130000072441891", u.ID)
	}
}

func TestUserIDFromNumber(t *testing.T) {
	input := `{"id":1130000072441891,"email":"test@example.com"}`
	var u struct {
		ID    UserID `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal([]byte(input), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.ID != 1130000072441891 {
		t.Errorf("ID = %d, want 1130000072441891", u.ID)
	}
}

func TestUserIDInvalidString(t *testing.T) {
	input := `{"id":"not-a-number"}`
	var u struct {
		ID UserID `json:"id"`
	}
	if err := json.Unmarshal([]byte(input), &u); err == nil {
		t.Fatal("expected error for non-numeric string id")
	}
}

func TestUserIDMarshalJSON(t *testing.T) {
	id := UserID(1130000072441891)
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != "1130000072441891" {
		t.Errorf("marshaled = %s, want 1130000072441891", data)
	}
}

// ==========================================
// LISTUSERS
// ==========================================

func TestListUsers_SinglePage(t *testing.T) {
	resp := `{
		"users": [
			{"id":"100","email":"alice@test.com","isEnabled":true,"isDismissed":false},
			{"id":"200","email":"bob@test.com","isEnabled":true,"isDismissed":false}
		],
		"page": 1, "pages": 1, "total": 2
	}`
	api, ts := newTestAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(resp))
	}))
	defer ts.Close()

	users := mustUsers(t, api)

	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if u, ok := userByEmail(users, "alice@test.com"); !ok || u.ID != 100 {
		t.Errorf("alice: got ID=%d, ok=%v", u.ID, ok)
	}
}

func TestListUsers_Pagination(t *testing.T) {
	callCount := 0
	api, ts := newTestAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var resp string
		switch callCount {
		case 1:
			resp = `{"users":[{"id":"1","email":"u1@test.com","isEnabled":true,"isDismissed":false}],"page":1,"pages":3,"total":3}`
		case 2:
			resp = `{"users":[{"id":"2","email":"u2@test.com","isEnabled":true,"isDismissed":false}],"page":2,"pages":3,"total":3}`
		case 3:
			resp = `{"users":[{"id":"3","email":"u3@test.com","isEnabled":true,"isDismissed":false}],"page":3,"pages":3,"total":3}`
		}
		w.Write([]byte(resp))
	}))
	defer ts.Close()

	users := mustUsers(t, api)

	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if callCount != 3 {
		t.Errorf("made %d requests, want 3", callCount)
	}
}

func TestListUsers_FilterDisabledAndDismissed(t *testing.T) {
	resp := `{
		"users": [
			{"id":"1","email":"active@test.com","isEnabled":true,"isDismissed":false},
			{"id":"2","email":"disabled@test.com","isEnabled":false,"isDismissed":false},
			{"id":"3","email":"dismissed@test.com","isEnabled":true,"isDismissed":true},
			{"id":"4","email":"both@test.com","isEnabled":false,"isDismissed":true}
		],
		"page": 1, "pages": 1, "total": 4
	}`
	api, ts := newTestAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(resp))
	}))
	defer ts.Close()

	users := mustUsers(t, api)

	if len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}
	if users[0].Email != "active@test.com" {
		t.Errorf("got email=%s, want active@test.com", users[0].Email)
	}
}

func TestListUsers_EmptyPages0(t *testing.T) {
	resp := `{"users":[],"page":1,"pages":0,"total":0}`
	api, ts := newTestAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(resp))
	}))
	defer ts.Close()

	users, err := api.ListUsers()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("got %d users, want 0", len(users))
	}
}

// ==========================================
// EXCHANGE TOKEN (EXPIRY)
// ==========================================

func TestExchangeToken_Expired(t *testing.T) {
	token := &ExchangeToken{
		AccessToken: "abc",
		ExpiresIn:   3600,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	}
	if !token.Expired() {
		t.Error("expected Expired() == true for 2h-old token with 1h TTL")
	}
}

func TestExchangeToken_NotExpired(t *testing.T) {
	token := &ExchangeToken{
		AccessToken: "abc",
		ExpiresIn:   3600,
		CreatedAt:   time.Now(),
	}
	if token.Expired() {
		t.Error("expected Expired() == false for fresh token")
	}
}

func TestExchangeToken_ExpiresSoon(t *testing.T) {
	token := &ExchangeToken{
		AccessToken: "abc",
		ExpiresIn:   3600,
		CreatedAt:   time.Now().Add(-3541 * time.Second), // 59s remaining
	}
	if !token.ExpiresSoon() {
		t.Error("expected ExpiresSoon() == true with ~59s remaining")
	}
}

func TestExchangeToken_NotExpiresSoon(t *testing.T) {
	token := &ExchangeToken{
		AccessToken: "abc",
		ExpiresIn:   3600,
		CreatedAt:   time.Now().Add(-3539 * time.Second), // 61s remaining
	}
	if token.ExpiresSoon() {
		t.Error("expected ExpiresSoon() == false with ~61s remaining")
	}
}

func TestExchangeToken_DefaultExpiresIn(t *testing.T) {
	api, ts := newTestAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"tok","token_type":"bearer"}`))
	}))
	defer ts.Close()

	token, err := api.ExchangeToken("cid", "csecret", 123)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600 (default)", token.ExpiresIn)
	}
}

// ==========================================
// CLASSIFY API ERROR
// ==========================================

func TestClassifyAPIError_Code16_InvalidToken(t *testing.T) {
	api := &API{}
	err := api.classifyAPIError(&APIError{Code: 16, Message: "Unauthorized"})
	var invalidToken *InvalidTokenError
	if !errors.As(err, &invalidToken) {
		t.Errorf("got %T, want *InvalidTokenError", err)
	}
}

func TestClassifyAPIError_Code7_NotAnOwner(t *testing.T) {
	api := &API{}
	err := api.classifyAPIError(&APIError{Code: 7, Message: "Not an owner"})
	var notOwner *NotAnOwnerError
	if !errors.As(err, &notOwner) {
		t.Errorf("got %T, want *NotAnOwnerError", err)
	}
}

func TestClassifyAPIError_Code7_NoRequiredScope(t *testing.T) {
	api := &API{}
	err := api.classifyAPIError(&APIError{Code: 7, Message: "No required scope"})
	var insScope *InsufficientScopeError
	if !errors.As(err, &insScope) {
		t.Errorf("got %T, want *InsufficientScopeError", err)
	}
}

func TestClassifyAPIError_Code7_CaseInsensitive(t *testing.T) {
	api := &API{}
	err := api.classifyAPIError(&APIError{Code: 7, Message: "NOT AN OWNER"})
	var notOwner *NotAnOwnerError
	if !errors.As(err, &notOwner) {
		t.Errorf("got %T, want *NotAnOwnerError", err)
	}
}

func TestClassifyAPIError_UnknownCode(t *testing.T) {
	api := &API{}
	original := &APIError{Code: 99, Message: "something"}
	err := api.classifyAPIError(original)
	if !errors.Is(err, original) {
		t.Errorf("expected original *APIError returned for unknown code, got %T", err)
	}
}

// ==========================================
// CLIENT.DO (HTTP)
// ==========================================

func TestClientDo_Success(t *testing.T) {
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "OAuth test-token" {
			t.Errorf("Authorization = %q, want 'OAuth test-token'", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/test", nil)
	_, body, err := c.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %s", body)
	}
}

func TestClientDo_429_RateLimit(t *testing.T) {
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":"rate limit"}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/test", nil)
	_, _, err := c.Do(req)
	var rateLimitErr *RateLimitError
	if !errors.As(err, &rateLimitErr) {
		t.Fatalf("got %T, want *RateLimitError", err)
	}
	if rateLimitErr.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", rateLimitErr.RetryAfter)
	}
}

func TestClientDo_4xx_APIError(t *testing.T) {
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"code":7,"message":"Not an owner"}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/test", nil)
	_, _, err := c.Do(req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %T, want *APIError", err)
	}
	if apiErr.Code != 7 {
		t.Errorf("Code = %d, want 7", apiErr.Code)
	}
}

func TestClientDo_5xx_Retry(t *testing.T) {
	attempts := 0
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts <= 2 {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"internal"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/test", nil)
	_, body, err := c.Do(req)
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("body = %s", body)
	}
}

func TestClientDo_5xx_ExhaustsRetries(t *testing.T) {
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/test", nil)
	_, _, err := c.Do(req)
	var serverErr *ServerError
	if !errors.As(err, &serverErr) {
		t.Fatalf("got %T, want *ServerError", err)
	}
}

func TestClientDo_POST_NoRetry(t *testing.T) {
	attempts := 0
	c, ts := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"fail"}`))
	}))
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/test", strings.NewReader(`{}`))
	_, _, err := c.Do(req)
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("POST retried %d times, want 0 retries (1 attempt)", attempts)
	}
}
