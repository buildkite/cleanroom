package controlservice

type eventFeed[T any] struct {
	history     []T
	subscribers map[int]chan T
	nextSubID   int
	limit       int
}

func newEventFeed[T any](limit int) eventFeed[T] {
	return eventFeed[T]{
		subscribers: map[int]chan T{},
		limit:       limit,
	}
}

func (f *eventFeed[T]) snapshot() []T {
	return append([]T(nil), f.history...)
}

func (f *eventFeed[T]) subscribe(done <-chan struct{}, buffer int) ([]T, <-chan T, int) {
	history := f.snapshot()
	updates := make(chan T, buffer)
	subID := f.nextSubID
	f.nextSubID++
	if f.subscribers == nil {
		f.subscribers = map[int]chan T{}
	}

	select {
	case <-done:
		close(updates)
	default:
		f.subscribers[subID] = updates
	}

	return history, updates, subID
}

func (f *eventFeed[T]) unsubscribe(subID int) {
	if ch, ok := f.subscribers[subID]; ok {
		delete(f.subscribers, subID)
		close(ch)
	}
}

func (f *eventFeed[T]) publish(item T) {
	f.history = appendBounded(f.history, item, f.limit)
	for id, ch := range f.subscribers {
		select {
		case ch <- item:
		default:
			close(ch)
			delete(f.subscribers, id)
		}
	}
}

func (f *eventFeed[T]) closeSubscribers() {
	for id, ch := range f.subscribers {
		close(ch)
		delete(f.subscribers, id)
	}
}
