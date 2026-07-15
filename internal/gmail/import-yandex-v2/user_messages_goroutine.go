package importyandex

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/mail"
	"regexp"
	"time"
)

// LetterTask — email for processing (body downloaded on the spot).
type LetterTask struct {
	Letter Letter
}

// MessageReport — result of processing one email.
type MessageReport struct {
	MsgID string
	Err   error
}

// runMessagesGoroutine — pipeline: download .eml from S3 → parse date → append via IMAP.
func runMessagesGoroutine(
	ctx context.Context,
	s3 S3Reader,
	worker *ImapWorker,
	taskChan <-chan LetterTask,
	msgReportChan chan<- MessageReport,
) {
	for task := range taskChan {
		start := time.Now()
		report := MessageReport{MsgID: task.Letter.MsgID}
		letter := task.Letter

		folder := ResolveFolder(letter.LabelIDs, letter.LabelNames)
		flags := ResolveFlags(letter.LabelIDs)
		log.Printf("[INFO] [MSG] %s: start (path=%s folder=%s flags=%v)", letter.MsgID, letter.Path, folder, flags)

		// 1. Download .eml from S3
		raw, err := s3.GetEmail(ctx, letter.Path)
		if err != nil {
			log.Printf("[ERROR] [MSG] %s: GetEmail FAILED key=%s: %v", letter.MsgID, letter.Path, err)
			report.Err = fmt.Errorf("get email: %w", err)
			msgReportChan <- report
			continue
		}
		log.Printf("[DEBUG] [MSG] %s: S3 OK size=%d %s", letter.MsgID, len(raw), time.Since(start))

		// 2. Parse date
		date := parseDateFromRaw(raw)

		// 3. Append via IMAP
		appendStart := time.Now()
		if err := worker.Append(ctx, letter, date, raw); err != nil {
			log.Printf("[ERROR] [MSG] %s: append FAILED folder=%s: %v (%s)", letter.MsgID, folder, err, time.Since(appendStart))
			report.Err = fmt.Errorf("append: %w", err)
			msgReportChan <- report
			continue
		}

		log.Printf("[INFO] [MSG] %s: OK folder=%s (%s total)", letter.MsgID, folder, time.Since(start))
		msgReportChan <- report
	}
}

// reTimeColon matches time values like "17:7:14" that need zero-padding.
var reTimeColon = regexp.MustCompile(`\b(\d{1,2}):(\d{1,2}):(\d{1,2})\b`)

func normalizeDateHeader(dateStr string) string {
	return reTimeColon.ReplaceAllStringFunc(dateStr, func(m string) string {
		parts := reTimeColon.FindStringSubmatch(m)
		h, mi, s := parts[1], parts[2], parts[3]
		if len(h) == 1 {
			h = "0" + h
		}
		if len(mi) == 1 {
			mi = "0" + mi
		}
		if len(s) == 1 {
			s = "0" + s
		}
		return h + ":" + mi + ":" + s
	})
}

func parseDateFromRaw(raw []byte) time.Time {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err == nil {
		if date, derr := msg.Header.Date(); derr == nil {
			return date
		}
		// Date header есть, но не парсится — пробуем починить и распарсить вручную
		if raw := msg.Header.Get("Date"); raw != "" {
			if t, perr := mail.ParseDate(normalizeDateHeader(raw)); perr == nil {
				log.Printf("[INFO] [MSG] parseDate: fixed malformed date %q → %s", raw, t.Format(time.RFC3339))
				return t
			}
		}
	}
	log.Printf("[WARN] [MSG] parseDate: не удалось распарсить, fallback time.Now()")
	return time.Now()
}
