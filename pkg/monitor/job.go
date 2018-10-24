package monitor

import (
	"bytes"
	"fmt"
	"io"
	_ "sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	_ "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions/resource"
	"k8s.io/kubernetes/pkg/apis/batch"
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
		GetLogs(pod.ResourceName, &core.PodLogOptions{
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

	_, err = watch.Until(pod.Timeout, watcher, func(e watch.Event) (bool, error) {
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
				pod.Kube.Log("Pod %s container %s state changed %v -> %v", pod.ResourceName, cs.Name, oldState, pod.ContainerMonitorStates[cs.Name])
			}
		}

		return false, nil
	})

	return nil
}

type JobWatchMonitor struct {
	WatchMonitor

	State string

	Started     chan bool
	Succeeded   chan bool
	Error       chan error
	AddedPod    chan *PodWatchMonitor
	PodLogChunk chan *PodLogChunk
	PodError    chan PodError

	MonitoredPods []*PodWatchMonitor

	FinalJobStatus batch.JobStatus
}

func (job *JobWatchMonitor) Watch() error {
	client := job.Kube

	watcher, err := client.Batch().Jobs(job.Namespace).
		Watch(metav1.ListOptions{
			ResourceVersion: job.InitialResourceVersion,
			Watch:           true,
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", job.ResourceName).String(),
		})
	if err != nil {
		return err
	}

	_, err = watch.Until(job.Timeout, watcher, func(e watch.Event) (bool, error) {
		switch job.State {
		case "":
			if e.Type == watch.Added {
				job.Started <- true

				job.State = "Started"

				job.Kube.Log("Starting to watch job %s pods", job.ResourceName)

				go func() {
					err := job.WatchPods()
					if err != nil {
						job.Error <- err
					}
				}()
			}

		case "Started":
			object, ok := e.Object.(*batch.Job)
			if !ok {
				return true, fmt.Errorf("Expected %s to be a *batch.Job, got %T", job.ResourceName, e.Object)
			}

			for _, c := range object.Status.Conditions {
				if c.Type == batch.JobComplete && c.Status == core.ConditionTrue {
					job.State = "Succeeded"

					job.FinalJobStatus = object.Status

					job.Succeeded <- true

					return true, nil
				} else if c.Type == batch.JobFailed && c.Status == core.ConditionTrue {
					job.State = "Failed"

					return true, fmt.Errorf("Job failed: %s", c.Reason)
				}
			}

		default:
			return true, fmt.Errorf("Unknown job %s watcher state: %s", job.ResourceName, job.State)
		}

		return false, nil
	})

	if err != nil {
		return err
	}

	return nil
}

func (job *JobWatchMonitor) WatchPods() error {
	client := job.Kube

	jobManifest, err := client.Batch().
		Jobs(job.Namespace).
		Get(job.ResourceName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	selector, err := metav1.LabelSelectorAsSelector(jobManifest.Spec.Selector)
	if err != nil {
		return err
	}

	podListWatcher, err := client.Core().
		Pods(job.Namespace).
		Watch(metav1.ListOptions{
			Watch:         true,
			LabelSelector: selector.String(),
		})
	if err != nil {
		return err
	}

	// TODO: calculate timeout since job-watch started
	_, err = watch.Until(job.Timeout, podListWatcher, func(e watch.Event) (bool, error) {
		podObject, ok := e.Object.(*core.Pod)
		if !ok {
			return true, fmt.Errorf("Expected %s to be a *core.Pod, got %T", job.ResourceName, e.Object)
		}

		for _, pod := range job.MonitoredPods {
			if pod.ResourceName == podObject.Name {
				// Already under monitoring
				return false, nil
			}
		}

		// TODO: constructor from job & podObject
		pod := &PodWatchMonitor{
			WatchMonitor: WatchMonitor{
				Kube:    job.Kube,
				Timeout: job.Timeout,

				Namespace:              job.Namespace,
				ResourceName:           podObject.Name,
				InitialResourceVersion: "", // this will make PodWatchMonitor receive podObject again and handle its state properly by itself
			},

			PodLogChunk: job.PodLogChunk,
			PodError:    job.PodError,
			Error:       job.Error,

			ContainerMonitorStates:          make(map[string]string),
			ProcessedContainerLogTimestamps: make(map[string]time.Time),
		}

		for _, containerConf := range podObject.Spec.InitContainers {
			pod.InitContainersNames = append(pod.InitContainersNames, containerConf.Name)
		}
		for _, containerConf := range podObject.Spec.Containers {
			pod.ContainersNames = append(pod.ContainersNames, containerConf.Name)
		}

		job.MonitoredPods = append(job.MonitoredPods, pod)

		go func() {
			err := pod.Watch()
			if err != nil {
				job.Error <- err
			}
		}()

		job.AddedPod <- pod

		return false, nil
	})

	return nil
}

func (c *Client) WatchJobsUntilReady(namespace string, reader io.Reader, watchFeed WatchFeed, timeout time.Duration) error {
	infos, err := c.Build(namespace, reader)
	if err != nil {
		return err
	}

	return perform(infos, func(info *resource.Info) error {
		return c.watchJobUntilReady(info, watchFeed, timeout)
	})
}

func (c *Client) watchJobUntilReady(jobInfo *resource.Info, watchFeed WatchFeed, timeout time.Duration) error {
	if jobInfo.Mapping.GroupVersionKind.Kind != "Job" {
		return nil
	}

	// TODO: constructor
	job := &JobWatchMonitor{
		WatchMonitor: WatchMonitor{
			Kube:    c,
			Timeout: timeout,

			Namespace:              jobInfo.Namespace,
			ResourceName:           jobInfo.Name,
			InitialResourceVersion: jobInfo.ResourceVersion,
		},

		Started:     make(chan bool, 0),
		Succeeded:   make(chan bool, 0),
		AddedPod:    make(chan *PodWatchMonitor, 10),
		PodLogChunk: make(chan *PodLogChunk, 1000),

		PodError: make(chan PodError, 0),
		Error:    make(chan error, 0),
	}

	go func() {
		err := job.Watch()
		if err != nil {
			job.Error <- err
		}
	}()

	for {
		select {
		case <-job.Started:
			c.Log("Job %s started", job.ResourceName)
		case <-job.Succeeded:
			c.Log("%s: Jobs active: %d, jobs failed: %d, jobs succeeded: %d", job.ResourceName, job.FinalJobStatus.Active, job.FinalJobStatus.Failed, job.FinalJobStatus.Succeeded)
			return nil
		case err := <-job.Error:
			return err
		case pod := <-job.AddedPod:
			c.Log("Job %s pod %s added", job.ResourceName, pod.ResourceName)
		case podLogChunk := <-job.PodLogChunk:
			watchFeed.WriteJobLogChunk(JobLogChunk{
				PodLogChunk: *podLogChunk,
				JobName:     job.ResourceName,
			})
		case podError := <-job.PodError:
			watchFeed.WriteJobPodError(JobPodError{
				JobName:  job.ResourceName,
				PodError: podError,
			})
		}
	}
}