// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package imports

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/ast/astutil"
)

// Debug controls verbose logging.
var Debug = false

// importToGroup is a list of functions which map from an import path to
// a group number.
var importToGroup = []func(importPath string) (num int, ok bool){
	func(importPath string) (num int, ok bool) {
		if strings.HasPrefix(importPath, "appengine") {
			return 2, true
		}
		return
	},
	func(importPath string) (num int, ok bool) {
		if strings.Contains(importPath, ".") {
			return 1, true
		}
		return
	},
}

func importGroup(importPath string) int {
	for _, fn := range importToGroup {
		if n, ok := fn(importPath); ok {
			return n
		}
	}
	return 0
}

func fixImports(fset *token.FileSet, f *ast.File, filename string) (added []string, err error) {
	// refs are a set of possible package references currently unsatisfied by imports.
	// first key: either base package (e.g. "fmt") or renamed package
	// second key: referenced package symbol (e.g. "Println")
	refs := make(map[string]map[string]bool)

	// decls are the current package imports. key is base package or renamed package.
	decls := make(map[string]*ast.ImportSpec)

	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	srcDir := filepath.Dir(abs)
	if Debug {
		log.Printf("fixImports(filename=%q), abs=%q, srcDir=%q ...", filename, abs, srcDir)
	}

	// collect potential uses of packages.
	var visitor visitFn
	visitor = visitFn(func(node ast.Node) ast.Visitor {
		if node == nil {
			return visitor
		}
		switch v := node.(type) {
		case *ast.ImportSpec:
			if v.Name != nil {
				decls[v.Name.Name] = v
				break
			}
			ipath := strings.Trim(v.Path.Value, `\"`)
			if ipath == "C" {
				break
			}
			local := importPathToName(ipath, srcDir)
			decls[local] = v
		case *ast.SelectorExpr:
			xident, ok := v.X.(*ast.Ident)
			if !ok {
				break
			}
			if xident.Obj != nil {
				// if the parser can resolve it, it's not a package ref
				break
			}
			pkgName := xident.Name
			if refs[pkgName] == nil {
				refs[pkgName] = make(map[string]bool)
			}
			if decls[pkgName] == nil {
				refs[pkgName][v.Sel.Name] = true
			}
		}
		return visitor
	})
	ast.Walk(visitor, f)

	// Nil out any unused ImportSpecs, to be removed in following passes
	unusedImport := map[string]string{}
	for pkg, is := range decls {
		if refs[pkg] == nil && pkg != "_" && pkg != "." {
			name := ""
			if is.Name != nil {
				name = is.Name.Name
			}
			unusedImport[strings.Trim(is.Path.Value, `"`)] = name
		}
	}
	for ipath, name := range unusedImport {
		if ipath == "C" {
			// Don't remove cgo stuff.
			continue
		}
		astutil.DeleteNamedImport(fset, f, name, ipath)
	}

	for pkgName, symbols := range refs {
		if len(symbols) == 0 {
			// skip over packages already imported
			delete(refs, pkgName)
		}
	}

	// Search for imports matching potential package references.
	searches := 0
	type result struct {
		ipath string // import path (if err == nil)
		name  string // optional name to rename import as
		err   error
	}
	results := make(chan result)
	for pkgName, symbols := range refs {
		go func(pkgName string, symbols map[string]bool) {
			ipath, rename, err := findImport(pkgName, symbols, filename)
			r := result{ipath: ipath, err: err}
			if rename {
				r.name = pkgName
			}
			results <- r
		}(pkgName, symbols)
		searches++
	}
	for i := 0; i < searches; i++ {
		result := <-results
		if result.err != nil {
			return nil, result.err
		}
		if result.ipath != "" {
			if result.name != "" {
				astutil.AddNamedImport(fset, f, result.name, result.ipath)
			} else {
				astutil.AddImport(fset, f, result.ipath)
			}
			added = append(added, result.ipath)
		}
	}

	return added, nil
}

// importPathToName returns the package name for the given import path.
var importPathToName func(importPath, srcDir string) (packageName string) = importPathToNameGoPath

// importPathToNameBasic assumes the package name is the base of import path.
func importPathToNameBasic(importPath, srcDir string) (packageName string) {
	return path.Base(importPath)
}

// importPathToNameGoPath finds out the actual package name, as declared in its .go files.
// If there's a problem, it falls back to using importPathToNameBasic.
func importPathToNameGoPath(importPath, srcDir string) (packageName string) {
	// Fast path for standard library without going to disk:
	if pkg, ok := stdImportPackage[importPath]; ok {
		return pkg
	}

	pkgName, err := importPathToNameGoPathParse(importPath, srcDir)
	if Debug {
		log.Printf("importPathToNameGoPathParse(%q, srcDir=%q) = %q, %v", importPath, srcDir, pkgName, err)
	}
	if err == nil {
		return pkgName
	}
	return importPathToNameBasic(importPath, srcDir)
}

// importPathToNameGoPathParse is a faster version of build.Import if
// the only thing desired is the package name. It uses build.FindOnly
// to find the directory and then only parses one file in the package,
// trusting that the files in the directory are consistent.
func importPathToNameGoPathParse(importPath, srcDir string) (packageName string, err error) {
	buildPkg, err := build.Import(importPath, srcDir, build.FindOnly)
	if err != nil {
		return "", err
	}
	d, err := os.Open(buildPkg.Dir)
	if err != nil {
		return "", err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return "", err
	}
	sort.Strings(names) // to have predictable behavior
	var lastErr error
	var nfile int
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		nfile++
		fullFile := filepath.Join(buildPkg.Dir, name)

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, fullFile, nil, parser.PackageClauseOnly)
		if err != nil {
			lastErr = err
			continue
		}
		pkgName := f.Name.Name
		if pkgName == "documentation" {
			// Special case from go/build.ImportDir, not
			// handled by ctx.MatchFile.
			continue
		}
		if pkgName == "main" {
			// Also skip package main, assuming it's a
			// +build ignore generator or example. Since
			// you can't import a package main anyway,
			// there's no harm here.
			continue
		}
		return pkgName, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no importable package found in %d Go files", nfile)
}

var stdImportPackage = map[string]string{} // "net/http" => "http"

func init() {
	// Nothing in the standard library has a package name not
	// matching its import base name.
	for _, pkg := range stdlib {
		if _, ok := stdImportPackage[pkg]; !ok {
			stdImportPackage[pkg] = path.Base(pkg)
		}
	}
}

// Directory-scanning state.
var (
	// scanGoRootOnce guards calling scanGoRoot (for $GOROOT)
	scanGoRootOnce = &sync.Once{}
	// scanGoPathOnce guards calling scanGoPath (for $GOPATH)
	scanGoPathOnce = &sync.Once{}

	dirScanMu sync.RWMutex
	dirScan   map[string]*pkg // abs dir path => *pkg
)

type pkg struct {
	dir             string // absolute file path to pkg directory ("/usr/lib/go/src/net/http")
	importPath      string // full pkg import path ("net/http", "foo/bar/vendor/a/b")
	importPathShort string // vendorless import path ("net/http", "a/b")
}

// byImportPathShortLength sorts by the short import path length, breaking ties on the
// import string itself.
type byImportPathShortLength []*pkg

func (s byImportPathShortLength) Len() int { return len(s) }
func (s byImportPathShortLength) Less(i, j int) bool {
	vi, vj := s[i].importPathShort, s[j].importPathShort
	return len(vi) < len(vj) || (len(vi) == len(vj) && vi < vj)

}
func (s byImportPathShortLength) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// gate is a semaphore for limiting concurrency.
type gate chan struct{}

func (g gate) enter() { g <- struct{}{} }
func (g gate) leave() { <-g }

// fsgate protects the OS & filesystem from too much concurrency.
// Too much disk I/O -> too many threads -> swapping and bad scheduling.
var fsgate = make(gate, 8)

var visitedSymlinks struct {
	sync.Mutex
	m map[string]struct{}
}

var ignoredDirs []os.FileInfo

// populateIgnoredDirs reads an optional config file at <path>/.goimportsignore
// of relative directories to ignore when scanning for go files.
// The provided path is one of the $GOPATH entries with "src" appended.
func populateIgnoredDirs(path string) {
	slurp, err := ioutil.ReadFile(filepath.Join(path, ".goimportsignore"))
	if err != nil {
		return
	}
	bs := bufio.NewScanner(bytes.NewReader(slurp))
	for bs.Scan() {
		line := strings.TrimSpace(bs.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(path, line)); err == nil {
			ignoredDirs = append(ignoredDirs, fi)
		}
	}
}

func skipDir(fi os.FileInfo) bool {
	for _, ignoredDir := range ignoredDirs {
		if os.SameFile(fi, ignoredDir) {
			return true
		}
	}
	return false
}

// shouldTraverse checks if fi, found in dir, is a directory or a symlink to a directory.
// It makes sure symlinks were never visited before to avoid symlink loops.
func shouldTraverse(dir string, fi os.FileInfo) bool {
	if fi.IsDir() {
		if skipDir(fi) {
			if Debug {
				log.Printf("skipping directory %q under %s", fi.Name(), dir)
			}
			return false
		}
		return true
	}

	if fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	path := filepath.Join(dir, fi.Name())
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		return false
	}
	ts, err := os.Stat(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	if !ts.IsDir() {
		return false
	}

	realParent, err := filepath.EvalSymlinks(dir)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		return false
	}
	realPath := filepath.Join(realParent, fi.Name())
	visitedSymlinks.Lock()
	defer visitedSymlinks.Unlock()
	if visitedSymlinks.m == nil {
		visitedSymlinks.m = make(map[string]struct{})
	}
	if _, ok := visitedSymlinks.m[realPath]; ok {
		return false
	}
	visitedSymlinks.m[realPath] = struct{}{}
	return true
}

var testHookScanDir = func(dir string) {}

func scanGoRoot() { scanGoDirs(true) }
func scanGoPath() { scanGoDirs(false) }

func scanGoDirs(goRoot bool) {
	if Debug {
		which := "$GOROOT"
		if !goRoot {
			which = "$GOPATH"
		}
		log.Printf("scanning " + which)
		defer log.Printf("scanned " + which)
	}
	dirScanMu.Lock()
	if dirScan == nil {
		dirScan = make(map[string]*pkg)
	}
	dirScanMu.Unlock()

	var wg sync.WaitGroup
	for _, path := range build.Default.SrcDirs() {
		isGoroot := path == filepath.Join(build.Default.GOROOT, "src")
		if isGoroot != goRoot {
			continue
		}
		if !goRoot {
			populateIgnoredDirs(path)
		}
		fsgate.enter()
		testHookScanDir(path)
		if Debug {
			log.Printf("scanGoDir, open dir: %v\n", path)
		}
		f, err := os.Open(path)
		if err != nil {
			fsgate.leave()
			fmt.Fprintf(os.Stderr, "goimports: scanning directories: %v\n", err)
			continue
		}
		children, err := f.Readdir(-1)
		f.Close()
		fsgate.leave()
		if err != nil {
			fmt.Fprintf(os.Stderr, "goimports: scanning directory entries: %v\n", err)
			continue
		}
		for _, child := range children {
			if !shouldTraverse(path, child) {
				continue
			}
			wg.Add(1)
			go func(path, name string) {
				defer wg.Done()
				scanDir(&wg, path, name)
			}(path, child.Name())
		}
	}
	wg.Wait()
}

func scanDir(wg *sync.WaitGroup, root, pkgrelpath string) {
	importpath := filepath.ToSlash(pkgrelpath)
	dir := filepath.Join(root, importpath)

	fsgate.enter()
	defer fsgate.leave()
	if Debug {
		log.Printf("scanning dir %s", dir)
	}
	pkgDir, err := os.Open(dir)
	if err != nil {
		return
	}
	children, err := pkgDir.Readdir(-1)
	pkgDir.Close()
	if err != nil {
		return
	}
	// hasGo tracks whether a directory actually appears to be a
	// Go source code directory. If $GOPATH == $HOME, and
	// $HOME/src has lots of other large non-Go projects in it,
	// then the calls to importPathToName below can be expensive.
	hasGo := false
	for _, child := range children {
		// Avoid .foo, _foo, and testdata directory trees.
		name := child.Name()
		if name == "" || name[0] == '.' || name[0] == '_' || name == "testdata" {
			continue
		}
		if strings.HasSuffix(name, ".go") {
			hasGo = true
		}
		if shouldTraverse(dir, child) {
			wg.Add(1)
			go func(root, name string) {
				defer wg.Done()
				scanDir(wg, root, name)
			}(root, filepath.Join(importpath, name))
		}
	}
	if hasGo {
		dirScanMu.Lock()
		dirScan[dir] = &pkg{
			importPath:      importpath,
			importPathShort: vendorlessImportPath(importpath),
			dir:             dir,
		}
		dirScanMu.Unlock()
	}
}

// vendorlessImportPath returns the devendorized version of the provided import path.
// e.g. "foo/bar/vendor/a/b" => "a/b"
func vendorlessImportPath(ipath string) string {
	// Devendorize for use in import statement.
	if i := strings.LastIndex(ipath, "/vendor/"); i >= 0 {
		return ipath[i+len("/vendor/"):]
	}
	if strings.HasPrefix(ipath, "vendor/") {
		return ipath[len("vendor/"):]
	}
	return ipath
}

// loadExports returns the set of exported symbols in the package at dir.
// It returns nil on error or if the package name in dir does not match expectPackage.
var loadExports func(expectPackage, dir string) map[string]bool = loadExportsGoPath

func loadExportsGoPath(expectPackage, dir string) map[string]bool {
	if Debug {
		log.Printf("loading exports in dir %s (seeking package %s)", dir, expectPackage)
	}
	exports := make(map[string]bool)

	ctx := build.Default

	// ReadDir is like ioutil.ReadDir, but only returns *.go files
	// and filters out _test.go files since they're not relevant
	// and only slow things down.
	ctx.ReadDir = func(dir string) (notTests []os.FileInfo, err error) {
		all, err := ioutil.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		notTests = all[:0]
		for _, fi := range all {
			name := fi.Name()
			if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				notTests = append(notTests, fi)
			}
		}
		return notTests, nil
	}

	files, err := ctx.ReadDir(dir)
	if err != nil {
		log.Print(err)
		return nil
	}

	fset := token.NewFileSet()

	for _, fi := range files {
		match, err := ctx.MatchFile(dir, fi.Name())
		if err != nil || !match {
			continue
		}
		fullFile := filepath.Join(dir, fi.Name())
		f, err := parser.ParseFile(fset, fullFile, nil, 0)
		if err != nil {
			if Debug {
				log.Printf("Parsing %s: %v", fullFile, err)
			}
			return nil
		}
		pkgName := f.Name.Name
		if pkgName == "documentation" {
			// Special case from go/build.ImportDir, not
			// handled by ctx.MatchFile.
			continue
		}
		if pkgName != expectPackage {
			if Debug {
				log.Printf("scan of dir %v is not expected package %v (actually %v)", dir, expectPackage, pkgName)
			}
			return nil
		}
		for name := range f.Scope.Objects {
			if ast.IsExported(name) {
				exports[name] = true
			}
		}
	}

	if Debug {
		exportList := make([]string, 0, len(exports))
		for k := range exports {
			exportList = append(exportList, k)
		}
		sort.Strings(exportList)
		log.Printf("loaded exports in dir %v (package %v): %v", dir, expectPackage, strings.Join(exportList, ", "))
	}
	return exports
}

// findImport searches for a package with the given symbols.
// If no package is found, findImport returns ("", false, nil)
//
// This is declared as a variable rather than a function so goimports
// can be easily extended by adding a file with an init function.
//
// The rename value tells goimports whether to use the package name as
// a local qualifier in an import. For example, if findImports("pkg",
// "X") returns ("foo/bar", rename=true), then goimports adds the
// import line:
// 	import pkg "foo/bar"
// to satisfy uses of pkg.X in the file.
var findImport func(pkgName string, symbols map[string]bool, filename string) (foundPkg string, rename bool, err error) = findImportGoPath

// findImportGoPath is the normal implementation of findImport. (Some
// companies have their own internally)
func findImportGoPath(pkgName string, symbols map[string]bool, filename string) (foundPkg string, rename bool, err error) {
	// Fast path for the standard library.
	// In the common case we hopefully never have to scan the GOPATH, which can
	// be slow with moving disks.
	if pkg, rename, ok := findImportStdlib(pkgName, symbols); ok {
		return pkg, rename, nil
	}
	if pkgName == "rand" && symbols["Read"] {
		// Special-case rand.Read.
		//
		// If findImportStdlib didn't find it above, don't go
		// searching for it, lest it find and pick math/rand
		// in GOROOT (new as of Go 1.6)
		//
		// crypto/rand is the safer choice.
		return "", false, nil
	}

	// TODO(sameer): look at the import lines for other Go files in the
	// local directory, since the user is likely to import the same packages
	// in the current Go file.  Return rename=true when the other Go files
	// use a renamed package that's also used in the current file.

	scanGoRootOnce.Do(scanGoRoot)
	if !strings.HasPrefix(filename, build.Default.GOROOT) {
		scanGoPathOnce.Do(scanGoPath)
	}

	var candidates []*pkg

	for _, pkg := range dirScan {
		if !strings.Contains(lastTwoComponents(pkg.importPathShort), pkgName) {
			// Speed optimization to minimize disk I/O:
			// the last two components on disk must contain the
			// package name somewhere.
			//
			// This permits mismatch naming like directory
			// "go-foo" being package "foo", or "pkg.v3" being "pkg",
			// or directory "google.golang.org/api/cloudbilling/v1"
			// being package "cloudbilling", but doesn't
			// permit a directory "foo" to be package
			// "bar", which is strongly discouraged
			// anyway. There's no reason goimports needs
			// to be slow just to accomodate that.
			continue
		}
		if !canUse(filename, pkg.dir) {
			continue
		}
		candidates = append(candidates, pkg)
	}

	sort.Sort(byImportPathShortLength(candidates))
	if Debug {
		for i, pkg := range candidates {
			log.Printf("%s candidate %d/%d: %v", pkgName, i+1, len(candidates), pkg.importPathShort)
		}
	}

	// Collect exports for packages with matching names.

	done := make(chan struct{}) // closed when we find the answer
	defer close(done)

	rescv := make([]chan *pkg, len(candidates))
	for i := range candidates {
		rescv[i] = make(chan *pkg, 1)
	}
	const maxConcurrentPackageImport = 4
	loadExportsSem := make(chan struct{}, maxConcurrentPackageImport)

	go func() {
		for i, pkg := range candidates {
			select {
			case loadExportsSem <- struct{}{}:
			case <-done:
				return
			}
			pkg := pkg
			resc := rescv[i]
			go func() {
				defer func() { <-loadExportsSem }()
				exports := loadExports(pkgName, pkg.dir)

				// If it doesn't have the right
				// symbols, send nil to mean no match.
				for symbol := range symbols {
					if !exports[symbol] {
						pkg = nil
						break
					}
				}
				resc <- pkg
			}()
		}
	}()
	for _, resc := range rescv {
		pkg := <-resc
		if pkg == nil {
			continue
		}
		// If the package name in the source doesn't match the import path's base,
		// return true so the rewriter adds a name (import foo "github.com/bar/go-foo")
		needsRename := path.Base(pkg.importPath) != pkgName
		return pkg.importPathShort, needsRename, nil
	}
	return "", false, nil
}

// canUse reports whether the package in dir is usable from filename,
// respecting the Go "internal" and "vendor" visibility rules.
func canUse(filename, dir string) bool {
	// Fast path check, before any allocations. If it doesn't contain vendor
	// or internal, it's not tricky:
	// Note that this can false-negative on directories like "notinternal",
	// but we check it correctly below. This is just a fast path.
	if !strings.Contains(dir, "vendor") && !strings.Contains(dir, "internal") {
		return true
	}

	dirSlash := filepath.ToSlash(dir)
	if !strings.Contains(dirSlash, "/vendor/") && !strings.Contains(dirSlash, "/internal/") && !strings.HasSuffix(dirSlash, "/internal") {
		return true
	}
	// Vendor or internal directory only visible from children of parent.
	// That means the path from the current directory to the target directory
	// can contain ../vendor or ../internal but not ../foo/vendor or ../foo/internal
	// or bar/vendor or bar/internal.
	// After stripping all the leading ../, the only okay place to see vendor or internal
	// is at the very beginning of the path.
	abs, err := filepath.Abs(filename)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(abs, dir)
	if err != nil {
		return false
	}
	relSlash := filepath.ToSlash(rel)
	if i := strings.LastIndex(relSlash, "../"); i >= 0 {
		relSlash = relSlash[i+len("../"):]
	}
	return !strings.Contains(relSlash, "/vendor/") && !strings.Contains(relSlash, "/internal/") && !strings.HasSuffix(relSlash, "/internal")
}

// lastTwoComponents returns at most the last two path components
// of v, using either / or \ as the path separator.
func lastTwoComponents(v string) string {
	nslash := 0
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '/' || v[i] == '\\' {
			nslash++
			if nslash == 2 {
				return v[i:]
			}
		}
	}
	return v
}

type visitFn func(node ast.Node) ast.Visitor

func (fn visitFn) Visit(node ast.Node) ast.Visitor {
	return fn(node)
}

func findImportStdlib(shortPkg string, symbols map[string]bool) (importPath string, rename, ok bool) {
	for symbol := range symbols {
		key := shortPkg + "." + symbol
		path := stdlib[key]
		if path == "" {
			if key == "rand.Read" {
				continue
			}
			return "", false, false
		}
		if importPath != "" && importPath != path {
			// Ambiguous. Symbols pointed to different things.
			return "", false, false
		}
		importPath = path
	}
	if importPath == "" && shortPkg == "rand" && symbols["Read"] {
		return "crypto/rand", false, true
	}
	return importPath, false, importPath != ""
}
