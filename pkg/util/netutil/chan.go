package netutil

// SafeWrite attempts a non-blocking send to a channel. Returns false if the channel is full.
func SafeWrite[T any](c chan<- T, value T, fallback ...func(v T)) bool {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	select {
	case c <- value:
		return true
	default:
		for _, f := range fallback {
			f(value)
		}
		return false
	}
}

// SafeClose closes a channel without panicking if already closed.
func SafeClose[T any](c chan T) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	close(c)
}
