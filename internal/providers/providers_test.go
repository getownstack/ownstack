package providers

import (
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/catalog"
)

func TestCatalogIsConsistent(t *testing.T) {
	cat := Catalog()
	if err := cat.Validate(); err != nil {
		t.Fatal(err)
	}
	for key := range registry {
		found := false
		for _, p := range cat.Providers {
			if catalog.Key(p.Aspect, p.ID) == key {
				found = p.Available
			}
		}
		if !found {
			t.Errorf("registered provider %s is missing from the catalog", key)
		}
	}
	for _, aspect := range cat.Aspects {
		if aspect.Required {
			available := false
			for _, p := range cat.ProvidersFor(aspect.ID) {
				available = available || p.Available
			}
			if !available {
				t.Errorf("required aspect %s has no available provider", aspect.ID)
			}
		}
	}
}
