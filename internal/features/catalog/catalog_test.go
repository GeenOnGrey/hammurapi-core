package catalog

import "testing"

const doc = `apiVersion: backstage.io/v1alpha1
kind: Domain
metadata:
  name: fleet
  annotations:
    hammurapi/key: FMS
---
apiVersion: backstage.io/v1alpha1
kind: System
metadata:
  name: car-sharing
spec:
  domain: fleet
---
apiVersion: backstage.io/v1alpha1
kind: System
metadata:
  name: cars
spec:
  domain: domain:default/fleet
`

// CAT-01 / CAT-02: keys from the annotation or the upper-cased name.
func TestParseAndKeys(t *testing.T) {
	es, err := Parse([]byte(doc), "org/catalog", "fleet/catalog-info.yaml")
	if err != nil || len(es) != 3 {
		t.Fatalf("parse: %v %d", err, len(es))
	}
	if k, ok := Key(es[0]); !ok || k != "FMS" {
		t.Fatalf("domain key %s %v", k, ok)
	}
	if _, ok := Key(es[1]); ok {
		t.Fatal("kebab-case name without annotation must be rejected")
	}
	if k, ok := Key(es[2]); !ok || k != "CARS" {
		t.Fatalf("system key %s %v", k, ok)
	}
	if refName(es[2].Spec.Domain) != "fleet" {
		t.Fatalf("ref %s", refName(es[2].Spec.Domain))
	}
}

func TestMatchGlob(t *testing.T) {
	for p, want := range map[string]bool{"catalog-info.yaml": true, "a/b/catalog-info.yaml": true, "a/catalog-info.yml": false} {
		if MatchGlob("**/catalog-info.yaml", p) != want {
			t.Errorf("%s", p)
		}
	}
	if !MatchGlob("domains/*.yaml", "domains/fleet.yaml") || MatchGlob("domains/*.yaml", "domains/x/fleet.yaml") {
		t.Error("single star")
	}
}
