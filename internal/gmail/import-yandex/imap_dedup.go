package importyandex

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// fetchExistingMsgIDs делает один UID FETCH 1:* для извлечения X-Gwsferry-MsgID
// из всех сообщений папки. Возвращает map[msgID]bool — какие msgID уже на сервере.
// Стоимость: один round-trip на папку, объём = только заголовок (несколько байт × N писем).
func fetchExistingMsgIDs(ctx context.Context, c *client.Client, folder string) (map[string]bool, error) {
	start := time.Now()
	log.Printf("[DEBUG] [IMAP-DEDUP] fetchExistingMsgIDs: folder=%s", folder)

	state, err := c.Select(folder, true)
	if err != nil {
		log.Printf("[ERROR] [IMAP-DEDUP] select %s failed: %v", folder, err)
		return nil, err
	}
	if state.Messages == 0 {
		log.Printf("[DEBUG] [IMAP-DEDUP] folder=%s пуст (0 сообщений)", folder)
		return map[string]bool{}, nil
	}
	log.Printf("[DEBUG] [IMAP-DEDUP] folder=%s has %d messages", folder, state.Messages)

	seqSet := new(imap.SeqSet)
	seqSet.Add("1:*")

	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    []string{"X-Gwsferry-MsgID"},
		},
		Peek: true,
	}

	messages := make(chan *imap.Message, 100)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqSet, []imap.FetchItem{section.FetchItem()}, messages)
	}()

	existing := make(map[string]bool)
	for msg := range messages {
		for _, literal := range msg.Body {
			raw, _ := io.ReadAll(literal)
			if id := extractMsgIDFromHeader(raw); id != "" {
				existing[id] = true
			}
		}
	}
	if err := <-done; err != nil {
		log.Printf("[ERROR] [IMAP-DEDUP] UidFetch failed в %s: %v", folder, err)
		return nil, err
	}

	log.Printf("[INFO] [IMAP-DEDUP] folder=%s: найдено %d существующих msgID (за %s)",
		folder, len(existing), time.Since(start))
	return existing, nil
}

// extractMsgIDFromHeader извлекает значение X-Gwsferry-MsgID из raw заголовков.
func extractMsgIDFromHeader(raw []byte) string {
	needle := []byte("X-Gwsferry-MsgID: ")
	idx := bytes.Index(raw, needle)
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
