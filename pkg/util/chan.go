package util

func SafeRead[T any](c chan T) (T, bool) {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	tt, ok := <-c
	return tt, ok
}

func SafeWrite[T any](c chan<- T, value T) bool {
	defer func() {
		if r := recover(); r != nil {
		}
	}()
	select {
	case c <- value:
		return true
	default:
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
