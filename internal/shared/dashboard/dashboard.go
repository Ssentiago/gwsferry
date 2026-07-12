package dashboard

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/pterm/pterm"
	"gwsferry/internal/shared/timer"
)

// ==========================================
// STATE
// ==========================================

const maxLogLines = 12

type WorkerState struct {
	Task   string
	Status string
	ETA    string
}

type OverallState struct {
	UsersTotal   int
	UsersDone    int
	UsersError   int
	UsersPending int
}

type logLine struct {
	level string
	text  string
}

// ==========================================
// DASHBOARD
// ==========================================

type Dashboard struct {
	area         *pterm.AreaPrinter
	mu           sync.Mutex
	termHeight   int
	termWidth    int
	workerOrder  []string
	workers      map[string]WorkerState
	overall      OverallState
	stuckThreads int
	generalLogs  []logLine
	workerLogs   []logLine
	timer        *timer.Timer
}

func New() *Dashboard {
	return &Dashboard{
		workers: make(map[string]WorkerState),
		timer:   timer.New(),
	}
}

func (d *Dashboard) StartTimer() {
	d.timer.Start()
}

func (d *Dashboard) Start() {
	w, h, _ := pterm.GetTerminalSize()
	if h > 0 {
		d.termHeight = h
	}
	if w > 0 {
		d.termWidth = w
	}
	if d.termHeight <= 0 {
		d.termHeight = 40
	}
	area, _ := pterm.DefaultArea.Start()
	d.area = area
}

func (d *Dashboard) Stop() {
	if d.area != nil {
		d.area.Stop()
	}
}

func (d *Dashboard) ForceRedraw() {
	d.mu.Lock()
	_, h, _ := pterm.GetTerminalSize()
	if h > 0 {
		d.termHeight = h
	}
	d.mu.Unlock()
	d.redraw()
}

func (d *Dashboard) UpdateOverall(fn func(*OverallState)) {
	d.mu.Lock()
	fn(&d.overall)
	d.mu.Unlock()
	d.redraw()
}

func (d *Dashboard) UpdateWorker(key, task, status, eta string) {
	d.mu.Lock()
	if _, ok := d.workers[key]; !ok {
		d.workerOrder = append(d.workerOrder, key)
	}
	if eta == "" {
		eta = "--:--"
	}
	d.workers[key] = WorkerState{Task: task, Status: status, ETA: eta}
	d.mu.Unlock()
	d.redraw()
}

func (d *Dashboard) SetStuckThreads(n int) {
	d.mu.Lock()
	d.stuckThreads = n
	d.mu.Unlock()
	d.redraw()
}

func isWorkerTagged(msg string) bool {
	if !strings.HasPrefix(msg, "[sa") {
		return false
	}
	return strings.Index(msg, "]") > 0
}

func (d *Dashboard) Log(level, msg string) {
	d.mu.Lock()
	line := logLine{level: level, text: msg}
	if isWorkerTagged(msg) {
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
	d.mu.Unlock()

	switch level {
	case "ERROR":
		log.Printf("[ERROR] %s", msg)
	case "WARN":
		log.Printf("[WARN] %s", msg)
	default:
		log.Printf("[INFO] %s", msg)
	}

	d.redraw()
}

func (d *Dashboard) redraw() {
	d.mu.Lock()
	o := d.overall
	workers := make(map[string]WorkerState, len(d.workers))
	for k, v := range d.workers {
		workers[k] = v
	}
	order := make([]string, len(d.workerOrder))
	copy(order, d.workerOrder)
	stuck := d.stuckThreads
	genLogs := make([]logLine, len(d.generalLogs))
	copy(genLogs, d.generalLogs)
	wrkLogs := make([]logLine, len(d.workerLogs))
	copy(wrkLogs, d.workerLogs)
	termH := d.termHeight
	d.mu.Unlock()

	if d.area == nil {
		return
	}

	pct := 0
	if o.UsersTotal > 0 {
		pct = (o.UsersDone + o.UsersError) * 100 / o.UsersTotal
	}

	// === Header ===
	timerStr := d.timer.Render()
	header := pterm.DefaultHeader.
		WithBackgroundStyle(pterm.NewStyle(pterm.BgLightBlue)).
		WithTextStyle(pterm.NewStyle(pterm.FgBlack, pterm.Bold)).
		Sprintf("Progress [%d%%]  |  %s", pct, timerStr)

	// === Summary ===
	summary := fmt.Sprintf(
		"%s | %s | %s | всего %s",
		pterm.Green(fmt.Sprintf("%d done", o.UsersDone)),
		pterm.Red(fmt.Sprintf("%d error", o.UsersError)),
		pterm.Yellow(fmt.Sprintf("%d в очереди", o.UsersPending)),
		pterm.FgLightWhite.Sprintf("%d", o.UsersTotal),
	)
	if stuck > 0 {
		summary += " | " + pterm.Red(fmt.Sprintf("%d зависших", stuck))
	}

	// === Worker Table ===
	cols := []string{"WORKER", "ЗАДАЧА", "СТАТУС", "ОСТАЛОСЬ"}
	var rows [][]string
	sortedKeys := append([]string(nil), order...)
	sort.Slice(sortedKeys, func(i, j int) bool {
		return naturalLess(sortedKeys[i], sortedKeys[j])
	})
	for _, k := range sortedKeys {
		w := workers[k]
		eta := w.ETA
		if eta == "" {
			eta = "--:--"
		}
		rows = append(rows, []string{
			workerColorStr(w.Status, strings.ToUpper(k)),
			truncate(w.Task, 28),
			workerColorStr(w.Status, truncate(w.Status, 24)),
			eta,
		})
	}

	tbl, _ := pterm.DefaultTable.
		WithHasHeader().
		WithBoxed().
		WithHeaderRowSeparator("═").
		WithRowSeparator("─").
		WithSeparator("│").
		WithStyle(pterm.NewStyle(pterm.FgLightWhite)).
		WithHeaderStyle(pterm.NewStyle(pterm.FgLightMagenta, pterm.Bold)).
		WithData(append([][]string{cols}, rows...)).
		Srender()

	// === Log Panels ===
	genLogStr := d.renderLogLines(genLogs)
	wrkLogStr := d.renderLogLines(wrkLogs)

	leftPanel, _ := pterm.DefaultPanel.
		WithPanels([][]pterm.Panel{
			{pterm.Panel{
				Data: pterm.Cyan("Общие события") + "\n" + genLogStr,
			}},
			{pterm.Panel{
				Data: pterm.Cyan("События воркеров") + "\n" + wrkLogStr,
			}},
		}).
		Srender()

	// === Assembly ===
	var out strings.Builder
	out.WriteString(header)
	out.WriteString("\n")
	out.WriteString(pterm.FgLightWhite.Sprint(summary))
	out.WriteString("\n\n")
	out.WriteString(tbl)
	out.WriteString("\n\n")
	out.WriteString(leftPanel)

	// === Padding до фиксированной высоты ===
	// Считаем сколько строк реально занимает контент.
	contentLines := strings.Count(out.String(), "\n") + 1
	// Оставляем место для prompt-строки внизу.
	maxContentLines := termH - 2
	if maxContentLines < 5 {
		maxContentLines = 5
	}

	// Обрезаем если контент длиннее терминала.
	content := out.String()
	if contentLines > maxContentLines {
		lines := strings.Split(content, "\n")
		content = strings.Join(lines[:maxContentLines], "\n")
		contentLines = maxContentLines
	}

	// Дополняем пустыми строками до фиксированной высоты —
	// это УБИРАЕТ мерцание, т.к. высота между кадрами постоянна.
	if contentLines < maxContentLines {
		for i := contentLines; i < maxContentLines; i++ {
			content += "\n"
		}
	}

	d.area.Update(content)
}

func (d *Dashboard) renderLogLines(logs []logLine) string {
	if len(logs) == 0 {
		return pterm.FgLightWhite.Sprint("(логов пока нет)")
	}
	var b strings.Builder
	for _, l := range lastN(logs, maxLogLines) {
		switch l.level {
		case "ERROR":
			b.WriteString(pterm.FgRed.Sprint(l.text))
		case "WARN":
			b.WriteString(pterm.FgYellow.Sprint(l.text))
		default:
			b.WriteString(pterm.FgCyan.Sprint(l.text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ==========================================
// HELPERS
// ==========================================

func workerColorStr(status, text string) string {
	switch {
	case strings.Contains(status, "QUOTA") || strings.Contains(status, "DEAD"):
		return pterm.Red(text)
	case strings.Contains(status, "retry") || strings.Contains(status, "пауза"):
		return pterm.Yellow(text)
	case status == "IDLE" || status == "подключен":
		return pterm.Green(text)
	default:
		return text
	}
}

func lastN(s []logLine, n int) []logLine {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// naturalLess сравнивает строки с учётом числовых блоков:
// sa1 < sa2 < ... < sa9 < sa10 < sa11.
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
