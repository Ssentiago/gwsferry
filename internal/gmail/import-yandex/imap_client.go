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

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ==========================================
// CONFIGURATION
// ==========================================

var ImapTimeout = 10 * time.Minute
var imapHost = "imap.yandex.ru:993"

const imapDialTimeout = 30 * time.Second

func SetImapHost(host string) {
	if host != "" {
		imapHost = host
	}
}

// ==========================================
// COUNTERS
// ==========================================

var (
	imapAppendCount int64
	imapErrorCount  int64
)

func IncrAppend()           { atomic.AddInt64(&imapAppendCount, 1) }
func IncrError()            { atomic.AddInt64(&imapErrorCount, 1) }
func GetAppendCount() int64 { return atomic.LoadInt64(&imapAppendCount) }
func GetErrorCount() int64  { return atomic.LoadInt64(&imapErrorCount) }

// ==========================================
// TYPES
// ==========================================

type ErrAuthFailed struct{ Msg string }

func (e *ErrAuthFailed) Error() string { return e.Msg }

type ErrOperationTimeout struct {
	Op      string
	Timeout time.Duration
}

func (e *ErrOperationTimeout) Error() string {
	return fmt.Sprintf("imap timeout %s (limit %s)", e.Op, e.Timeout)
}

type ConnectionLostError struct {
	Op  string
	Err error
}

func (e *ConnectionLostError) Error() string {
	return fmt.Sprintf("connection lost during %s: %v", e.Op, e.Err)
}
func (e *ConnectionLostError) Unwrap() error { return e.Err }

// ==========================================
// CONNECTION — go-imap v2
// ==========================================

// ConnectAndAuth — TLS + XOAUTH2 через go-imap v2.
// v2 поддерживает context.Context для всех операций.
func ConnectAndAuth(ctx context.Context, email, token string) (*imapclient.Client, error) {
	log.Printf("[INFO] [IMAP] ConnectAndAuth: email=%s host=%s", email, imapHost)

	start := time.Now()
	conn, err := net.DialTimeout("tcp", imapHost, imapDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("imap dial: %w", err)
	}

	tlsServerName := imapHost
	if idx := strings.LastIndex(imapHost, ":"); idx > 0 {
		tlsServerName = imapHost[:idx]
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: tlsServerName})
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap tls handshake: %w", err)
	}
	log.Printf("[DEBUG] [IMAP] TLS OK %s", time.Since(start))

	c := imapclient.New(tlsConn, nil)

	// Ждём greeting
	if err := c.WaitGreeting(); err != nil {
		c.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}

	// XOAUTH2
	saslClient := &xoauth2Client{email: email, token: token}
	if err := c.Authenticate(saslClient); err != nil {
		c.Close()
		if isAuthError(err) {
			return nil, &ErrAuthFailed{Msg: err.Error()}
		}
		return nil, fmt.Errorf("imap auth: %w", err)
	}
	log.Printf("[INFO] [IMAP] auth OK %s %s", email, time.Since(start))

	return c, nil
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

// ==========================================
// HELPERS
// ==========================================

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

// injectMsgIDHeader добавляет X-Gwsferry-MsgID в RFC822 заголовки.
func injectMsgIDHeader(raw []byte, msgID string) []byte {
	header := "\r\n" + fmt.Sprintf("X-Gwsferry-MsgID: %s\r\n", msgID)
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx == -1 {
		idx = bytes.Index(raw, []byte("\n\n"))
	}
	if idx == -1 {
		return append(raw, []byte(header+"\r\n")...)
	}
	result := make([]byte, 0, len(raw)+len(header))
	result = append(result, raw[:idx]...)
	result = append(result, []byte(header)...)
	result = append(result, raw[idx:]...)
	return result
}

// FormatNumSet форматирует NumSet для логов.
func formatNumSet(nums []imap.UID) string {
	if len(nums) == 0 {
		return "empty"
	}
	if len(nums) <= 3 {
		s := make([]string, len(nums))
		for i, u := range nums {
			s[i] = strconv.FormatUint(uint64(u), 10)
		}
		return strings.Join(s, ",")
	}
	return fmt.Sprintf("%d items [%d...%d]", len(nums), nums[0], nums[len(nums)-1])
}
