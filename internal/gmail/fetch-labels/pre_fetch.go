package fetchlabels

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pterm/pterm"
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

func preFetchMsgIDs(ctx context.Context, emails []string, validKeys []string, st *store.Store, n int) {
	tmpMsgIdx, err := os.CreateTemp("", "msg_ids_*.json")
	if err != nil {
		log.Fatalf("[ERROR] Создание temp-файла: %v", err)
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

			for e := range fetchEmailCh {
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

	prefetchArea, _ := pterm.DefaultArea.WithRemoveWhenDone().Start()
	fetchDoneCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-fetchDoneCh:
				return
			case <-ticker.C:
				fetchMu.Lock()
				workers := make([]fetchWorkerState, n)
				copy(workers, fetchWorkers)
				doneTotal := fetchDoneTotal
				fetchMu.Unlock()

				pct := 0
				if fetchTotal > 0 {
					pct = doneTotal * 100 / fetchTotal
				}

				cols := []string{"WORKER", "ЗАДАЧА", "СТАТУС"}
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

				header := pterm.DefaultHeader.
					WithBackgroundStyle(pterm.NewStyle(pterm.BgLightBlue)).
					WithTextStyle(pterm.NewStyle(pterm.FgBlack, pterm.Bold)).
					Sprintf("Pre-fetch msg_id [%d%%] %d/%d", pct, doneTotal, fetchTotal)

				prefetchArea.Update(header + "\n\n" + tbl)
			}
		}
	}()

	for done := 0; done < fetchTotal; done++ {
		r := <-fetchResults
		fetchMu.Lock()
		if r.err != nil {
			log.Printf("[WARN] Pre-fetch %s: %v", r.email, r.err)
		} else {
			st.SetMsgIndex(r.email, r.msgIDs)
		}
		fetchMu.Unlock()
	}
	fetchWg.Wait()
	time.Sleep(300 * time.Millisecond)
	close(fetchDoneCh)
	prefetchArea.Stop()
	fmt.Print("\033[2J\033[H")
	os.Stdout.Sync()

	if err := st.SaveMsgIndex(tmpMsgIdxPath); err != nil {
		log.Printf("[WARN] Сохранение msg_index: %v", err)
	}
}
