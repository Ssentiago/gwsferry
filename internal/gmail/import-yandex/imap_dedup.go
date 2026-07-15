package importyandex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	imapclient "github.com/emersion/go-imap/v2/imapclient"
)

// fetchExistingMsgIDs делает UID FETCH для извлечения X-Gwsferry-MsgID.
// v2: context.Context поддерживается нативно, таймаут работает корректно.
func fetchExistingMsgIDs(ctx context.Context, c *imapclient.Client, folder string) (map[string]bool, error) {
	start := time.Now()
	log.Printf("[DEBUG] [IMAP-DEDUP] fetchExistingMsgIDs: folder=%s", folder)

	ctx, cancel := context.WithTimeout(ctx, ImapTimeout)
	defer cancel()

	// Select
	state, err := c.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		log.Printf("[ERROR] [IMAP-DEDUP] select %s failed: %v", folder, err)
		return nil, fmt.Errorf("select %s: %w", folder, err)
	}
	if state.NumMessages == 0 {
		log.Printf("[DEBUG] [IMAP-DEDUP] folder=%s пуст (0 сообщений)", folder)
		return make(map[string]bool), nil
	}
	log.Printf("[DEBUG] [IMAP-DEDUP] folder=%s has %d messages", folder, state.NumMessages)

	// Fetch BODY[HEADER (X-Gwsferry-MsgID)] для всех сообщений
	numSet := imap.SeqSetNum(1, state.NumMessages)
	fetchCmd := c.Fetch(numSet, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{
			{
				Specifier:    imap.PartSpecifierHeader,
				HeaderFields: []string{"X-Gwsferry-MsgID"},
			},
		},
	})

	existing := make(map[string]bool)
	for {
		msg := fetchCmd.Next()
		if msg == nil {
			break
		}

		for {
			item := msg.Next()
			if item == nil {
				break
			}
			bodySection, ok := item.(imapclient.FetchItemDataBodySection)
			if ok && bodySection.Literal != nil {
				raw, _ := io.ReadAll(bodySection.Literal)
				if id := extractMsgIDFromHeader(raw); id != "" {
					existing[id] = true
				}
			}
		}
	}

	if err := fetchCmd.Close(); err != nil {
		log.Printf("[WARN] [IMAP-DEDUP] fetch close error: %v", err)
	}

	log.Printf("[INFO] [IMAP-DEDUP] folder=%s: найдено %d существующих msgID (за %s)",
		folder, len(existing), time.Since(start))
	return existing, nil
}

// extractMsgIDFromHeader извлекает значение X-Gwsferry-MsgID из raw заголовков.
func extractMsgIDFromHeader(raw []byte) string {
	needle := []byte("x-gwsferry-msgid: ")
	lower := bytes.ToLower(raw)
	idx := bytes.Index(lower, needle)
	if idx == -1 {
		return ""
	}
	start := idx + len(needle)
	end := start
	for end < len(raw) && raw[end] != '\r' && raw[end] != '\n' {
		end++
	}
	return strings.TrimSpace(string(raw[start:end]))
}
