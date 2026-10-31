package transcribe

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	ErrQueueFull     = errors.New("stt queue is full")
	ErrAudioTooLarge = errors.New("audio exceeds maximum allowed size")
	ErrQueueClosed   = errors.New("stt queue is closed")
)

// Default limits for conservative bounded local inference.
const (
	DefaultQueueCapacity = 16
	DefaultMaxAudioBytes = 10 * 1024 * 1024 // 10MB
)

type QueueConfig struct {
	Capacity      int
	MaxAudioBytes int64
}

type Job struct {
	ChatJID      string
	MessageID    string
	Audio        []byte
	Filename     string
	DeclaredSize uint64
	Download     func(ctx context.Context) ([]byte, error)
	OnSuccess    func(ctx context.Context, text string) error
	Ctx          context.Context
	Done         chan struct{}
	Result       string
	Err          error

	closeOnce sync.Once
}

func (j *Job) finish(result string, err error) {
	j.Result = result
	j.Err = err
	if j.Done != nil {
		j.closeOnce.Do(func() {
			close(j.Done)
		})
	}
}

type QueueStatus struct {
	Queued    int
	Processed int64
	Dropped   int64
	Failed    int64
}

type Queue struct {
	transcriber   Transcriber
	maxAudioBytes int64
	ch            chan *Job
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup

	mu       sync.Mutex
	inFlight map[string]struct{}
	closed   bool
	dropped  int64
	success  int64
	failed   int64
}

func jobKey(j *Job) string {
	if j.ChatJID != "" && j.MessageID != "" {
		return j.ChatJID + "\x00" + j.MessageID
	}
	if j.MessageID != "" {
		return j.MessageID
	}
	return ""
}

func NewQueue(transcriber Transcriber, cfg QueueConfig) *Queue {
	capacity := cfg.Capacity
	if capacity <= 0 {
		capacity = DefaultQueueCapacity
	}
	maxAudio := cfg.MaxAudioBytes
	if maxAudio <= 0 {
		maxAudio = DefaultMaxAudioBytes
	}

	ctx, cancel := context.WithCancel(context.Background())

	q := &Queue{
		transcriber:   transcriber,
		maxAudioBytes: maxAudio,
		ch:            make(chan *Job, capacity),
		ctx:           ctx,
		cancel:        cancel,
		inFlight:      make(map[string]struct{}),
	}

	// Exactly ONE inference worker to bound host CPU/memory consumption.
	q.wg.Add(1)
	go q.worker()

	return q
}

func (q *Queue) cleanupInFlight(key string) {
	if key == "" {
		return
	}
	q.mu.Lock()
	delete(q.inFlight, key)
	q.mu.Unlock()
}

func (q *Queue) Submit(ctx context.Context, job *Job) error {
	if job == nil {
		return errors.New("nil job")
	}

	if job.DeclaredSize > 0 && int64(job.DeclaredSize) > q.maxAudioBytes {
		atomic.AddInt64(&q.failed, 1)
		job.finish("", ErrAudioTooLarge)
		return ErrAudioTooLarge
	}

	if int64(len(job.Audio)) > q.maxAudioBytes {
		atomic.AddInt64(&q.failed, 1)
		job.finish("", ErrAudioTooLarge)
		return ErrAudioTooLarge
	}

	if ctx.Err() != nil {
		job.finish("", ctx.Err())
		return ctx.Err()
	}

	if job.Ctx != nil && job.Ctx.Err() != nil {
		job.finish("", job.Ctx.Err())
		return job.Ctx.Err()
	}

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		job.finish("", ErrQueueClosed)
		return ErrQueueClosed
	}

	key := jobKey(job)
	if key != "" {
		if _, exists := q.inFlight[key]; exists {
			q.mu.Unlock()
			job.finish("", nil)
			return nil
		}
		q.inFlight[key] = struct{}{}
	}
	q.mu.Unlock()

	select {
	case q.ch <- job:
		return nil
	case <-q.ctx.Done():
		q.cleanupInFlight(key)
		job.finish("", ErrQueueClosed)
		return ErrQueueClosed
	case <-ctx.Done():
		q.cleanupInFlight(key)
		job.finish("", ctx.Err())
		return ctx.Err()
	default:
		// Queue is full: apply backpressure / drop policy
		q.cleanupInFlight(key)
		atomic.AddInt64(&q.dropped, 1)
		job.finish("", ErrQueueFull)
		return ErrQueueFull
	}
}

func (q *Queue) worker() {
	defer q.wg.Done()

	for {
		select {
		case <-q.ctx.Done():
			return
		case job, ok := <-q.ch:
			if !ok {
				return
			}
			q.process(job)
		}
	}
}

func (q *Queue) process(job *Job) {
	key := jobKey(job)
	defer func() {
		q.cleanupInFlight(key)
	}()

	jobCtx := q.ctx
	var cancel context.CancelFunc
	if job.Ctx != nil {
		jobCtx, cancel = context.WithCancel(q.ctx)
		defer cancel()
		stop := context.AfterFunc(job.Ctx, cancel)
		defer stop()
	}

	if err := jobCtx.Err(); err != nil {
		job.finish("", err)
		atomic.AddInt64(&q.failed, 1)
		return
	}

	data := job.Audio
	if len(data) == 0 && job.Download != nil {
		d, err := job.Download(jobCtx)
		if err != nil {
			job.finish("", err)
			atomic.AddInt64(&q.failed, 1)
			return
		}
		data = d
	}

	if int64(len(data)) > q.maxAudioBytes {
		job.finish("", ErrAudioTooLarge)
		atomic.AddInt64(&q.failed, 1)
		return
	}

	if q.transcriber == nil {
		job.finish("", errors.New("transcriber is nil"))
		atomic.AddInt64(&q.failed, 1)
		return
	}

	text, err := q.transcriber.Transcribe(jobCtx, data, job.Filename)
	if err != nil {
		job.finish("", err)
		atomic.AddInt64(&q.failed, 1)
		return
	}

	if job.OnSuccess != nil {
		if err := job.OnSuccess(jobCtx, text); err != nil {
			job.finish(text, err)
			atomic.AddInt64(&q.failed, 1)
			return
		}
	}

	job.finish(text, nil)
	atomic.AddInt64(&q.success, 1)
}

func (q *Queue) Status() QueueStatus {
	return QueueStatus{
		Queued:    len(q.ch),
		Processed: atomic.LoadInt64(&q.success),
		Dropped:   atomic.LoadInt64(&q.dropped),
		Failed:    atomic.LoadInt64(&q.failed),
	}
}

func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.cancel()

	// Drain remaining jobs in q.ch and finish them with ErrQueueClosed
	for {
		select {
		case job := <-q.ch:
			key := jobKey(job)
			delete(q.inFlight, key)
			job.finish("", ErrQueueClosed)
			atomic.AddInt64(&q.dropped, 1)
		default:
			goto drained
		}
	}
drained:
	q.mu.Unlock()

	q.wg.Wait()
}
