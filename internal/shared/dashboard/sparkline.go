package dashboard

import (
	"strings"
	
	"github.com/charmbracelet/lipgloss"
)

var sparklineChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

func renderSparkline(values []float64, width int, style lipgloss.Style) string {
	if len(values) == 0 {
		return style.Render(strings.Repeat(string(sparklineChars[0]), width))
	}
	data := values
	if len(data) > width {
		data = data[len(data)-width:]
	}
	minVal, maxVal := data[0], data[0]
	for _, v := range data[1:] {
		if v < minVal { minVal = v }
		if v > maxVal { maxVal = v }
	}
	var b strings.Builder
	for _, v := range data {
		var idx int
		if maxVal-minVal > 0 {
			idx = int(((v - minVal) / (maxVal - minVal)) * float64(len(sparklineChars)-1))
		}
		if idx < 0 { idx = 0 }
		if idx >= len(sparklineChars) { idx = len(sparklineChars) - 1 }
		b.WriteRune(sparklineChars[idx])
	}
	return style.Render(b.String())
}
