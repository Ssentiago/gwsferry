package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// buildWorkerDetail рендерит панель деталей выбранного воркера.
func (d *Dashboard) buildWorkerDetail() string {
	if !d.showDetail || d.selectedRow < 0 || d.selectedRow >= len(d.workerOrder) {
		return ""
	}

	w := d.termWidth
	if w <= 0 {
		w = 80
	}

	key := d.workerOrder[d.selectedRow]
	worker := d.workers[key]

	var b strings.Builder
	b.WriteString(styleCyan.Render("▸ "+key))
	b.WriteString("\n")
	b.WriteString("  Задача:      " + styleWhite.Render(worker.Task))
	b.WriteString("\n")
	b.WriteString("  Статус:      " + workerColorStr(worker.Status, worker.Status))
	b.WriteString("\n")
	b.WriteString("  ETA:         " + styleWhite.Render(worker.ETA))
	if worker.RetryRound != "" {
		b.WriteString("\n")
		b.WriteString("  Retry round: " + styleYellow.Render(worker.RetryRound))
	}
	if worker.BatchSize != "" {
		b.WriteString("\n")
		b.WriteString("  Batch size:  " + styleWhite.Render(worker.BatchSize))
	}

	// Последние 10 логов этого воркера
	workerLogs := workerLogsByKey(d.workerLogs, key)
	if len(workerLogs) > 0 {
		b.WriteString("\n")
		b.WriteString("  " + styleCyan.Render("Логи:"))
		shown := lastN(workerLogs, 10)
		for _, l := range shown {
			b.WriteString("\n  ")
			switch l.level {
			case "ERROR":
				b.WriteString(styleRed.Render(l.text))
			case "WARN":
				b.WriteString(styleYellow.Render(l.text))
			default:
				b.WriteString(styleCyan.Render(l.text))
			}
		}
	}

	_ = fmt.Sprintf // avoid unused import

	panel := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(colorCyan).
		Padding(0, 1).
		Width(w - 4).
		Render(b.String())
	return panel
}

// buildSummaryFooter рендерит unified footer с метриками.
func (d *Dashboard) buildSummaryFooter() string {
	w := d.termWidth
	if w <= 0 {
		w = 80
	}

	o := d.overall
	parts := []string{
		"elapsed " + styleWhite.Render(d.timer.Render()),
	}

	if len(d.throughputHistory) > 0 {
		last := d.throughputHistory[len(d.throughputHistory)-1]
		parts = append(parts, "rate: "+styleCyan.Render(fmt.Sprintf("%.1f/s", last)))
	}

	if o.UsersTotal > 0 {
		parts = append(parts, "errors: "+styleRed.Render(fmt.Sprintf("%d", o.UsersError)))
	}

	footer := strings.Join(parts, "  │  ")
	return lipgloss.NewStyle().
		Width(w).
		Foreground(lipgloss.Color("240")).
		Render(" " + footer)
}
