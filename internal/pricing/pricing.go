// Package pricing puts a list-price cost on logged usage
// (docs/usage-and-pricing.md). Prices come from llm-metadata / models.dev
// provider files mirrored into a local cache by Sync, merged with a local
// overrides file, and are looked up by exact model name only.
package pricing

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"verbatim/internal/protocol"
)

// Price is the cost entry of one model. All prices are USD per 1M tokens.
type Price struct {
	Source     string   // provider id of the file the cost came from, or "overrides"
	Input      float64  // uncached input
	Output     *float64 // output, reasoning included; nil: the model has no text output
	CacheRead  *float64 // nil: unknown; a cost with cache reads is not estimated
	CacheWrite *float64 // nil: priced at Input
	Reasoning  *float64 // informational only; reasoning tokens are priced as Output
	// CacheWrite1hMultiple is the provider's billing.cacheWrite1h: 1-hour cache
	// writes cost Input times this. nil: priced like other cache writes.
	CacheWrite1hMultiple *float64
	// ImageInput and ImageOutput price the image tokens of an image model,
	// which are part of its input and output tokens. Only the overrides file
	// sets them: the provider files have no image prices.
	ImageInput, ImageOutput *float64
	Tiers                   []Tier // ascending by Size
}

// HasImagePrice reports whether the price has an image price. An images
// request's token counts mix text and image tokens, so only such a price
// prices it.
func (p Price) HasImagePrice() bool { return p.ImageInput != nil || p.ImageOutput != nil }

// Tier is a context-size price tier. It applies when the request's context
// exceeds Size; a nil price inherits the base Price.
type Tier struct {
	Size                                 int64
	Input, Output, CacheRead, CacheWrite *float64
}

// Table is an immutable price table with the configured aliases.
type Table struct {
	prices  map[string]Price
	aliases map[string]string
}

// providerFile is the llm-metadata / models.dev provider file shape, which
// the overrides file shares.
type providerFile struct {
	ID      string `json:"id"`
	Billing *struct {
		CacheWrite1h *float64 `json:"cacheWrite1h"`
	} `json:"billing"`
	Models map[string]struct {
		Cost *costJSON `json:"cost"`
	} `json:"models"`
}

type costJSON struct {
	Input       *float64 `json:"input"`
	Output      *float64 `json:"output"`
	CacheRead   *float64 `json:"cache_read"`
	CacheWrite  *float64 `json:"cache_write"`
	Reasoning   *float64 `json:"reasoning"`
	ImageInput  *float64 `json:"image_input"`
	ImageOutput *float64 `json:"image_output"`
	Currency    string   `json:"currency"`
	Tiers       []struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
		Tier       struct {
			Size int64  `json:"size"`
			Type string `json:"type"`
		} `json:"tier"`
	} `json:"tiers"`
}

// cacheFiles maps each source URL to its cache file name, the last element of
// the URL path (with ".json" added if missing). Sync, Load, and Watch must
// agree on it, and two sources may not share a name.
func cacheFiles(sources []string) ([]string, error) {
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("pricing.sources: %w", err)
		}
		name := path.Base(u.Path)
		if name == "/" || name == "." {
			return nil, fmt.Errorf("pricing.sources: %q has no file name in its path", s)
		}
		if !strings.HasSuffix(name, ".json") {
			name += ".json"
		}
		if i := slices.Index(names, name); i >= 0 {
			return nil, fmt.Errorf("pricing.sources: %q and %q share the cache file name %s", sources[i], s, name)
		}
		names = append(names, name)
	}
	return names, nil
}

// Load builds the price table from the cached source files, in the order of
// sources (a later source wins for a model id both define), then the
// overrides file. A source not synced yet and a missing overrides file are
// not errors. Warnings name the skipped non-USD models, unsynced sources, and
// ids defined by more than one source.
func Load(cacheDir string, sources []string, overridesPath string, aliases map[string]string) (*Table, []string, error) {
	names, err := cacheFiles(sources)
	if err != nil {
		return nil, nil, err
	}
	t := &Table{prices: map[string]Price{}, aliases: maps.Clone(aliases)}
	var warnings []string
	for i, name := range names {
		file := filepath.Join(cacheDir, name)
		f, err := readFile(file)
		if os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("pricing source %s is not synced yet (run verbatim pricing sync)", sources[i]))
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		source := cmp.Or(f.ID, strings.TrimSuffix(name, ".json"))
		var multiple *float64
		if f.Billing != nil {
			multiple = f.Billing.CacheWrite1h
		}
		for _, id := range slices.Sorted(maps.Keys(f.Models)) {
			c := f.Models[id].Cost
			if c == nil {
				continue
			}
			p, warn := newPrice(id, source, *c, multiple)
			if warn != "" {
				warnings = append(warnings, warn)
				continue
			}
			if prev, ok := t.prices[id]; ok {
				warnings = append(warnings, fmt.Sprintf("model %q is priced by both %s and %s; using %s", id, prev.Source, source, source))
			}
			t.prices[id] = p
		}
	}

	if overridesPath == "" {
		return t, warnings, nil
	}
	f, err := readFile(overridesPath)
	if os.IsNotExist(err) {
		return t, warnings, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(f.Models)) {
		c := f.Models[id].Cost
		if c == nil {
			continue
		}
		// The override replaces the cost; the 1-hour multiple is provider
		// billing, so it comes from the overrides file if it declares one,
		// else from the source that priced the model before.
		multiple := t.prices[id].CacheWrite1hMultiple
		if f.Billing != nil && f.Billing.CacheWrite1h != nil {
			multiple = f.Billing.CacheWrite1h
		}
		p, warn := newPrice(id, "overrides", *c, multiple)
		if warn != "" {
			warnings = append(warnings, warn)
			continue
		}
		t.prices[id] = p
	}
	return t, warnings, nil
}

func readFile(name string) (providerFile, error) {
	var f providerFile
	b, err := os.ReadFile(name)
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("pricing file %s: %w", name, err)
	}
	return f, nil
}

// newPrice converts one cost object; a non-empty warning means the model is
// skipped.
func newPrice(id, source string, c costJSON, multiple *float64) (Price, string) {
	if c.Currency != "" && c.Currency != "USD" {
		return Price{}, fmt.Sprintf("model %q in %s is priced in %s, not USD; skipped", id, source, c.Currency)
	}
	if c.Input == nil || c.Output == nil && c.ImageOutput == nil {
		return Price{}, fmt.Sprintf("model %q in %s has no input or output price; skipped", id, source)
	}
	p := Price{
		Source:               source,
		Input:                *c.Input,
		Output:               c.Output,
		CacheRead:            c.CacheRead,
		CacheWrite:           c.CacheWrite,
		Reasoning:            c.Reasoning,
		CacheWrite1hMultiple: multiple,
		ImageInput:           c.ImageInput,
		ImageOutput:          c.ImageOutput,
	}
	for _, t := range c.Tiers {
		if t.Tier.Type != "context" {
			return Price{}, fmt.Sprintf("model %q in %s has a %q price tier, only context tiers are supported; skipped", id, source, t.Tier.Type)
		}
		p.Tiers = append(p.Tiers, Tier{Size: t.Tier.Size, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite})
	}
	slices.SortFunc(p.Tiers, func(a, b Tier) int { return cmp.Compare(a.Size, b.Size) })
	return p, ""
}

// Lookup finds the price for a request (docs/usage-and-pricing.md): the
// client model name, then its alias, then the upstream model name, then its
// alias. key is the table entry used.
func (t *Table) Lookup(clientModel, upstreamModel string) (key string, p Price, ok bool) {
	for _, name := range []string{clientModel, upstreamModel} {
		if name == "" {
			continue
		}
		if p, ok := t.prices[name]; ok {
			return name, p, true
		}
		if alias, ok := t.aliases[name]; ok {
			if p, ok := t.prices[alias]; ok {
				return alias, p, true
			}
		}
	}
	return "", Price{}, false
}

// Prices reports whether the table has a price for a request of the protocol
// logged with these model names.
func (t *Table) Prices(proto protocol.ID, clientModel, upstreamModel string) bool {
	_, p, ok := t.Lookup(clientModel, upstreamModel)
	return ok && (proto != protocol.Images || p.HasImagePrice())
}

// Models returns the priced model ids, sorted.
func (t *Table) Models() []string {
	return slices.Sorted(maps.Keys(t.prices))
}

// unitPrices are the per-column prices a cost was computed with, after tier
// selection and fallbacks; this is the log's price_json.
type unitPrices struct {
	Source       string   `json:"source"`
	TierSize     int64    `json:"tier_size,omitempty"` // context bound of the tier used; 0 for base prices
	Input        float64  `json:"input"`
	CacheRead    *float64 `json:"cache_read"` // null when the model has no cache read price
	CacheWrite   float64  `json:"cache_write"`
	CacheWrite1h float64  `json:"cache_write_1h"`
	Output       *float64 `json:"output"`                 // null when the model has no text output
	ImageInput   *float64 `json:"image_input,omitempty"`  // image models only
	ImageOutput  *float64 `json:"image_output,omitempty"` // image models only
}

// Cost computes the USD cost of a response (docs/usage-and-pricing.md). Image
// tokens are priced at the image prices, the rest of input and output at the
// text prices. ok is false when the usage is incomplete (input or output
// tokens missing, or an image model's image tokens not reported) or has
// tokens the model has no price for, such as cache reads without a cache read
// price or image tokens without an image price: nothing is estimated.
// priceJSON records the per-1M-token unit prices used.
func Cost(u protocol.Usage, p Price) (usd float64, priceJSON []byte, ok bool) {
	if u.Input == nil || u.Output == nil {
		return 0, nil, false
	}
	n := func(v *int64) float64 {
		if v == nil {
			return 0
		}
		return float64(*v)
	}
	price := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}

	input, output, cacheRead, cacheWrite := p.Input, p.Output, p.CacheRead, p.CacheWrite
	contextTokens := int64(n(u.Input) + n(u.CacheRead) + n(u.CacheWrite))
	var tier *Tier
	for i, t := range p.Tiers {
		if contextTokens > t.Size {
			tier = &p.Tiers[i] // ascending, so the last one exceeded is the highest applicable
		}
	}
	var tierSize int64
	if tier != nil {
		tierSize = tier.Size
		if tier.Input != nil {
			input = *tier.Input
		}
		if tier.Output != nil {
			output = tier.Output
		}
		if tier.CacheRead != nil {
			cacheRead = tier.CacheRead
		}
		if tier.CacheWrite != nil {
			cacheWrite = tier.CacheWrite
		}
	}

	// Image tokens are part of input and output. An image model, one with an
	// image price, needs them reported, except that one without a text
	// output price outputs only images.
	textIn, textOut := n(u.Input), n(u.Output)
	imageIn, imageOut := n(u.InputImage), n(u.OutputImage)
	if p.ImageInput != nil || p.ImageOutput != nil {
		if u.InputImage == nil || u.OutputImage == nil && output != nil {
			return 0, nil, false
		}
		if u.OutputImage == nil {
			imageOut = textOut
		}
	}
	textIn -= imageIn
	textOut -= imageOut
	if textIn < 0 || textOut < 0 || textOut > 0 && output == nil ||
		imageIn > 0 && p.ImageInput == nil || imageOut > 0 && p.ImageOutput == nil {
		return 0, nil, false
	}

	// Without a cache read price only usage with no cache reads is priced.
	var readPrice float64
	switch {
	case cacheRead != nil:
		readPrice = *cacheRead
	case n(u.CacheRead) > 0:
		return 0, nil, false
	}
	up := unitPrices{Source: p.Source, TierSize: tierSize, Input: input, Output: output, CacheRead: cacheRead, CacheWrite: input,
		ImageInput: p.ImageInput, ImageOutput: p.ImageOutput}
	if cacheWrite != nil {
		up.CacheWrite = *cacheWrite
	}
	up.CacheWrite1h = up.CacheWrite
	if p.CacheWrite1hMultiple != nil {
		up.CacheWrite1h = input * *p.CacheWrite1hMultiple
	}

	write1h := n(u.CacheWrite1h)
	usd = (textIn*up.Input +
		imageIn*price(up.ImageInput) +
		n(u.CacheRead)*readPrice +
		(n(u.CacheWrite)-write1h)*up.CacheWrite +
		write1h*up.CacheWrite1h +
		textOut*price(up.Output) +
		imageOut*price(up.ImageOutput)) / 1e6
	priceJSON, _ = json.Marshal(up)
	return usd, priceJSON, true
}
