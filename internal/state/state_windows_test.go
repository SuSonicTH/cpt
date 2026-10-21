//go:build windows

package state

import "testing"

func TestOwnershipIsCaseInsensitive(t *testing.T) {
	st := New()
	st.SetOwner("Foo.EXE", "alpha")
	if owner, ok := st.Owner("foo.exe"); !ok || owner != "alpha" {
		t.Fatalf("Owner = %q, %v", owner, ok)
	}
	st.SetOwner("foo.exe", "beta")
	if _, ok := st.Provides["Foo.EXE"]; ok {
		t.Fatalf("differently-cased owner survived: %+v", st.Provides)
	}
	if owner := st.Provides["foo.exe"]; owner != "beta" {
		t.Fatalf("new owner = %q", owner)
	}

	p := &Package{Bins: map[string]string{"Foo.EXE": "hash"}}
	DeleteBin(p, "foo.exe")
	if len(p.Bins) != 0 {
		t.Fatalf("DeleteBin left %+v", p.Bins)
	}
}
