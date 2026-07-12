package main

import (
	"fmt"
	"os"

	"github.com/AlecAivazis/survey/v2"
	"github.com/pterm/pterm"
	gmailfetch "gwsferry/internal/gmail/fetch-labels"
)

func main() {
	pterm.EnableStyling()
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()

	for {
		action := showMainMenu()
		switch action {
		case "gmail-fetch-labels":
			gmailfetch.Run()
		case "gmail-fetch-bodies":
			pterm.Warning.Println("Fetch message bodies — в разработке.")
		case "gmail-verify":
			pterm.Warning.Println("Verify (Gmail vs S3) — в разработке.")
		case "drive":
			pterm.Warning.Println("Drive — в разработке.")
		case "exit":
			return
		}
		fmt.Println()
	}
}

func showMainMenu() string {
	var mainChoice string
	prompt := &survey.Select{
		Message: "gwsferry — Google Workspace Ferry",
		Options: []string{"Gmail", "Drive", "Выход"},
	}
	survey.AskOne(prompt, &mainChoice)

	switch mainChoice {
	case "Drive":
		return "drive"
	case "Выход":
		return "exit"
	}

	var gmailChoice string
	gmailPrompt := &survey.Select{
		Message: "Gmail",
		Options: []string{
			"Fetch labelIds → JSON",
			"Fetch message bodies (raw) → S3",
			"Verify (Gmail vs S3 reconciliation)",
			"← Назад",
		},
	}
	survey.AskOne(gmailPrompt, &gmailChoice)

	switch gmailChoice {
	case "Fetch labelIds → JSON":
		return "gmail-fetch-labels"
	case "Fetch message bodies (raw) → S3":
		return "gmail-fetch-bodies"
	case "Verify (Gmail vs S3 reconciliation)":
		return "gmail-verify"
	default:
		return "back"
	}
}
