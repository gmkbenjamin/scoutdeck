package main

import (
	"sync"
	"testing"
)

func TestBatterySnapshotConcurrent(t *testing.T) {
	a := &application{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				onBatteryStatus(&status{Status: []int32{1, 75}})
				_ = a.status()
			}
		}()
	}
	wg.Wait()
	batteryMu.RLock()
	defer batteryMu.RUnlock()
	if batteryPercent != 75 || batteryChargeState != 1 || batteryAt.IsZero() {
		t.Fatal("invalid battery snapshot")
	}
}
