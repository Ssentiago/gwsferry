package importyandex

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestFlaggedDiagnosis — диагностика: сохраняет ли Yandex \Flagged при APPEND.
// Тестирует три варианта:
// 1. APPEND с \Flagged напрямую
// 2. APPEND без флагов + STORE +FLAGS (\Flagged)
// 3. FETCH после каждого варианта для проверки
func TestFlaggedDiagnosis(t *testing.T) {
	c, email := connectTestUser(t)
	defer Close(c)

	date := time.Date(2026, 7, 15, 10, 0, 0, 0, time.FixedZone("MSK", 3*3600))

	// --- Вариант 1: APPEND с \Flagged ---
	subject1 := "flagged-diag-1-append-with-flag"
	raw1 := []byte(
		"From: test@dinord.ru\r\n" +
			"To: " + email + "\r\n" +
			"Subject: " + subject1 + "\r\n" +
			"Date: Mon, 15 Jul 2026 10:00:00 +0300\r\n" +
			"Message-ID: <" + subject1 + "@dinord.ru>\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"Flagged diagnosis test #1: APPEND with \\Flagged\r\n")

	flags1 := []string{`\Seen`, `\Flagged`}
	t.Logf("[VARIANT 1] APPEND с флагами %v", flags1)
	if err := Append(context.Background(), c, "INBOX", date, raw1, flags1, "flagged-diag-1"); err != nil {
		t.Fatalf("Append variant 1: %v", err)
	}

	// FETCH и проверяем флаги
	msgs1, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range msgs1 {
		if m.Subject == subject1 {
			t.Logf("[VARIANT 1] uid=%d flags=%v", m.UID, m.Flags)
			if containsStr(m.Flags, `\Flagged`) {
				t.Logf("[RESULT] \\Flagged СОХРАНЁН при APPEND — всё работает")
			} else {
				t.Logf("[RESULT] \\Flagged НЕ сохранён при APPEND — серверная особенность")
			}
			// Удаляем
			Delete(context.Background(), c, "INBOX", m.UID)
			break
		}
	}

	// --- Вариант 2: APPEND без флагов + STORE ---
	subject2 := "flagged-diag-2-store-after-append"
	raw2 := []byte(
		"From: test@dinord.ru\r\n" +
			"To: " + email + "\r\n" +
			"Subject: " + subject2 + "\r\n" +
			"Date: Mon, 15 Jul 2026 10:01:00 +0300\r\n" +
			"Message-ID: <" + subject2 + "@dinord.ru>\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"Flagged diagnosis test #2: APPEND then STORE +FLAGS\r\n")

	t.Logf("[VARIANT 2] APPEND без флагов + STORE +FLAGS (\\Flagged)")
	if err := Append(context.Background(), c, "INBOX", date, raw2, []string{`\Seen`}, "flagged-diag-2"); err != nil {
		t.Fatalf("Append variant 2: %v", err)
	}

	// Находим UID только что добавленного письма
	msgs2, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var uid2 uint32
	for _, m := range msgs2 {
		if m.Subject == subject2 {
			uid2 = m.UID
			t.Logf("[VARIANT 2] найдено uid=%d, flags до STORE=%v", uid2, m.Flags)
			break
		}
	}
	if uid2 == 0 {
		t.Fatal("письмо variant 2 не найдено")
	}

	// STORE +FLAGS (\Flagged)
	if err := StoreFlag(c, "INBOX", uid2, `\Flagged`); err != nil {
		t.Fatalf("StoreFlag: %v", err)
	}

	// FETCH после STORE
	msgs2b, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List after store: %v", err)
	}
	for _, m := range msgs2b {
		if m.UID == uid2 {
			t.Logf("[VARIANT 2] uid=%d flags после STORE=%v", uid2, m.Flags)
			if containsStr(m.Flags, `\Flagged`) {
				t.Logf("[RESULT] \\Flagged СОХРАНЁН через STORE — рабочий обходной путь")
			} else {
				t.Logf("[RESULT] \\Flagged НЕ сохранён даже через STORE — серьёзная особенность сервера")
			}
			// Удаляем
			Delete(context.Background(), c, "INBOX", uid2)
			break
		}
	}

	// --- Вариант 3: APPEND с \Flagged через STORE сразу после ---
	subject3 := "flagged-diag-3-append-then-store"
	raw3 := []byte(
		"From: test@dinord.ru\r\n" +
			"To: " + email + "\r\n" +
			"Subject: " + subject3 + "\r\n" +
			"Date: Mon, 15 Jul 2026 10:02:00 +0300\r\n" +
			"Message-ID: <" + subject3 + "@dinord.ru>\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"Flagged diagnosis test #3: APPEND+STORE combo\r\n")

	t.Logf("[VARIANT 3] APPEND с \\Flagged + STORE для надёжности")
	if err := Append(context.Background(), c, "INBOX", date, raw3, []string{`\Seen`, `\Flagged`}, "flagged-diag-3"); err != nil {
		t.Fatalf("Append variant 3: %v", err)
	}

	msgs3, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var uid3 uint32
	for _, m := range msgs3 {
		if m.Subject == subject3 {
			uid3 = m.UID
			break
		}
	}
	if uid3 == 0 {
		t.Fatal("письмо variant 3 не найдено")
	}

	// STORE на случай если APPEND проигнорировал флаг
	if err := StoreFlag(c, "INBOX", uid3, `\Flagged`); err != nil {
		t.Fatalf("StoreFlag: %v", err)
	}

	msgs3b, err := List(context.Background(), c, "INBOX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range msgs3b {
		if m.UID == uid3 {
			t.Logf("[VARIANT 3] uid=%d flags=%v", uid3, m.Flags)
			Delete(context.Background(), c, "INBOX", uid3)
			break
		}
	}

	fmt.Println("\n=== ДИАГНОСТИКА \\Flagged ЗАВЕРШЕНА ===")
	fmt.Println("Если VARIANT 1 не сохранил \\Flagged, а VARIANT 2/3 сохранил через STORE —")
	fmt.Println("это подтверждённая серверная особенность Yandex: APPEND игнорирует \\Flagged.")
	fmt.Println("Решение: после APPEND всегда делать STORE +FLAGS (\\Flagged) для STARRED писем.")
}
