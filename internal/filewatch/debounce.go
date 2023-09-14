package filewatch

import "time"

const watchDebounceTime = 50 * time.Millisecond

// debounce will combine all received events
// within the given threshold.
func debounceCh[T any](
	ch <-chan T, threshold time.Duration,
) <-chan []T {
	out := make(chan []T)
	go func() {
		defer close(out)

		var (
			t             = time.NewTimer(threshold)
			dispatchQueue []T
		)
		t.Stop()
		defer t.Stop()

		for {
			select {
			case e, ok := <-ch:
				if !ok {
					return
				}

				if len(dispatchQueue) == 0 {
					t.Reset(threshold)
				}
				dispatchQueue = append(dispatchQueue, e)

			case <-t.C:
				out <- dispatchQueue
				dispatchQueue = nil
			}
		}
	}()
	return out
}
