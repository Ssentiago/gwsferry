package importyandex

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// fetchExistingMsgIDs делает один UID FETCH 1:* для извлечения X-Gwsferry-MsgID.
// При таймауте закрывает соединение для принудительного завершения goroutine.
func fetchExistingMsgIDs(ctx context.Context, c *client.Client, folder string) (map[string]bool, error) {
	start := time.Now()
	log.Printf("[DEBUG] [IMAP-DEDUP] fetchExistingMsgIDs: folder=%s", folder)

	ctx, cancel := context.WithTimeout(ctx, ImapTimeout)
	defer cancel()

	type result struct {
		existing map[string]bool
		err      error
	}
	ch := make(chan result, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[ERROR] [IMAP-DEDUP] panic: %v", r)
				ch <- result{nil, fmt.Errorf("panic: %v", r)}
			}
		}()

		state, err := c.Select(folder, true)
		if err != nil {
			log.Printf("[ERROR] [IMAP-DEDUP] select %s failed: %v", folder, err)
			ch <- result{nil, err}
			return
		}
		if state.Messages == 0 {
			log.Printf("[DEBUG] [IMAP-DEDUP] folder=%s пуст (0 сообщений)", folder)
			ch <- result{make(map[string]bool), nil}
			return
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
		fetchDone := make(chan error, 1)
		go func() {
			fetchDone <- c.UidFetch(seqSet, []imap.FetchItem{section.FetchItem()}, messages)
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
		ch <- result{existing, <-fetchDone}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			log.Printf("[ERROR] [IMAP-DEDUP] fetchExistingMsgIDs failed для %s: %v (за %s)", folder, res.err, time.Since(start))
			return nil, res.err
		}
		log.Printf("[INFO] [IMAP-DEDUP] folder=%s: найдено %d существующих msgID (за %s)",
			folder, len(res.existing), time.Since(start))
		return res.existing, nil
	case <-ctx.Done():
		// Таймаут — закрываем соединение чтобы goroutine завершилась
		log.Printf("[WARN] [IMAP-DEDUP] таймаут для %s, закрываю соединение", folder)
		c.Close()
		return nil, &ErrOperationTimeout{Op: "fetch dedup " + folder, Timeout: ImapTimeout}
	}
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
