package util

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
)

// type for signalling whether one-off events have completed successfully within a timeout
type ReadyChannel struct {
	ch          chan bool
	receivedYet bool
	firstValue  bool
	mutex       *sync.Mutex
}

// Return a new safeBoolChannel whith the timeout counting down
func NewReadyChannel(timeout time.Duration) *ReadyChannel {
	ch := make(chan bool, 1)
	var m sync.Mutex
	rc := &ReadyChannel{
		ch:          ch,
		receivedYet: false,
		firstValue:  false,
		mutex:       &m,
	}
	go func() {
		time.Sleep(timeout)
		rc.Send(false)
	}()
	return rc
}

// Attempt to send value into the ReadyChannel's channel.
// If the buffer is already full, this will do nothing.
func (t *ReadyChannel) Send(value bool) {
	select {
	case t.ch <- value:
	default:
	}
}

// Return the first value that was input to t.Send().
// If there hasn't been one yet, block until there is one.
func (t *ReadyChannel) Receive() bool {
	// use the ReadyChannel's mutex to block other goroutines where t.Receive is called until this returns
	t.mutex.Lock()
	defer func() {
		t.mutex.Unlock()
	}()

	// if a value has been received from this ReadyChannel, return that value
	if t.receivedYet {
		return t.firstValue
	}
	// otherwise, this is the first time Receive is called
	// block until the first value is ready in the channel, which will either be from t.Send() or the timeout
	value := <-t.ch

	// set t.firstValue to true so that subsequent t.Receive() will return value immediately
	t.receivedYet = true
	t.firstValue = value
	return value
}

// Do ch<-value if the channel is ready to receive a value,
// otherwise do nothing
// This allows the goroutine attempting a send to continue without blocking
// To ensure ch can take a value, make it a buffered channel with enough space
func TrySend(ch chan<- bool, value bool) {
	select {
	case ch <- value:
	default:
	}
}

// Block until an input was received from each channel in inputChannels,
// then send output <- input0 && input 1 && input2...
func CombineReadyChannels(inputChannels []*ReadyChannel, outputChannel *ReadyChannel) {
	output := ReceiveReadyChannels(inputChannels)
	outputChannel.Send(output)
}

func ReceiveReadyChannels(inputChannels []*ReadyChannel) bool {
	output := true
	for _, ch := range inputChannels {
		// Block until able to receive from each channel,
		// if any are false, then the output is false
		if !ch.Receive() {
			output = false
		}
	}
	return output
}

// Gets the IP of the source that made the request, either r.RemoteAddr,
// or if it was forwarded, the first address in the X-Forwarded-For header
func getRemoteIP(r *http.Request) string {
	// When running this behind caddy, r.RemoteAddr is just the caddy process's IP addr,
	// and X-Forward-For header should contain the silo's IP address.
	// This may be different with ingress.
	var siloIP string
	value, forwarded := r.Header["X-Forwarded-For"]
	if forwarded {
		siloIP = value[0]
	} else {
		siloIP = r.RemoteAddr
	}
	return regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`).FindString(siloIP)
}

// read saved tokens or pod info from disk
// TODO change to a nicer way of serializing this
func ReadSavedMap(dirName string) (map[string]string, error) {
	savedMap := make(map[string]string)
	fileList, err := ioutil.ReadDir(dirName)
	if err != nil {
		if os.IsNotExist(err) {
			// if the directory doesn't exist, stop here
			return savedMap, nil
		} else {
			// if there's some problem other than the directory not existing, log it
			return savedMap, err
		}
	}
	for _, file := range fileList {
		fileName := file.Name()
		content, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", dirName, fileName))
		if err != nil {
			return savedMap, err
		}
		savedMap[fileName] = string(content)
	}
	return savedMap, nil
}

// Returns whether a pod has a container listening on port 22
func NeedsSshService(pod *apiv1.Pod) bool {
	listensSsh := false
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.ContainerPort == 22 {
				listensSsh = true
				break
			}
		}
	}
	return listensSsh
}
