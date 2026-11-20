package realtime_test

import (
	"slices"
	"testing"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/realtime"
)

func ev(id string, version int64) domain.StatusEvent {
	return domain.StatusEvent{NotificationID: id, Version: version}
}

// TestVersionFilter: an event passes only when its version is above the last
// one sent for the same notification; other notifications' versions do not
// count (K24).
func TestVersionFilter(t *testing.T) {
	type sent struct {
		id      string
		version int64
	}
	for name, tc := range map[string]struct {
		snapshot []domain.StatusEvent
		events   []domain.StatusEvent
		want     []sent
	}{
		"older and equal events after the snapshot are dropped": {
			snapshot: []domain.StatusEvent{ev("a", 3)},
			events:   []domain.StatusEvent{ev("a", 1), ev("a", 2), ev("a", 3), ev("a", 4)},
			want:     []sent{{"a", 4}},
		},
		"a repeated event is sent once": {
			snapshot: []domain.StatusEvent{ev("a", 0)},
			events:   []domain.StatusEvent{ev("a", 1), ev("a", 1), ev("a", 2)},
			want:     []sent{{"a", 1}, {"a", 2}},
		},
		"an event out of order after a newer one is dropped": {
			snapshot: []domain.StatusEvent{ev("a", 0)},
			events:   []domain.StatusEvent{ev("a", 2), ev("a", 1), ev("a", 3)},
			want:     []sent{{"a", 2}, {"a", 3}},
		},
		"versions are per notification": {
			snapshot: []domain.StatusEvent{ev("a", 5), ev("b", 1)},
			events:   []domain.StatusEvent{ev("b", 2), ev("a", 4), ev("a", 6), ev("b", 3)},
			want:     []sent{{"b", 2}, {"a", 6}, {"b", 3}},
		},
		"a notification missing from the snapshot passes": {
			snapshot: nil,
			events:   []domain.StatusEvent{ev("c", 1), ev("c", 1)},
			want:     []sent{{"c", 1}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := realtime.NewFilter()
			for _, s := range tc.snapshot {
				f.Seed(s)
			}
			var got []sent
			for _, e := range tc.events {
				if f.Allow(e) {
					got = append(got, sent{e.NotificationID, e.Version})
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("sent %v, want %v", got, tc.want)
			}
		})
	}
}
