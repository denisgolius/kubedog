package monitor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/kubernetes/pkg/apis/core"
)

type PodWatchMonitor struct {
	WatchMonitor

	PodLogChunk chan *PodLogChunk
	PodError    chan PodError
	Error       chan error

	ContainerMonitorStates          map[string]string
	ProcessedContainerLogTimestamps map[string]time.Time

	InitContainersNames []string
	ContainersNames     []string
}

func (pod *PodWatchMonitor) FollowContainerLogs(containerName string) error {
	client := pod.Kube

	req := client.Core().
		Pods(pod.Namespace).
		GetLogs(pod.ResourceName, &v1.PodLogOptions{
			Container:  containerName,
			Timestamps: true,
			Follow:     true,
		})

	readCloser, err := req.Stream()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	lineBuf := bytes.Buffer{}
	rawBuf := make([]byte, 4096)

	for {
		n, err := readCloser.Read(rawBuf)
		if err != nil && err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		chunkLines := make([]LogLine, 0)
		for i := 0; i < n; i++ {
			if rawBuf[i] == '\n' {
				lineParts := strings.SplitN(lineBuf.String(), " ", 2)
				if len(lineParts) == 2 {
					chunkLines = append(chunkLines, LogLine{Timestamp: lineParts[0], Data: lineParts[1]})
				}

				lineBuf.Reset()
				continue
			}

			lineBuf.WriteByte(rawBuf[i])
		}

		pod.PodLogChunk <- &PodLogChunk{
			PodName:       pod.ResourceName,
			ContainerName: containerName,
			LogLines:      chunkLines,
		}
	}

	return nil
}

func (pod *PodWatchMonitor) WatchContainerLogs(containerName string) error {
	for {
		switch pod.ContainerMonitorStates[containerName] {
		case "Running", "Terminated":
			return pod.FollowContainerLogs(containerName)
		case "Waiting":
		default:
		}

		time.Sleep(time.Duration(200) * time.Millisecond)
	}

	return nil
}

func (pod *PodWatchMonitor) Watch() error {
	allContainersNames := make([]string, 0)
	for _, containerName := range pod.InitContainersNames {
		allContainersNames = append(allContainersNames, containerName)
	}
	for _, containerName := range pod.ContainersNames {
		allContainersNames = append(allContainersNames, containerName)
	}

	for i := range allContainersNames {
		containerName := allContainersNames[i]
		go func() {
			err := pod.WatchContainerLogs(containerName)
			if err != nil {
				pod.Error <- err
			}
		}()
	}

	client := pod.Kube

	watcher, err := client.Core().Pods(pod.Namespace).
		Watch(metav1.ListOptions{
			ResourceVersion: pod.InitialResourceVersion,
			Watch:           true,
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", pod.ResourceName).String(),
		})
	if err != nil {
		return err
	}

	ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), pod.Timeout)
	defer cancel()

	_, err = watchtools.UntilWithoutRetry(ctx, watcher, func(e watch.Event) (bool, error) {
		object, ok := e.Object.(*core.Pod)
		if !ok {
			return true, fmt.Errorf("Expected %s to be a *core.Pod, got %T", pod.ResourceName, e.Object)
		}

		allContainerStatuses := make([]core.ContainerStatus, 0)
		for _, cs := range object.Status.InitContainerStatuses {
			allContainerStatuses = append(allContainerStatuses, cs)
		}
		for _, cs := range object.Status.ContainerStatuses {
			allContainerStatuses = append(allContainerStatuses, cs)
		}

		for _, cs := range allContainerStatuses {
			oldState := pod.ContainerMonitorStates[cs.Name]

			if cs.State.Waiting != nil {
				pod.ContainerMonitorStates[cs.Name] = "Waiting"

				switch cs.State.Waiting.Reason {
				case "ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff":
					pod.PodError <- PodError{
						ContainerName: cs.Name,
						PodName:       pod.ResourceName,
						Message:       fmt.Sprintf("%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message),
					}
				}
			}
			if cs.State.Running != nil {
				pod.ContainerMonitorStates[cs.Name] = "Running"
			}
			if cs.State.Terminated != nil {
				pod.ContainerMonitorStates[cs.Name] = "Running"
			}

			if oldState != pod.ContainerMonitorStates[cs.Name] {
				if debug() {
					fmt.Printf("Pod %s container %s state changed %v -> %v", pod.ResourceName, cs.Name, oldState, pod.ContainerMonitorStates[cs.Name])
				}
			}
		}

		return false, nil
	})

	return nil
}
