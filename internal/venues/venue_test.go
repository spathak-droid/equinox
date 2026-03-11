package venues

import (
	"encoding/json"
	"testing"

	"github.com/equinox/internal/models"
)

func TestRawMarketFields(t *testing.T) {
	payload := json.RawMessage(`{"key":"value"}`)
	rm := &RawMarket{
		VenueID:       models.VenuePolymarket,
		VenueMarketID: "test-123",
		FetchCategory: "crypto",
		Payload:       payload,
	}

	if rm.VenueID != models.VenuePolymarket {
		t.Errorf("expected VenuePolymarket, got %s", rm.VenueID)
	}
	if rm.VenueMarketID != "test-123" {
		t.Errorf("expected test-123, got %s", rm.VenueMarketID)
	}
	if rm.FetchCategory != "crypto" {
		t.Errorf("expected crypto, got %s", rm.FetchCategory)
	}

	var parsed map[string]string
	if err := json.Unmarshal(rm.Payload, &parsed); err != nil {
		t.Fatalf("payload unmarshal error: %v", err)
	}
	if parsed["key"] != "value" {
		t.Errorf("expected payload key=value, got %s", parsed["key"])
	}
}
