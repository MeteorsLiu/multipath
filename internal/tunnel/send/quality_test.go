package send

import (
	"testing"
)

func TestLegQualityTrackerDefaultsToHealthy(t *testing.T) {
	var tr legQualityTracker
	rate := tr.deliveryRate()
	if rate != 1.0 {
		t.Fatalf("deliveryRate = %f, want 1.0 (no data yet)", rate)
	}
}

func TestLegQualityTrackerRecordsOnTime(t *testing.T) {
	var tr legQualityTracker
	tr.recordDelivery(true)
	tr.recordDelivery(true)
	if rate := tr.deliveryRate(); rate != 1.0 {
		t.Fatalf("deliveryRate = %f, want 1.0", rate)
	}
}

func TestLegQualityTrackerRecordsMisses(t *testing.T) {
	var tr legQualityTracker
	tr.recordDelivery(true)
	tr.recordDelivery(false)
	tr.recordDelivery(true)
	tr.recordDelivery(false)
	if rate := tr.deliveryRate(); rate != 0.5 {
		t.Fatalf("deliveryRate = %f, want 0.5", rate)
	}
}

func TestLegQualityTrackerWindowReset(t *testing.T) {
	var tr legQualityTracker
	tr.recordDelivery(true)
	tr.recordDelivery(false)
	tr.resetWindow()
	if rate := tr.deliveryRate(); rate != 1.0 {
		t.Fatalf("deliveryRate after reset = %f, want 1.0", rate)
	}
}
