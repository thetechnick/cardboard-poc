package filewatch

import (
	"context"
	"fmt"

	"github.com/fsnotify/fsnotify"
	"k8s.io/client-go/util/workqueue"
)

type WatchWorkQueue struct {
	do      doFn
	queue   workqueue.Interface
	watcher filewatcher
}

type filewatcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type doFn func(ctx context.Context) error

func NewWorkQueue(w filewatcher, doFn doFn) *WatchWorkQueue {
	return &WatchWorkQueue{
		do:      doFn,
		queue:   workqueue.New(),
		watcher: w,
	}
}

type empty struct{}

func (w *WatchWorkQueue) Run(ctx context.Context) error {
	workerErrCh := make(chan error)
	go func() {
		for {
			i, shutdown := w.queue.Get()
			if shutdown {
				return
			}
			defer w.queue.Done(i)

			if err := w.do(ctx); err != nil {
				workerErrCh <- err
			}
		}
	}()

	defer w.queue.ShutDown()

	for {
		select {
		case <-debounceCh(
			w.watcher.Events(), watchDebounceTime):
			w.queue.Add(empty{})

			return nil
		case err := <-w.watcher.Errors():
			return fmt.Errorf("watcher error: %w", err)

		case err := <-workerErrCh:
			return fmt.Errorf("worker crashed: %w", err)
		}
	}
}
