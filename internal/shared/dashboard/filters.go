package dashboard

import (
	"strings"
	
	"github.com/charmbracelet/lipgloss"
)

func filterLogsByLevel(logs []logLine, level string) []logLine {
	if level == "" || level == "ALL" {
		return logs
	}
	var filtered []logLine
	for _, l := range logs {
		if strings.EqualFold(l.level, level) {
			filtered = append(filtered, l)
		}
	}
	return filtered
}

func (d *Dashboard) buildLogPanel(title string, logs []logLine) string {
	w := d.termWidth
	if w <= 0 { w = 80 }
	panelWidth := w/2 - 1
	filtered := filterLogsByLevel(logs, d.logFilter)
	var b strings.Builder
	b.WriteString(styleCyan.Render(title))
	if d.logFilter != "" && d.logFilter != "ALL" {
		b.WriteString(" ")
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("[" + d.logFilter + "]"))
	}
	b.WriteString("\n")
	if len(filtered) == 0 {
		if len(logs) > 0 {
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("(нет логов по фильтру)"))
		} else {
			b.WriteString(styleWhite.Render("(логов пока нет)"))
		}
	} else {
		for _, l := range lastN(filtered, maxLogLines) {
			switch l.level {
			case "ERROR":
				b.WriteString(styleRed.Render(l.text))
			case "WARN":
				b.WriteString(styleYellow.Render(l.text))
			default:
				b.WriteString(styleCyan.Render(l.text))
			}
			b.WriteString("\n")
		}
	}
	return lipgloss.NewStyle().Width(panelWidth).Render(b.String())
}
