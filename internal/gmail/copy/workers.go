package copy

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gwsferry/internal/shared/config"
	"gwsferry/internal/shared/dashboard"
	"gwsferry/internal/shared/util"
)

type app struct {
	st       *migrationState
	dash     *dashboard.Dashboard
	shutdown *util.ShutdownFlag
	s3client *s3.Client
	cfg      *config.Config
}

// onNetErr — обработка сетевой ошибки с учётом shutdown.
// Если shutdown: помечает юзера pending, выходит из воркера.
// Если обычная ошибка: логирует, бампит ошибку.
func (a *app) onNetErr(workerKey, email, shortEmail, op string, err error) (shutdown bool) {
	if a.shutdown.IsSet() {
		log.Printf("[DEBUG] [%s] shutdown прервал %s для %s", workerKey, op, shortEmail)
		setUserStatus(a.st, email, "pending", "", a.cfg.StateFile)
		return true
	}
	a.dash.Log("ERROR", fmt.Sprintf("[%s] %s для %s: %v", workerKey, op, shortEmail, err))
	log.Printf("[ERROR] [USER] %s: %s %s: %v", workerKey, op, shortEmail, err)
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

func (a *app) worker(ctx context.Context, idx int, saKeyPath string, emailCh chan string, requeue chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	workerKey := fmt.Sprintf("sa%d", idx)
	log.Printf("[DEBUG] [%s] Воркер запущен", workerKey)

	stagger := time.Duration(idx) * workerStartStagger
	if stagger > 0 {
		a.dash.UpdateWorker(workerKey, "IDLE", fmt.Sprintf("старт через %s", stagger), "")
		log.Printf("[DEBUG] [%s] Stagger %s", workerKey, stagger)
		if util.SleepOrShutdown(a.shutdown, stagger) {
			log.Printf("[DEBUG] [%s] Stagger прерван shutdown", workerKey)
			return
		}
	}
	a.dash.UpdateWorker(workerKey, "IDLE", "подключен", "")
	log.Printf("[DEBUG] [%s] Подключен, жду задачи", workerKey)

	defer func() {
		a.dash.UpdateWorker(workerKey, "FINISH", "done", "")
		log.Printf("[DEBUG] [%s] Воркер завершён", workerKey)
	}()

	for {
		if a.shutdown.IsSet() {
			log.Printf("[DEBUG] [%s] Shutdown, выхожу", workerKey)
			return
		}
		var email string
		select {
		case e, ok := <-emailCh:
			if !ok {
				log.Printf("[DEBUG] [%s] Канал задач закрыт", workerKey)
				return
			}
			email = e
		default:
			log.Printf("[DEBUG] [%s] Задач нет, выхожу", workerKey)
			return
		}

		shortEmail := strings.Split(email, "@")[0]
		log.Printf("[DEBUG] [%s] === Юзер: %s ===", workerKey, email)

		svc, err := buildGmailClient(saKeyPath, email)
		if err != nil {
			if a.onNetErr(workerKey, email, shortEmail, "BuildClient", err) {
				return
			}
			continue
		}

		a.dash.UpdateWorker(workerKey, shortEmail, "листинг Gmail...", "")
		log.Printf("[DEBUG] [%s] %s: старт листинга message IDs", workerKey, shortEmail)
		msgIDs, err := listAllMessageIDs(ctx, svc, email, func(collected, page int) {
				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("Gmail %d стр.%d", collected, page), "")
		})
		if err != nil {
			if a.onNetErr(workerKey, email, shortEmail, "ListAllMessageIDs", err) {
				return
			}
			continue
		}

		total := len(msgIDs)
		log.Printf("[DEBUG] [%s] %s: Gmail отдал %d message IDs", workerKey, shortEmail, total)
		if total == 0 {
			log.Printf("[DEBUG] [%s] %s: 0 писем, помечаю done", workerKey, shortEmail)
			setUserStatus(a.st, email, "done", "", a.cfg.StateFile)
			a.bumpDone()
			continue
		}

		// Проверяем S3
		s3Prefix := filepath.Join(a.cfg.Workspace, "users", email, "gmail") + "/"
		log.Printf("[DEBUG] [%s] %s: листинг S3 prefix=%s", workerKey, shortEmail, s3Prefix)
		a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("S3 листинг... Gmail=%d", total), "")
		existingInS3, err := getExistingMsgIDs(ctx, a.s3client, a.cfg.S3.Bucket, s3Prefix)
		if err != nil {
			if a.shutdown.IsSet() {
				setUserStatus(a.st, email, "pending", "", a.cfg.StateFile)
				return
			}
			a.dash.Log("WARN", fmt.Sprintf("[%s] Ошибка листинга S3 для %s: %v", workerKey, shortEmail, err))
			log.Printf("[WARN] [%s] Ошибка листинга S3 для %s: %v", workerKey, shortEmail, err)
			existingInS3 = make(map[string]struct{})
		}

		missing := diffMissing(msgIDs, existingInS3)
		s3Count := total - len(missing)
		if len(missing) == 0 {
			a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("done (%d/%d)", total, total), "0")
			a.dash.Log("INFO", fmt.Sprintf("[%s] %s: все %d писем в S3 — done", workerKey, shortEmail, total))
			log.Printf("[DEBUG] [%s] %s: все %d в S3, done", workerKey, shortEmail, total)
			setUserStatus(a.st, email, "done", "", a.cfg.StateFile)
			a.bumpDone()
			continue
		}

		log.Printf("[DEBUG] [%s] %s: качаем %d из %d", workerKey, shortEmail, len(missing), total)
		a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("S3=%d/%d, качаем %d", s3Count, total, len(missing)), "")
		a.dash.Log("INFO", fmt.Sprintf("[%s] %s: в S3 %d/%d, качаем %d", workerKey, shortEmail, s3Count, total, len(missing)))

		downloaded := 0
		errorLog := []string{}
		fatalQuota := false
		pendingIDs := missing
		retryRound := 0
		concurrentRetryRound := 0
		adaptive := newAdaptiveBatchSize()

		a.dash.UpdateWorkerDetail(workerKey, fmt.Sprintf("0/%d", maxRetries), fmt.Sprintf("%d", adaptive.current))

		for len(pendingIDs) > 0 && retryRound < maxRetries {
			if a.shutdown.IsSet() {
				log.Printf("[DEBUG] [%s] %s: shutdown в цикле retry", workerKey, shortEmail)
				break
			}
			var nextPending []string
			roundConcurrentOnly := true
			anyBatches := false

			log.Printf("[DEBUG] [%s] %s: раунд %d, batch_size=%d, pending=%d",
				workerKey, shortEmail, retryRound+1, adaptive.current, len(pendingIDs))

			for i := 0; i < len(pendingIDs); {
				if a.shutdown.IsSet() {
					nextPending = append(nextPending, pendingIDs[i:]...)
					break
				}
				chunkSize := adaptive.current
				end := i + chunkSize
				if end > len(pendingIDs) {
					end = len(pendingIDs)
				}
				chunk := pendingIDs[i:end]
				i = end
				anyBatches = true

				log.Printf("[DEBUG] [%s] %s: батч %d-%d (%d шт)", workerKey, shortEmail, i-chunkSize, i, len(chunk))

				written, retryIDs, batchConcurrentOnly, batchErr := fetchMessagesBatch(
					ctx, svc, chunk,
					func(msgID string, rawBytes []byte) error {
						key := filepath.Join(s3Prefix, msgID+".eml")
						return putObject(ctx, a.s3client, a.cfg.S3.Bucket, key, rawBytes)
					},
				)
				if batchErr != nil {
					if dq, ok := batchErr.(*dailyQuotaExceeded); ok {
						log.Printf("[WARN] [%s] %s: daily quota exceeded", workerKey, shortEmail)
						nextPending = append(dq.PendingIDs, pendingIDs[i:]...)
						fatalQuota = true
						break
					}
					if a.shutdown.IsSet() {
						setUserStatus(a.st, email, "pending", "", a.cfg.StateFile)
						return
					}
					errorLog = append(errorLog, fmt.Sprintf("batch error: %v", batchErr))
					nextPending = append(nextPending, chunk...)
					continue
				}

				downloaded += written
				log.Printf("[DEBUG] [%s] %s: батч written=%d retry=%d concurrent=%v",
					workerKey, shortEmail, written, len(retryIDs), batchConcurrentOnly)

				if len(retryIDs) > 0 {
					nextPending = append(nextPending, retryIDs...)
					if !batchConcurrentOnly {
						roundConcurrentOnly = false
					}
					adaptive.shrink()
					log.Printf("[DEBUG] [%s] %s: batch_size уменьшен до %d", workerKey, shortEmail, adaptive.current)
				} else {
					adaptive.reportCleanBatch()
				}

				a.dash.UpdateWorker(workerKey, shortEmail, fmt.Sprintf("S3=%d/%d, качаем %d/%d", s3Count+downloaded, total, downloaded, len(missing)), "")
				a.dash.UpdateWorkerDetail(workerKey, fmt.Sprintf("%d/%d", retryRound+1, maxRetries), fmt.Sprintf("%d", adaptive.current))

				util.SleepOrShutdown(a.shutdown, interBatchDelay)
			}

			if fatalQuota {
				break
			}
			pendingIDs = nextPending
			if len(pendingIDs) == 0 || a.shutdown.IsSet() {
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

			a.dash.Log("WARN", fmt.Sprintf("[%s] %s: %d на retry, пауза %s", workerKey, shortEmail, len(pendingIDs), delay))
			log.Printf("[DEBUG] [%s] %s: retry_round=%d, delay=%s, pending=%d", workerKey, shortEmail, retryRound, delay, len(pendingIDs))
			if util.SleepOrShutdown(a.shutdown, delay) {
				break
			}
		}

		if fatalQuota {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Суточная квота исчерпана на %s, возвращаем в пул (%d/%d уже скачано)", workerKey, email, downloaded, len(missing)))
			a.dash.UpdateWorker(workerKey, "DAILY QUOTA", "DEAD", "")
			a.dash.SetWorkerDeadQuota(workerKey, true)
			requeue <- email
			return
		}

		if a.shutdown.IsSet() {
			a.dash.Log("WARN", fmt.Sprintf("[%s] Graceful shutdown: %s остановлен на %d/%d", workerKey, shortEmail, downloaded, len(missing)))
			setUserStatus(a.st, email, "pending", "", a.cfg.StateFile)
			return
		}

		if len(pendingIDs) > 0 {
			errorLog = append(errorLog, fmt.Sprintf("%d писем не докачано после retry", len(pendingIDs)))
		}

		status := "done"
		if len(errorLog) > 0 {
			status = "error"
			for _, line := range errorLog {
				a.dash.Log("WARN", fmt.Sprintf("[%s] %s: %s", workerKey, email, line))
			}
			log.Printf("[ERROR] [USER] %s: завершён с ошибками (%d), %d/%d скачано", workerKey, len(errorLog), downloaded, len(missing))
			a.bumpError()
		} else {
			log.Printf("[INFO] [USER] %s: завершён OK, %d/%d скачано", workerKey, downloaded, len(missing))
			a.bumpDone()
		}
		setUserStatus(a.st, email, status, strings.Join(errorLog, "; "), a.cfg.StateFile)
	}
}
