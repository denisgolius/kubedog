package monitor

var (
	JobWatchFeedStub = &JobWatchFeedProto{
		StartedFunc:   func() error { return nil },
		SucceededFunc: func() error { return nil },
		AddedPodFunc:  func(string) error { return nil },
		LogChunkFunc:  func(JobLogChunk) error { return nil },
		PodErrorFunc:  func(JobPodError) error { return nil },
	}
)

// Prototype-struct helper to create feed with callbacks specified in-place of creation (such as JobWatchFeedStub)
type JobWatchFeedProto struct {
	StartedFunc   func() error
	SucceededFunc func() error
	AddedPodFunc  func(string) error
	LogChunkFunc  func(JobLogChunk) error
	PodErrorFunc  func(JobPodError) error
}

func (proto *JobWatchFeedProto) Started() error {
	return proto.StartedFunc()
}
func (proto *JobWatchFeedProto) Succeeded() error {
	return proto.SucceededFunc()
}
func (proto *JobWatchFeedProto) AddedPod(arg string) error {
	return proto.AddedPodFunc(arg)
}
func (proto *JobWatchFeedProto) LogChunk(arg JobLogChunk) error {
	return proto.LogChunkFunc(arg)
}
func (proto *JobWatchFeedProto) PodError(arg JobPodError) error {
	return proto.PodErrorFunc(arg)
}
