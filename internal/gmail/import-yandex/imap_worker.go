package importyandex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	imapclient "github.com/emersion/go-imap/v2/imapclient"

	yandexapi "gwsferry/internal/gmail/import-yandex/api"
)

const (
	maxRetries      = 3
	tokenRefreshMin = 5 * time.Minute
)

var backoffDelays = [maxRetries]time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

type SharedToken struct {
	mu    sync.Mutex
	token *yandexapi.ExchangeToken
}

type ImapWorker struct {
	email        string
	api          *yandexapi.API
	clientID     string
	clientSecret string
	userID       yandexapi.UserID

	mu             sync.Mutex
	conn           *imapclient.Client
	sharedToken    *SharedToken
	createdFolders *sync.Map
	statusFn       func(status string) // callback для обновления статуса в dashboard
}

func NewImapWorker(user yandexapi.User, api *yandexapi.API, clientID, clientSecret string, sharedToken *SharedToken, createdFolders *sync.Map, statusFn func(string)) *ImapWorker {
	return &ImapWorker{
		email:          user.Email,
		api:            api,
		clientID:       clientID,
		clientSecret:   clientSecret,
		userID:         user.ID,
		sharedToken:    sharedToken,
		createdFolders: createdFolders,
		statusFn:       statusFn,
	}
}

func (w *ImapWorker) Append(ctx context.Context, letter Letter, dateFromHeader time.Time, rawMessage []byte) error {
	folder := ResolveFolder(letter.LabelIDs, letter.LabelNames)
	flags := ResolveFlags(letter.LabelIDs)
	needFlagged := containsStr(flags, `\Flagged`)
	appendFlags := filterOut(flags, `\Flagged`)

	folderCreated := false
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 0 {
			delay := backoffDelays[attempt-1]
			log.Printf("[DEBUG] [IMAP-W] %s: retry %d/%d after %s", w.email, attempt, maxRetries, delay)
			// Обратный отсчёт в dashboard
			if w.statusFn != nil {
				w.statusFn(fmt.Sprintf("retry %d/%d, backoff %s", attempt, maxRetries, delay))
			}
			remaining := delay
			for remaining > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
					remaining -= time.Second
					if remaining > 0 && w.statusFn != nil {
						w.statusFn(fmt.Sprintf("retry %d/%d, %ds", attempt, maxRetries, int(remaining.Seconds())))
					}
				}
			}
		}

		if w.conn == nil {
			if err := w.connect(ctx); err != nil {
				lastErr = err
				if isAuthError(err) {
					w.forceTokenRefresh()
				}
				continue
			}
		}

		err := w.doAppend(ctx, folder, dateFromHeader, rawMessage, appendFlags, letter.MsgID)
		if err == nil {
			if needFlagged {
				w.doStoreFlag(ctx, folder, letter.MsgID, `\Flagged`)
			}
			return nil
		}

		lastErr = err

		if isNoSuchFolder(err) && !folderCreated {
			if w.createdFolders != nil {
				if _, loaded := w.createdFolders.LoadOrStore(folder, true); loaded {
					folderCreated = true
					continue
				}
			}
			log.Printf("[INFO] [IMAP-W] %s: folder %q не существует, создаю...", w.email, folder)
			if crErr := w.conn.Create(folder, nil).Wait(); crErr != nil {
				return fmt.Errorf("create folder %q: %w", folder, crErr)
			}
			folderCreated = true
			continue
		}

		if isTransient(err) {
			log.Printf("[WARN] [IMAP-W] %s: transient error (attempt %d): %v", w.email, attempt+1, err)
			continue
		}

		if isConnectionLost(err) {
			log.Printf("[WARN] [IMAP-W] %s: connection lost, reconnecting", w.email)
			w.closeConn()
			folderCreated = false
			continue
		}

		if isAuthErrorWrapped(err) {
			log.Printf("[WARN] [IMAP-W] %s: auth error, refreshing token", w.email)
			w.closeConn()
			w.forceTokenRefresh()
			folderCreated = false
			continue
		}

		return err
	}

	return fmt.Errorf("append failed after %d retries: %w", maxRetries, lastErr)
}

func (w *ImapWorker) doAppend(ctx context.Context, folder string, dateFromHeader time.Time, rawMessage []byte, flags []string, msgID string) error {
	enriched := injectMsgIDHeader(rawMessage, msgID)

	var flagList []imap.Flag
	for _, f := range flags {
		flagList = append(flagList, imap.Flag(f))
	}

	opts := &imap.AppendOptions{
		Flags: flagList,
		Time:  dateFromHeader,
	}
	cmd := w.conn.Append(folder, int64(len(enriched)), opts)

	if _, err := cmd.Write(enriched); err != nil {
		return fmt.Errorf("append write: %w", err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("append close: %w", err)
	}
	if _, err := cmd.Wait(); err != nil {
		if isConnectionLost(err) {
			return &ConnectionLostError{Op: "append to " + folder, Err: err}
		}
		return err
	}

	IncrAppend()
	return nil
}

func (w *ImapWorker) doStoreFlag(ctx context.Context, folder, msgID, flag string) {
	criteria := &imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{
			{Key: "X-Gwsferry-MsgID", Value: msgID},
		},
	}
	searchResult, err := w.conn.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return
	}
	uids := searchResult.AllUIDs()
	if len(uids) == 0 {
		return
	}

	numSet := imap.UIDSetNum(uids[0])
	storeOpts := &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.Flag(flag)},
	}
	w.conn.Store(numSet, storeOpts, nil).Close()
}

func (w *ImapWorker) PreFetchExistingIDs(ctx context.Context, folders []string) (map[string]bool, error) {
	if w.conn == nil {
		if err := w.connect(ctx); err != nil {
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

func (w *ImapWorker) Close() {
	w.closeConn()
}

func (w *ImapWorker) closeConn() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}

func (w *ImapWorker) connect(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil {
		return nil
	}

	token, err := w.ensureTokenLocked()
	if err != nil {
		return err
	}

	c, err := ConnectAndAuth(ctx, w.email, token.AccessToken)
	if err != nil {
		return err
	}

	w.conn = c
	log.Printf("[INFO] [IMAP-W] %s: connected", w.email)
	return nil
}

func (w *ImapWorker) ensureTokenLocked() (*yandexapi.ExchangeToken, error) {
	st := w.sharedToken
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.token != nil && !st.token.ExpiresSoon() {
		remaining := time.Duration(st.token.ExpiresIn)*time.Second - time.Since(st.token.CreatedAt)
		if remaining > tokenRefreshMin {
			return st.token, nil
		}
		log.Printf("[DEBUG] [IMAP-W] %s: token expiring in %s, refreshing", w.email, remaining.Round(time.Second))
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

func isNoSuchFolder(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such folder") ||
		strings.Contains(msg, "does not exist")
}

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

func isConnectionLost(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "connection reset")
}

func isAuthErrorWrapped(err error) bool {
	var authErr *ErrAuthFailed
	return errors.As(err, &authErr)
}
