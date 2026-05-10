package executor

import (
	"fmt"
	"testing"
	"time"
)

func TestCodexWebsocketSession_ClearActiveDoesNotCloseChannel(t *testing.T) {
	sess := &codexWebsocketSession{
		sessionID: "test-session",
	}

	readCh := make(chan codexWebsocketRead, 16)
	sess.setActive(readCh)

	sess.activeMu.Lock()
	ch := sess.activeCh
	sess.activeMu.Unlock()

	if ch == nil {
		t.Fatal("expected activeCh to be non-nil after setActive")
	}

	errMsg := "upstream read error"
	select {
	case ch <- codexWebsocketRead{conn: nil, err: fmt.Errorf("%s", errMsg)}:
	default:
		t.Fatal("expected to be able to send error to readCh")
	}

	sess.clearActive(readCh)

	sess.activeMu.Lock()
	afterCh := sess.activeCh
	sess.activeMu.Unlock()

	if afterCh != nil {
		t.Fatal("expected activeCh to be nil after clearActive")
	}

	select {
	case ev, ok := <-readCh:
		if !ok {
			t.Fatal("readCh should NOT be closed after clearActive - this would cause send-on-closed-channel panics")
		}
		if ev.err == nil || ev.err.Error() != errMsg {
			t.Fatalf("expected error message %q, got %v", errMsg, ev.err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting to read error from readCh")
	}
}

func TestCodexWebsocketSession_ClearActiveCancelsContext(t *testing.T) {
	sess := &codexWebsocketSession{
		sessionID: "test-session",
	}

	readCh := make(chan codexWebsocketRead, 16)
	sess.setActive(readCh)

	sess.activeMu.Lock()
	done := sess.activeDone
	sess.activeMu.Unlock()

	select {
	case <-done:
		t.Fatal("done channel should not be closed before clearActive")
	default:
	}

	sess.clearActive(readCh)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done channel should be closed after clearActive")
	}
}

func TestCodexWebsocketSession_SetActiveReplacesPrevious(t *testing.T) {
	sess := &codexWebsocketSession{
		sessionID: "test-session",
	}

	readCh1 := make(chan codexWebsocketRead, 16)
	sess.setActive(readCh1)

	sess.activeMu.Lock()
	done1 := sess.activeDone
	sess.activeMu.Unlock()

	readCh2 := make(chan codexWebsocketRead, 16)
	sess.setActive(readCh2)

	sess.activeMu.Lock()
	done2 := sess.activeDone
	ch2 := sess.activeCh
	sess.activeMu.Unlock()

	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatal("previous done channel should be closed after setActive replaces it")
	}

	if ch2 != readCh2 {
		t.Fatal("expected activeCh to be readCh2 after setActive")
	}

	select {
	case <-done2:
		t.Fatal("new done channel should not be closed")
	default:
	}

	select {
	case readCh1 <- codexWebsocketRead{err: fmt.Errorf("old message")}:
	default:
	}

	select {
	case readCh2 <- codexWebsocketRead{err: fmt.Errorf("new message")}:
	default:
	}

	ev1, ok1 := <-readCh1
	if !ok1 {
		t.Fatal("readCh1 should still be open")
	}
	if ev1.err == nil || ev1.err.Error() != "old message" {
		t.Fatalf("expected 'old message', got %v", ev1.err)
	}

	ev2, ok2 := <-readCh2
	if !ok2 {
		t.Fatal("readCh2 should still be open")
	}
	if ev2.err == nil || ev2.err.Error() != "new message" {
		t.Fatalf("expected 'new message', got %v", ev2.err)
	}
}

func TestCodexWebsocketSession_ConcurrentReadLoopNoPanic(t *testing.T) {
	sess := &codexWebsocketSession{
		sessionID: "test-session",
	}

	readCh := make(chan codexWebsocketRead, 16)
	sess.setActive(readCh)

	panicCh := make(chan interface{}, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		for i := 0; i < 100; i++ {
			sess.activeMu.Lock()
			currentCh := sess.activeCh
			currentDone := sess.activeDone
			sess.activeMu.Unlock()
			if currentCh == nil {
				continue
			}
			select {
			case currentCh <- codexWebsocketRead{conn: nil, err: nil}:
			case <-currentDone:
			default:
			}
		}
	}()

	select {
	case readCh <- codexWebsocketRead{conn: nil, err: fmt.Errorf("read error")}:
	default:
	}

	sess.clearActive(readCh)

	select {
	case p := <-panicCh:
		t.Fatalf("goroutine panicked: %v", p)
	case <-time.After(2 * time.Second):
	}
}
