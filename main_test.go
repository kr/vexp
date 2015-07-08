package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindDeps(t *testing.T) {
	findDeps := []struct{ root, update, want, tab string }{
		{
			root: "p",
			want: "d",
			tab: `
				p/p.go: package p; import _ "d"
				d/d.go: package d
			`,
		},
		{
			root: "p",
			want: "",
			tab: `
				p/p.go:          package p; import _ "d"
				p/vendor/d/d.go: package d
				d/d.go:          package d
			`,
		},
		{
			root:   "p",
			update: "d",
			want:   "d",
			tab: `
				p/p.go:          package p; import _ "d"
				p/vendor/d/d.go: package d
				d/d.go:          package d
			`,
		},
	}

	for _, test := range findDeps {
		paths := strings.Fields(test.root)
		clean := setup(t, paths[0], test.tab)
		defer clean()
		skipVendor = flagUPats(test.update)
		got := names(dependencies(packages(paths)))
		want := strings.Fields(test.want)
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("dependencies(packages(%q)) = %v want %v", test.root, got, want)
			t.Logf("flag -u=%q", test.update)
			t.Log("in", strings.Replace(test.tab, "\t", "", -1))
		}
		clean()
	}
}

func names(ps []*Package) (a []string) {
	for _, p := range ps {
		a = append(a, p.ImportPath)
	}
	return a
}

// setup sets up a test directory using the filesystem table
// described in tab.
// It sets buildContext.GOPATH to a new empty directory,
// populates that workspace with the source files and contents
// in tab, and sets cwd to the directory containing
// the first file listed in tab.
// clean resets buildContext.GOPATH and cwd to their
// previous values and removes the temporary directory.
func setup(t *testing.T, start, tab string) (clean func()) {
	wksp, err := ioutil.TempDir("", "vexp-test-")
	if err != nil {
		t.Fatal("setup", err)
	}
	src := filepath.Join(wksp, "src")

	saveCwd := cwd
	saveGOPATH := buildContext.GOPATH
	buildContext.GOPATH = wksp
	cwd = filepath.Join(src, filepath.FromSlash(start))

	for _, field := range strings.Split(tab, "\n") {
		field = strings.TrimSpace(field)
		i := strings.Index(field, ":")
		if i < 1 {
			continue
		}
		path := filepath.Join(src, filepath.FromSlash(strings.TrimSpace(field[:i])))
		body := strings.TrimSpace(field[i+1:]) + "\n"
		err = os.MkdirAll(filepath.Dir(path), 0777)
		if err == nil {
			err = ioutil.WriteFile(path, []byte(body), 0666)
		}
		if err != nil {
			os.RemoveAll(wksp)
			t.Fatal("setup", err)
		}
	}

	return func() {
		cwd = saveCwd
		buildContext.GOPATH = saveGOPATH
		os.RemoveAll(wksp)
		packageCache = map[string]*Package{}
		skipVendor = nil
	}
}
