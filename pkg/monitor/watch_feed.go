package monitor

var (
	WatchFeedStub = &WatchFeedProto{
		WriteJobLogChunkFunc: func(JobLogChunk) error { return nil },
		WriteJobPodErrorFunc: func(JobPodError) error { return nil },
	}
)

type WatchFeed interface {
	WriteJobLogChunk(JobLogChunk) error
	WriteJobPodError(JobPodError) error
}

// Prototype-struct helper to create feed with callbacks specified in-place of creation (such as WatchFeedStub var)
type WatchFeedProto struct {
	WriteJobLogChunkFunc func(JobLogChunk) error
	WriteJobPodErrorFunc func(JobPodError) error
}

func (proto *WatchFeedProto) WriteJobLogChunk(arg JobLogChunk) error {
	return proto.WriteJobLogChunkFunc(arg)
}
func (proto *WatchFeedProto) WriteJobPodError(arg JobPodError) error {
	return proto.WriteJobPodErrorFunc(arg)
}

type LogLine struct {
	Timestamp string
	Data      string
}

type PodLogChunk struct {
	PodName       string
	ContainerName string
	LogLines      []LogLine
}

type PodError struct {
	Message       string
	PodName       string
	ContainerName string
}

type JobLogChunk struct {
	PodLogChunk
	JobName string
}

type JobPodError struct {
	PodError
	JobName string
}
