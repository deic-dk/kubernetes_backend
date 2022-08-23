package util

import (
	"testing"
	"fmt"
	"time"
	"sync"
)

func TestReadyChannel(t *testing.T) {
	c := NewReadyChannel(1 * time.Second)
	nReceivers := 15
	receiveCounter := 0
	var w sync.WaitGroup
	for i := 0; i < nReceivers; i++ {
		w.Add(1)
		go func() {
			c.Receive()
			receiveCounter += 1
			w.Done()
		}()
	}
	received := c.Receive()
	fmt.Printf("Received %+v from readyChannel\n", received)
	c.Send(true)
	received = c.Receive()
	fmt.Printf("Received %+v from readyChannel\n", received)
	if received {
		t.Fatal("Later send overwrote first input to channel")
	}
	w.Wait()
	if receiveCounter != nReceivers {
		t.Fatal("Not all goroutines finished c.Receive")
	}
}
