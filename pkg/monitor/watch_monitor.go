package monitor

import (
	"time"

	"k8s.io/client-go/kubernetes"
)

type WatchMonitor struct {
	Kube    kubernetes.Interface
	Timeout time.Duration

	Namespace              string
	ResourceName           string
	InitialResourceVersion string
}
