package util

func SafeRead[T any](c chan T) (T, bool) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	tt, ok := <-c
	return tt, ok
}

func SafeWrite[T any](c chan<- T, value T) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	select {
	case c <- value:
	default:
	}
}

func SafeClose[T any](c chan T) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	close(c)
}
