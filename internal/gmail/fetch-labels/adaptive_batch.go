package fetchlabels

type adaptiveBatchSize struct {
	current     int
	cleanStreak int
}

func newAdaptiveBatchSize() *adaptiveBatchSize {
	return &adaptiveBatchSize{current: batchSizeStart}
}

func (a *adaptiveBatchSize) shrink() int {
	a.current = int(float64(a.current) * batchShrinkFactor)
	if a.current < batchSizeMin {
		a.current = batchSizeMin
	}
	a.cleanStreak = 0
	return a.current
}

func (a *adaptiveBatchSize) reportCleanBatch() {
	a.cleanStreak++
	if a.cleanStreak >= batchGrowthStreak && a.current < batchSizeMax {
		a.current += batchGrowthStep
		if a.current > batchSizeMax {
			a.current = batchSizeMax
		}
		a.cleanStreak = 0
	}
}
