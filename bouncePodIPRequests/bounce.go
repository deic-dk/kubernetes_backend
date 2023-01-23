package bounce

// This is just a quick http server to bounce get_podip_owner requests from user pods to the apiserver
// The problem this is meant to solve is that pods can start running their entrypoint scripts before the kubernetes
// apiserver has all the correct metadata about them, so if they try to curl to their owner's silo, the
// get_podip_owner request might fail.
// With this running on a node in the cluster, a testing pod can make many requests to this server as soon as its
// entrypoint begins, and this will record the time that they are first successful, so an appropriate timeout
// can be set in server/server.go:getPodIPOwner

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
)

func bounce(w http.ResponseWriter, r *http.Request) {
	remoteAddr := r.RemoteAddr
	v4regex := regexp.MustCompile(`(\d{1,3}[.]){3}\d{1,3}`)
	remoteIP := v4regex.FindString(remoteAddr)
	response, err := http.Get(fmt.Sprintf("http://kube.sciencedata.dk/get_podip_owner?ip=%s", remoteIP))
	if err != nil {
		fmt.Printf("Error making get request: %s\n", err.Error())
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		fmt.Printf("Got error code %s\n", response.StatusCode)
		return
	}
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		fmt.Printf("Couldn't read body: %s\n", err.Error())
		return
	}
	fmt.Printf("Successful Request from %s: %s\n", remoteIP, string(body))
}

func main() {
	http.HandleFunc("/bounce", bounce)
	fmt.Printf("Listening\n")
	err := http.ListenAndServe(":10180", nil)
	if err != nil {
		panic(err.Error())
	}
}
