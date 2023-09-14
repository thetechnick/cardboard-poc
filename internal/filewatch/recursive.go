package filewatch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

type RecursiveWatcher struct {
	events chan fsnotify.Event
	errors chan error

	close     chan struct{}
	closeOnce sync.Once
	watcher   *fsnotify.Watcher
}

func NewRecursiveWatcher() (*RecursiveWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	rw := &RecursiveWatcher{
		events: make(chan fsnotify.Event),
		errors: make(chan error),

		close:   make(chan struct{}),
		watcher: watcher,
	}
	go rw.run()
	return rw, nil
}

func (w *RecursiveWatcher) Events() <-chan fsnotify.Event {
	return w.events
}

func (w *RecursiveWatcher) Errors() <-chan error {
	return w.errors
}

func (w *RecursiveWatcher) Add(name string) error {
	return w.watch(name, false)
}

func (w *RecursiveWatcher) Remove(name string) error {
	return w.watch(name, true)
}

func (w *RecursiveWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.close)
		w.watcher.Close()
	})
	return nil
}

func (w *RecursiveWatcher) run() {
	defer close(w.events)
	defer close(w.errors)

	for {
		select {
		case e := <-w.watcher.Events:
			s, err := os.Stat(e.Name)
			if err == nil && s != nil && s.IsDir() {
				if e.Op&fsnotify.Create != 0 {
					if err := w.watch(e.Name, false); err != nil {
						w.errors <- err
					}
				}
			}
			w.events <- e

		case e := <-w.watcher.Errors:
			w.errors <- e

		case <-w.close:
			return
		}
	}
}

func (m *RecursiveWatcher) watch(path string, remove bool) error {
	return filepath.WalkDir(
		path,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}

			if strings.HasPrefix(filepath.Base(path), ".") &&
				d.Name() != "." {
				return filepath.SkipDir
			}

			path = filepath.Clean(path)
			return m.watcher.Add(path)
		})
}
