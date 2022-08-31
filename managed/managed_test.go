package managed

import (
	"testing"
	"time"

	"github.com/deic.dk/user_pods_k8s_backend/k8sclient"
	"github.com/deic.dk/user_pods_k8s_backend/util"
)

func TestCleanNonexistentUserStorage(t *testing.T) {
	u := NewUser("foo@bar", *k8sclient.NewK8sClient())
	finished := util.NewReadyChannel(time.Second)
	err := u.CleanUserStorage(finished)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !finished.Receive() {
		t.Fatal("Received false for deletion of user storage")
	}
}
