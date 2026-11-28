package claim

import (
	"testing"
	"time"
)

func TestMonitor_StopBeforeStart(t *testing.T) {
	mon := NewMonitor(nil, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Stop()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未曾呼叫 Start 時呼叫 Stop 不應永久阻塞")
	}
}
