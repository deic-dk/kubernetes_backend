package util

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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
	tests := []struct {
		input map[string]string
		want  string
	}{
		{map[string]string{"user": "foo", "domain": "bar"}, "foo@bar"},
		{map[string]string{"user": "foo", "domain": ""}, "foo"},
		{map[string]string{"domain": "bar"}, ""},
		{map[string]string{"user": "", "domain": "bar"}, ""},
		{map[string]string{"user": "foo", "domain": ""}, "foo"},
		{map[string]string{"user": "foo"}, "foo"},
	}
	for _, test := range tests {
		if GetUserIDFromLabels(test.input) != test.want {
			t.Fatalf("Failed to get userID for %v", test.input)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	// Load the config as it currently is
	configOriginal := MustLoadGlobalConfig()

	// Get current environment variables to be able to reset them later
	varNamespace := fmt.Sprintf("%s_%s", strings.ToUpper(environmentPrefix), "NAMESPACE")
	varTimeoutDelete := fmt.Sprintf("%s_%s", strings.ToUpper(environmentPrefix), "TIMEOUTDELETE")
	currentNamespace := os.Getenv(varNamespace)
	currentTimeoutDelete := os.Getenv(varTimeoutDelete)

	// Overwrite the environment variables
	newNamespace := "foobar"
	newTimeoutDelete := "5m3s"
	os.Setenv(varNamespace, newNamespace)
	os.Setenv(varTimeoutDelete, newTimeoutDelete)

	// Load the config which should include the overwritten values
	configOverwritten := MustLoadGlobalConfig()
	if (configOverwritten.TimeoutDelete == configOriginal.TimeoutDelete) && (configOverwritten.Namespace == configOriginal.Namespace) && (newNamespace != currentNamespace) && (newTimeoutDelete != currentTimeoutDelete) {
		t.Fatal("Overwritten environment variables didn't affect the loaded config")
	}

	if configOverwritten.TimeoutDelete.String() != newTimeoutDelete {
		t.Fatal("Didn't set timeout correctly from environment variable")
	}

	if configOverwritten.Namespace != newNamespace {
		t.Fatal("Didn't set namespace correctly from environment variable")
	}

	// Set environment variables back to their previous value
	os.Setenv(varNamespace, currentNamespace)
	os.Setenv(varTimeoutDelete, currentTimeoutDelete)
}
