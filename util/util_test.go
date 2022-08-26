package util

import (
	"testing"
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
	t.Logf("Received %+v from readyChannel\n", received)
	c.Send(true)
	received = c.Receive()
	t.Logf("Received %+v from readyChannel\n", received)
	if received {
		t.Fatal("Later send overwrote first input to channel")
	}
	w.Wait()
	if receiveCounter != nReceivers {
		t.Fatal("Not all goroutines finished c.Receive")
	}
}

func TestGetUserIDFromLabels(t *testing.T) {
	tests := []struct{
		input map[string]string
		want string
	}{
		{map[string]string{"user":"foo","domain":"bar"}, "foo@bar"},
		{map[string]string{"user":"foo","domain":""}, "foo"},
		{map[string]string{"domain":"bar"}, ""},
		{map[string]string{"user":"","domain":"bar"}, ""},
		{map[string]string{"user":"foo","domain":""}, "foo"},
		{map[string]string{"user":"foo"}, "foo"},
	}
	for _, test := range tests {
		if GetUserIDFromLabels(test.input) != test.want {
			t.Fatalf("Failed to get userID for %v", test.input)
		}
	}
}
