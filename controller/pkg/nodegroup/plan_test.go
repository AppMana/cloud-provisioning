package nodegroup

import "testing"

func TestCapacityLifecycle(t *testing.T) {
	var children []Child
	for i := 0; i < 3; i++ {
		p, e := Next("group", 3, children)
		if e != nil || p.CreateOrdinal == nil || *p.CreateOrdinal != i {
			t.Fatalf("create %d: %+v %v", i, p, e)
		}
		children = append(children, Child{Name: string(rune('a' + i)), OwnerUID: "group", Ordinal: i})
	}
	p, e := Next("group", 3, children)
	if e != nil || p.CreateOrdinal != nil || p.Drain != "" {
		t.Fatal(p, e)
	}
	p, e = Next("group", 1, children)
	if e != nil || p.Drain != "c" {
		t.Fatal(p, e)
	}
	children[2].Draining = true
	p, e = Next("group", 0, children)
	if e != nil || !p.Waiting || p.Drain != "" {
		t.Fatal("concurrent removal", p, e)
	}
	children[2].Draining = false
	children[2].Terminating = true
	p, e = Next("group", 4, children)
	if e != nil || !p.Waiting || p.CreateOrdinal != nil {
		t.Fatal("reused terminating slot", p, e)
	}
	children = children[:2]
	p, e = Next("group", 1, children)
	if e != nil || p.Drain != "b" {
		t.Fatal(p, e)
	}
	children = children[:1]
	p, e = Next("group", 0, children)
	if e != nil || p.Drain != "a" {
		t.Fatal(p, e)
	}
	p, e = Next("group", 0, nil)
	if e != nil || p != (Plan{}) {
		t.Fatal(p, e)
	}
}
func TestOwnershipAndEnumeration(t *testing.T) {
	children := []Child{{Name: "c", OwnerUID: "group", Ordinal: 2}, {Name: "a", OwnerUID: "group", Ordinal: 0}}
	p, e := Next("group", 3, children)
	if e != nil || p.CreateOrdinal == nil || *p.CreateOrdinal != 1 {
		t.Fatal(p, e)
	}
	if children[0].Name != "c" {
		t.Fatal("mutated caller")
	}
	for _, bad := range [][]Child{
		{{Name: "a", OwnerUID: "old-group", Ordinal: 0}},
		{{Name: "a", OwnerUID: "group", Ordinal: -1}},
		{{OwnerUID: "group", Ordinal: 0}},
		{{Name: "a", OwnerUID: "group", Ordinal: 0}, {Name: "b", OwnerUID: "group", Ordinal: 0}},
		{{Name: "a", OwnerUID: "group", Ordinal: 0}, {Name: "a", OwnerUID: "group", Ordinal: 1}},
	} {
		if _, e := Next("group", 1, bad); e == nil {
			t.Fatal("accepted invalid children")
		}
	}
	if _, e := Next("", 1, nil); e == nil {
		t.Fatal("missing owner")
	}
	if _, e := Next("group", -1, nil); e == nil {
		t.Fatal("negative replicas")
	}
}
