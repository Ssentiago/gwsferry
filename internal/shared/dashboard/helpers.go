package dashboard

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func workerColorStr(status, text string) string {
	switch {
	case strings.Contains(status, "QUOTA") || strings.Contains(status, "DEAD"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(text)
	case strings.Contains(status, "retry") || strings.Contains(status, "пауза"):
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render(text)
	case status == "IDLE" || status == "подключен":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(text)
	default:
		return text
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func lastN(s []logLine, n int) []logLine {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func naturalLess(a, b string) bool {
	for a != "" || b != "" {
		if a == "" {
			return true
		}
		if b == "" {
			return false
		}
		ai := a[0]
		bi := b[0]
		da := ai >= '0' && ai <= '9'
		db := bi >= '0' && bi <= '9'
		switch {
		case da && !db:
			return true
		case !da && db:
			return false
		case da && db:
			na, restA := readNum(a)
			nb, restB := readNum(b)
			if na != nb {
				return na < nb
			}
			a, b = restA, restB
		default:
			if ai != bi {
				return ai < bi
			}
			a = a[1:]
			b = b[1:]
		}
	}
	return false
}

func readNum(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n := 0
	for _, c := range s[:i] {
		n = n*10 + int(c-'0')
	}
	return n, s[i:]
}
