package send

type legQualityTracker struct {
	onTimeCount uint32
	totalCount  uint32
}

func (t *legQualityTracker) recordDelivery(onTime bool) {
	t.totalCount++
	if onTime {
		t.onTimeCount++
	}
}

func (t *legQualityTracker) deliveryRate() float64 {
	if t.totalCount == 0 {
		return 1.0
	}
	return float64(t.onTimeCount) / float64(t.totalCount)
}

func (t *legQualityTracker) resetWindow() {
	t.onTimeCount = 0
	t.totalCount = 0
}
