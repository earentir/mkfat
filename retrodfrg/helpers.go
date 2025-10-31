package retrodfrg

import (
	"io"
	"time"
)

// WriteSpan writes a buffer at an absolute sector offset while updating the UI progressively.
func WriteSpan(w io.WriterAt, absStart int64, buf []byte, u *UI) error {
	const chunk = 1 << 20
	wr := int64(0)

	var dt time.Duration
	if u.emulate {
		secBytes := 512.0
		dt = time.Duration((secBytes) / u.rateBps * float64(time.Second))
		if dt < time.Millisecond {
			dt = time.Millisecond
		}
	}

	for wr < int64(len(buf)) {
		n := int64(len(buf)) - wr
		if n > chunk {
			n = chunk
		}
		if _, err := w.WriteAt(buf[wr:wr+n], (absStart*512)+wr); err != nil {
			return err
		}
		secs := n / 512
		if secs <= 0 {
			secs = 1
		}
		for i := int64(0); i < secs; i++ {
			if u.IsStopped() {
				return ErrInterrupted
			}
			u.MarkRange(absStart+wr/512+i, 1)
			// Throttle UI updates on real devices
			if u.emulate || (u.updateEvery <= 1) || ((wr/512+i)%int64(u.updateEvery) == 0) {
				u.LayoutAndDraw()
			}
			// Per-sector sync on real devices when supported
			if !u.emulate && u.syncMode == "sector" {
				if sw, ok := any(w).(interface{ Sync() error }); ok {
					_ = sw.Sync()
				}
			}
			if u.emulate {
				select {
				case <-u.stopChan:
					return ErrInterrupted
				case <-time.After(dt):
				}
			}
		}
		wr += n
	}
	u.LayoutAndDraw()
	return nil
}

// ZeroSpan writes zeroes across a span of sectors while updating the UI.
func ZeroSpan(w io.WriterAt, absStart, sectors int64, u *UI) error {
	const zSize = 1 << 20
	z := make([]byte, zSize)
	written := int64(0)
	bytes := sectors * 512

	var dt time.Duration
	if u.emulate {
		secBytes := 512.0
		dt = time.Duration((secBytes) / u.rateBps * float64(time.Second))
		if dt < time.Millisecond {
			dt = time.Millisecond
		}
	}

	for written < bytes {
		k := bytes - written
		if k > zSize {
			k = zSize
		}
		if _, err := w.WriteAt(z[:k], (absStart*512)+written); err != nil {
			return err
		}
		secs := k / 512
		if secs <= 0 {
			secs = 1
		}
		for i := int64(0); i < secs; i++ {
			if u.IsStopped() {
				return ErrInterrupted
			}
			u.MarkRange(absStart+written/512+i, 1)
			if u.emulate || (u.updateEvery <= 1) || ((written/512+i)%int64(u.updateEvery) == 0) {
				u.LayoutAndDraw()
			}
			if !u.emulate && u.syncMode == "sector" {
				if sw, ok := any(w).(interface{ Sync() error }); ok {
					_ = sw.Sync()
				}
			}
			if u.emulate {
				select {
				case <-u.stopChan:
					return ErrInterrupted
				case <-time.After(dt):
				}
			}
		}
		written += k
	}
	u.LayoutAndDraw()
	return nil
}

// WaitWithStop waits briefly while allowing early interruption.
func WaitWithStop(u *UI) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-u.stopChan:
		return ErrInterrupted
	case <-timer.C:
		return nil
	}
}
