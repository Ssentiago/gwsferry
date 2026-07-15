package dashboard

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ltable "github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
	"gwsferry/internal/shared/timer"
)

// === Msg Types ===

type MsgUpdateWorker struct {
	Key    string
	Task   string
	Status string
	ETA    string
}

type MsgUpdateOverall struct {
	Mutate func(*OverallState)
}

type MsgSetStuckThreads struct{ N int }
type MsgSetModeInfo struct{ Info string }
type MsgLog struct{ Level, Msg string }
type MsgFlush struct{}
type tickMsg time.Time
type MsgUpdateWorkerDetail struct {
	Key        string
	RetryRound string
	BatchSize  string
}
type MsgSetWorkerDeadQuota struct {
	Key  string
	Dead bool
}

// === Dashboard ===

type Dashboard struct {
	program      *tea.Program
	workers      map[string]WorkerState
	workerOrder  []string
	overall      OverallState
	stuckThreads int
	generalLogs  []logLine
	workerLogs   []logLine
	timer        *timer.Timer
	modeInfo     string
	termWidth    int
	termHeight   int
	cachedTable  string
	quitCh       chan struct{}

	// UI improvements
	logFilter        string   // "ALL", "INFO", "WARN", "ERROR"
	selectedRow      int      // -1 = none
	showDetail       bool
	throughputHistory []float64
	lastDone         int
}

func New() *Dashboard {
	return &Dashboard{
		workers:     make(map[string]WorkerState),
		timer:       timer.New(),
		quitCh:      make(chan struct{}),
		logFilter:   "ALL",
		selectedRow: -1,
	}
}

// QuitCh returns a channel that is closed when the user presses Ctrl+C.
func (d *Dashboard) QuitCh() <-chan struct{} {
	return d.quitCh
}

// === Public API (same signatures as old Dashboard) ===

func (d *Dashboard) StartTimer() {
	d.timer.Start()
}

func (d *Dashboard) Start() {
	p := tea.NewProgram(d, tea.WithAltScreen(), tea.WithoutSignalHandler())
	d.program = p
	go p.Run()
}

func (d *Dashboard) send(msg tea.Msg) {
	if d.program == nil {
		return
	}
	select {
	case <-d.quitCh:
		return
	default:
	}
	go func() {
		defer func() { recover() }()
		d.program.Send(msg)
	}()
}

func (d *Dashboard) Stop() {
	if d.program != nil {
		p := d.program
		d.program = nil
		p.Quit()
		p.Wait()
	}
}

func (d *Dashboard) UpdateWorker(key, task, status, eta string) {
	d.send(MsgUpdateWorker{Key: key, Task: task, Status: status, ETA: eta})
}

func (d *Dashboard) UpdateWorkerDetail(key, retryRound, batchSize string) {
	d.send(MsgUpdateWorkerDetail{Key: key, RetryRound: retryRound, BatchSize: batchSize})
}

func (d *Dashboard) SetWorkerDeadQuota(key string, dead bool) {
	d.send(MsgSetWorkerDeadQuota{Key: key, Dead: dead})
}

func (d *Dashboard) UpdateOverall(fn func(*OverallState)) {
	d.send(MsgUpdateOverall{Mutate: fn})
}

func (d *Dashboard) SetStuckThreads(n int) {
	d.send(MsgSetStuckThreads{N: n})
}

func (d *Dashboard) SetModeInfo(info string) {
	d.send(MsgSetModeInfo{Info: info})
}

func (d *Dashboard) Log(level, msg string) {
	switch level {
	case "ERROR":
		log.Printf("[ERROR] %s", msg)
	case "WARN":
		log.Printf("[WARN] %s", msg)
	default:
		log.Printf("[INFO] %s", msg)
	}
	d.send(MsgLog{Level: level, Msg: msg})
}

func (d *Dashboard) Flush() {
	d.send(MsgFlush{})
}

func (d *Dashboard) ForceRedraw() {
	// With Bubble Tea, SIGWINCH is handled via ForwardWindowSize().
	// Kept for API compatibility.
}

func (d *Dashboard) ForwardWindowSize() {
	if d.program == nil {
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	d.send(tea.WindowSizeMsg{Width: w, Height: h})
}

// === tea.Model ===

func (d *Dashboard) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		d.timerTickCmd(),
	)
}

func (d *Dashboard) timerTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (d *Dashboard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			select {
			case <-d.quitCh:
			default:
				close(d.quitCh)
			}
			return d, tea.Quit
		}

		// Navigation
		switch msg.String() {
		case "up", "k":
			if d.selectedRow > 0 {
				d.selectedRow--
			}
		case "down", "j":
			if d.selectedRow < len(d.workerOrder)-1 {
				d.selectedRow++
			}
		case "enter":
			if d.selectedRow >= 0 {
				d.showDetail = !d.showDetail
			}
		case "esc":
			d.showDetail = false
			d.selectedRow = -1
		// Log filter hotkeys
		case "1":
			d.logFilter = "ALL"
		case "2":
			d.logFilter = "INFO"
		case "3":
			d.logFilter = "WARN"
		case "4":
			d.logFilter = "ERROR"
		}

	case tea.WindowSizeMsg:
		d.termWidth = msg.Width
		d.termHeight = msg.Height

	case tickMsg:
		// Track throughput for sparkline
		currentDone := d.overall.UsersDone + d.overall.UsersError
		speed := float64(currentDone - d.lastDone)
		d.lastDone = currentDone
		d.throughputHistory = append(d.throughputHistory, speed)
		if len(d.throughputHistory) > 60 {
			d.throughputHistory = d.throughputHistory[1:]
		}
		return d, d.timerTickCmd()

	case MsgUpdateWorker:
		if _, ok := d.workers[msg.Key]; !ok {
			d.workerOrder = append(d.workerOrder, msg.Key)
		}
		eta := msg.ETA
		if eta == "" {
			eta = "--:--"
		}
		d.workers[msg.Key] = WorkerState{Task: msg.Task, Status: msg.Status, ETA: eta}
		d.rebuildTable()

	case MsgUpdateWorkerDetail:
		if w, ok := d.workers[msg.Key]; ok {
			if msg.RetryRound != "" {
				w.RetryRound = msg.RetryRound
			}
			if msg.BatchSize != "" {
				w.BatchSize = msg.BatchSize
			}
			d.workers[msg.Key] = w
		}

	case MsgSetWorkerDeadQuota:
		if w, ok := d.workers[msg.Key]; ok {
			w.DeadQuota = msg.Dead
			d.workers[msg.Key] = w
			d.rebuildTable()
		}

	case MsgUpdateOverall:
		msg.Mutate(&d.overall)

	case MsgSetStuckThreads:
		d.stuckThreads = msg.N

	case MsgSetModeInfo:
		d.modeInfo = msg.Info

	case MsgLog:
		line := logLine{level: msg.Level, text: msg.Msg, worker: extractWorkerKey(msg.Msg)}
		if isWorkerTagged(msg.Msg) {
			d.workerLogs = append(d.workerLogs, line)
			if len(d.workerLogs) > maxLogLines {
				d.workerLogs = d.workerLogs[len(d.workerLogs)-maxLogLines:]
			}
		} else {
			d.generalLogs = append(d.generalLogs, line)
			if len(d.generalLogs) > maxLogLines {
				d.generalLogs = d.generalLogs[len(d.generalLogs)-maxLogLines:]
			}
		}

	case MsgFlush:
		// No-op: View() will be called automatically.
	}
	return d, nil
}

func (d *Dashboard) rebuildTable() {
	sortedKeys := append([]string(nil), d.workerOrder...)
	sort.Slice(sortedKeys, func(i, j int) bool {
		return naturalLess(sortedKeys[i], sortedKeys[j])
	})

	statuses := make([]string, 0, len(sortedKeys))
	deadQuotas := make(map[string]bool)
	var rows [][]string
	for _, k := range sortedKeys {
		w := d.workers[k]
		eta := w.ETA
		if eta == "" {
			eta = "--:--"
		}
		batch := w.BatchSize
		if batch == "" {
			batch = "—"
		}
		quotaInd := "  "
		if w.DeadQuota {
			quotaInd = "‼ "
			deadQuotas[k] = true
		}
		rows = append(rows, []string{quotaInd + k, truncate(w.Task, 28), truncate(w.Status, 24), eta, batch})
		statuses = append(statuses, w.Status)
	}

	t := ltable.New().
		Border(lipgloss.NormalBorder()).
		BorderHeader(true).
		BorderColumn(true).
		BorderRow(true).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("240"))).
		Headers("WORKER", "ЗАДАЧА", "СТАТУС", "ОСТАЛОСЬ", "BATCH").
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == ltable.HeaderRow {
				return lipgloss.NewStyle().
					Foreground(colorMagenta).
					Bold(true).
					Padding(0, 1)
			}
			s := lipgloss.NewStyle().Padding(0, 1).Foreground(colorWhite)
			if row < len(statuses) {
				status := statuses[row]
				workerKey := sortedKeys[row]
				// Column 0 (WORKER) — color by status
				if col == 0 || col == 2 {
					switch {
					case deadQuotas[workerKey]:
						s = s.Foreground(colorRed).Bold(true)
					case strings.Contains(status, "QUOTA") || strings.Contains(status, "DEAD"):
						s = s.Foreground(colorRed)
					case strings.Contains(status, "retry") || strings.Contains(status, "пауза"):
						s = s.Foreground(colorYellow)
					case status == "IDLE" || status == "подключен":
						s = s.Foreground(colorGreen)
					}
				}
				// Column 4 (BATCH) — cyan
				if col == 4 {
					s = s.Foreground(colorCyan)
				}
			}
			return s
		})

	for _, row := range rows {
		t.Row(row...)
	}

	d.cachedTable = t.Render()
}

func (d *Dashboard) View() string {
	if d.program == nil {
		return ""
	}

	w := d.termWidth
	if w <= 0 {
		w = 80
	}

	// 1. Header with progress bar + sparkline + memory
	pct := 0.0
	if d.overall.UsersTotal > 0 {
		pct = float64(d.overall.UsersDone+d.overall.UsersError) / float64(d.overall.UsersTotal)
	}
	pctInt := int(pct * 100)
	sparkline := renderSparkline(d.throughputHistory, 20, styleCyan)
	memStr := ""
	if d.overall.MemoryMB > 0 {
		memStyle := styleWhite
		if d.overall.MemoryLimit > 0 && d.overall.MemoryMB > float64(d.overall.MemoryLimit)*8/10 {
			memStyle = styleYellow
		}
		if d.overall.MemoryLimit > 0 && d.overall.MemoryMB > float64(d.overall.MemoryLimit)*95/100 {
			memStyle = styleRed
		}
		memStr = "  " + memStyle.Render(fmt.Sprintf("MEM %.0f/%dMB", d.overall.MemoryMB, d.overall.MemoryLimit))
	}
	header := styleHeader.Width(w).Render(
		fmt.Sprintf(" Progress [%d%%]  %s%s  |  %s ", pctInt, sparkline, memStr, d.timer.Render()),
	)

	// 2. Mode banner
	modeBanner := ""
	if d.modeInfo != "" {
		modeBanner = styleModeBanner.Render("  " + d.modeInfo + "  ")
	}

	// 3. Summary
	summary := d.buildSummary()

	// 4. Worker table
	tbl := d.cachedTable

	// 5. Worker detail panel (if selected)
	detail := d.buildWorkerDetail()

	// 6. Log panels (with filter)
	leftPanel := d.buildLogPanel("Общие события", d.generalLogs)
	rightPanel := d.buildLogPanel("События воркеров", d.workerLogs)
	logs := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)

	// 7. Assembly
	parts := []string{header}
	if modeBanner != "" {
		parts = append(parts, modeBanner)
	}
	parts = append(parts, summary, "", tbl)
	if detail != "" {
		parts = append(parts, "", detail)
	}
	parts = append(parts, "", logs)
	parts = append(parts, d.buildSummaryFooter(), d.buildFooter())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (d *Dashboard) buildSummary() string {
	o := d.overall
	parts := []string{
		styleGreen.Render(fmt.Sprintf("%d done", o.UsersDone)),
		" | ",
		styleRed.Render(fmt.Sprintf("%d error", o.UsersError)),
		" | ",
		styleYellow.Render(fmt.Sprintf("%d в очереди", o.UsersPending)),
		" | всего ",
		styleWhite.Render(fmt.Sprintf("%d", o.UsersTotal)),
	}
	if d.stuckThreads > 0 {
		parts = append(parts, " | ", styleRed.Render(fmt.Sprintf("%d зависших", d.stuckThreads)))
	}
	return strings.Join(parts, "")
}
