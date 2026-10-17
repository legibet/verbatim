package proxy

import (
	"time"

	"verbatim/internal/route"
)

// Stats is the gateway's live in-memory state, for /ui/api/providers and
// `verbatim status`. Everything here is a point-in-time snapshot.
type Stats struct {
	// The maps are keyed by config.EndpointName.
	Breakers   map[string]route.State   // endpoints never seen have no entry
	Pins       map[string]int           // live pins per endpoint
	PinsTotal  int                      // live pins
	ModelsAge  map[string]time.Duration // age of each endpoint's cached model list; no entry = never fetched
	LogDropped int64                    // log rows dropped because the writer queue was full
}
