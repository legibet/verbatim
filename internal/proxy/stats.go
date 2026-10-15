package proxy

import (
	"time"

	"verbatim/internal/route"
)

// Stats is the gateway's live in-memory state, for /ui/api/providers and
// `verbatim status`. Everything here is a point-in-time snapshot.
type Stats struct {
	Breakers   map[string]route.State   // per provider; providers never seen have no entry
	Pins       map[string]int           // live pins per provider
	PinsTotal  int                      // live pins
	ModelsAge  map[string]time.Duration // age of each provider's cached model list; no entry = never fetched
	LogDropped int64                    // log rows dropped because the writer queue was full
}
