package hopfs

import (
	"reflect"
	"testing"
)

func listTestFS() *FS {
	return &FS{root: &node{dir: true, children: map[string]*node{
		"zeta":  {},
		"alpha": {},
		"dir":   {dir: true, children: map[string]*node{}},
	}}}
}

func TestListNSorteertBinnenLimiet(t *testing.T) {
	f := listTestFS()
	names, truncated, err := f.ListN("/", 3)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("listing past exact binnen de entrylimiet maar werd afgekapt")
	}
	want := []string{"alpha", "dir/", "zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("namen = %q, wil %q", names, want)
	}
}

func TestListNBreektVoorTeVeelEntriesAf(t *testing.T) {
	f := listTestFS()
	names, truncated, err := f.ListN("/", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatalf("listing werd niet begrensd: %q", names)
	}
	if names != nil {
		t.Fatalf("te grote listing hield namen vast: %q", names)
	}
}

func TestListOnbegrensdBlijftCompatibel(t *testing.T) {
	f := listTestFS()
	names, err := f.List("/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "dir/", "zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("namen = %q, wil %q", names, want)
	}
}
