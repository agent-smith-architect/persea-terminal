package terminal

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
)

// NonblockingFDWriter uses O_NONBLOCK plus select(2). A private pipe is the
// independent cancellation wakeup; the target descriptor is never closed.
type NonblockingFDWriter struct {
	target    *os.File
	targetFD  int
	wakeR     *os.File
	wakeRFD   int
	wakeW     *os.File
	wakeWFD   int
	closeOnce sync.Once
	closeErr  error
}

func NewNonblockingFDWriter(target *os.File) (*NonblockingFDWriter, error) {
	if target == nil {
		return nil, ErrMalformed
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fds := []int{int(target.Fd()), int(r.Fd()), int(w.Fd())}
	for _, fd := range fds {
		if fd < 0 || fd >= len(syscall.FdSet{}.Bits)*64 {
			r.Close()
			w.Close()
			return nil, ErrMalformed
		}
		if err := syscall.SetNonblock(fd, true); err != nil {
			r.Close()
			w.Close()
			return nil, err
		}
	}
	return &NonblockingFDWriter{target: target, targetFD: fds[0], wakeR: r, wakeRFD: fds[1], wakeW: w, wakeWFD: fds[2]}, nil
}

func fdSet(set *syscall.FdSet, fd int)        { set.Bits[fd/64] |= int64(1) << uint(fd%64) }
func fdIsSet(set *syscall.FdSet, fd int) bool { return set.Bits[fd/64]&(int64(1)<<uint(fd%64)) != 0 }

func (w *NonblockingFDWriter) WriteContext(ctx context.Context, data []byte) (int, error) {
	if ctx == nil {
		return 0, ErrMalformed
	}
	w.drainWake()
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_, _ = syscall.Write(w.wakeWFD, []byte{1})
		case <-done:
		}
	}()
	defer func() {
		close(done)
		<-watcherDone
	}()
	total, targetFD, wakeFD := 0, w.targetFD, w.wakeRFD
	for total < len(data) {
		if ctx.Err() != nil {
			return total, ErrCanceled
		}
		n, err := syscall.Write(targetFD, data[total:])
		if n > 0 {
			total += n
			continue
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return total, err
		}
		var readSet, writeSet syscall.FdSet
		fdSet(&readSet, wakeFD)
		fdSet(&writeSet, targetFD)
		maxFD := targetFD
		if wakeFD > maxFD {
			maxFD = wakeFD
		}
		if _, err := syscall.Select(maxFD+1, &readSet, &writeSet, nil, nil); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return total, err
		}
		if fdIsSet(&readSet, wakeFD) {
			var b [64]byte
			_, _ = syscall.Read(wakeFD, b[:])
			if ctx.Err() != nil {
				return total, ErrCanceled
			}
		}
	}
	return total, nil
}

func (w *NonblockingFDWriter) drainWake() {
	var b [64]byte
	for {
		_, err := syscall.Read(w.wakeRFD, b[:])
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return
		}
		if err != nil {
			return
		}
	}
}

func (w *NonblockingFDWriter) Close() error {
	w.closeOnce.Do(func() { w.closeErr = errors.Join(w.wakeR.Close(), w.wakeW.Close()) })
	return w.closeErr
}
