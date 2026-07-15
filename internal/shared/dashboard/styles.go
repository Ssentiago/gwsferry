package dashboard

import "github.com/charmbracelet/lipgloss"

var (
	colorRed      = lipgloss.Color("1")
	colorGreen    = lipgloss.Color("2")
	colorYellow   = lipgloss.Color("3")
	colorCyan     = lipgloss.Color("6")
	colorWhite    = lipgloss.Color("15")
	colorMagenta  = lipgloss.Color("13")
	colorBlack    = lipgloss.Color("0")
	colorBgBlue   = lipgloss.Color("12")
	colorBgYellow = lipgloss.Color("11")

	styleHeader     = lipgloss.NewStyle().Background(colorBgBlue).Foreground(colorBlack).Bold(true)
	styleModeBanner = lipgloss.NewStyle().Background(colorBgYellow).Foreground(colorBlack).Bold(true)
	styleGreen      = lipgloss.NewStyle().Foreground(colorGreen)
	styleRed        = lipgloss.NewStyle().Foreground(colorRed)
	styleYellow     = lipgloss.NewStyle().Foreground(colorYellow)
	styleCyan       = lipgloss.NewStyle().Foreground(colorCyan)
	styleWhite      = lipgloss.NewStyle().Foreground(colorWhite)
)
