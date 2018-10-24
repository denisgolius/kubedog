package kubedog

import (
	"fmt"

	"github.com/flant/kubedog/pkg/monitor"
	"k8s.io/client-go/kubernetes"
)

func WatchJobTillDone(name, namespace string, kube kubernetes.Interface) error {
	fmt.Printf("WatchJobTillDone %s %s\n", name, namespace)
	return monitor.WatchJobUntilReady("test-job", "myns", kube, monitor.JobWatchFeedStub, monitor.WatchOptions{})
}
