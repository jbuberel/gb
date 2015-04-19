package gb

import (
	"fmt"
	"path/filepath"
)

// Build returns a Target representing the result of compiling the Package pkg
// and its dependencies. If pkg is a command, then the results of build include
// linking the final binary into pkg.Context.Bindir().
func Build(pkg *Package) Target {
	t := buildPackage(pkg)
	if err := t.Result(); err == nil {
		if pkg.isMain() {
			t = Ld(pkg, t.(PkgTarget))
		}
	}
	return t
}

// buildPackage returns a Target repesenting the results of compiling
// pkg and its dependencies.
func buildPackage(pkg *Package) Target {
	if err := pkg.Result(); err != nil {
		return errTarget{fmt.Errorf("buildPackage: %v", err)}
	}
	return pkg.ctx.targetOrMissing(fmt.Sprintf("compile:%s:%s", pkg.Scope, pkg.p.ImportPath), func() Target {
		deps := buildDependencies(pkg.ctx, pkg.p.Imports...)
		return Compile(pkg, deps...)
	})
}

// buildCommand returns a Target repesenting the results of compiling
// pkg as a command and linking the result into pkg.Context.Bindir().
func buildCommand(pkg *Package) Target {
	var deps []Target
	for _, dep := range pkg.p.Imports {
		if _, ok := stdlib[dep]; ok {
			continue
		}
		pkg := resolvePackage(pkg.ctx, dep)
		deps = append(deps, buildPackage(pkg))
	}
	compile := Compile(pkg, deps...)
	ld := Ld(pkg, compile)
	return ld
}

// Compile returns a Target representing all the steps required to build a go package.
func Compile(pkg *Package, deps ...Target) PkgTarget {
	if err := pkg.Result(); err != nil {
		return errTarget{fmt.Errorf("compile: %v", err)}
	}
	return pkg.ctx.addTargetIfMissing(fmt.Sprintf("compile:%s:%s", pkg.Scope, pkg.p.ImportPath), func() Target {
		if !isStale(pkg) {
			return cachedPackage(pkg)
		}
		var gofiles []string
		gofiles = append(gofiles, pkg.p.GoFiles...)
		var objs []ObjTarget
		if len(pkg.p.CgoFiles) > 0 {
			// cgo, cgofiles := cgo(pkg, deps...)
			// deps = append(deps, cgo[0])
			// objs = append(objs, cgo...)
			// gofiles = append(gofiles, cgofiles...)
		}
		objs = append(objs, Gc(pkg, gofiles, deps...))
		for _, sfile := range pkg.p.SFiles {
			objs = append(objs, Asm(pkg, sfile))
		}
		if pkg.Complete() {
			return Install(pkg, objs[0].(PkgTarget))
		}
		return Install(pkg, Pack(pkg, objs...))
	}).(PkgTarget)
}

// ObjTarget represents a compiled Go object (.5, .6, etc)
type ObjTarget interface {
	Target

	// Objfile is the name of the file that is produced if the target is successful.
	Objfile() string
}

type gc struct {
	target
	pkg     *Package
	gofiles []string
}

func (g *gc) String() string {
	return fmt.Sprintf("compile %v", g.pkg)
}

func (g *gc) compile() error {
	Infof("compile %v %v", g.pkg.p.ImportPath, g.gofiles)
	includes := g.pkg.ctx.IncludePaths()
	importpath := g.pkg.p.ImportPath
	if g.pkg.Scope == "test" {
		// TODO(dfc) gross
		includes = append(includes, g.pkg.ExtraIncludes)
	}
	return g.pkg.ctx.tc.Gc(includes, importpath, g.pkg.p.Dir, g.Objfile(), g.gofiles, g.pkg.Complete())
}

func (g *gc) Objfile() string {
	return filepath.Join(objdir(g.pkg), g.pkg.Name()+".a")
}

func (g *gc) Pkgfile() string {
	return g.Objfile()
}

// Gc returns a Target representing the result of compiling a set of gofiles with the Context specified gc Compiler.
func Gc(pkg *Package, gofiles []string, deps ...Target) interface {
	ObjTarget
	Pkgfile() string // implements PkgTarget
} {
	gc := gc{
		pkg:     pkg,
		gofiles: gofiles,
	}
	gc.target = newTarget(gc.compile, deps...)
	return &gc
}

// PkgTarget represents a Target that produces a pkg (.a) file.
type PkgTarget interface {
	Target

	// Pkgfile returns the name of the file that is produced by the Target if successful.
	Pkgfile() string
}

type pack struct {
	c   chan error
	pkg *Package
}

func (p *pack) Result() error {
	err := <-p.c
	p.c <- err
	return err
}

func (p *pack) pack(objs ...ObjTarget) {
	Debugf("pack %v", p.pkg)
	afiles := make([]string, 0, len(objs))
	for _, obj := range objs {
		err := obj.Result()
		if err != nil {
			p.c <- err
			return
		}
		// pkg.a (compiled Go code) is always first
		afiles = append(afiles, obj.Objfile())
	}
	p.c <- p.pkg.ctx.tc.Pack(afiles...)
}

func (p *pack) Pkgfile() string {
	return filepath.Join(objdir(p.pkg), p.pkg.Name()+".a")
}

// Pack returns a Target representing the result of packing a
// set of Context specific object files into an archive.
func Pack(pkg *Package, deps ...ObjTarget) PkgTarget {
	pack := pack{
		c:   make(chan error, 1),
		pkg: pkg,
	}
	go pack.pack(deps...)
	return &pack
}

type asm struct {
	target
	pkg   *Package
	sfile string
}

func (a *asm) Objfile() string {
	return filepath.Join(a.pkg.ctx.workdir, a.pkg.p.ImportPath, stripext(a.sfile)+".6")
}

func (a *asm) asm() error {
	Infof("asm %v", a.sfile)
	return a.pkg.ctx.tc.Asm(a.pkg.p.Dir, a.Objfile(), filepath.Join(a.pkg.p.Dir, a.sfile))
}

// Asm returns a Target representing the result of assembling
// sfile with the Context specified asssembler.
func Asm(pkg *Package, sfile string) ObjTarget {
	asm := asm{
		pkg:   pkg,
		sfile: sfile,
	}
	asm.target = newTarget(asm.asm)
	return &asm
}

type ld struct {
	target
	pkg   *Package
	afile PkgTarget
}

func (l *ld) link() error {
	target := filepath.Join(objdir(l.pkg), l.pkg.p.Name)
	Infof("link %v [%v]", target, l.afile.Pkgfile())
	includes := l.pkg.ctx.IncludePaths()
	if l.pkg.Scope == "test" {
		// TODO(dfc) gross
		includes = append(includes, l.pkg.ExtraIncludes)
		target += ".test"
	}
	return l.pkg.ctx.tc.Ld(includes, target, l.afile.Pkgfile())
}

// Ld returns a Target representing the result of linking a
// Package into a command with the Context provided linker.
func Ld(pkg *Package, afile PkgTarget) Target {
	ld := ld{
		pkg:   pkg,
		afile: afile,
	}
	ld.target = newTarget(ld.link, afile)
	return &ld
}

func stripext(path string) string {
	ext := filepath.Ext(path)
	return path[:len(ext)]
}

// objdir returns the destination for object files compiled for this Package.
func objdir(pkg *Package) string {
	switch pkg.Scope {
	case "test":
		return filepath.Join(testobjdir(pkg), filepath.Dir(filepath.FromSlash(pkg.p.ImportPath)))
	default:
		return filepath.Join(pkg.ctx.workdir, filepath.Dir(filepath.FromSlash(pkg.p.ImportPath)))
	}
}

func testobjdir(pkg *Package) string {
	return filepath.Join(pkg.ctx.workdir, filepath.FromSlash(pkg.p.ImportPath), "_test")
}

// buildDependencies resolves the dependencies the package paths.
func buildDependencies(ctx *Context, imports ...string) []Target {
	var deps []Target
	for _, dep := range imports {
		if _, ok := stdlib[dep]; ok {
			continue
		}
		pkg := resolvePackage(ctx, dep)
		deps = append(deps, buildPackage(pkg))
	}
	return deps
}
