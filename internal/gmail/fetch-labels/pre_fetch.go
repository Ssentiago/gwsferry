package fetchlabels

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ltable "github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/lipgloss"
	"gwsferry/internal/gmail/gmailapi"
	"gwsferry/internal/gmail/fetch-labels/store"
)

type fetchResult struct {
	email  string
	msgIDs []string
	err    error
}

type fetchWorkerState struct {
	task      string
	done      int
	errors    int
	collected int
	page      int
}

// === Pre-fetch Bubble Tea model ===

type preFetchTickMsg struct{}

var (
	pfColorWhite   = lipgloss.Color("15")
	pfColorMagenta = lipgloss.Color("13")
	pfColorBgBlue  = lipgloss.Color("12")
	pfColorBlack   = lipgloss.Color("0")
	pfColor240     = lipgloss.Color("240")
	pfStyleHeader  = lipgloss.NewStyle().Background(pfColorBgBlue).Foreground(pfColorBlack).Bold(true)
)

type preFetchModel struct {
	fetchMu    *sync.Mutex
	workers    []fetchWorkerState
	doneTotal  *int
	fetchTotal int
	done       chan struct{}
}

func (m preFetchModel) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		m.tickCmd(),
	)
}

func (m preFetchModel) tickCmd() tea.Cmd {
	return tea.Every(200*time.Millisecond, func(t time.Time) tea.Msg {
		return preFetchTickMsg{}
	})
}

func (m preFetchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case preFetchTickMsg:
		select {
		case <-m.done:
			return m, tea.Quit
		default:
		}
		return m, m.tickCmd()
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m preFetchModel) View() string {
	m.fetchMu.Lock()
	workers := make([]fetchWorkerState, len(m.workers))
	copy(workers, m.workers)
	doneTotal := *m.doneTotal
	m.fetchMu.Unlock()

	pct := 0
	if m.fetchTotal > 0 {
		pct = doneTotal * 100 / m.fetchTotal
	}

	header := pfStyleHeader.Render(fmt.Sprintf("Pre-fetch msg_id [%d%%] %d/%d", pct, doneTotal, m.fetchTotal))

	var rows [][]string
	for i, w := range workers {
		task := "—"
		if w.task != "" && w.task != "done" {
			task = w.task
		}
		status := "—"
		if w.page > 0 {
			status = fmt.Sprintf("%d msg_id, стр. %d", w.collected, w.page)
		}
		rows = append(rows, []string{
			fmt.Sprintf("sa%d", i),
			task,
			status,
		})
	}

	t := ltable.New().
		Border(lipgloss.NormalBorder()).
		BorderHeader(true).
		BorderColumn(true).
		BorderRow(true).
		BorderStyle(lipgloss.NewStyle().Foreground(pfColor240)).
		Headers("WORKER", "ЗАДАЧА", "СТАТУС").
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == ltable.HeaderRow {
				return lipgloss.NewStyle().Foreground(pfColorMagenta).Bold(true).Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1).Foreground(pfColorWhite)
		})

	for _, row := range rows {
		t.Row(row...)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, "", t.Render())
}

// === Main function ===

func preFetchMsgIDs(ctx context.Context, emails []string, validKeys []string, st *store.Store, n int) {
	start := time.Now()
	log.Printf("[INFO] [PRE-FETCH] старт: %d юзеров, %d воркеров", len(emails), n)

	tmpMsgIdx, err := os.CreateTemp("", "msg_ids_*.json")
	if err != nil {
		log.Fatalf("[ERROR] [PRE-FETCH] Создание temp-файла: %v", err)
	}
	tmpMsgIdxPath := tmpMsgIdx.Name()
	tmpMsgIdx.Close()
	os.Remove(tmpMsgIdxPath)

	fetchResults := make(chan fetchResult, len(emails))
	fetchTotal := len(emails)

	fetchWorkers := make([]fetchWorkerState, n)
	fetchDoneTotal := 0
	fetchMu := sync.Mutex{}

	fetchEmailCh := make(chan string, len(emails))
	for _, e := range emails {
		fetchEmailCh <- e
	}
	close(fetchEmailCh)

	var fetchWg sync.WaitGroup
	for i := 0; i < n; i++ {
		fetchWg.Add(1)
		go func(idx int) {
			defer fetchWg.Done()
			log.Printf("[DEBUG] [PRE-FETCH] воркер %d запущен", idx)

			for e := range fetchEmailCh {
				if ctx.Err() != nil {
					return
				}
				shortEmail := strings.Split(e, "@")[0]
				fetchMu.Lock()
				fetchWorkers[idx].task = shortEmail
				fetchWorkers[idx].collected = 0
				fetchWorkers[idx].page = 0
				fetchMu.Unlock()

				svc, err := gmailapi.BuildClient(ctx, validKeys[0], e)
				if err != nil {
					fetchMu.Lock()
					fetchWorkers[idx].errors++
					fetchDoneTotal++
					fetchMu.Unlock()
					fetchResults <- fetchResult{email: e, err: err}
					continue
				}
				ids, err := gmailapi.ListAllMessageIDs(ctx, svc, e, func(collected, page int) {
					fetchMu.Lock()
					fetchWorkers[idx].collected = collected
					fetchWorkers[idx].page = page
					fetchMu.Unlock()
				})
				fetchMu.Lock()
				fetchWorkers[idx].done++
				fetchDoneTotal++
				fetchMu.Unlock()
				fetchResults <- fetchResult{email: e, msgIDs: ids, err: err}
			}

			fetchMu.Lock()
			fetchWorkers[idx].task = "done"
			fetchMu.Unlock()
		}(i)
	}

	// Bubble Tea program
	done := make(chan struct{})
	m := preFetchModel{
		fetchMu:    &fetchMu,
		workers:    fetchWorkers,
		doneTotal:  &fetchDoneTotal,
		fetchTotal: fetchTotal,
		done:       done,
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	go p.Run()

	// Collect results
	for doneCount := 0; doneCount < fetchTotal; doneCount++ {
		r := <-fetchResults
		fetchMu.Lock()
		if r.err != nil {
			log.Printf("[WARN] [PRE-FETCH] %s: %v", r.email, r.err)
		} else {
			st.SetMsgIndex(r.email, r.msgIDs)
			log.Printf("[DEBUG] [PRE-FETCH] %s: %d msg_ids", r.email, len(r.msgIDs))
		}
		fetchMu.Unlock()
	}
	fetchWg.Wait()
	time.Sleep(300 * time.Millisecond)
	close(done)
	p.Wait()

	log.Printf("[INFO] [PRE-FETCH] завершён: %d/%d (за %s)", fetchDoneTotal, fetchTotal, time.Since(start))

	if err := st.SaveMsgIndex(tmpMsgIdxPath); err != nil {
		log.Printf("[WARN] [PRE-FETCH] сохранение msg_index: %v", err)
	}
}
