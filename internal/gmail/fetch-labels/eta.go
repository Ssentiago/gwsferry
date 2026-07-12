package fetchlabels

import (
	"fmt"
	"time"
)

type etaPoint struct {
	t         time.Time
	collected int
}

type etaTracker struct {
	points []etaPoint
}

func (e *etaTracker) record(collected int) {
	e.points = append(e.points, etaPoint{t: time.Now(), collected: collected})
	if len(e.points) > etaWindowSize {
		e.points = e.points[1:]
	}
}

func (e *etaTracker) estimateSeconds(remaining int) float64 {
	if len(e.points) < 2 || remaining <= 0 {
		return -1
	}
	first, last := e.points[0], e.points[len(e.points)-1]
	elapsed := last.t.Sub(first.t).Seconds()
	collectedInWindow := last.collected - first.collected
	if elapsed <= 0 || collectedInWindow <= 0 {
		return -1
	}
	speed := float64(collectedInWindow) / elapsed
	return float64(remaining) / speed
}

func formatETA(seconds float64) string {
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
