package catalog

// Providers Prowl can route to that the upstream directories never listed.
//
// The merged directory comes from the public free-API lists and LiteLLM, and
// those lists do not cover every provider this gateway has a wire adapter for.
// Leaving the gap meant the Providers page hid two dozen providers the product
// can actually serve, while the Add-key dialog offered them from a separate
// hardcoded list - two vocabularies that disagreed, so a provider you could
// add did not appear in the catalogue you browsed.
//
// Every field here is carried from repository data (the adapter registry and
// the key dialog's own labels), not estimated. Where a count is genuinely
// unknown it stays zero and the UI renders it as unknown rather than as none:
// inventing a free-model number would misrepresent what the provider offers.

// prowlDirectoryProviders contains entries the upstream source lists omit.
// Most have a Prowl adapter; listed-only exceptions say so explicitly.
var prowlDirectoryProviders = []DirectoryEntry{
	{
		ID: "sail", Name: "Sail Research", Class: ClassCredits,
		Friction: "card", APIKeyURL: "https://app.sailresearch.com",
		Note: "$5 monthly credit. Listed only: Sail uses a Responses API with background polling that Prowl does not yet implement.",
	},
	{
		ID: "electronhub", Name: "ElectronHub", Class: ClassCredits,
		Friction: "registration", APIKeyURL: "https://app.electronhub.ai",
		Note: "shared credit pool that refills weekly",
	},
	{
		ID: "experiential", Name: "Experiential Labs", Class: ClassCredits,
		Friction: "registration", APIKeyURL: "https://platform.experientiallabs.ai",
		Note: "shared credit pool that refills monthly",
	},
	{
		ID: "router9", Name: "Router9", Class: ClassCredits,
		Friction: "registration", APIKeyURL: "https://www.router9.com",
		Note: "shared credit pool that refills monthly",
	},
	{
		ID: "septor", Name: "Septor Labs", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://septorlabs.com/dashboard",
		Note: "daily quota on its free models",
	},
	{
		ID: "bai", Name: "B.AI", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://b.ai",
		Note: "one promotional free model",
	},
	{
		ID: "radeon", Name: "AMD Radeon Cloud", Class: ClassFree,
		Friction:  "registration",
		APIKeyURL: "https://developer.amd.com.cn/radeon/tokenfactory",
		Note:      "free shared models",
	},
	{
		ID: "pollinations", Name: "Pollinations", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://enter.pollinations.ai",
	},
	{
		ID: "reka", Name: "Reka", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://platform.reka.ai",
	},
	{
		ID: "routeway", Name: "Routeway", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://routeway.ai",
	},
	{
		ID: "bazaarlink", Name: "BazaarLink", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://bazaarlink.ai",
	},
	{
		ID: "ainative", Name: "AINative Studio", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://ainative.studio",
	},
	{
		ID: "requesty", Name: "Requesty", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://www.requesty.ai",
	},
	{
		ID: "navy", Name: "NavyAI", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://api.navy",
	},
	{
		ID: "nara", Name: "NaraRouter", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://router.bynara.id",
	},
	{
		ID: "sealion", Name: "SEA-LION", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://sea-lion.ai",
	},
	{
		ID: "orcarouter", Name: "OrcaRouter", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://www.orcarouter.ai",
	},
	{
		ID: "unorouter", Name: "UnoRouter", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://unorouter.com",
	},
	{
		ID: "xkiro", Name: "xKiro", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://xkiro.com",
	},
	{
		ID: "anyapi", Name: "AnyAPI", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://anyapi.ai",
		// Live testing could not get a free-tier request served, so no quota
		// is claimed for it here either.
		Note: "advertises a daily free allowance that has not served a request under test",
	},
	{
		ID: "aihorde", Name: "AI Horde", Class: ClassFree,
		Friction: "none", APIKeyURL: "https://aihorde.net/register",
		Note: "no key required, but volunteer-powered and slow",
	},
	{
		ID: "longcat", Name: "LongCat", Class: ClassFree,
		Friction: "registration", APIKeyURL: "https://longcat.chat/platform",
		Note: "free daily allowance, email signup",
	},
	{
		ID: "qianfan", Name: "Baidu Qianfan", Class: ClassFree,
		Friction: "phone", APIKeyURL: "https://console.bce.baidu.com/qianfan/overview",
		Note: "free ERNIE models, requires Chinese real-name verification",
	},
	{
		ID: "xfyun", Name: "iFlytek Spark", Class: ClassFree,
		Friction: "phone", APIKeyURL: "https://console.xfyun.cn",
		Note: "free Lite tier, requires Chinese real-name verification",
	},
}

// withProwlDirectoryProviders appends the entries the upstream lists omit,
// skipping any id a source already covers so a real listing always wins.
// It builds a new slice rather than appending to the caller's: appending to a
// cached slice writes into its spare capacity, which is how a shared
// catalogue ends up mutated behind every other reader's back.
func withProwlDirectoryProviders(entries []DirectoryEntry) []DirectoryEntry {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		seen[e.ID] = struct{}{}
	}
	merged := make([]DirectoryEntry, 0, len(entries)+len(prowlDirectoryProviders))
	merged = append(merged, entries...)
	for _, extra := range prowlDirectoryProviders {
		if _, ok := seen[extra.ID]; ok {
			continue
		}
		extra.Routable = extra.ID != "sail"
		extra.Probe = ProbeNone
		extra.Sources = []string{"prowl"}
		merged = append(merged, extra)
	}
	return merged
}
