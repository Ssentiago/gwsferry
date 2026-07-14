package importyandex

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// ==========================================
// CONFIGURATION
// ==========================================

// ImapTimeout — timeout for one IMAP operation (Append, List, Fetch, etc.).
var ImapTimeout = 10 * time.Minute

const imapHost = "imap.yandex.ru:993"
const imapDialTimeout = 30 * time.Second

// ==========================================
// COUNTERS
// ==========================================

var (
	imapAppendCount int64
	imapErrorCount  int64
)

func IncrAppend()          { atomic.AddInt64(&imapAppendCount, 1) }
func IncrError()           { atomic.AddInt64(&imapErrorCount, 1) }
func GetAppendCount() int64 { return atomic.LoadInt64(&imapAppendCount) }
func GetErrorCount() int64  { return atomic.LoadInt64(&imapErrorCount) }
func ResetCounters() {
	atomic.StoreInt64(&imapAppendCount, 0)
	atomic.StoreInt64(&imapErrorCount, 0)
}

// ==========================================
// TYPES
// ==========================================

type MessageMeta struct {
	UID          uint32
	Flags        []string
	InternalDate time.Time
	Subject      string
	From         string
	Date         string
}

type ErrAuthFailed struct{ Msg string }

func (e *ErrAuthFailed) Error() string { return e.Msg }

type ConnectionLostError struct {
	Op  string
	Err error
}

func (e *ConnectionLostError) Error() string {
	return fmt.Sprintf("connection lost during %s: %v", e.Op, e.Err)
}
func (e *ConnectionLostError) Unwrap() error { return e.Err }

type ErrOperationTimeout struct {
	Op      string
	Timeout time.Duration
}

func (e *ErrOperationTimeout) Error() string {
	return fmt.Sprintf("imap timeout %s (limit %s)", e.Op, e.Timeout)
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

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "authenticationfailed") ||
		strings.Contains(msg, "invalid credentials") ||
		strings.Contains(msg, "login incorrect") ||
		strings.Contains(msg, "no login")
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func filterOut(slice []string, exclude string) []string {
	var result []string
	for _, v := range slice {
		if v != exclude {
			result = append(result, v)
		}
	}
	return result
}

// ==========================================
// CONNECTION
// ==========================================

// ConnectAndAuth — TLS + XOAUTH2 with connection and operation timeouts.
func ConnectAndAuth(email, token string) (*client.Client, error) {
	log.Printf("[INFO] [IMAP] ConnectAndAuth: email=%s host=%s dialTimeout=%s imapTimeout=%s",
		email, imapHost, imapDialTimeout, ImapTimeout)

	start := time.Now()
	conn, err := net.DialTimeout("tcp", imapHost, imapDialTimeout)
	if err != nil {
		log.Printf("[ERROR] [IMAP] ConnectAndAuth: dial failed %s: %v (%s)", email, err, time.Since(start))
		return nil, fmt.Errorf("imap dial: %w", err)
	}
	log.Printf("[DEBUG] [IMAP] ConnectAndAuth: TCP dial OK %s %s", email, time.Since(start))

	tlsConn := tls.Client(conn, &tls.Config{ServerName: "imap.yandex.ru"})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap tls handshake: %w", err)
	}
	log.Printf("[DEBUG] [IMAP] ConnectAndAuth: TLS OK %s %s", email, time.Since(start))

	tlsConn.SetDeadline(time.Now().Add(ImapTimeout))

	c, err := client.New(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("imap client: %w", err)
	}

	saslClient := &xoauth2Client{email: email, token: token}
	if err := c.Authenticate(saslClient); err != nil {
		c.Close()
		if isAuthError(err) {
			return nil, &ErrAuthFailed{Msg: err.Error()}
		}
		return nil, fmt.Errorf("imap auth: %w", err)
	}
	log.Printf("[INFO] [IMAP] ConnectAndAuth: auth OK %s %s", email, time.Since(start))

	tlsConn.SetDeadline(time.Time{})
	return c, nil
}

// ==========================================
// IMAP OPERATIONS
// ==========================================

// withTimeout wraps a function with a context timeout.
func withTimeout(ctx context.Context, op string, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, ImapTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return &ErrOperationTimeout{Op: op, Timeout: ImapTimeout}
	}
}

// Append appends a message to an IMAP folder.
func Append(ctx context.Context, c *client.Client, folder string, dateFromHeader time.Time, rawMessage []byte, flags []string, msgID string) error {
	enriched := injectMsgIDHeader(rawMessage, msgID)
	err := withTimeout(ctx, "append to "+folder, func() error {
		lit := bytes.NewReader(enriched)
		return c.Append(folder, flags, dateFromHeader, lit)
	})
	if err != nil {
		IncrError()
		if isConnectionLost(err) {
			return &ConnectionLostError{Op: "append to " + folder, Err: err}
		}
		return err
	}
	IncrAppend()
	return nil
}

// searchByMsgID finds a UID by X-Gwsferry-MsgID header via IMAP SEARCH.
func searchByMsgID(c *client.Client, folder, msgID string) uint32 {
	state, err := c.Select(folder, true)
	if err != nil {
		return 0
	}
	_ = state

	criteria := &imap.SearchCriteria{
		Header: map[string][]string{
			"X-Gwsferry-MsgID": {msgID},
		},
	}
	uids, err := c.UidSearch(criteria)
	if err != nil || len(uids) == 0 {
		return 0
	}
	return uids[0]
}

// StoreFlag sets a flag by UID.
func StoreFlag(c *client.Client, folder string, uid uint32, flag string) error {
	return withTimeout(context.Background(), "store flag "+folder, func() error {
		_, err := c.Select(folder, false)
		if err != nil {
			return fmt.Errorf("select %s: %w", folder, err)
		}
		seqSet := new(imap.SeqSet)
		seqSet.Add(strconv.FormatUint(uint64(uid), 10))
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		return c.UidStore(seqSet, item, []interface{}{flag}, nil)
	})
}

// List returns messages from an IMAP folder.
func List(ctx context.Context, c *client.Client, folder string) ([]MessageMeta, error) {
	var result []MessageMeta
	err := withTimeout(ctx, "list "+folder, func() error {
		state, err := c.Select(folder, true)
		if err != nil {
			return fmt.Errorf("select %s: %w", folder, err)
		}
		if state.Messages == 0 {
			return nil
		}
		seqSet := new(imap.SeqSet)
		seqSet.Add("1:*")
		messages := make(chan *imap.Message, 100)
		done := make(chan error, 1)
		go func() {
			done <- c.UidFetch(seqSet, []imap.FetchItem{
				imap.FetchUid,
				imap.FetchFlags,
				imap.FetchInternalDate,
				imap.FetchEnvelope,
			}, messages)
		}()
		for msg := range messages {
			meta := MessageMeta{UID: msg.Uid, InternalDate: msg.InternalDate}
			for _, f := range msg.Flags {
				meta.Flags = append(meta.Flags, string(f))
			}
			if msg.Envelope != nil {
				meta.Subject = msg.Envelope.Subject
				if len(msg.Envelope.From) > 0 {
					meta.From = msg.Envelope.From[0].Address()
				}
				meta.Date = msg.Envelope.Date.Format(time.RFC1123Z)
			}
			result = append(result, meta)
		}
		return <-done
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Delete marks a message as \Deleted and expunges.
func Delete(ctx context.Context, c *client.Client, folder string, uid uint32) error {
	return withTimeout(ctx, "delete "+folder, func() error {
		_, err := c.Select(folder, false)
		if err != nil {
			return fmt.Errorf("select %s: %w", folder, err)
		}
		seqSet := new(imap.SeqSet)
		seqSet.Add(strconv.FormatUint(uint64(uid), 10))
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		if err := c.UidStore(seqSet, item, []interface{}{imap.DeletedFlag}, nil); err != nil {
			return fmt.Errorf("flag \\Deleted uid %d: %w", uid, err)
		}
		return c.Expunge(nil)
	})
}

// injectMsgIDHeader adds X-Gwsferry-MsgID to RFC822 headers.
func injectMsgIDHeader(raw []byte, msgID string) []byte {
	header := fmt.Sprintf("X-Gwsferry-MsgID: %s\r\n", msgID)
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx == -1 {
		return append(raw, []byte(header+"\r\n")...)
	}
	result := make([]byte, 0, len(raw)+len(header))
	result = append(result, raw[:idx]...)
	result = append(result, []byte(header)...)
	result = append(result, raw[idx:]...)
	return result
}

// Close closes an IMAP connection.
func Close(c *client.Client) {
	if c != nil {
		c.Logout()
		c.Close()
	}
}

// CreateFolderIfNotExists checks if a folder exists and creates it if not.
func CreateFolderIfNotExists(ctx context.Context, c *client.Client, folder string) error {
	return withTimeout(ctx, "create folder "+folder, func() error {
		mailboxes := make(chan *imap.MailboxInfo, 10)
		done := make(chan error, 1)
		go func() {
			done <- c.List("", folder, mailboxes)
		}()
		exists := false
		for range mailboxes {
			exists = true
		}
		if err := <-done; err != nil {
			return fmt.Errorf("list folder %s: %w", folder, err)
		}
		if exists {
			return nil
		}
		return c.Create(folder)
	})
}

// DeleteFolder deletes an IMAP folder.
func DeleteFolder(ctx context.Context, c *client.Client, folder string) error {
	return withTimeout(ctx, "delete folder "+folder, func() error {
		return c.Delete(folder)
	})
}

// ==========================================
// XOAUTH2 SASL
// ==========================================

type xoauth2Client struct {
	email string
	token string
}

func (a *xoauth2Client) Start() (mech string, ir []byte, err error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.email, a.token)), nil
}

func (a *xoauth2Client) Next(challenge []byte) (ir []byte, err error) {
	return nil, fmt.Errorf("xoauth2: unexpected server challenge: %s", challenge)
}
