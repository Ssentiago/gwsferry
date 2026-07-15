package copy

import "time"

const (
	hardTimeout = 45 * time.Second

	maxRetries         = 5
	listMaxRetries     = 5
	workerStartStagger = 3 * time.Second

	batchSizeMax      = 50
	batchSizeMin      = 10
	batchSizeStart    = 25
	batchShrinkFactor = 0.5
	batchGrowthStreak = 8
	batchGrowthStep   = 5

	interBatchDelay = 2 * time.Second

	rateLimitBackoffBase          = 60 * time.Second
	concurrentLimitBackoffBase    = 3 * time.Second
	concurrentLimitBackoffMaxRnds = 6

	memoryLimitMB     = 4500
	stateDumpInterval = 60 * time.Second
)
