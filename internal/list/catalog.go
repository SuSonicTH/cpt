package list

import (
	"fmt"
	"sort"
	"strings"
)

// Named is a list together with the identifier it was loaded under.
type Named struct {
	Name string
	List *List
}

// Entry is one package occurrence in the catalog.
type Entry struct {
	List string
	Pkg  *Package
}

// Catalog is the merged view of all loaded lists.
type Catalog struct {
	byName map[string][]Entry
	all    []Entry
}

// BuildCatalog merges lists, detecting name collisions across them.
func BuildCatalog(lists []Named) (*Catalog, error) {
	sort.Slice(lists, func(i, j int) bool { return lists[i].Name < lists[j].Name })
	c := &Catalog{byName: map[string][]Entry{}}
	for _, nl := range lists {
		if nl.List == nil {
			continue
		}
		for i := range nl.List.Packages {
			e := Entry{List: nl.Name, Pkg: &nl.List.Packages[i]}
			c.byName[e.Pkg.Name] = append(c.byName[e.Pkg.Name], e)
			c.all = append(c.all, e)
		}
	}
	sort.Slice(c.all, func(i, j int) bool {
		if c.all[i].Pkg.Name != c.all[j].Pkg.Name {
			return c.all[i].Pkg.Name < c.all[j].Pkg.Name
		}
		return c.all[i].List < c.all[j].List
	})
	return c, nil
}

// Lookup resolves a package name to exactly one entry. When several lists
// define the name, a conflict error is returned listing the origins.
func (c *Catalog) Lookup(name string) (Entry, error) {
	entries := c.byName[name]
	switch len(entries) {
	case 0:
		return Entry{}, fmt.Errorf("unknown package %q", name)
	case 1:
		return entries[0], nil
	default:
		lists := make([]string, 0, len(entries))
		for _, e := range entries {
			lists = append(lists, e.List)
		}
		return Entry{}, fmt.Errorf("package %q is defined by multiple lists: %s", name, strings.Join(lists, ", "))
	}
}

// Entries returns every package occurrence sorted by name then list.
func (c *Catalog) Entries() []Entry { return c.all }

// Conflicts returns package names defined by more than one list.
func (c *Catalog) Conflicts() map[string][]string {
	out := map[string][]string{}
	for name, entries := range c.byName {
		if len(entries) < 2 {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.List)
		}
		sort.Strings(names)
		out[name] = names
	}
	return out
}

// Has reports whether the catalog contains name.
func (c *Catalog) Has(name string) bool { return len(c.byName[name]) > 0 }
