package fetchlabels

import "time"

const (
	labelsDumpInterval = 60 * time.Second

	maxRetries         = 5
	workerStartStagger = 3 * time.Second

	batchSizeMax      = 80
	batchSizeMin      = 10
	batchSizeStart    = 40
	batchShrinkFactor = 0.5
	batchGrowthStreak = 8
	batchGrowthStep   = 10

	interBatchDelay = 200 * time.Millisecond

	rateLimitBackoffBase          = 60 * time.Second
	concurrentLimitBackoffBase    = 3 * time.Second
	concurrentLimitBackoffMaxRnds = 6
)
