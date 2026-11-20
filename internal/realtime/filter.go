package realtime

import "github.com/ardaklc20/event-driven-notification-system/internal/domain"

// Filter keeps the last version sent for each notification of one
// subscription. An event passes only when its version is higher than the last
// one sent for the same notification; versions of different notifications
// are never compared (K24). It is used by one writer, without a lock.
type Filter struct {
	last map[string]int64
}

// NewFilter returns an empty filter.
func NewFilter() *Filter { return &Filter{last: make(map[string]int64)} }

// Seed records a snapshot's version of one notification.
func (f *Filter) Seed(ev domain.StatusEvent) { f.last[ev.NotificationID] = ev.Version }

// Allow reports whether ev is newer than what was sent for its notification
// and, if so, records it as sent.
func (f *Filter) Allow(ev domain.StatusEvent) bool {
	if last, ok := f.last[ev.NotificationID]; ok && ev.Version <= last {
		return false
	}
	f.last[ev.NotificationID] = ev.Version
	return true
}
