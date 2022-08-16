package util

import (
	"http"
	"regexp"
	"ioutil"
	"os"
	"fmt"
)

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

// Block until an input was received from each channel in chans,
// then send combined <- chans0 && chans1 && chans2...
func CombineBoolChannels(chans []<-chan bool, combined chan<- bool) {
	output := true
	for _, ch := range chans {
		if !<-ch {
			output = false
		}
	}
	combined <- output
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

