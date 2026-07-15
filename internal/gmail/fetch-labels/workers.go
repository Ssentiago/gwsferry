package fetchlabels

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gwsferry/internal/gmail/fetch-labels/store"
	"gwsferry/internal/gmail/gmailapi"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/etatracker"
	"gwsferry/internal/shared/util"
)

type app struct {
	st       *store.Store
	dash     *dashboard.Dashboard
	shutdown *util.ShutdownFlag
}

// onNetErr — обработка сетевой ошибки с учётом shutdown.
func (a *app) onNetErr(workerKey, email, shortEmail, op string, err error) (shutdown bool) {
	if a.shutdown.IsSet() {
		log.Printf("[DEBUG] [%s] shutdown прервал %s для %s", workerKey, op, shortEmail)
		return true
	}
	a.dash.Log("ERROR", fmt.Sprintf("[%s] %s для %s: %v", workerKey, op, shortEmail, err))
	log.Printf("[ERROR] [WORKER] %s: %s %s: %v", workerKey, op, shortEmail, err)
	a.bumpError()
	return false
}

func (a *app) bumpDone() {
	a.dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersDone++
		if o.UsersPending > 0 {
			o.UsersPending--
		}
	})
}

func (a *app) bumpError() {
	a.dash.UpdateOverall(func(o *dashboard.OverallState) {
		o.UsersError++
		if o.UsersPending > 0 {
			o.UsersPending--
		}
	})
}

func (a *app) worker(ctx context.Context, idx int, saKeyPath string, emailCh chan string, tasksDone <-chan struct{}, consumed *atomic.Int32, requeue chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	workerKey := fmt.Sprintf("sa%d", idx)

	stagger := time.Duration(idx) * workerStartStagger
	if stagger > 0 {
		a.dash.UpdateWorker(workerKey, "IDLE", fmt.Sprintf("старт через %s", stagger), "")
		timer := time.NewTimer(stagger)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-tasksDone:
			a.dash.UpdateWorker(workerKey, "IDLE", "нет задач", "")
			log.Printf("[DEBUG] [WORKER] %s: нет задач, завершаю", workerKey)
			return
		case <-a.shutdown.Done():
			log.Printf("[DEBUG] [WORKER] %s: shutdown до старта", workerKey)
			return
		}
	}
	a.dash.UpdateWorker(workerKey, "IDLE", "подключен", "")
	log.Printf("[INFO] [WORKER] %s: запущен, stagger=%s", workerKey, stagger)

	defer func() {
		a.dash.UpdateWorker(workerKey, "FINISH", "done", "")
		log.Printf("[DEBUG] [WORKER] %s: завершён", workerKey)
	}()

	for {
		if a.shutdown.IsSet() {
			return
		}
		var email string
		select {
		case e, ok := <-emailCh:
			if !ok {
				return
			}
			email = e
		default:
			return
		}
		consumed.Add(1)

		shortEmail := strings.Split(email, "@")[0]

		if a.st.IsUserCollected(email) {
			a.dash.Log("INFO", fmt.Sprintf("[%s] %s: уже собран, пропуск", workerKey, shortEmail))
			log.Printf("[DEBUG] [WORKER] %s: %s уже собран, пропуск", workerKey, shortEmail)
			a.bumpDone()
			continue
		}

		a.dash.UpdateWorker(workerKey, shortEmail, "получение labelIds...", "")
		log.Printf("[INFO] [WORKER] %s: начало обработки %s", workerKey, email)
		svc, err := gmailapi.BuildClient(ctx, saKeyPath, email)
		if err != nil {
			if a.onNetErr(workerKey, email, shortEmail, "BuildClient", err) {
				return
			}
			continue
		}

		msgIDs := a.st.ExpectedMsgIDs(email)
		if len(msgIDs) == 0 {
			a.dash.UpdateWorker(workerKey, shortEmail, "сбор индексов писем...", "")
			listStart := time.Now()
			msgIDs, err = gmailapi.ListAllMessageIDs(ctx, svc, email, func(collected, page int) {
				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("индексация: %d писем, стр. %d", collected, page), "")
			})
			if err != nil {
				if a.onNetErr(workerKey, email, shortEmail, "ListAllMessageIDs", err) {
					return
				}
				continue
			}
			log.Printf("[DEBUG] [WORKER] %s: %s: %d msg_ids (за %s)", workerKey, email, len(msgIDs), time.Since(listStart))
		}

		total := len(msgIDs)
		if total == 0 {
			names, err := gmailapi.FetchLabelNames(ctx, svc, email)
			if err != nil {
				if a.onNetErr(workerKey, email, shortEmail, "FetchLabelNames", err) {
					return
				}
				continue
			}
			a.st.FinalizeUser(email, names)
			a.bumpDone()
			log.Printf("[INFO] [WORKER] %s: %s: 0 писем, финализация labels", workerKey, email)
			continue
		}

		cached := a.st.CachedLabels(email)
		var pending []string
		for _, id := range msgIDs {
			if _, ok := cached[id]; !ok {
				pending = append(pending, id)
			}
		}
		if len(cached) > 0 {
			a.dash.Log("INFO", fmt.Sprintf("[%s] %s: resume, в кэше %d/%d, осталось %d", workerKey, shortEmail, len(cached), total, len(pending)))
		}

		collected := len(cached)
		retryRound := 0
		concurrentRetryRound := 0
		adaptive := newAdaptiveBatchSize()
		eta := etatracker.New(0.3)
		eta.Record(collected)

		a.dash.UpdateWorkerDetail(workerKey, fmt.Sprintf("0/%d", maxRetries), fmt.Sprintf("%d", adaptive.current))

		fatalQuota := false
		var errorLog []string

		log.Printf("[INFO] [WORKER] %s: %s: start batch loop, total=%d, cached=%d, pending=%d", workerKey, shortEmail, total, len(cached), len(pending))

		for len(pending) > 0 && retryRound < maxRetries {
			if a.shutdown.IsSet() {
				break
			}
			var nextPending []string
			roundConcurrentOnly := true
			anyBatches := false

			for i := 0; i < len(pending); {
				if a.shutdown.IsSet() {
					nextPending = append(nextPending, pending[i:]...)
					break
				}
				chunkSize := adaptive.current
				end := i + chunkSize
				if end > len(pending) {
					end = len(pending)
				}
				chunk := pending[i:end]
				i = end
				anyBatches = true

				res, err := gmailapi.FetchLabelsBatch(ctx, svc, chunk, func(msgID string, labelIDs []string) {
					a.st.SaveMsgLabels(email, msgID, labelIDs)
				})
				if err != nil {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ошибка: %v", workerKey, shortEmail, len(chunk), err)
					if dq, ok := asDailyQuota(err); ok {
						leftover := dq.PendingIDs
						nextPending = append(append([]string{}, leftover...), pending[i:]...)
						fatalQuota = true
						break
					}
					if a.shutdown.IsSet() {
						return
					}
					errorLog = append(errorLog, fmt.Sprintf("batch error: %v", err))
					nextPending = append(nextPending, chunk...)
					continue
				}

				collected += res.Written
				if len(res.RetryIDs) > 0 {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ok=%d retry=%d", workerKey, shortEmail, len(chunk), res.Written, len(res.RetryIDs))
					nextPending = append(nextPending, res.RetryIDs...)
					if !res.ConcurrentLimitOnly {
						roundConcurrentOnly = false
					}
					adaptive.shrink()
				} else {
					log.Printf("[DEBUG] [%s] %s: batch(%d) ok=%d", workerKey, shortEmail, len(chunk), res.Written)
					adaptive.reportCleanBatch()
				}

				eta.Record(res.Written)
				remaining := total - collected
				etaStr := etatracker.FormatETA(eta.EstimateSeconds(remaining))
				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("%d/%d", collected, total), etaStr)
				a.dash.UpdateWorkerDetail(workerKey, fmt.Sprintf("%d/%d", retryRound+1, maxRetries), fmt.Sprintf("%d", adaptive.current))

				util.SleepOrShutdown(a.shutdown, interBatchDelay)
			}

			if fatalQuota {
				break
			}
			pending = nextPending
			if len(pending) == 0 || a.shutdown.IsSet() {
				break
			}

			var delay time.Duration
			if anyBatches && roundConcurrentOnly {
				concurrentRetryRound++
				if concurrentRetryRound > concurrentLimitBackoffMaxRnds {
					retryRound++
					delay = rateLimitBackoffBase * time.Duration(1<<(retryRound-1))
				} else {
					delay = concurrentLimitBackoffBase
				}
			} else {
				retryRound++
				if retryRound >= maxRetries {
					break
				}
				delay = rateLimitBackoffBase * time.Duration(1<<(retryRound-1))
			}

			a.dash.Log("WARN", fmt.Sprintf("[%s] %s: %d на retry, пауза %s", workerKey, shortEmail, len(pending), delay))
			if util.SleepOrShutdown(a.shutdown, delay) {
				break
			}
		}

		if fatalQuota {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Суточная квота исчерпана на %s, возвращаем в пул (%d/%d уже собрано)", workerKey, email, collected, total))
			a.dash.UpdateWorker(workerKey, "DAILY QUOTA", "DEAD", "")
			requeue <- email
			return
		}

		if a.shutdown.IsSet() {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Остановка: %s собрано %d/%d, докачает недостающее при следующем запуске", workerKey, shortEmail, collected, total))
			return
		}

		if len(pending) > 0 {
			errorLog = append(errorLog, fmt.Sprintf("%d msg_id не удалось собрать после исчерпания retry", len(pending)))
		}

		if len(errorLog) > 0 {
			for _, line := range errorLog {
				log.Printf("[WARN] [%s] %s: %s", workerKey, email, line)
			}
			a.dash.Log("ERROR", fmt.Sprintf("[%s] %s: завершён с ошибками (%d), юзер НЕ помечен как done", workerKey, shortEmail, len(errorLog)))
			a.bumpError()
			continue
		}

		names, err := gmailapi.FetchLabelNames(ctx, svc, email)
		if err != nil {
			if a.onNetErr(workerKey, email, shortEmail, "FetchLabelNames(final)", err) {
				return
			}
			continue
		}
		a.st.FinalizeUser(email, names)
		a.bumpDone()
		log.Printf("[INFO] [WORKER] %s: %s: labels финализированы, done (%d/%d collected)", workerKey, email, collected, total)
	}
}

func asDailyQuota(err error) (*gmailapi.DailyQuotaExceededError, bool) {
	var dq *gmailapi.DailyQuotaExceededError
	if errors.As(err, &dq) {
		return dq, true
	}
	return nil, false
}
