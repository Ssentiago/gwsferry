// Package etatracker предоставляет трекер ETA на основе
// экспоненциального скользящего среднего (EMA).
//
// EMA лучше простого скользящего среднего тем, что:
// - Дешевле по памяти (одно число vs массив точек)
// - Больше вес свежих данных, меньше — старых
// - Плавно реагирует на изменение скорости без рывков
package etatracker

import (
	"fmt"
	"sync"
	"time"
)

// Tracker трекает скорость обработки и оценивает ETA.
// Потокобезопасен.
type Tracker struct {
	mu       sync.Mutex
	alpha    float64
	lastRate float64
	lastTime time.Time
	init     bool
}

// New создаёт трекер с указанным alpha.
// alpha=0.3 — стандартное значение.
func New(alpha float64) *Tracker {
	return &Tracker{alpha: alpha}
}

// Record фиксирует количество обработанных записей.
func (t *Tracker) Record(processed int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if !t.init {
		t.lastRate = 0
		t.lastTime = now
		t.init = true
		return
	}

	elapsed := now.Sub(t.lastTime).Seconds()
	if elapsed <= 0 {
		return
	}

	instantRate := float64(processed) / elapsed
	t.lastRate = t.alpha*instantRate + (1-t.alpha)*t.lastRate
	t.lastTime = now
}

// EstimateSeconds возвращает оценку ETA в секундах. -1 если данных недостаточно.
func (t *Tracker) EstimateSeconds(remaining int) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.init || remaining <= 0 || t.lastRate <= 0 {
		return -1
	}
	return float64(remaining) / t.lastRate
}

// Rate возвращает текущую скорость (записей/сек).
func (t *Tracker) Rate() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastRate
}

// Reset сбрасывает трекер.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastRate = 0
	t.init = false
}

// FormatETA форматирует секунды в MM:SS или H:MM:SS.
func FormatETA(seconds float64) string {
	if seconds < 0 {
		return "--:--"
	}
	s := int(seconds)
	if s < 3600 {
		return fmt.Sprintf("%02d:%02d", s/60, s%60)
	}
	h := s / 3600
	m := (s % 3600) / 60
	return fmt.Sprintf("%d:%02d:00", h, m)
}
