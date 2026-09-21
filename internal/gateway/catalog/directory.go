package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

//go:embed directory.json
var directoryFS embed.FS

// Provider classes. They are separate because they answer different questions
// for someone looking for capacity: a free tier costs nothing and refreshes, a
// subscription is already paid for and has no per-key quota to forecast, and a
// local runtime has no quota at all.
const (
	ClassFree    = "free"
	ClassCredits = "credits"
	ClassPaid    = "paid"
	ClassOAuth   = "oauth"
	ClassLocal   = "local"
)

// Probe kinds describe how much a provider will tell us about remaining usage.
const (
	// ProbeKeyEndpoint means the provider publishes a real usage endpoint.
	ProbeKeyEndpoint = "key_endpoint"
	// ProbeHeaders means remaining quota is only learnable from the rate-limit
	// headers on an ordinary response.
	ProbeHeaders = "headers"
	// ProbeNone means the provider publishes nothing, so any number shown
	// would be invented.
	ProbeNone = "none"
)

// DirectoryEntry is one provider in the merged directory.
type DirectoryEntry struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Class      string   `json:"class"`
	Friction   string   `json:"friction"`
	FreeModels int      `json:"freeModels"`
	MaxContext int      `json:"maxContext"`
	Modalities []string `json:"modalities"`
	APIKeyURL  string   `json:"apiKeyUrl"`
	DocsURL    string   `json:"docsUrl"`
	Env        string   `json:"env"`
	Routable   bool     `json:"routable"`
	Sources    []string `json:"sources"`
	Probe      string   `json:"probe"`
	Note       string   `json:"note,omitempty"`
}

type directoryAsset struct {
	Sources   map[string]string `json:"sources"`
	Providers []DirectoryEntry  `json:"providers"`
}

var loadDirectory = sync.OnceValues(func() (directoryAsset, error) {
	raw, err := directoryFS.ReadFile("directory.json")
	if err != nil {
		return directoryAsset{}, err
	}
	var asset directoryAsset
	if err := json.Unmarshal(raw, &asset); err != nil {
		return directoryAsset{}, fmt.Errorf("parse provider directory: %w", err)
	}
	// The upstream lists omit providers this gateway can route to, so they
	// are merged in and the whole list ordered here, where it is done once.
	asset.Providers = sortDirectory(withProwlDirectoryProviders(asset.Providers))
	if len(asset.Providers) == 0 {
		return directoryAsset{}, fmt.Errorf("provider directory is empty")
	}
	return asset, nil
})

// Directory returns every known provider, free tiers first.
// Directory returns the merged provider list, ordered so free capacity leads.
//
// The merge and the sort happen ONCE, inside the cached loader. Doing them
// here appended to and reordered the cached slice on every call: the first
// caller saw all 208 entries and every caller after it saw 184, with two
// dozen providers silently dropped as the sort pushed them past the cached
// length. Each caller gets its own copy for the same reason -- a consumer
// that sorts or filters in place must not be able to corrupt the catalogue
// for everyone else.
func Directory() ([]DirectoryEntry, error) {
	asset, err := loadDirectory()
	if err != nil {
		return nil, err
	}
	out := slices.Clone(asset.Providers)
	// slices.Clone copies each entry struct but not the slices it points at, so
	// Modalities and Sources would still alias the cached asset. A caller that
	// sorted, appended to, or truncated either one in place would corrupt what
	// every later reader sees. Clone both per entry so each caller owns its copy
	// whole, nested slices included.
	for i := range out {
		out[i].Modalities = slices.Clone(out[i].Modalities)
		out[i].Sources = slices.Clone(out[i].Sources)
	}
	return out, nil
}

// directoryClassRank orders the classes by what someone looking for capacity
// cares about: what costs nothing, then what refills, then what runs locally,
// then what a subscription already pays for, and last what only bills.
func directoryClassRank(class string) int {
	switch class {
	case ClassFree:
		return 0
	case ClassCredits:
		return 1
	case ClassLocal:
		return 2
	case ClassOAuth:
		return 3
	case ClassPaid:
		return 4
	default:
		return 5
	}
}

// sortDirectory puts free providers first, then ranks within a class by the
// free models on offer, so the most useful entry of each class leads. Name is
// the final tiebreak, which keeps the order stable across runs.
func sortDirectory(entries []DirectoryEntry) []DirectoryEntry {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if ra, rb := directoryClassRank(a.Class), directoryClassRank(b.Class); ra != rb {
			return ra < rb
		}
		if a.FreeModels != b.FreeModels {
			return a.FreeModels > b.FreeModels
		}
		if a.MaxContext != b.MaxContext {
			return a.MaxContext > b.MaxContext
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return entries
}

// DirectorySources names where the directory came from, so the dashboard can
// credit the lists rather than presenting them as ours.
func DirectorySources() (map[string]string, error) {
	asset, err := loadDirectory()
	if err != nil {
		return nil, err
	}
	return asset.Sources, nil
}
