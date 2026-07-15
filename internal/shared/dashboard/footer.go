package dashboard

import (
	"strings"
	
	"github.com/charmbracelet/lipgloss"
)

func (d *Dashboard) buildFooter() string {
	w := d.termWidth
	if w <= 0 { w = 80 }
	keybinds := []struct{ key, desc string }{
		{"1-4", "логи"},
		{"↑↓", "навигация"},
		{"Enter", "детали"},
		{"Esc", "закрыть"},
		{"Ctrl+C", "выход"},
	}
	var parts []string
	for _, kb := range keybinds {
		parts = append(parts, styleCyan.Render(kb.key)+" "+styleWhite.Render(kb.desc))
	}
	footer := strings.Join(parts, "  │  ")
	return lipgloss.NewStyle().Width(w).Foreground(lipgloss.Color("240")).Render(" " + footer)
}
