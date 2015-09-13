// Copyright 2015 Keith Rarick.
// Portions copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"go/token"
	"io"
	"log"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	update  = flag.String("u", "", "update `packages` (colon-separated list of patterns)")
	verbose = flag.Bool("v", false, "verbose")
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: vexp [-v] [-u packages]")
	flag.PrintDefaults()
	os.Exit(2)
}

var (
	cwd, _       = os.Getwd()
	gobin        = os.Getenv("GOBIN")
	buildContext = defaultBuildContext()
	// list of import paths not to search for in vendor directories
	skipVendor []func(string) bool
)

func main() {
	flag.Usage = usage
	flag.Parse()
	skipVendor = flagUPats(*update)
	roots := packages(matchPackagesInFS("./..."))
	if len(roots) == 0 {
		fmt.Fprintln(os.Stderr, "warning: ./... matched no packages")
	}
	deps := dependencies(roots)
	ok := true
	for _, pkg := range append(roots, deps...) {
		if pkg.Error != nil {
			fmt.Fprintln(os.Stderr, pkg.Error)
			ok = false
		}
		if pkg.Standard {
			fmt.Fprintf(os.Stderr, "package %s is in the standard library\n", pkg.ImportPath)
			ok = false
		}
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "error(s) loading dependencies")
		os.Exit(1)
	}

	var seen []string
	for _, pkg := range deps {
		if isSeen(pkg, seen) {
			continue
		}
		seen = append(seen, pkg.ImportPath)
		copyDep(pkg)
	}
}

func flagUPats(u string) (a []func(string) bool) {
	for _, pat := range splitList(u) {
		a = append(a, matchPattern(pat))
	}
	return
}

// dependencies returns the list of dependencies
// of the given packages,
// excluding any from cwd or the standard library.
func dependencies(packages []*Package) (deps []*Package) {
	for _, p := range packages {
		if *verbose {
			fmt.Println("root", p.ImportPath)
		}
		for _, d := range p.deps {
			if inCWD(d.Dir) {
				continue
			}
			deps = append(deps, d)
		}
	}
	sort.Sort(byImportPath(deps))
	return deps
}

func isSeen(pkg *Package, seen []string) bool {
	for _, prefix := range seen {
		if hasPathPrefix(pkg.ImportPath, prefix) {
			return true
		}
	}
	return false
}

// An importStack is a stack of import paths.
type importStack []string

func (s *importStack) push(p string) {
	*s = append(*s, p)
}

func (s *importStack) pop() {
	*s = (*s)[0 : len(*s)-1]
}

func (s *importStack) copy() []string {
	return append([]string{}, *s...)
}

// shorterThan returns true if sp is shorter than t.
// We use this to record the shortest import sequence
// that leads to a particular package.
func (sp *importStack) shorterThan(t []string) bool {
	s := *sp
	if len(s) != len(t) {
		return len(s) < len(t)
	}
	// If they are the same length, settle ties using string ordering.
	for i := range s {
		if s[i] != t[i] {
			return s[i] < t[i]
		}
	}
	return false // they are equal
}

// A Package describes a single package found in a directory.
type Package struct {
	*build.Package
	Standard   bool          // is this package part of the standard Go library?
	Error      *PackageError // error loading this package (not dependencies)
	loadedDeps bool
	deps       []*Package
}

func (p *Package) copyBuild(pp *build.Package) {
	p.Package = pp
	p.Standard = p.Goroot && p.ImportPath != "" && !strings.Contains(p.ImportPath, ".")
}

// packages returns the packages named by the
// command line arguments 'args'.  If there is an error
// loading the package (for example, if the directory does not exist),
// then packages returns a *Package for that argument with p.Error != nil.
func packages(args []string) []*Package {
	var pkgs []*Package
	var stk importStack
	var set = make(map[string]bool)

	for _, arg := range args {
		if !set[arg] {
			pkgs = append(pkgs, loadPackage(arg, &stk))
			set[arg] = true
		}
	}

	return pkgs
}

// loadPackage is like loadImport but is used for command-line arguments,
// not for paths found in import statements.  In addition to ordinary import paths,
// loadPackage accepts pseudo-paths beginning with cmd/ to denote commands
// in the Go command directory, as well as paths to those directories.
func loadPackage(arg string, stk *importStack) *Package {
	// If it is a local import path but names a standard package,
	// we treat it as if the user specified the standard package.
	// This lets you run go test ./ioutil in package io and be
	// referring to io/ioutil rather than a hypothetical import of
	// "./ioutil".
	if build.IsLocalImport(arg) {
		bp, _ := buildContext.ImportDir(filepath.Join(cwd, arg), build.FindOnly)
		if bp.ImportPath != "" && bp.ImportPath != "." {
			arg = bp.ImportPath
		}
	}
	return loadImport(arg, cwd, nil, stk, nil)
}

// packageCache is a lookup cache for loadPackage,
// so that if we look up a package multiple times
// we return the same pointer each time.
var packageCache = map[string]*Package{}

// loadImport scans the directory named by path, which must be an import path,
// but possibly a local import path (an absolute file system path or one beginning
// with ./ or ../).  A local relative path is interpreted relative to srcDir.
// It returns a *Package describing the package found in that directory.
func loadImport(path, srcDir string, parent *Package, stk *importStack, importPos []token.Position) *Package {
	stk.push(path)
	defer stk.pop()

	// Determine canonical identifier for this package.
	// For a local import the identifier is the pseudo-import path
	// we create from the full directory to the package.
	// Otherwise it is the usual import path.
	// For vendored imports, it is the expanded form.
	importPath := path
	var vendorSearch []string
	path, vendorSearch = vendoredImportPath(parent, path)
	importPath = path

	if p := packageCache[importPath]; p != nil {
		return reusePackage(p, stk)
	}

	p := new(Package)
	packageCache[importPath] = p

	// Load package.
	// Import always returns bp != nil, even if an error occurs,
	// in order to return partial information.
	//
	// TODO: After Go 1, decide when to pass build.AllowBinary here.
	// See issue 3268 for mistakes to avoid.
	bp, err := buildContext.Import(path, srcDir, build.ImportComment)

	// If we got an error from go/build about package not found,
	// it contains the directories from $GOROOT and $GOPATH that
	// were searched. Add to that message the vendor directories
	// that were searched.
	if err != nil && len(vendorSearch) > 0 {
		// NOTE(rsc): The direct text manipulation here is fairly awful,
		// but it avoids defining new go/build API (an exported error type)
		// late in the Go 1.5 release cycle. If this turns out to be a more general
		// problem we could define a real error type when the decision can be
		// considered more carefully.
		text := err.Error()
		if strings.Contains(text, "cannot find package \"") && strings.Contains(text, "\" in any of:\n\t") {
			old := strings.SplitAfter(text, "\n")
			lines := []string{old[0]}
			for _, dir := range vendorSearch {
				lines = append(lines, "\t"+dir+" (vendor tree)\n")
			}
			lines = append(lines, old[1:]...)
			err = errors.New(strings.Join(lines, ""))
		}
	}
	bp.ImportPath = importPath
	if gobin != "" {
		bp.BinDir = gobin
	}
	if err == nil && bp.ImportComment != "" && bp.ImportComment != path && !strings.Contains(path, "/vendor/") {
		err = fmt.Errorf("code in directory %s expects import %q", bp.Dir, bp.ImportComment)
	}
	p.copyBuild(bp)
	if p.Standard {
		return p
	}
	loadDeps(p, stk, err)
	if p.Error != nil && len(importPos) > 0 {
		pos := importPos[0]
		pos.Filename = shortPath(pos.Filename)
		p.Error.Pos = pos.String()
	}
	return p
}

// loadDeps loads p's deps
// it omits the standard library
func loadDeps(p *Package, stk *importStack, err error) {
	if err != nil {
		p.Error = &PackageError{
			ImportStack: stk.copy(),
			Err:         err.Error(),
		}
		return
	}

	importPaths := p.Imports
	importPaths = append(importPaths, p.TestImports...)
	importPaths = append(importPaths, p.XTestImports...)

	// Check for case-insensitive collision of input files.
	// To avoid problems on case-insensitive files, we reject any package
	// where two different input files have equal names under a case-insensitive
	// comparison.
	f1, f2 := foldDup(stringList(
		p.GoFiles,
		p.CgoFiles,
		p.IgnoredGoFiles,
		p.CFiles,
		p.CXXFiles,
		p.MFiles,
		p.HFiles,
		p.SFiles,
		p.SysoFiles,
		p.SwigFiles,
		p.SwigCXXFiles,
		p.TestGoFiles,
		p.XTestGoFiles,
	))
	if f1 != "" {
		p.Error = &PackageError{
			ImportStack: stk.copy(),
			Err:         fmt.Sprintf("case-insensitive file name collision: %q and %q", f1, f2),
		}
		return
	}

	// Build list of imported packages and full dependency list.
	deps := make(map[string]*Package)
	for i, path := range importPaths {
		if path == "C" {
			continue
		}
		if build.IsLocalImport(path) {
			p.Error = &PackageError{
				ImportStack: stk.copy(),
				Err:         fmt.Sprintf("local import %q in non-local package", path),
			}
			pos := p.Package.ImportPos[path]
			if len(pos) > 0 {
				p.Error.Pos = pos[0].String()
			}
			return
		}
		p1 := loadImport(path, p.Dir, p, stk, p.Package.ImportPos[path])
		if p1.Name == "main" {
			p.Error = &PackageError{
				ImportStack: stk.copy(),
				Err:         fmt.Sprintf("import %q is a program, not an importable package", path),
			}
			pos := p.Package.ImportPos[path]
			if len(pos) > 0 {
				p.Error.Pos = pos[0].String()
			}
		}
		path = p1.ImportPath
		if i < len(p.Imports) {
			p.Imports[i] = path
		}
		if p1.Standard {
			continue
		}
		deps[path] = p1
		for _, dep := range p1.deps {
			deps[dep.ImportPath] = dep
		}
	}
	p.loadedDeps = true

	var depErrors []error
	depPaths := make([]string, 0, len(deps))
	for dep := range deps {
		depPaths = append(depPaths, dep)
	}
	sort.Strings(depPaths)
	for _, dep := range depPaths {
		p1 := deps[dep]
		if p1 == nil {
			panic("impossible: missing entry in package cache for " + dep + " imported by " + p.ImportPath)
		}
		p.deps = append(p.deps, p1)
		if p1.Error != nil {
			depErrors = append(depErrors, p1.Error)
		}
	}

	// In the absence of errors lower in the dependency tree,
	// check for case-insensitive collisions of import paths.
	if len(depErrors) == 0 {
		dep1, dep2 := foldDup(depPaths)
		if dep1 != "" {
			p.Error = &PackageError{
				ImportStack: stk.copy(),
				Err:         fmt.Sprintf("case-insensitive import collision: %q and %q", dep1, dep2),
			}
			return
		}
	}
}

var isDirCache = map[string]bool{}

func isDir(path string) bool {
	result, ok := isDirCache[path]
	if ok {
		return result
	}

	fi, err := os.Stat(path)
	result = err == nil && fi.IsDir()
	isDirCache[path] = result
	return result
}

// vendoredImportPath returns the expansion of path when it appears in parent.
// If parent is x/y/z, then path might expand to x/y/z/vendor/path, x/y/vendor/path,
// x/vendor/path, vendor/path, or else stay x/y/z if none of those exist.
// vendoredImportPath returns the expanded path or, if no expansion is found, the original.
// If no epxansion is found, vendoredImportPath also returns a list of vendor directories
// it searched along the way, to help prepare a useful error message should path turn
// out not to exist.
// It skips paths that match the patterns in skipVendor.
func vendoredImportPath(parent *Package, path string) (found string, searched []string) {
	if parent == nil {
		return path, nil
	}
	for _, match := range skipVendor {
		if match(path) {
			return path, nil
		}
	}
	dir := filepath.Clean(parent.Dir)
	root := filepath.Clean(parent.Root)
	if !strings.HasPrefix(dir, root) || len(dir) <= len(root) || dir[len(root)] != filepath.Separator {
		log.Printf("invalid vendoredImportPath: dir=%q root=%q separator=%q", dir, root, string(filepath.Separator))
		os.Exit(1)
	}
	if !inCWD(dir) {
		// We consider vendored packages only for the root set
		// we're trying to operate on, not its dependencies.
		return path, nil
	}
	vpath := "vendor/" + path
	for i := len(dir); i >= len(root); i-- {
		if i < len(dir) && dir[i] != filepath.Separator {
			continue
		}
		// Note: checking for the vendor directory before checking
		// for the vendor/path directory helps us hit the
		// isDir cache more often. It also helps us prepare a more useful
		// list of places we looked, to report when an import is not found.
		if !isDir(filepath.Join(dir[:i], "vendor")) {
			continue
		}
		targ := filepath.Join(dir[:i], vpath)
		if isDir(targ) {
			// We started with parent's dir c:\gopath\src\foo\bar\baz\quux\xyzzy.
			// We know the import path for parent's dir.
			// We chopped off some number of path elements and
			// added vendor\path to produce c:\gopath\src\foo\bar\baz\vendor\path.
			// Now we want to know the import path for that directory.
			// Construct it by chopping the same number of path elements
			// (actually the same number of bytes) from parent's import path
			// and then append /vendor/path.
			chopped := len(dir) - i
			if chopped == len(parent.ImportPath)+1 {
				// We walked up from c:\gopath\src\foo\bar
				// and found c:\gopath\src\vendor\path.
				// We chopped \foo\bar (length 8) but the import path is "foo/bar" (length 7).
				// Use "vendor/path" without any prefix.
				return vpath, nil
			}
			return parent.ImportPath[:len(parent.ImportPath)-chopped] + "/" + vpath, nil
		}
		// Note the existence of a vendor directory in case path is not found anywhere.
		searched = append(searched, targ)
	}
	return path, searched
}

// A PackageError describes an error loading information about a package.
type PackageError struct {
	ImportStack   []string // shortest path from package named on command line to this one
	Pos           string   // position of error
	Err           string   // the error itself
	isImportCycle bool     // the error is an import cycle
	hard          bool     // whether the error is soft or hard; soft errors are ignored in some places
}

func (p *PackageError) Error() string {
	// Import cycles deserve special treatment.
	if p.isImportCycle {
		return fmt.Sprintf("%s\npackage %s\n", p.Err, strings.Join(p.ImportStack, "\n\timports "))
	}
	if p.Pos != "" {
		// Omit import stack.  The full path to the file where the error
		// is the most important thing.
		return p.Pos + ": " + p.Err
	}
	if len(p.ImportStack) == 0 {
		return p.Err
	}
	return "package " + strings.Join(p.ImportStack, "\n\timports ") + ": " + p.Err
}

// assumes path and cwd are clean
func inCWD(path string) bool {
	return path == cwd || strings.HasPrefix(path, cwd+string(os.PathSeparator))
}

func matchPackagesInFS(pattern string) []string {
	// Find directory to begin the scan.
	// Could be smarter but this one optimization
	// is enough for now, since ... is usually at the
	// end of a path.
	i := strings.Index(pattern, "...")
	dir, _ := pathpkg.Split(pattern[:i])

	// pattern begins with ./ or ../.
	// path.Clean will discard the ./ but not the ../.
	// We need to preserve the ./ for pattern matching
	// and in the returned import paths.
	prefix := ""
	if strings.HasPrefix(pattern, "./") {
		prefix = "./"
	}
	match := matchPattern(pattern)

	var pkgs []string
	filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil || !fi.IsDir() {
			return nil
		}
		if path == dir {
			// filepath.Walk starts at dir and recurses. For the recursive case,
			// the path is the result of filepath.Join, which calls filepath.Clean.
			// The initial case is not Cleaned, though, so we do this explicitly.
			//
			// This converts a path like "./io/" to "io". Without this step, running
			// "cd $GOROOT/src; go list ./io/..." would incorrectly skip the io
			// package, because prepending the prefix "./" to the unclean path would
			// result in "././io", and match("././io") returns false.
			path = filepath.Clean(path)
		}

		// Avoid .foo, _foo, and testdata directory trees, but do not avoid "." or "..".
		_, elem := filepath.Split(path)
		dot := strings.HasPrefix(elem, ".") && elem != "." && elem != ".."
		if dot || strings.HasPrefix(elem, "_") || elem == "testdata" {
			return filepath.SkipDir
		}

		name := prefix + filepath.ToSlash(path)
		if !match(name) {
			return nil
		}
		if _, err = build.ImportDir(path, 0); err != nil {
			if _, noGo := err.(*build.NoGoError); !noGo {
				log.Print(err)
			}
			return nil
		}
		pkgs = append(pkgs, name)
		return nil
	})
	return pkgs
}

// reusePackage reuses package p to satisfy the import at the top
// of the import stack stk.  If this use causes an import loop,
// reusePackage updates p's error information to record the loop.
func reusePackage(p *Package, stk *importStack) *Package {
	// (all the recursion below happens before p.loadedDeps gets set).
	if p.loadedDeps {
		if p.Error == nil {
			p.Error = &PackageError{
				ImportStack:   stk.copy(),
				Err:           "import cycle not allowed",
				isImportCycle: true,
			}
		}
	}
	// Don't rewrite the import stack in the error if we have an import cycle.
	// If we do, we'll lose the path that describes the cycle.
	if p.Error != nil && !p.Error.isImportCycle && stk.shorterThan(p.Error.ImportStack) {
		p.Error.ImportStack = stk.copy()
	}
	return p
}

// matchPattern(pattern)(name) reports whether
// name matches pattern.  Pattern is a limited glob
// pattern in which '...' means 'any string' and there
// is no other special syntax.
func matchPattern(pattern string) func(name string) bool {
	re := regexp.QuoteMeta(pattern)
	re = strings.Replace(re, `\.\.\.`, `.*`, -1)
	// Special case: foo/... matches foo too.
	if strings.HasSuffix(re, `/.*`) {
		re = re[:len(re)-len(`/.*`)] + `(/.*)?`
	}
	reg := regexp.MustCompile(`^` + re + `$`)
	return reg.MatchString
}

func copyDep(pkg *Package) {
	if *verbose {
		fmt.Println("copy", pkg.ImportPath)
	}
	dstRoot := filepath.Join("vendor", filepath.FromSlash(pkg.ImportPath))
	err := os.RemoveAll(dstRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	filepath.Walk(pkg.Dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return nil
		}

		// Avoid .foo, _foo, and testdata directory trees, but do not avoid "." or "..".
		_, elem := filepath.Split(path)
		dot := strings.HasPrefix(elem, ".") && elem != "." && elem != ".."
		if dot || strings.HasPrefix(elem, "_") || elem == "testdata" {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, _ := filepath.Rel(pkg.Dir, path)
		dst := filepath.Join(dstRoot, rel)
		if fi.IsDir() {
			err = os.MkdirAll(dst, 0777)
		} else {
			err = copyFile(dst, path)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return nil
	})
}

func copyFile(dst, src string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(df, sf)
	if err != nil {
		df.Close()
		return err
	}
	return df.Close()
}

func splitList(path string) []string {
	if path == "" {
		return []string{}
	}
	return strings.Split(path, ":")
}

type byImportPath []*Package

func (a byImportPath) Len() int           { return len(a) }
func (a byImportPath) Less(i, j int) bool { return a[i].ImportPath < a[j].ImportPath }
func (a byImportPath) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// hasPathPrefix reports whether the path s begins with the
// elements in prefix.
func hasPathPrefix(s, prefix string) bool {
	switch {
	default:
		return false
	case len(s) == len(prefix):
		return s == prefix
	case len(s) > len(prefix):
		if prefix != "" && prefix[len(prefix)-1] == '/' {
			return strings.HasPrefix(s, prefix)
		}
		return s[len(prefix)] == '/' && s[:len(prefix)] == prefix
	}
}

// shortPath returns an absolute or relative name for path, whatever is shorter.
func shortPath(path string) string {
	if rel, err := filepath.Rel(cwd, path); err == nil && len(rel) < len(path) {
		return rel
	}
	return path
}

// stringList's arguments should be a sequence of string or []string values.
// stringList flattens them into a single []string.
func stringList(args ...interface{}) []string {
	var x []string
	for _, arg := range args {
		switch arg := arg.(type) {
		case []string:
			x = append(x, arg...)
		case string:
			x = append(x, arg)
		default:
			panic("stringList: invalid argument of type " + fmt.Sprintf("%T", arg))
		}
	}
	return x
}

// toFold returns a string with the property that
// strings.EqualFold(s, t) iff toFold(s) == toFold(t)
// This lets us test a large set of strings for fold-equivalent
// duplicates without making a quadratic number of calls
// to EqualFold. Note that strings.ToUpper and strings.ToLower
// have the desired property in some corner cases.
func toFold(s string) string {
	// Fast path: all ASCII, no upper case.
	// Most paths look like this already.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf || 'A' <= c && c <= 'Z' {
			goto Slow
		}
	}
	return s

Slow:
	var buf bytes.Buffer
	for _, r := range s {
		// SimpleFold(x) cycles to the next equivalent rune > x
		// or wraps around to smaller values. Iterate until it wraps,
		// and we've found the minimum value.
		for {
			r0 := r
			r = unicode.SimpleFold(r0)
			if r <= r0 {
				break
			}
		}
		// Exception to allow fast path above: A-Z => a-z
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		buf.WriteRune(r)
	}
	return buf.String()
}

// foldDup reports a pair of strings from the list that are
// equal according to strings.EqualFold.
// It returns "", "" if there are no such strings.
func foldDup(list []string) (string, string) {
	clash := map[string]string{}
	for _, s := range list {
		fold := toFold(s)
		if t := clash[fold]; t != "" {
			if s > t {
				s, t = t, s
			}
			return s, t
		}
		clash[fold] = s
	}
	return "", ""
}

func defaultBuildContext() build.Context {
	c := build.Default
	c.UseAllFiles = true
	return c
}
