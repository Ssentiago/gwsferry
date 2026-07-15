package importyandex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	imaplib "github.com/emersion/go-imap/client"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
)

const (
	maxRetries      = 3
	backoffBase     = 1 * time.Second
	tokenRefreshMin = 5 * time.Minute
)

var backoffDelays = [maxRetries]time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// SharedToken — shared token across all workers for the same user.
// One refresh serves all workers, preventing 500 concurrent ExchangeToken calls.
type SharedToken struct {
	mu    sync.Mutex
	token *yandexapi.ExchangeToken
}

// ImapWorker — persistent IMAP connection per msg goroutine.
// One worker = one connection = one goroutine. Not shared, not pooled.
type ImapWorker struct {
	email        string
	api          *yandexapi.API
	clientID     string
	clientSecret string
	userID       yandexapi.UserID

	mu             sync.Mutex
	conn           *imaplib.Client
	sharedToken    *SharedToken
	createdFolders *sync.Map
}

// NewImapWorker создаёт воркер с OAuth2 auth (TARGET: Yandex IMAP).
func NewImapWorker(user yandexapi.User, api *yandexapi.API, clientID, clientSecret string, sharedToken *SharedToken, createdFolders *sync.Map) *ImapWorker {
	return &ImapWorker{
		email:          user.Email,
		api:            api,
		clientID:       clientID,
		clientSecret:   clientSecret,
		userID:         user.ID,
		sharedToken:    sharedToken,
		createdFolders: createdFolders,
	}
}

// Append appends one email to IMAP with auto-reconnect and retry.
// Header injection is handled by imap_client.Append.
func (w *ImapWorker) Append(ctx context.Context, letter Letter, dateFromHeader time.Time, rawMessage []byte) error {
	folder := ResolveFolder(letter.LabelIDs, letter.LabelNames)
	flags := ResolveFlags(letter.LabelIDs)
	needFlagged := containsStr(flags, `\Flagged`)
	appendFlags := filterOut(flags, `\Flagged`)

	folderCreated := false
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelays[attempt-1]
			log.Printf("[DEBUG] [IMAP-W] %s: retry %d/%d after %s", w.email, attempt, maxRetries, delay)
			time.Sleep(delay)
		}

		// Lazy connect
		if w.conn == nil {
			if err := w.connect(); err != nil {
				lastErr = err
				if isAuthError(err) {
					w.forceTokenRefresh()
				}
				continue
			}
		}

		// Try append
		err := Append(ctx, w.conn, folder, dateFromHeader, rawMessage, appendFlags, letter.MsgID)
		if err == nil {
			// Success — handle \Flagged via STORE
			if needFlagged {
				uid := searchByMsgID(w.conn, folder, letter.MsgID)
				if uid > 0 {
					if sfErr := StoreFlag(w.conn, folder, uid, `\Flagged`); sfErr != nil {
						log.Printf("[WARN] [IMAP-W] %s: \\Flagged STORE failed uid=%d: %v", w.email, uid, sfErr)
					}
				}
			}
			return nil
		}

		lastErr = err

		// "No such folder" → create folder and retry once
		if !folderCreated && isNoSuchFolder(err) {
			// Check shared cache — another worker may have created it already
			if w.createdFolders != nil {
				if _, loaded := w.createdFolders.LoadOrStore(folder, true); loaded {
					log.Printf("[DEBUG] [IMAP-W] %s: folder %q уже создан другим worker'ом, retry append", w.email, folder)
					folderCreated = true
					continue
				}
			}
			log.Printf("[INFO] [IMAP-W] %s: folder %q не существует, создаю...", w.email, folder)
			if crErr := CreateFolderIfNotExists(ctx, w.conn, folder); crErr != nil {
				log.Printf("[ERROR] [IMAP-W] %s: CreateFolderIfNotExists %q failed: %v", w.email, folder, crErr)
				return fmt.Errorf("create folder %q: %w", folder, crErr)
			}
			folderCreated = true
			continue // retry append
		}

		// Transient error (rate limit, "try again later", backend error) → retry with backoff
		if isTransient(err) {
			log.Printf("[WARN] [IMAP-W] %s: transient error (attempt %d): %v", w.email, attempt+1, err)
			continue
		}

		// Connection lost → reconnect
		if isConnectionLost(err) {
			log.Printf("[WARN] [IMAP-W] %s: connection lost, reconnecting (attempt %d)", w.email, attempt+1)
			w.closeConn()
			folderCreated = false // reset — will need to check again after reconnect
			continue
		}

		// Auth error → refresh token + reconnect
		if isAuthErrorWrapped(err) {
			log.Printf("[WARN] [IMAP-W] %s: auth error, refreshing token (attempt %d)", w.email, attempt+1)
			w.closeConn()
			w.forceTokenRefresh()
			folderCreated = false
			continue
		}

		// Other IMAP error — don't retry
		return err
	}

	return fmt.Errorf("append failed after %d retries: %w", maxRetries, lastErr)
}

// isNoSuchFolder checks if an IMAP error is "No such folder".
func isNoSuchFolder(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such folder") ||
		strings.Contains(msg, "does not exist")
}

// isTransient checks if an IMAP error is transient and should be retried.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "try again later") ||
		strings.Contains(msg, "failed to store") ||
		strings.Contains(msg, "backend error") ||
		strings.Contains(msg, "temporary") ||
		strings.Contains(msg, "rate limit")
}

// Close closes the persistent connection.
func (w *ImapWorker) Close() {
	w.closeConn()
}

// PreFetchExistingIDs загружает X-Gwsferry-MsgID для списка папок.
// Делает один FETCH на папку. Возвращает объединённый map[msgID]bool.
func (w *ImapWorker) PreFetchExistingIDs(ctx context.Context, folders []string) (map[string]bool, error) {
	// Лениво подключаемся если ещё нет
	if w.conn == nil {
		if err := w.connect(); err != nil {
			return nil, err
		}
	}

	combined := make(map[string]bool)
	for _, folder := range folders {
		existing, err := fetchExistingMsgIDs(ctx, w.conn, folder)
		if err != nil {
			log.Printf("[WARN] [IMAP-W] %s: PreFetchExistingIDs failed для %s: %v, пропускаю папку", w.email, folder, err)
			continue
		}
		for id := range existing {
			combined[id] = true
		}
	}
	log.Printf("[INFO] [IMAP-W] %s: PreFetchExistingIDs: %d уникальных msgID в %d папках", w.email, len(combined), len(folders))
	return combined, nil
}

func (w *ImapWorker) closeConn() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		w.conn.Logout()
		w.conn.Close()
		w.conn = nil
	}
}

func (w *ImapWorker) connect() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		return nil
	}

	// OAuth2 auth (TARGET: Yandex IMAP)
	token, err := w.ensureTokenLocked()
	if err != nil {
		return err
	}

	c, err := ConnectAndAuth(w.email, token.AccessToken)
	if err != nil {
		return err
	}

	w.conn = c
	log.Printf("[INFO] [IMAP-W] %s: connected", w.email)
	return nil
}

// ensureTokenLocked refreshes token if it expires within tokenRefreshMin.
// Uses shared token — one refresh serves all workers.
// Must be called with w.mu held.
func (w *ImapWorker) ensureTokenLocked() (*yandexapi.ExchangeToken, error) {
	st := w.sharedToken
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.token != nil && !st.token.ExpiresSoon() {
		remaining := time.Duration(st.token.ExpiresIn)*time.Second - time.Since(st.token.CreatedAt)
		if remaining > tokenRefreshMin {
			return st.token, nil
		}
		log.Printf("[DEBUG] [IMAP-W] %s: token expiring in %s, refreshing proactively", w.email, remaining.Round(time.Second))
	}

	token, err := w.api.ExchangeToken(w.clientID, w.clientSecret, w.userID)
	if err != nil {
		return nil, fmt.Errorf("exchange token for %s: %w", w.email, err)
	}
	st.token = token
	log.Printf("[DEBUG] [IMAP-W] %s: token refreshed, expires_in=%ds", w.email, token.ExpiresIn)
	return token, nil
}

func (w *ImapWorker) forceTokenRefresh() {
	w.sharedToken.mu.Lock()
	defer w.sharedToken.mu.Unlock()
	w.sharedToken.token = nil
}

// ==========================================
// HELPERS
// ==========================================

func isAuthErrorWrapped(err error) bool {
	var authErr *ErrAuthFailed
	return errors.As(err, &authErr)
}
