package util

func SafeRead[T any](c chan T) (T, bool) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	tt, ok := <-c
	return tt, ok
}

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

func SafeClose[T any](c chan T) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	close(c)
}
