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
	findDeps := []struct {
		root, update, want, tab string
		wantErr                 bool
	}{
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
		{
			root: "p",
			want: "d q",
			tab: `
				p/p.go: package p; import _ "q"
				q/q.go: package q; import _ "d"
				d/d.go: package d
			`,
		},
		{
			root: "p",
			want: "d q",
			tab: `
				p/p.go:          package p; import _ "q"
				q/q.go:          package q; import _ "d"
				q/vendor/d/d.go: package d
				d/d.go:          package d
			`,
		},
		{
			root: "p",
			want: "d", // d has Error set
			tab: `
				p/p.go: package p; import _ "d"
			`,
			wantErr: true,
		},
		{
			root: "p",
			want: "d q", // d has Error set
			tab: `
				p/p.go:          package p; import _ "q"
				q/q.go:          package q; import _ "d"
				q/vendor/d/d.go: package d
			`,
			wantErr: true,
		},
		{
			// copy test dependencies
			root: "p",
			want: "pt q qt xt",
			tab: `
				p/p.go:      package p;      import _ "q"
				p/p_test.go: package p;      import _ "pt"
				p/x_test.go: package p_test; import _ "xt"
				q/q.go:      package q
				q/q_test.go: package q;      import _ "qt"
				pt/pt.go:    package pt
				xt/xt.go:    package xt
				qt/qt.go:    package qt
			`,
		},
		{
			root: "p",
			want: "d e",
			tab: `
				p/p_darwin.go: package p; import _ "d"
				p/p_linux.go:  package p; import _ "e"
				d/d.go: package d
				e/e.go: package e
			`,
		},
		{
			root: "p",
			want: "",
			tab: `
				p/p.go:   package p; import _ "./d"
				p/d/d.go: package d
			`,
			wantErr: true,
		},
	}

	for _, test := range findDeps {
		paths := strings.Fields(test.root)
		clean := setup(t, paths[0], test.tab)
		defer clean()
		skipVendor = flagUPats(test.update)
		pkgs := packages(paths)
		deps := dependencies(pkgs)
		if got := anyErr(append(pkgs, deps...)); got != test.wantErr {
			t.Errorf("dependencies(packages(%q)) error = %v want %v", test.root, got, test.wantErr)
			t.Logf("flag -u=%q", test.update)
			t.Log("in", strings.Replace(test.tab, "\t", "", -1))
		}
		got := names(deps)
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

func anyErr(ps []*Package) bool {
	for _, p := range ps {
		if p.Error != nil {
			return true
		}
	}
	return false
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
