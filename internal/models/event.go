// Package models — event.go defines the CanonicalEvent, a group of related markets
// from a single venue that share a parent event/series.
package models

import "time"

// CanonicalEvent groups child markets that belong to the same venue-level event.
// Matching happens at the event level first, then child markets are paired within
// matched events.
type CanonicalEvent struct {
	// Identity
	VenueID    VenueID `json:"venue_id"`
	EventID    string  `json:"event_id"`     // venue event ticker/slug
	EventTitle string  `json:"event_title"`  // human-readable event title
	Category   string  `json:"category,omitempty"`
	ImageURL   string  `json:"image_url,omitempty"`

	// Resolution — earliest close date across child markets
	ResolutionDate *time.Time `json:"resolution_date,omitempty"`

	// Child markets belonging to this event
	Markets []*CanonicalMarket `json:"markets"`
}

// GroupByEvent takes a flat list of canonical markets and groups them into events.
// Markets are grouped by VenueEventTicker (event ID). Markets without an event
// ticker become singleton events keyed by their own market ID.
func GroupByEvent(markets []*CanonicalMarket) []*CanonicalEvent {
	eventMap := map[string]*CanonicalEvent{} // key = venueID + "|" + eventID
	var order []string                       // preserve insertion order

	for _, m := range markets {
		eventID := m.VenueEventTicker
		if eventID == "" {
			// Singleton: market is its own event
			eventID = m.VenueMarketID
		}

		key := string(m.VenueID) + "|" + eventID
		ev, ok := eventMap[key]
		if !ok {
			ev = &CanonicalEvent{
				VenueID:    m.VenueID,
				EventID:    eventID,
				EventTitle: m.VenueEventTitle,
				Category:   m.Category,
				ImageURL:   m.ImageURL,
			}
			eventMap[key] = ev
			order = append(order, key)
		}

		ev.Markets = append(ev.Markets, m)

		// Use earliest resolution date across children
		if m.HasResolutionDate() {
			if ev.ResolutionDate == nil || m.ResolutionDate.Before(*ev.ResolutionDate) {
				t := *m.ResolutionDate
				ev.ResolutionDate = &t
			}
		}

		// Fill event title from first market if not already set
		if ev.EventTitle == "" {
			ev.EventTitle = m.Title
		}
	}

	events := make([]*CanonicalEvent, 0, len(order))
	for _, key := range order {
		events = append(events, eventMap[key])
	}
	return events
}
