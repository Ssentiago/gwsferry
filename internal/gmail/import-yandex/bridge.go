package importyandex

import (
	"context"
	"fmt"
	"log"
	"time"
)

// BuildLetters загружает письма юзера из S3 и сопоставляет с лейблами из файла.
// Возвращает Letter-ы для писем с найденными лейблами и warnings для пропущенных.
func BuildLetters(ctx context.Context, s3 S3Reader, email string, labels LabelsFile) ([]Letter, []string, error) {
	start := time.Now()
	log.Printf("[DEBUG] [BRIDGE] %s: начинаю BuildLetters (загрузка из S3 + матчинг лейблов)", email)
	emails, err := s3.ListEmails(ctx, email)
	if err != nil {
		log.Printf("[ERROR] [BRIDGE] %s: ошибка ListEmails за %s: %v", email, time.Since(start), err)
		return nil, nil, fmt.Errorf("list emails for %s: %w", email, err)
	}

	if len(emails) == 0 {
		log.Printf("[INFO] [BRIDGE] %s: 0 emails в S3, пропуск (за %s)", email, time.Since(start))
		return nil, nil, nil
	}

	log.Printf("[DEBUG] [BRIDGE] %s: %d emails загружено из S3 за %s, матчу с лейблами...", email, len(emails), time.Since(start))
	letters, warnings := MatchLetters(email, emails, labels)
	log.Printf("[DEBUG] [BRIDGE] %s: BuildLetters завершён: %d letters, %d warnings (total %s)", email, len(letters), len(warnings), time.Since(start))
	return letters, warnings, nil
}

// BuildAllLetters проходит по списку юзеров, для каждого загружает письма из S3
// и матчит с лейблами. Возвращает map[email]→[]Letter + общий список warnings.
func BuildAllLetters(ctx context.Context, s3 S3Reader, userFiles map[string][]string, labels LabelsFile) (map[string][]Letter, []string, error) {
	start := time.Now()
	log.Printf("[INFO] [BRIDGE] BuildAllLetters: %d юзеров для обработки", len(userFiles))
	result := make(map[string][]Letter, len(userFiles))
	var allWarnings []string

	for email := range userFiles {
		log.Printf("[DEBUG] [BRIDGE] BuildAllLetters: обрабатываю %s", email)
		letters, warnings, err := BuildLetters(ctx, s3, email, labels)
		if err != nil {
			log.Printf("[ERROR] [BRIDGE] BuildAllLetters: ошибка для %s за %s: %v", email, time.Since(start), err)
			return nil, nil, fmt.Errorf("build letters for %s: %w", email, err)
		}
		if len(letters) > 0 {
			result[email] = letters
			log.Printf("[DEBUG] [BRIDGE] BuildAllLetters: %s → %d letters", email, len(letters))
		} else {
			log.Printf("[DEBUG] [BRIDGE] BuildAllLetters: %s → 0 letters (пропуск)", email)
		}
		allWarnings = append(allWarnings, warnings...)
	}

	totalLetters := 0
	for _, l := range result {
		totalLetters += len(l)
	}
	log.Printf("[INFO] [BRIDGE] BuildAllLetters итого: %d писем с лейблами из %d юзеров, %d пропущено (за %s)",
		totalLetters, len(result), len(allWarnings), time.Since(start))

	return result, allWarnings, nil
}
