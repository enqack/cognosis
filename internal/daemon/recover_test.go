package daemon

import (
	"log/slog"
	"testing"
)

func TestRecoverPanic(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	run := func() (err error) {
		defer RecoverPanic(log, "test", func(e error) { err = e })
		panic("boom")
	}

	err := run()
	if err == nil {
		t.Fatal("expected RecoverPanic to turn the panic into an error, got nil")
	}
}

func TestRecoverPanicNoPanic(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	run := func() (err error) {
		defer RecoverPanic(log, "test", func(e error) { err = e })
		return nil
	}

	if err := run(); err != nil {
		t.Fatalf("expected no error when nothing panics, got %v", err)
	}
}
