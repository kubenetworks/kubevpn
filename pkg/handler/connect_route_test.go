package handler

import (
	"testing"
	"time"
)

func TestNewTickerResetHandler(t *testing.T) {
	t.Run("handler functions are non-nil", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		handler := newTickerResetHandler(ticker)

		if handler.AddFunc == nil {
			t.Fatal("AddFunc should not be nil")
		}
		if handler.UpdateFunc == nil {
			t.Fatal("UpdateFunc should not be nil")
		}
		if handler.DeleteFunc == nil {
			t.Fatal("DeleteFunc should not be nil")
		}
	})

	t.Run("add event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.AddFunc(nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after AddFunc reset")
		}
	})

	t.Run("update event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.UpdateFunc(nil, nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after UpdateFunc reset")
		}
	})

	t.Run("delete event resets ticker", func(t *testing.T) {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		handler := newTickerResetHandler(ticker)

		handler.DeleteFunc(nil)

		select {
		case <-ticker.C:
			// ticker fired after reset, as expected
		case <-time.After(5 * time.Second):
			t.Fatal("ticker did not fire after DeleteFunc reset")
		}
	})
}
