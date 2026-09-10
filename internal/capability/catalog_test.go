package capability

import "testing"

func TestBuiltinCatalogSearchAndDetail(t *testing.T) {
	catalog, err := BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	matches := catalog.Search("stale knowledge evidence", 2)
	if len(matches) == 0 || matches[0].Name != "maintain-knowledge" {
		t.Fatalf("matches = %+v", matches)
	}
	if matches[0].Triggers == nil {
		t.Fatal("summary collections must not be nil")
	}
	manifest, ok := catalog.Get(matches[0].Name)
	if !ok || len(manifest.Tools) == 0 || manifest.Version != "1" {
		t.Fatalf("manifest = %+v ok=%v", manifest, ok)
	}
	all := catalog.Search("", 20)
	if len(all) != 6 {
		t.Fatalf("built-in count = %d", len(all))
	}
	for index := 1; index < len(all); index++ {
		if all[index-1].Name > all[index].Name {
			t.Fatalf("empty search is not deterministic: %+v", all)
		}
	}
}
