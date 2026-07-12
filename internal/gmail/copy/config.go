package copy

import "time"

var (
	stateFile    = "migration_gmail_state_ru.json"
	msgCacheFile = "migration_msg_cache_ru.json"
)

const (
	workspacePrefix = "ru"
	usersJSONPath   = "users.json"
	saKeysDir       = "workers"
	destMount       = "/mnt/s3gmail"

	gmailHTTPTimeout = 30 * time.Second
	hardTimeout      = 45 * time.Second

	maxRetries           = 5
	listMaxRetries       = 5
	workerStartStagger   = 3 * time.Second
	maxConcurrentWorkers = 5

	batchSizeMax      = 50
	batchSizeMin      = 10
	batchSizeStart    = 25
	batchShrinkFactor = 0.5
	batchGrowthStreak = 8
	batchGrowthStep   = 5

	interBatchDelay = 2 * time.Second

	rateLimitBackoffBase          = 60 * time.Second
	rateLimitBackoffMaxRounds     = 3
	concurrentLimitBackoffBase    = 3 * time.Second
	concurrentLimitBackoffMaxRnds = 6

	stuckThreadsShutdownThreshold = 8
	memoryLimitMB                 = 4500
	memoryCheckInterval           = 10 * time.Second

	stateDumpInterval    = 60 * time.Second
	msgCacheDumpInterval = 60 * time.Second
	progressEvery        = 50
	maxLogLines          = 12
)
