package controlservice

import "testing"

func TestEventFeedRetainsBoundedHistory(t *testing.T) {
	feed := newEventFeed[int](2)

	feed.publish(1)
	feed.publish(2)
	feed.publish(3)

	history := feed.snapshot()
	if got, want := len(history), 2; got != want {
		t.Fatalf("unexpected history length: got %d want %d", got, want)
	}
	if got, want := history[0], 2; got != want {
		t.Fatalf("unexpected first history item: got %d want %d", got, want)
	}
	if got, want := history[1], 3; got != want {
		t.Fatalf("unexpected second history item: got %d want %d", got, want)
	}
}

func TestEventFeedDropsSlowSubscriber(t *testing.T) {
	feed := newEventFeed[int](4)
	done := make(chan struct{})

	_, updates, subID := feed.subscribe(done, 1)
	feed.publish(1)
	feed.publish(2)
	feed.unsubscribe(subID)

	if got, want := <-updates, 1; got != want {
		t.Fatalf("unexpected first update: got %d want %d", got, want)
	}
	if _, ok := <-updates; ok {
		t.Fatal("expected subscriber channel to be closed after overflow")
	}
}
