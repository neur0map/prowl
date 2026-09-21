package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedPresetCatalogue writes a catalogue with the mix a real install has: free
// models, priced models, and models owned by an enrolled login.
func seedPresetCatalogue(t *testing.T, s *Server) {
	t.Helper()
	_, err := s.engine.DB().Exec(`
		INSERT INTO models(platform, model_id, display_name, intelligence_rank,
			speed_rank, context_window, enabled, supports_vision, supports_tools,
			paid_output_per_m, source)
		VALUES
			('groq','free-big','Free Big',4,6,131072,1,0,1,NULL,'catalog'),
			('groq','free-small','Free Small',9,1,32768,1,0,1,NULL,'catalog'),
			('openrouter','paid-cheap','Paid Cheap',6,2,262144,1,1,1,0.5,'catalog'),
			('openrouter','paid-dear','Paid Dear',2,5,1000000,1,1,1,60.0,'catalog'),
			('anthropic','sub-flagship','Sub Flagship',1,5,200000,1,1,1,25.0,'login'),
			('anthropic','sub-small','Sub Small',8,1,200000,1,0,1,4.0,'login')`)
	require.NoError(t, err)
	// Presets only take models a connected provider can serve, so the
	// fixture connects every platform it seeds.
	connectPlatforms(t, s, "groq", "openrouter", "anthropic")
}

// TestPresetsTakeOnlyConnectedProviders proves a preset neither counts nor
// inserts a model whose provider has no usable credential: a set built from a
// preset must route as large as it advertised, not carry members the router
// would drop.
func TestPresetsTakeOnlyConnectedProviders(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)
	// A tool-capable, large-context model on a provider nobody connected.
	_, err := s.engine.DB().Exec(`
		INSERT INTO models(platform, model_id, display_name, intelligence_rank,
			speed_rank, context_window, enabled, supports_vision, supports_tools,
			paid_output_per_m, source)
		VALUES ('mistral','orphan','Orphan',3,3,262144,1,0,1,NULL,'catalog')`)
	require.NoError(t, err)

	count := func() int {
		_, body := do(t, s, http.MethodGet, "/api/profiles/presets", "", authed(tok))
		var payload struct {
			Presets []struct {
				ID     string `json:"id"`
				Models int    `json:"models"`
			} `json:"presets"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &payload))
		for _, p := range payload.Presets {
			if p.ID == "coding" {
				return p.Models
			}
		}
		t.Fatal("no coding preset")
		return 0
	}
	before := count()
	status, created := createFromPreset(t, s, tok, "coding", "coding-a", false)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, before, created.Models)
	members, _ := profileMembers(t, s, created.ID)
	var orphan int64
	require.NoError(t, s.engine.DB().QueryRow(`SELECT id FROM models WHERE model_id = 'orphan'`).Scan(&orphan))
	require.NotContains(t, members, orphan, "an unconnected provider's model must not be taken")

	// Connecting the provider brings its model into the count and the set.
	_, err = s.engine.DB().Exec(`
		INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
		VALUES ('mistral', 'test', 'x', 'y', 'z', 'unknown', 1, 0)`)
	require.NoError(t, err)
	require.Equal(t, before+1, count(), "a freshly connected provider's matching model joins the count")
	_, created = createFromPreset(t, s, tok, "coding", "coding-b", false)
	members, _ = profileMembers(t, s, created.ID)
	require.Contains(t, members, orphan)
}

// presetCreated is the response both save paths return.
type presetCreated struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Models int    `json:"models"`
	CallAs string `json:"callAs"`
	Active bool   `json:"active"`
}

func createFromPreset(t *testing.T, s *Server, tok, presetID, name string, activate bool) (int, presetCreated) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "activate": activate})
	resp, raw := do(t, s, http.MethodPost, "/api/profiles/presets/"+presetID, string(body), authed(tok))
	var out presetCreated
	if resp.StatusCode == http.StatusCreated {
		require.NoError(t, json.Unmarshal([]byte(raw), &out), "body was %s", raw)
	}
	return resp.StatusCode, out
}

func createFromSelection(t *testing.T, s *Server, tok, name string, ids []int64, activate bool) (int, presetCreated) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "modelDbIds": ids, "activate": activate})
	resp, raw := do(t, s, http.MethodPost, "/api/profiles/from-selection", string(body), authed(tok))
	var out presetCreated
	if resp.StatusCode == http.StatusCreated {
		require.NoError(t, json.Unmarshal([]byte(raw), &out), "body was %s", raw)
	}
	return resp.StatusCode, out
}

func enabledModelIDs(t *testing.T, s *Server) []int64 {
	t.Helper()
	rows, err := s.engine.DB().Query(`SELECT id FROM models WHERE enabled = 1 ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

func profileMembers(t *testing.T, s *Server, profileID int64) (modelIDs []int64, positions []int) {
	t.Helper()
	rows, err := s.engine.DB().Query(`
		SELECT pm.model_db_id, pm.position FROM profile_models pm
		WHERE pm.profile_id = ? ORDER BY pm.position ASC`, profileID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid int64
		var pos int
		require.NoError(t, rows.Scan(&mid, &pos))
		modelIDs = append(modelIDs, mid)
		positions = append(positions, pos)
	}
	return
}

// TestChainPresetsCountWhatTheyWouldContain is what makes a preset pickable:
// the count shown must equal the number of rows the same preset inserts, or the
// operator chooses blind and gets something else. Asserted by creating every
// preset and comparing its reported member count to what the GET advertised.
func TestChainPresetsCountWhatTheyWouldContain(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)

	resp, body := do(t, s, http.MethodGet, "/api/profiles/presets", "", authed(tok))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var payload struct {
		Presets []struct {
			ID           string   `json:"id"`
			Name         string   `json:"name"`
			Group        string   `json:"group"`
			Requirements []string `json:"requirements"`
			Models       int      `json:"models"`
		} `json:"presets"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	require.NotEmpty(t, payload.Presets)

	for _, p := range payload.Presets {
		require.NotEmpty(t, p.Name, "%s needs a name to be pickable", p.ID)
		require.Contains(t, []string{"task", "catalogue"}, p.Group,
			"%s must declare its group", p.ID)

		status, created := createFromPreset(t, s, tok, p.ID, p.ID, false)
		require.Equal(t, http.StatusCreated, status, "%s should create", p.ID)
		require.Equal(t, p.Models, created.Models,
			"%s advertised %d members but inserted %d", p.ID, p.Models, created.Models)
	}
}

// TestChainFromPresetArrivesRankedAndCallable covers the whole point: one
// click produces a list that is populated, ordered, and usable from a client
// by name.
func TestChainFromPresetArrivesRankedAndCallable(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)

	resp, body := do(t, s, http.MethodPost, "/api/profiles/presets/subscriptions",
		`{"name":"Subs"}`, authed(tok))
	require.Equal(t, http.StatusCreated, resp.StatusCode, "body was %s", body)

	var created struct {
		ID     int64  `json:"id"`
		Models int    `json:"models"`
		CallAs string `json:"callAs"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, 2, created.Models)
	require.Equal(t, "auto:subs", created.CallAs,
		"the response must hand back the id a client can call")

	// Ranked by capability, densely positioned.
	rows, err := s.engine.DB().Query(`
		SELECT m.model_id, pm.position FROM profile_models pm
		  JOIN models m ON m.id = pm.model_db_id
		 WHERE pm.profile_id = ? ORDER BY pm.position ASC`, created.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var order []string
	var positions []int
	for rows.Next() {
		var id string
		var pos int
		require.NoError(t, rows.Scan(&id, &pos))
		order = append(order, id)
		positions = append(positions, pos)
	}
	require.Equal(t, []string{"sub-flagship", "sub-small"}, order,
		"the most capable model of the list must be tried first")
	require.Equal(t, []int{1, 2}, positions, "positions must be dense")

	// And it must be listed, or nobody can find the id.
	listResp, listBody := do(t, s, http.MethodGet, "/v1/models", "",
		map[string]string{"Authorization": "Bearer " + compatMachineKey})
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	require.Contains(t, listBody, `"auto:subs"`, "a named list must be discoverable")
	require.Contains(t, listBody, `"auto:smart"`, "the sort axes must be discoverable too")
}

// TestFlagshipsPresetUsesTheBestOrdinalRank guards the easy-to-miss rank
// direction: rank 1 is stronger than rank 9, so MAX would select each
// provider's weakest row while still producing a plausible-looking set.
func TestFlagshipsPresetUsesTheBestOrdinalRank(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)

	status, created := createFromPreset(t, s, tok, "flagships", "Flagships", false)
	require.Equal(t, http.StatusCreated, status)
	rows, err := s.engine.DB().Query(`
		SELECT m.model_id FROM profile_models pm
		  JOIN models m ON m.id = pm.model_db_id
		 WHERE pm.profile_id = ? ORDER BY pm.position`, created.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.ElementsMatch(t, []string{"free-big", "paid-dear", "sub-flagship"}, got)
}

// TestChainFromPresetRefusesADuplicateName keeps two lists from sharing the
// name they are called by.
func TestChainFromPresetRefusesADuplicateName(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)

	_, _ = do(t, s, http.MethodPost, "/api/profiles/presets/free", `{"name":"Free"}`, authed(tok))
	resp, _ := do(t, s, http.MethodPost, "/api/profiles/presets/free", `{"name":"Free"}`, authed(tok))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestUnknownPresetIs404 keeps a typo from creating an empty list.
func TestUnknownPresetIs404(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	resp, _ := do(t, s, http.MethodPost, "/api/profiles/presets/nonsense", `{}`, authed(tok))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestPresetRequirementsNameEveryCondition proves the requirements list is
// rendered from the predicate, not written beside it: deep-work's list names
// tool calling, reasoning and its context floor, and no preset is unlabelled.
func TestPresetRequirementsNameEveryCondition(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)

	_, body := do(t, s, http.MethodGet, "/api/profiles/presets", "", authed(tok))
	var payload struct {
		Presets []struct {
			ID           string   `json:"id"`
			Requirements []string `json:"requirements"`
		} `json:"presets"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &payload))

	byID := map[string][]string{}
	for _, p := range payload.Presets {
		require.NotEmpty(t, p.Requirements, "%s must state what it requires", p.ID)
		byID[p.ID] = p.Requirements
	}

	deep := byID["deep-work"]
	require.Contains(t, deep, "Tool calling")
	require.Contains(t, deep, "Reasoning")
	require.Contains(t, deep, "128K+ context",
		"the context floor must appear exactly as the predicate demands")
}

// connectPlatforms stores a usable key per platform so presets, which only
// take models a connected provider can serve, see the seeded rows.
func connectPlatforms(t *testing.T, s *Server, platforms ...string) {
	t.Helper()
	for _, platform := range platforms {
		_, err := s.engine.DB().Exec(`
			INSERT INTO api_keys (platform, label, encrypted_key, iv, auth_tag, status, enabled, created_at)
			VALUES (?, 'test', 'x', 'y', 'z', 'healthy', 1, 0)`, platform)
		require.NoError(t, err)
	}
}

func seedRankedModels(t *testing.T, s *Server, ranks map[string]int) {
	t.Helper()
	connectPlatforms(t, s, "test")
	for modelID, rank := range ranks {
		_, err := s.engine.DB().Exec(`
			INSERT INTO models(platform, model_id, display_name, intelligence_rank,
				speed_rank, context_window, enabled, supports_vision, supports_tools,
				paid_output_per_m, source)
			VALUES('test', ?, ?, ?, 50, 100000, 1, 0, 0, NULL, 'catalog')`,
			modelID, modelID, rank)
		require.NoError(t, err)
	}
}

func writingMembers(t *testing.T, s *Server, tok string) map[string]bool {
	t.Helper()
	status, created := createFromPreset(t, s, tok, "writing", "writing-set", false)
	require.Equal(t, http.StatusCreated, status)
	rows, err := s.engine.DB().Query(`
		SELECT m.model_id FROM profile_models pm
		  JOIN models m ON m.id = pm.model_db_id
		 WHERE pm.profile_id = ? ORDER BY pm.position`, created.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	set := map[string]bool{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		set[id] = true
	}
	return set
}

// TestWritingCutIsAQuantileNotAConstant proves the "most capable" cut moves
// with the catalogue: the same unchanged rank (40) is in the writing set when
// most rows rank below it and out when enough rows rank above it. A constant
// threshold could never include and exclude the same rank.
func TestPresetWritingCutIsAQuantileNotAConstant(t *testing.T) {
	t.Parallel()

	low := testServer(t, Options{MachineKey: compatMachineKey})
	lowTok := session(t, low)
	seedRankedModels(t, low, map[string]int{
		"pivot": 40, "a50": 50, "a60": 60, "a70": 70, "a80": 80, "a90": 90,
	})
	lowSet := writingMembers(t, low, lowTok)

	high := testServer(t, Options{MachineKey: compatMachineKey})
	highTok := session(t, high)
	seedRankedModels(t, high, map[string]int{
		"b10": 10, "b20": 20, "b30": 30, "pivot": 40,
		"b200": 200, "b201": 201, "b202": 202, "b203": 203,
	})
	highSet := writingMembers(t, high, highTok)

	require.True(t, lowSet["pivot"], "rank 40 is top-quartile when every other rank is worse")
	require.False(t, highSet["pivot"], "rank 40 is outside the top quartile when three ranks are better")
}

// TestFromSelectionSavesAnOrderedSet covers the new save path: a hand-picked
// selection becomes a named, densely ordered set; an empty selection and a
// duplicate name are refused; and unknown ids are skipped without failing the
// save, with the response reporting only what actually landed.
func TestFromSelectionSavesAnOrderedSet(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)
	ids := enabledModelIDs(t, s)
	require.GreaterOrEqual(t, len(ids), 5)

	// Empty selection is refused before a useless empty set is created.
	status, _ := createFromSelection(t, s, tok, "Empty", []int64{}, false)
	require.Equal(t, http.StatusBadRequest, status)

	// The given order becomes dense positions.
	want := []int64{ids[2], ids[0], ids[1]}
	status, created := createFromSelection(t, s, tok, "Curated", want, false)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, 3, created.Models)
	require.Equal(t, "auto:curated", created.CallAs)
	members, positions := profileMembers(t, s, created.ID)
	require.Equal(t, want, members, "rows must keep the order they were given")
	require.Equal(t, []int{1, 2, 3}, positions, "positions must be dense")

	// A duplicate name is refused.
	status, _ = createFromSelection(t, s, tok, "Curated", []int64{ids[0]}, false)
	require.Equal(t, http.StatusConflict, status)

	// A bogus id is skipped, not fatal, and the count reflects only what landed.
	status, created = createFromSelection(t, s, tok, "Skips",
		[]int64{ids[3], 999999, ids[4]}, false)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, 2, created.Models, "the bogus id must not be counted")
	members, positions = profileMembers(t, s, created.ID)
	require.Equal(t, []int64{ids[3], ids[4]}, members)
	require.Equal(t, []int{1, 2}, positions, "a skipped id must leave no gap")
}

// TestPresetActivateFlagSwitchesTheActiveSet proves activate:true makes the new set
// the active one and activate:false leaves the previous active set alone.
func TestPresetActivateFlagSwitchesTheActiveSet(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)
	ids := enabledModelIDs(t, s)

	activeID := func() *int64 {
		_, body := do(t, s, http.MethodGet, "/api/profiles/active", "", authed(tok))
		var data struct {
			ActiveProfileID *int64 `json:"activeProfileId"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &data))
		return data.ActiveProfileID
	}

	status, a := createFromSelection(t, s, tok, "A", []int64{ids[0]}, true)
	require.Equal(t, http.StatusCreated, status)
	require.True(t, a.Active, "activate:true must report the set active")
	require.NotNil(t, activeID())
	require.Equal(t, a.ID, *activeID())

	// A preset saved with activate:false must not steal the active slot.
	status, b := createFromPreset(t, s, tok, "free", "B", false)
	require.Equal(t, http.StatusCreated, status)
	require.False(t, b.Active)
	require.Equal(t, a.ID, *activeID(), "the previous active set must remain active")

	// Neither must an unactivated selection.
	status, _ = createFromSelection(t, s, tok, "C", []int64{ids[1]}, false)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, a.ID, *activeID())
}

func activationState(t *testing.T, s *Server) (activeNames []string, settingName string) {
	t.Helper()
	db := s.engine.DB()
	rows, err := db.Query(`SELECT name FROM profiles WHERE active = 1 ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		activeNames = append(activeNames, n)
	}
	var val string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'active_profile_id'`).Scan(&val); err == nil {
		_ = db.QueryRow(`SELECT name FROM profiles WHERE id = ?`, val).Scan(&settingName)
	}
	return
}

// TestSelectionActivateAndActiveEndpointLeaveIdenticalState pins the two paths
// to one routine: creating-and-activating a set from a selection leaves exactly
// the state POST /api/profiles/active leaves for the same set - same active
// flags, same setting row - so the two can never drift.
func TestSelectionActivateAndActiveEndpointLeaveIdenticalState(t *testing.T) {
	t.Parallel()

	build := func(viaEndpoint bool) *Server {
		s := testServer(t, Options{MachineKey: compatMachineKey})
		tok := session(t, s)
		seedPresetCatalogue(t, s)
		ids := enabledModelIDs(t, s)
		status, created := createFromSelection(t, s, tok, "theset",
			[]int64{ids[0], ids[1]}, !viaEndpoint)
		require.Equal(t, http.StatusCreated, status)
		if viaEndpoint {
			body, _ := json.Marshal(map[string]any{"profileId": created.ID})
			resp, _ := do(t, s, http.MethodPost, "/api/profiles/active", string(body), authed(tok))
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}
		return s
	}

	selActive, selSetting := activationState(t, build(false))
	epActive, epSetting := activationState(t, build(true))
	require.Equal(t, epActive, selActive, "active flags must match")
	require.Equal(t, []string{"theset"}, selActive)
	require.Equal(t, epSetting, selSetting, "the active_profile_id setting must match")
	require.Equal(t, "theset", selSetting)
}

func presetMemberIDs(t *testing.T, s *Server, tok, presetID, name string) []int64 {
	t.Helper()
	status, created := createFromPreset(t, s, tok, presetID, name, false)
	require.Equal(t, http.StatusCreated, status, "%s should create", presetID)
	ids, _ := profileMembers(t, s, created.ID)
	return ids
}

// TestFreePresetsUseAccessTierNotOutputPriceNull proves the free and near-free
// presets classify by the catalogue's access tier, not an output-price-null
// shortcut: a subscription row (source='login', no per-token price) and an
// input-priced row (a published input price, output price merely unpublished)
// never enter a free set, a genuinely free row does, and the near-free set
// additionally admits a cheaply-priced paid row while still excluding the
// subscription and input-priced rows.
func TestFreePresetsUseAccessTierNotOutputPriceNull(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	db := s.engine.DB()
	connectPlatforms(t, s, "groq", "anthropic", "openrouter")

	free := seedCat(t, db, catSpec{platform: "groq", modelID: "free", name: "Free", sizeLabel: "Medium"})
	sub := seedCat(t, db, catSpec{platform: "anthropic", modelID: "sub", name: "Sub",
		sizeLabel: "Frontier", source: "login"})
	inPriced := seedCat(t, db, catSpec{platform: "openrouter", modelID: "in", name: "InPriced",
		sizeLabel: "Large", priceIn: new(3.0)})
	cheap := seedCat(t, db, catSpec{platform: "openrouter", modelID: "cheap", name: "Cheap",
		sizeLabel: "Large", priceIn: new(0.2), priceOut: new(0.5)})

	freeMembers := presetMemberIDs(t, s, tok, "free", "freeset")
	require.Contains(t, freeMembers, free, "a genuinely free row belongs in the free set")
	require.NotContains(t, freeMembers, sub, "a subscription row is not free capacity")
	require.NotContains(t, freeMembers, inPriced, "an input-priced row is not free")
	require.NotContains(t, freeMembers, cheap, "a paid row is not free")

	nearMembers := presetMemberIDs(t, s, tok, "quick-chores", "nearset")
	require.Contains(t, nearMembers, free, "a free row belongs in the near-free set")
	require.Contains(t, nearMembers, cheap, "a cheaply-priced paid row belongs in the near-free set")
	require.NotContains(t, nearMembers, sub, "a subscription row never enters the near-free set")
	require.NotContains(t, nearMembers, inPriced,
		"an input-priced/output-unpublished row never enters the near-free set")
}

// TestPresetCreatorsRejectReservedAndCaseCollidingNames proves both list
// creators reuse the routing name contract: a reserved name and a
// case-colliding name are refused, and a refused request leaves no profile.
func TestPresetCreatorsRejectReservedAndCaseCollidingNames(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)
	ids := enabledModelIDs(t, s)
	require.GreaterOrEqual(t, len(ids), 2)

	profileCount := func() int {
		var n int
		require.NoError(t, s.engine.DB().QueryRow(`SELECT COUNT(*) FROM profiles`).Scan(&n))
		return n
	}
	nameCount := func(name string) int {
		var n int
		require.NoError(t, s.engine.DB().QueryRow(
			`SELECT COUNT(*) FROM profiles WHERE LOWER(name) = LOWER(?)`, name).Scan(&n))
		return n
	}

	// A reserved name is refused by the preset creator without insertion.
	before := profileCount()
	status, _ := createFromPreset(t, s, tok, "free", "auto", false)
	require.Equal(t, http.StatusBadRequest, status, "a reserved name must be refused")
	require.Equal(t, before, profileCount(), "a refused preset must not create a profile")
	require.Zero(t, nameCount("auto"))

	// And by the selection creator.
	status, _ = createFromSelection(t, s, tok, "budget", []int64{ids[0]}, false)
	require.Equal(t, http.StatusBadRequest, status, "a reserved name must be refused")
	require.Equal(t, before, profileCount())
	require.Zero(t, nameCount("budget"))

	// A first list takes a name; a second colliding only by case is refused by
	// either creator, and never creates a second row.
	status, _ = createFromSelection(t, s, tok, "Coding", []int64{ids[0]}, false)
	require.Equal(t, http.StatusCreated, status)
	after := profileCount()

	status, _ = createFromSelection(t, s, tok, "coding", []int64{ids[1]}, false)
	require.Equal(t, http.StatusConflict, status, "a case-collision must be refused")
	status, _ = createFromPreset(t, s, tok, "free", "CODING", false)
	require.Equal(t, http.StatusConflict, status, "a case-collision must be refused across creators")
	require.Equal(t, after, profileCount(), "a refused collision must not create a profile")
	require.Equal(t, 1, nameCount("coding"), "exactly one list holds the case-insensitive name")
}

// TestPresetAppliesItsStrategy proves a preset's suggested routing strategy is
// both advertised and stored: every preset row carries a strategy, and applying
// quick-chores creates a set that routes fastest-first, so the set carries the
// preset's meaning rather than only its membership.
func TestPresetAppliesItsStrategy(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: compatMachineKey})
	tok := session(t, s)
	seedPresetCatalogue(t, s)

	_, body := do(t, s, http.MethodGet, "/api/profiles/presets", "", authed(tok))
	var presets struct {
		Presets []struct {
			ID       string `json:"id"`
			Strategy string `json:"strategy"`
		} `json:"presets"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &presets))
	byPreset := map[string]string{}
	for _, p := range presets.Presets {
		require.NotEmpty(t, p.Strategy, "%s must advertise a strategy", p.ID)
		byPreset[p.ID] = p.Strategy
	}
	require.Equal(t, "fastest", byPreset["quick-chores"], "quick chores routes fastest-first")

	status, created := createFromPreset(t, s, tok, "quick-chores", "chores", false)
	require.Equal(t, http.StatusCreated, status)

	_, listBody := do(t, s, http.MethodGet, "/api/profiles", "", authed(tok))
	var list []struct {
		ID       int64  `json:"id"`
		Strategy string `json:"strategy"`
	}
	require.NoError(t, json.Unmarshal([]byte(listBody), &list))
	got := map[int64]string{}
	for _, p := range list {
		got[p.ID] = p.Strategy
	}
	require.Equal(t, "fastest", got[created.ID], "the created set must carry the preset's strategy")
}
