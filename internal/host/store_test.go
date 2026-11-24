package host

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestStoreRetriesFailedTranscriptWrite(t *testing.T) {
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.timer != nil {
			store.timer.Stop()
			store.timer = nil
		}
	})

	const id = "retry"
	path := store.path(id)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	store.create(Meta{ID: id})
	store.append(id, Event{"type": "message_delta", "text": "before"})
	store.flush()

	store.mu.Lock()
	transcriptPending, indexPending := store.dirty[id], store.dirty[indexKey]
	retryScheduled := store.timer != nil
	store.mu.Unlock()
	if !transcriptPending || indexPending || !retryScheduled {
		t.Fatalf("after failed write: transcript pending=%v, index pending=%v, retry scheduled=%v", transcriptPending, indexPending, retryScheduled)
	}

	store.append(id, Event{"type": "message_delta", "text": " and after"})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(flushRetryDelay + 3*time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var events []Event
		if err := json.Unmarshal(data, &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0]["text"] != "before and after" {
			t.Fatalf("retry wrote stale events: %v", events)
		}
		return
	}
	t.Fatal("retry did not write the transcript")
}
