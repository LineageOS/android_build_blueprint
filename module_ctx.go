// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package blueprint

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/scanner"

	"github.com/google/blueprint/parser"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"
)

// A Module handles generating all of the Ninja build actions needed to build a
// single module based on properties defined in a Blueprints file.  Module
// objects are initially created during the parse phase of a Context using one
// of the registered module types (and the associated ModuleFactory function).
// The Module's properties struct is automatically filled in with the property
// values specified in the Blueprints file (see Context.RegisterModuleType for more
// information on this).
//
// A Module can be split into multiple Modules by a Mutator.  All existing
// properties set on the module will be duplicated to the new Module, and then
// modified as necessary by the Mutator.
//
// The Module implementation can access the build configuration as well as any
// modules on which it depends (as defined by the "deps" property
// specified in the Blueprints file, dynamically added by implementing the
// (deprecated) DynamicDependerModule interface, or dynamically added by a
// BottomUpMutator) using the ModuleContext passed to GenerateBuildActions.
// This ModuleContext is also used to create Ninja build actions and to report
// errors to the user.
//
// In addition to implementing the GenerateBuildActions method, a Module should
// implement methods that provide dependant modules and singletons information
// they need to generate their build actions.  These methods will only be called
// after GenerateBuildActions is called because the Context calls
// GenerateBuildActions in dependency-order (and singletons are invoked after
// all the Modules).  The set of methods a Module supports will determine how
// dependant Modules interact with it.
//
// For example, consider a Module that is responsible for generating a library
// that other modules can link against.  The library Module might implement the
// following interface:
//
//	type LibraryProducer interface {
//	    LibraryFileName() string
//	}
//
//	func IsLibraryProducer(module blueprint.Module) {
//	    _, ok := module.(LibraryProducer)
//	    return ok
//	}
//
// A binary-producing Module that depends on the library Module could then do:
//
//	func (m *myBinaryModule) GenerateBuildActions(ctx blueprint.ModuleContext) {
//	    ...
//	    var libraryFiles []string
//	    ctx.VisitDepsDepthFirstIf(IsLibraryProducer,
//	        func(module blueprint.Module) {
//	            libProducer := module.(LibraryProducer)
//	            libraryFiles = append(libraryFiles, libProducer.LibraryFileName())
//	        })
//	    ...
//	}
//
// to build the list of library file names that should be included in its link
// command.
//
// GenerateBuildActions may be called from multiple threads.  It is guaranteed to
// be called after it has finished being called on all dependencies and on all
// variants of that appear earlier in the ModuleContext.VisitAllModuleVariants list.
// Any accesses to global variables or to Module objects that are not dependencies
// or variants of the current Module must be synchronized by the implementation of
// GenerateBuildActions.
type Module interface {
	// Name returns a string used to uniquely identify each module.  The return
	// value must be unique across all modules.  It is only called once, during
	// initial blueprint parsing.  To change the name later a mutator must call
	// MutatorContext.Rename
	//
	// In most cases, Name should return the contents of a "name:" property from
	// the blueprint file.  An embeddable SimpleName object can be used for this
	// case.
	Name() string

	// GenerateBuildActions is called by the Context that created the Module
	// during its generate phase.  This call should generate all Ninja build
	// actions (rules, pools, and build statements) needed to build the module.
	GenerateBuildActions(ModuleContext)
}

type ModuleProxy struct {
	module Module
}

func CreateModuleProxy(module Module) ModuleProxy {
	return ModuleProxy{
		module: module,
	}
}

func (m ModuleProxy) Name() string {
	return m.module.Name()
}

func (m ModuleProxy) GenerateBuildActions(context ModuleContext) {
	m.module.GenerateBuildActions(context)
}

// A DynamicDependerModule is a Module that may add dependencies that do not
// appear in its "deps" property.  Any Module that implements this interface
// will have its DynamicDependencies method called by the Context that created
// it during generate phase.
//
// Deprecated, use a BottomUpMutator instead
type DynamicDependerModule interface {
	Module

	// DynamicDependencies is called by the Context that created the
	// DynamicDependerModule during its generate phase.  This call should return
	// the list of module names that the DynamicDependerModule depends on
	// dynamically.  Module names that already appear in the "deps" property may
	// but do not need to be included in the returned list.
	DynamicDependencies(DynamicDependerModuleContext) []string
}

type EarlyModuleContext interface {
	// Module returns the current module as a Module.  It should rarely be necessary, as the module already has a
	// reference to itself.
	Module() Module

	// ModuleName returns the name of the module.  This is generally the value that was returned by Module.Name() when
	// the module was created, but may have been modified by calls to BottomUpMutatorContext.Rename.
	ModuleName() string

	// ModuleDir returns the path to the directory that contains the definition of the module.
	ModuleDir() string

	// ModuleType returns the name of the module type that was used to create the module, as specified in
	// Context.RegisterModuleType().
	ModuleType() string

	// ModuleTags returns the tags for this module that should be passed to
	// ninja for analysis. For example:
	// [
	//   "module_name": "libfoo",
	//   "module_type": "cc_library",
	// ]
	ModuleTags() map[string]string

	// BlueprintsFile returns the name of the blueprint file that contains the definition of this
	// module.
	BlueprintsFile() string

	// Config returns the config object that was passed to Context.PrepareBuildActions.
	Config() interface{}

	// ContainsProperty returns true if the specified property name was set in the module definition.
	ContainsProperty(name string) bool

	// Errorf reports an error at the specified position of the module definition file.
	Errorf(pos scanner.Position, fmt string, args ...interface{})

	// ModuleErrorf reports an error at the line number of the module type in the module definition.
	ModuleErrorf(fmt string, args ...interface{})

	// PropertyErrorf reports an error at the line number of a property in the module definition.
	PropertyErrorf(property, fmt string, args ...interface{})

	// OtherModulePropertyErrorf reports an error at the line number of a property in the given module definition.
	OtherModulePropertyErrorf(logicModule Module, property string, format string, args ...interface{})

	// Failed returns true if any errors have been reported.  In most cases the module can continue with generating
	// build rules after an error, allowing it to report additional errors in a single run, but in cases where the error
	// has prevented the module from creating necessary data it can return early when Failed returns true.
	Failed() bool

	// GlobWithDeps returns a list of files and directories that match the
	// specified pattern but do not match any of the patterns in excludes.
	// Any directories will have a '/' suffix.  It also adds efficient
	// dependencies to rerun the primary builder whenever a file matching
	// the pattern as added or removed, without rerunning if a file that
	// does not match the pattern is added to a searched directory.
	GlobWithDeps(pattern string, excludes []string) ([]string, error)

	// Fs returns a pathtools.Filesystem that can be used to interact with files.  Using the Filesystem interface allows
	// the module to be used in build system tests that run against a mock filesystem.
	Fs() pathtools.FileSystem

	// AddNinjaFileDeps adds dependencies on the specified files to the rule that creates the ninja manifest.  The
	// primary builder will be rerun whenever the specified files are modified.
	AddNinjaFileDeps(deps ...string)

	moduleInfo() *moduleInfo

	error(err error)

	// Namespace returns the Namespace object provided by the NameInterface set by Context.SetNameInterface, or the
	// default SimpleNameInterface if Context.SetNameInterface was not called.
	Namespace() Namespace

	// ModuleFactories returns a map of all of the global ModuleFactories by name.
	ModuleFactories() map[string]ModuleFactory

	// HasMutatorFinished returns true if the given mutator has finished running.
	// It will panic if given an invalid mutator name.
	HasMutatorFinished(mutatorName string) bool
}

type BaseModuleContext interface {
	EarlyModuleContext

	// GetDirectDepWithTag returns the Module the direct dependency with the specified name, or nil if
	// none exists.  It panics if the dependency does not have the specified tag.
	GetDirectDepWithTag(name string, tag DependencyTag) Module

	// GetDirectDep returns the Module and DependencyTag for the  direct dependency with the specified
	// name, or nil if none exists.  If there are multiple dependencies on the same module it returns
	// the first DependencyTag.
	GetDirectDep(name string) (Module, DependencyTag)

	// VisitDirectDeps calls visit for each direct dependency.  If there are multiple direct dependencies on the same
	// module visit will be called multiple times on that module and OtherModuleDependencyTag will return a different
	// tag for each.
	//
	// The Module passed to the visit function should not be retained outside of the visit function, it may be
	// invalidated by future mutators.
	VisitDirectDeps(visit func(Module))

	VisitDirectDepsProxy(visit func(proxy ModuleProxy))

	// VisitDirectDepsIf calls pred for each direct dependency, and if pred returns true calls visit.  If there are
	// multiple direct dependencies on the same module pred and visit will be called multiple times on that module and
	// OtherModuleDependencyTag will return a different tag for each.
	//
	// The Module passed to the visit function should not be retained outside of the visit function, it may be
	// invalidated by future mutators.
	VisitDirectDepsIf(pred func(Module) bool, visit func(Module))

	// VisitDepsDepthFirst calls visit for each transitive dependency, traversing the dependency tree in depth first
	// order. visit will only be called once for any given module, even if there are multiple paths through the
	// dependency tree to the module or multiple direct dependencies with different tags.  OtherModuleDependencyTag will
	// return the tag for the first path found to the module.
	//
	// The Module passed to the visit function should not be retained outside of the visit function, it may be
	// invalidated by future mutators.
	VisitDepsDepthFirst(visit func(Module))

	// VisitDepsDepthFirstIf calls pred for each transitive dependency, and if pred returns true calls visit, traversing
	// the dependency tree in depth first order.  visit will only be called once for any given module, even if there are
	// multiple paths through the dependency tree to the module or multiple direct dependencies with different tags.
	// OtherModuleDependencyTag will return the tag for the first path found to the module.  The return value of pred
	// does not affect which branches of the tree are traversed.
	//
	// The Module passed to the visit function should not be retained outside of the visit function, it may be
	// invalidated by future mutators.
	VisitDepsDepthFirstIf(pred func(Module) bool, visit func(Module))

	// WalkDeps calls visit for each transitive dependency, traversing the dependency tree in top down order.  visit may
	// be called multiple times for the same (child, parent) pair if there are multiple direct dependencies between the
	// child and parent with different tags.  OtherModuleDependencyTag will return the tag for the currently visited
	// (child, parent) pair.  If visit returns false WalkDeps will not continue recursing down to child.
	//
	// The Modules passed to the visit function should not be retained outside of the visit function, they may be
	// invalidated by future mutators.
	WalkDeps(visit func(Module, Module) bool)

	WalkDepsProxy(visit func(ModuleProxy, ModuleProxy) bool)

	// PrimaryModule returns the first variant of the current module.  Variants of a module are always visited in
	// order by mutators and GenerateBuildActions, so the data created by the current mutator can be read from the
	// Module returned by PrimaryModule without data races.  This can be used to perform singleton actions that are
	// only done once for all variants of a module.
	PrimaryModule() Module

	// FinalModule returns the last variant of the current module.  Variants of a module are always visited in
	// order by mutators and GenerateBuildActions, so the data created by the current mutator can be read from all
	// variants using VisitAllModuleVariants if the current module == FinalModule().  This can be used to perform
	// singleton actions that are only done once for all variants of a module.
	FinalModule() Module

	// IsFinalModule returns if the current module is the last variant.  Variants of a module are always visited in
	// order by mutators and GenerateBuildActions, so the data created by the current mutator can be read from all
	// variants using VisitAllModuleVariants if the current module is the last one.  This can be used to perform
	// singleton actions that are only done once for all variants of a module.
	IsFinalModule(module Module) bool

	// VisitAllModuleVariants calls visit for each variant of the current module.  Variants of a module are always
	// visited in order by mutators and GenerateBuildActions, so the data created by the current mutator can be read
	// from all variants if the current module is the last one.  Otherwise, care must be taken to not access any
	// data modified by the current mutator.
	VisitAllModuleVariants(visit func(Module))

	// VisitAllModuleVariantProxies calls visit for each variant of the current module.  Variants of a module are always
	// visited in order by mutators and GenerateBuildActions, so the data created by the current mutator can be read
	// from all variants if the current module is the last one.  Otherwise, care must be taken to not access any
	// data modified by the current mutator.
	VisitAllModuleVariantProxies(visit func(proxy ModuleProxy))

	// OtherModuleName returns the name of another Module.  See BaseModuleContext.ModuleName for more information.
	// It is intended for use inside the visit functions of Visit* and WalkDeps.
	OtherModuleName(m Module) string

	// OtherModuleDir returns the directory of another Module.  See BaseModuleContext.ModuleDir for more information.
	// It is intended for use inside the visit functions of Visit* and WalkDeps.
	OtherModuleDir(m Module) string

	// OtherModuleType returns the type of another Module.  See BaseModuleContext.ModuleType for more information.
	// It is intended for use inside the visit functions of Visit* and WalkDeps.
	OtherModuleType(m Module) string

	// OtherModuleErrorf reports an error on another Module.  See BaseModuleContext.ModuleErrorf for more information.
	// It is intended for use inside the visit functions of Visit* and WalkDeps.
	OtherModuleErrorf(m Module, fmt string, args ...interface{})

	// OtherModuleDependencyTag returns the dependency tag used to depend on a module, or nil if there is no dependency
	// on the module.  When called inside a Visit* method with current module being visited, and there are multiple
	// dependencies on the module being visited, it returns the dependency tag used for the current dependency.
	OtherModuleDependencyTag(m Module) DependencyTag

	// OtherModuleExists returns true if a module with the specified name exists, as determined by the NameInterface
	// passed to Context.SetNameInterface, or SimpleNameInterface if it was not called.
	OtherModuleExists(name string) bool

	// ModuleFromName returns (module, true) if a module exists by the given name and same context namespace,
	// or (nil, false) if it does not exist. It panics if there is either more than one
	// module of the given name, or if the given name refers to an alias instead of a module.
	// There are no guarantees about which variant of the module will be returned.
	// Prefer retrieving the module using GetDirectDep or a visit function, when possible, as
	// this will guarantee the appropriate module-variant dependency is returned.
	//
	// WARNING: This should _only_ be used within the context of bp2build, where variants and
	// dependencies are not created.
	ModuleFromName(name string) (Module, bool)

	// OtherModuleDependencyVariantExists returns true if a module with the
	// specified name and variant exists. The variant must match the given
	// variations. It must also match all the non-local variations of the current
	// module. In other words, it checks for the module that AddVariationDependencies
	// would add a dependency on with the same arguments.
	OtherModuleDependencyVariantExists(variations []Variation, name string) bool

	// OtherModuleFarDependencyVariantExists returns true if a module with the
	// specified name and variant exists. The variant must match the given
	// variations, but not the non-local variations of the current module. In
	// other words, it checks for the module that AddFarVariationDependencies
	// would add a dependency on with the same arguments.
	OtherModuleFarDependencyVariantExists(variations []Variation, name string) bool

	// OtherModuleReverseDependencyVariantExists returns true if a module with the
	// specified name exists with the same variations as the current module. In
	// other words, it checks for the module that AddReverseDependency would add a
	// dependency on with the same argument.
	OtherModuleReverseDependencyVariantExists(name string) bool

	// OtherModuleProvider returns the value for a provider for the given module.  If the value is
	// not set it returns nil and false.  The value returned may be a deep copy of the value originally
	// passed to SetProvider.
	//
	// This method shouldn't be used directly, prefer the type-safe android.OtherModuleProvider instead.
	OtherModuleProvider(m Module, provider AnyProviderKey) (any, bool)

	// OtherModuleIsAutoGenerated returns true if a module has been generated from another module,
	// instead of being defined in Android.bp file
	OtherModuleIsAutoGenerated(m Module) bool

	// Provider returns the value for a provider for the current module.  If the value is
	// not set it returns nil and false.  It panics if called before the appropriate
	// mutator or GenerateBuildActions pass for the provider.  The value returned may be a deep
	// copy of the value originally passed to SetProvider.
	//
	// This method shouldn't be used directly, prefer the type-safe android.ModuleProvider instead.
	Provider(provider AnyProviderKey) (any, bool)

	// SetProvider sets the value for a provider for the current module.  It panics if not called
	// during the appropriate mutator or GenerateBuildActions pass for the provider, if the value
	// is not of the appropriate type, or if the value has already been set.  The value should not
	// be modified after being passed to SetProvider.
	//
	// This method shouldn't be used directly, prefer the type-safe android.SetProvider instead.
	SetProvider(provider AnyProviderKey, value any)

	EarlyGetMissingDependencies() []string

	EqualModules(m1, m2 Module) bool

	base() *baseModuleContext
}

type DynamicDependerModuleContext BottomUpMutatorContext

type ModuleContext interface {
	BaseModuleContext

	// ModuleSubDir returns a unique name for the current variant of a module that can be used as part of the path
	// to ensure that each variant of a module gets its own intermediates directory to write to.
	ModuleSubDir() string

	ModuleCacheKey() string

	// Variable creates a new ninja variable scoped to the module.  It can be referenced by calls to Rule and Build
	// in the same module.
	Variable(pctx PackageContext, name, value string)

	// Rule creates a new ninja rule scoped to the module.  It can be referenced by calls to Build in the same module.
	Rule(pctx PackageContext, name string, params RuleParams, argNames ...string) Rule

	// Build creates a new ninja build statement.
	Build(pctx PackageContext, params BuildParams)

	// GetMissingDependencies returns the list of dependencies that were passed to AddDependencies or related methods,
	// but do not exist.  It can be used with Context.SetAllowMissingDependencies to allow the primary builder to
	// handle missing dependencies on its own instead of having Blueprint treat them as an error.
	GetMissingDependencies() []string
}

var _ BaseModuleContext = (*baseModuleContext)(nil)

type baseModuleContext struct {
	context        *Context
	config         interface{}
	module         *moduleInfo
	errs           []error
	visitingParent *moduleInfo
	visitingDep    depInfo
	ninjaFileDeps  []string
}

func (d *baseModuleContext) moduleInfo() *moduleInfo {
	return d.module
}

func (d *baseModuleContext) Module() Module {
	return d.module.logicModule
}

func (d *baseModuleContext) ModuleName() string {
	return d.module.Name()
}

func (d *baseModuleContext) ModuleType() string {
	return d.module.typeName
}

func (d *baseModuleContext) ModuleTags() map[string]string {
	return map[string]string{
		"module_name": d.ModuleName(),
		"module_type": d.ModuleType(),
	}
}

func (d *baseModuleContext) ContainsProperty(name string) bool {
	_, ok := d.module.propertyPos[name]
	return ok
}

func (d *baseModuleContext) ModuleDir() string {
	return filepath.Dir(d.module.relBlueprintsFile)
}

func (d *baseModuleContext) BlueprintsFile() string {
	return d.module.relBlueprintsFile
}

func (d *baseModuleContext) Config() interface{} {
	return d.config
}

func (d *baseModuleContext) error(err error) {
	if err != nil {
		d.errs = append(d.errs, err)
	}
}

func (d *baseModuleContext) Errorf(pos scanner.Position,
	format string, args ...interface{}) {

	d.error(&BlueprintError{
		Err: fmt.Errorf(format, args...),
		Pos: pos,
	})
}

func (d *baseModuleContext) ModuleErrorf(format string,
	args ...interface{}) {

	d.error(d.context.moduleErrorf(d.module, format, args...))
}

func (d *baseModuleContext) PropertyErrorf(property, format string,
	args ...interface{}) {

	d.error(d.context.PropertyErrorf(d.module.logicModule, property, format, args...))
}

func (d *baseModuleContext) OtherModulePropertyErrorf(logicModule Module, property string, format string,
	args ...interface{}) {

	d.error(d.context.PropertyErrorf(logicModule, property, format, args...))
}

func (d *baseModuleContext) Failed() bool {
	return len(d.errs) > 0
}

func (d *baseModuleContext) GlobWithDeps(pattern string,
	excludes []string) ([]string, error) {
	return d.context.glob(pattern, excludes)
}

func (d *baseModuleContext) Fs() pathtools.FileSystem {
	return d.context.fs
}

func (d *baseModuleContext) Namespace() Namespace {
	return d.context.nameInterface.GetNamespace(newNamespaceContext(d.module))
}

func (d *baseModuleContext) HasMutatorFinished(mutatorName string) bool {
	return d.context.HasMutatorFinished(mutatorName)
}

var _ ModuleContext = (*moduleContext)(nil)

type moduleContext struct {
	baseModuleContext
	scope              *localScope
	actionDefs         localBuildActions
	handledMissingDeps bool
}

func (m *baseModuleContext) EqualModules(m1, m2 Module) bool {
	return getWrappedModule(m1) == getWrappedModule(m2)
}

func (m *baseModuleContext) OtherModuleName(logicModule Module) string {
	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	return module.Name()
}

func (m *baseModuleContext) OtherModuleDir(logicModule Module) string {
	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	return filepath.Dir(module.relBlueprintsFile)
}

func (m *baseModuleContext) OtherModuleType(logicModule Module) string {
	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	return module.typeName
}

func (m *baseModuleContext) OtherModuleErrorf(logicModule Module, format string,
	args ...interface{}) {

	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	m.errs = append(m.errs, &ModuleError{
		BlueprintError: BlueprintError{
			Err: fmt.Errorf(format, args...),
			Pos: module.pos,
		},
		module: module,
	})
}

func getWrappedModule(module Module) Module {
	if mp, isProxy := module.(ModuleProxy); isProxy {
		return mp.module
	}
	return module
}

func (m *baseModuleContext) OtherModuleDependencyTag(logicModule Module) DependencyTag {
	// fast path for calling OtherModuleDependencyTag from inside VisitDirectDeps
	if m.visitingDep.module != nil && getWrappedModule(logicModule) == m.visitingDep.module.logicModule {
		return m.visitingDep.tag
	}

	if m.visitingParent == nil {
		return nil
	}

	for _, dep := range m.visitingParent.directDeps {
		if dep.module.logicModule == getWrappedModule(logicModule) {
			return dep.tag
		}
	}

	return nil
}

func (m *baseModuleContext) ModuleFromName(name string) (Module, bool) {
	moduleGroup, exists := m.context.nameInterface.ModuleFromName(name, m.module.namespace())
	if exists {
		if len(moduleGroup.modules) != 1 {
			panic(fmt.Errorf("Expected exactly one module named %q, but got %d", name, len(moduleGroup.modules)))
		}
		moduleInfo := moduleGroup.modules[0]
		if moduleInfo != nil {
			return moduleInfo.logicModule, true
		} else {
			panic(fmt.Errorf(`Expected actual module named %q, but group did not contain a module.
    There may instead be an alias by that name.`, name))
		}
	}
	return nil, exists
}

func (m *baseModuleContext) OtherModuleExists(name string) bool {
	_, exists := m.context.nameInterface.ModuleFromName(name, m.module.namespace())
	return exists
}

func (m *baseModuleContext) OtherModuleDependencyVariantExists(variations []Variation, name string) bool {
	possibleDeps := m.context.moduleGroupFromName(name, m.module.namespace())
	if possibleDeps == nil {
		return false
	}
	found, _, errs := m.context.findVariant(m.module, m.config, possibleDeps, variations, false, false)
	if errs != nil {
		panic(errors.Join(errs...))
	}
	return found != nil
}

func (m *baseModuleContext) OtherModuleFarDependencyVariantExists(variations []Variation, name string) bool {
	possibleDeps := m.context.moduleGroupFromName(name, m.module.namespace())
	if possibleDeps == nil {
		return false
	}
	found, _, errs := m.context.findVariant(m.module, m.config, possibleDeps, variations, true, false)
	if errs != nil {
		panic(errors.Join(errs...))
	}
	return found != nil
}

func (m *baseModuleContext) OtherModuleReverseDependencyVariantExists(name string) bool {
	possibleDeps := m.context.moduleGroupFromName(name, m.module.namespace())
	if possibleDeps == nil {
		return false
	}
	found, _, errs := m.context.findVariant(m.module, m.config, possibleDeps, nil, false, true)
	if errs != nil {
		panic(errors.Join(errs...))
	}
	return found != nil
}

func (m *baseModuleContext) OtherModuleProvider(logicModule Module, provider AnyProviderKey) (any, bool) {
	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	return m.context.provider(module, provider.provider())
}

func (m *baseModuleContext) Provider(provider AnyProviderKey) (any, bool) {
	return m.context.provider(m.module, provider.provider())
}

func (m *baseModuleContext) SetProvider(provider AnyProviderKey, value interface{}) {
	m.context.setProvider(m.module, provider.provider(), value)
}

func (m *moduleContext) cacheModuleBuildActions(key *BuildActionCacheKey) {
	var providers []CachedProvider
	for i, p := range m.module.providers {
		if p != nil && providerRegistry[i].mutator == "" {
			providers = append(providers,
				CachedProvider{
					Id:    providerRegistry[i],
					Value: &p,
				})
		}
	}

	// These show up in the ninja file, so we need to cache these to ensure we
	// re-generate ninja file if they changed.
	relPos := m.module.pos
	relPos.Filename = m.module.relBlueprintsFile
	data := BuildActionCachedData{
		Providers: providers,
		Pos:       &relPos,
	}

	m.context.updateBuildActionsCache(key, &data)
}

func (m *moduleContext) restoreModuleBuildActions() (bool, *BuildActionCacheKey) {
	// Whether the incremental flag is set and the module type supports
	// incremental, this will decide weather to cache the data for the module.
	incrementalEnabled := false
	// Whether the above conditions are true and we can try to restore from
	// the cache for this module, i.e., no env, product variables and Soong
	// code changes.
	incrementalAnalysis := false
	var cacheKey *BuildActionCacheKey = nil
	if m.context.GetIncrementalEnabled() {
		if im, ok := m.module.logicModule.(Incremental); ok {
			incrementalEnabled = im.IncrementalSupported()
			incrementalAnalysis = m.context.GetIncrementalAnalysis() && incrementalEnabled
		}
	}
	if incrementalEnabled {
		hash, err := proptools.CalculateHash(m.module.properties)
		if err != nil {
			panic(newPanicErrorf(err, "failed to calculate properties hash"))
		}
		cacheInput := new(BuildActionCacheInput)
		cacheInput.PropertiesHash = hash
		m.VisitDirectDeps(func(module Module) {
			cacheInput.ProvidersHash =
				append(cacheInput.ProvidersHash, m.context.moduleInfo[module].providerInitialValueHashes)
		})
		hash, err = proptools.CalculateHash(&cacheInput)
		if err != nil {
			panic(newPanicErrorf(err, "failed to calculate cache input hash"))
		}
		cacheKey = &BuildActionCacheKey{
			Id:        m.ModuleCacheKey(),
			InputHash: hash,
		}
		m.module.buildActionCacheKey = cacheKey
	}

	restored := false
	if incrementalAnalysis && cacheKey != nil {
		// Try to restore from cache if there is a cache hit
		data := m.context.getBuildActionsFromCache(cacheKey)
		relPos := m.module.pos
		relPos.Filename = m.module.relBlueprintsFile
		if data != nil && data.Pos != nil && relPos == *data.Pos {
			for _, provider := range data.Providers {
				m.context.setProvider(m.module, provider.Id, *provider.Value)
			}
			m.module.incrementalRestored = true
			m.module.orderOnlyStrings = data.OrderOnlyStrings
			restored = true
		}
	}

	return restored, cacheKey
}

func (m *baseModuleContext) GetDirectDep(name string) (Module, DependencyTag) {
	for _, dep := range m.module.directDeps {
		if dep.module.Name() == name {
			return dep.module.logicModule, dep.tag
		}
	}

	return nil, nil
}

func (m *baseModuleContext) GetDirectDepWithTag(name string, tag DependencyTag) Module {
	var deps []depInfo
	for _, dep := range m.module.directDeps {
		if dep.module.Name() == name {
			if dep.tag == tag {
				return dep.module.logicModule
			}
			deps = append(deps, dep)
		}
	}

	if len(deps) != 0 {
		panic(fmt.Errorf("Unable to find dependency %q with requested tag %#v. Found: %#v", deps[0].module, tag, deps))
	}

	return nil
}

func (m *baseModuleContext) VisitDirectDeps(visit func(Module)) {
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDeps(%s, %s) for dependency %s",
				m.module, funcName(visit), m.visitingDep.module))
		}
	}()

	m.visitingParent = m.module

	for _, dep := range m.module.directDeps {
		m.visitingDep = dep
		visit(dep.module.logicModule)
	}

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) VisitDirectDepsProxy(visit func(proxy ModuleProxy)) {
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDeps(%s, %s) for dependency %s",
				m.module, funcName(visit), m.visitingDep.module))
		}
	}()

	m.visitingParent = m.module

	for _, dep := range m.module.directDeps {
		m.visitingDep = dep
		visit(ModuleProxy{dep.module.logicModule})
	}

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) VisitDirectDepsIf(pred func(Module) bool, visit func(Module)) {
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsIf(%s, %s, %s) for dependency %s",
				m.module, funcName(pred), funcName(visit), m.visitingDep.module))
		}
	}()

	m.visitingParent = m.module

	for _, dep := range m.module.directDeps {
		m.visitingDep = dep
		if pred(dep.module.logicModule) {
			visit(dep.module.logicModule)
		}
	}

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) VisitDepsDepthFirst(visit func(Module)) {
	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirst(%s, %s) for dependency %s",
				m.module, funcName(visit), m.visitingDep.module))
		}
	}()

	m.context.walkDeps(m.module, false, nil, func(dep depInfo, parent *moduleInfo) {
		m.visitingParent = parent
		m.visitingDep = dep
		visit(dep.module.logicModule)
	})

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) VisitDepsDepthFirstIf(pred func(Module) bool,
	visit func(Module)) {

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirstIf(%s, %s, %s) for dependency %s",
				m.module, funcName(pred), funcName(visit), m.visitingDep.module))
		}
	}()

	m.context.walkDeps(m.module, false, nil, func(dep depInfo, parent *moduleInfo) {
		if pred(dep.module.logicModule) {
			m.visitingParent = parent
			m.visitingDep = dep
			visit(dep.module.logicModule)
		}
	})

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) WalkDeps(visit func(child, parent Module) bool) {
	m.context.walkDeps(m.module, true, func(dep depInfo, parent *moduleInfo) bool {
		m.visitingParent = parent
		m.visitingDep = dep
		return visit(dep.module.logicModule, parent.logicModule)
	}, nil)

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) WalkDepsProxy(visit func(child, parent ModuleProxy) bool) {
	m.context.walkDeps(m.module, true, func(dep depInfo, parent *moduleInfo) bool {
		m.visitingParent = parent
		m.visitingDep = dep
		return visit(ModuleProxy{dep.module.logicModule}, ModuleProxy{parent.logicModule})
	}, nil)

	m.visitingParent = nil
	m.visitingDep = depInfo{}
}

func (m *baseModuleContext) PrimaryModule() Module {
	return m.module.group.modules.firstModule().logicModule
}

func (m *baseModuleContext) FinalModule() Module {
	return m.module.group.modules.lastModule().logicModule
}

func (m *baseModuleContext) IsFinalModule(module Module) bool {
	return m.module.group.modules.lastModule().logicModule == module
}

func (m *baseModuleContext) VisitAllModuleVariants(visit func(Module)) {
	m.context.visitAllModuleVariants(m.module, visit)
}

func (m *baseModuleContext) VisitAllModuleVariantProxies(visit func(proxy ModuleProxy)) {
	m.context.visitAllModuleVariants(m.module, visitProxyAdaptor(visit))
}

func (m *baseModuleContext) AddNinjaFileDeps(deps ...string) {
	m.ninjaFileDeps = append(m.ninjaFileDeps, deps...)
}

func (m *baseModuleContext) ModuleFactories() map[string]ModuleFactory {
	return m.context.ModuleTypeFactories()
}

func (m *baseModuleContext) base() *baseModuleContext {
	return m
}

func (m *baseModuleContext) OtherModuleIsAutoGenerated(logicModule Module) bool {
	module := m.context.moduleInfo[getWrappedModule(logicModule)]
	if module == nil {
		panic(fmt.Errorf("Module %s not found in baseModuleContext", logicModule.Name()))
	}
	return module.createdBy != nil
}

func (m *moduleContext) ModuleSubDir() string {
	return m.module.variant.name
}

func (m *moduleContext) ModuleCacheKey() string {
	return m.module.ModuleCacheKey()
}

func (m *moduleContext) Variable(pctx PackageContext, name, value string) {
	m.scope.ReparentTo(pctx)

	v, err := m.scope.AddLocalVariable(name, value)
	if err != nil {
		panic(err)
	}

	m.actionDefs.variables = append(m.actionDefs.variables, v)
}

func (m *moduleContext) Rule(pctx PackageContext, name string,
	params RuleParams, argNames ...string) Rule {

	m.scope.ReparentTo(pctx)

	r, err := m.scope.AddLocalRule(name, &params, argNames...)
	if err != nil {
		panic(err)
	}

	m.actionDefs.rules = append(m.actionDefs.rules, r)

	return r
}

func (m *moduleContext) Build(pctx PackageContext, params BuildParams) {
	m.scope.ReparentTo(pctx)

	def, err := parseBuildParams(m.scope, &params, m.ModuleTags())
	if err != nil {
		panic(err)
	}

	m.actionDefs.buildDefs = append(m.actionDefs.buildDefs, def)
}

func (m *moduleContext) GetMissingDependencies() []string {
	m.handledMissingDeps = true
	return m.module.missingDeps
}

func (m *baseModuleContext) EarlyGetMissingDependencies() []string {
	return m.module.missingDeps
}

//
// MutatorContext
//

type mutatorContext struct {
	baseModuleContext
	mutator          *mutatorInfo
	reverseDeps      []reverseDep
	rename           []rename
	replace          []replace
	newVariations    moduleList    // new variants of existing modules
	newModules       []*moduleInfo // brand new modules
	defaultVariation *string
	pauseCh          chan<- pauseSpec
}

type TopDownMutatorContext interface {
	BaseModuleContext
}

type BottomUpMutatorContext interface {
	BaseModuleContext

	// AddDependency adds a dependency to the given module.  It returns a slice of modules for each
	// dependency (some entries may be nil).  Does not affect the ordering of the current mutator
	// pass, but will be ordered correctly for all future mutator passes.
	//
	// This method will pause until the new dependencies have had the current mutator called on them.
	AddDependency(module Module, tag DependencyTag, name ...string) []Module

	// AddReverseDependency adds a dependency from the destination to the given module.
	// Does not affect the ordering of the current mutator pass, but will be ordered
	// correctly for all future mutator passes.  All reverse dependencies for a destination module are
	// collected until the end of the mutator pass, sorted by name, and then appended to the destination
	// module's dependency list.  May only  be called by mutators that were marked with
	// UsesReverseDependencies during registration.
	AddReverseDependency(module Module, tag DependencyTag, name string)

	// AddVariationDependencies adds deps as dependencies of the current module, but uses the variations
	// argument to select which variant of the dependency to use.  It returns a slice of modules for
	// each dependency (some entries may be nil).  A variant of the dependency must exist that matches
	// the all of the non-local variations of the current module, plus the variations argument.
	//
	//
	// This method will pause until the new dependencies have had the current mutator called on them.
	AddVariationDependencies([]Variation, DependencyTag, ...string) []Module

	// AddReverseVariationDependency adds a dependency from the named module to the current
	// module. The given variations will be added to the current module's varations, and then the
	// result will be used to find the correct variation of the depending module, which must exist.
	//
	// Does not affect the ordering of the current mutator pass, but will be ordered
	// correctly for all future mutator passes.  All reverse dependencies for a destination module are
	// collected until the end of the mutator pass, sorted by name, and then appended to the destination
	// module's dependency list.  May only  be called by mutators that were marked with
	// UsesReverseDependencies during registration.
	AddReverseVariationDependency([]Variation, DependencyTag, string)

	// AddFarVariationDependencies adds deps as dependencies of the current module, but uses the
	// variations argument to select which variant of the dependency to use.  It returns a slice of
	// modules for each dependency (some entries may be nil).  A variant of the dependency must
	// exist that matches the variations argument, but may also have other variations.
	// For any unspecified variation the first variant will be used.
	//
	// Unlike AddVariationDependencies, the variations of the current module are ignored - the
	// dependency only needs to match the supplied variations.
	//
	//
	// This method will pause until the new dependencies have had the current mutator called on them.
	AddFarVariationDependencies([]Variation, DependencyTag, ...string) []Module

	// ReplaceDependencies finds all the variants of the module with the specified name, then
	// replaces all dependencies onto those variants with the current variant of this module.
	// Replacements don't take effect until after the mutator pass is finished.  May only
	// be called by mutators that were marked with UsesReplaceDependencies during registration.
	ReplaceDependencies(string)

	// ReplaceDependenciesIf finds all the variants of the module with the specified name, then
	// replaces all dependencies onto those variants with the current variant of this module
	// as long as the supplied predicate returns true.
	// Replacements don't take effect until after the mutator pass is finished.  May only
	// be called by mutators that were marked with UsesReplaceDependencies during registration.
	ReplaceDependenciesIf(string, ReplaceDependencyPredicate)

	// Rename all variants of a module.  The new name is not visible to calls to ModuleName,
	// AddDependency or OtherModuleName until after this mutator pass is complete.  May only be called
	// by mutators that were marked with UsesRename during registration.
	Rename(name string)

	// CreateModule creates a new module by calling the factory method for the specified moduleType, and applies
	// the specified property structs to it as if the properties were set in a blueprint file.  May only
	// be called by mutators that were marked with UsesCreateModule during registration.
	CreateModule(ModuleFactory, string, ...interface{}) Module
}

// A Mutator function is called for each Module, and can modify properties on the modules.
// It is called after parsing all Blueprint files, but before generating any build rules,
// and is always called on dependencies before being called on the depending module.
//
// The Mutator function should only modify members of properties structs, and not
// members of the module struct itself, to ensure the modified values are copied
// if a second Mutator chooses to split the module a second time.
type TopDownMutator func(mctx TopDownMutatorContext)
type BottomUpMutator func(mctx BottomUpMutatorContext)

// DependencyTag is an interface to an arbitrary object that embeds BaseDependencyTag.  It can be
// used to transfer information on a dependency between the mutator that called AddDependency
// and the GenerateBuildActions method.
type DependencyTag interface {
	dependencyTag(DependencyTag)
}

type BaseDependencyTag struct {
}

func (BaseDependencyTag) dependencyTag(DependencyTag) {
}

var _ DependencyTag = BaseDependencyTag{}

func (mctx *mutatorContext) createVariationsWithTransition(variationNames []string, outgoingTransitions [][]string) []Module {
	return mctx.createVariations(variationNames, chooseDepByIndexes(mctx.mutator.name, outgoingTransitions))
}

func (mctx *mutatorContext) createVariations(variationNames []string, depChooser depChooser) []Module {
	var ret []Module
	modules, errs := mctx.context.createVariations(mctx.module, mctx.mutator, depChooser, variationNames)
	if len(errs) > 0 {
		mctx.errs = append(mctx.errs, errs...)
	}

	for _, module := range modules {
		ret = append(ret, module.logicModule)
	}

	if mctx.newVariations != nil {
		panic("module already has variations from this mutator")
	}
	mctx.newVariations = modules

	if len(ret) != len(variationNames) {
		panic("oops!")
	}

	return ret
}

func (mctx *mutatorContext) Module() Module {
	return mctx.module.logicModule
}

func (mctx *mutatorContext) AddDependency(module Module, tag DependencyTag, deps ...string) []Module {
	depInfos := make([]Module, 0, len(deps))
	for _, dep := range deps {
		modInfo := mctx.context.moduleInfo[module]
		depInfo, errs := mctx.context.addVariationDependency(modInfo, mctx.mutator, mctx.config, nil, tag, dep, false)
		if len(errs) > 0 {
			mctx.errs = append(mctx.errs, errs...)
		}
		if !mctx.pause(depInfo) {
			// Pausing not supported by this mutator, new dependencies can't be returned.
			depInfo = nil
		}
		depInfos = append(depInfos, maybeLogicModule(depInfo))
	}
	return depInfos
}

func (mctx *mutatorContext) AddReverseDependency(module Module, tag DependencyTag, destName string) {
	if !mctx.mutator.usesReverseDependencies {
		panic(fmt.Errorf("method AddReverseDependency called from mutator that was not marked UsesReverseDependencies"))
	}

	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	destModule, errs := mctx.context.findReverseDependency(mctx.context.moduleInfo[module], mctx.config, nil, destName)
	if len(errs) > 0 {
		mctx.errs = append(mctx.errs, errs...)
		return
	}

	if destModule == nil {
		// allowMissingDependencies is true and the module wasn't found
		return
	}

	mctx.reverseDeps = append(mctx.reverseDeps, reverseDep{
		destModule,
		depInfo{mctx.context.moduleInfo[module], tag},
	})
}

func (mctx *mutatorContext) AddReverseVariationDependency(variations []Variation, tag DependencyTag, destName string) {
	if !mctx.mutator.usesReverseDependencies {
		panic(fmt.Errorf("method AddReverseVariationDependency called from mutator that was not marked UsesReverseDependencies"))
	}

	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	destModule, errs := mctx.context.findReverseDependency(mctx.module, mctx.config, variations, destName)
	if len(errs) > 0 {
		mctx.errs = append(mctx.errs, errs...)
		return
	}

	if destModule == nil {
		// allowMissingDependencies is true and the module wasn't found
		return
	}

	mctx.reverseDeps = append(mctx.reverseDeps, reverseDep{
		destModule,
		depInfo{mctx.module, tag},
	})
}

func (mctx *mutatorContext) AddVariationDependencies(variations []Variation, tag DependencyTag,
	deps ...string) []Module {

	depInfos := make([]Module, 0, len(deps))
	for _, dep := range deps {
		depInfo, errs := mctx.context.addVariationDependency(mctx.module, mctx.mutator, mctx.config, variations, tag, dep, false)
		if len(errs) > 0 {
			mctx.errs = append(mctx.errs, errs...)
		}
		if !mctx.pause(depInfo) {
			// Pausing not supported by this mutator, new dependencies can't be returned.
			depInfo = nil
		}
		depInfos = append(depInfos, maybeLogicModule(depInfo))
	}
	return depInfos
}

func (mctx *mutatorContext) AddFarVariationDependencies(variations []Variation, tag DependencyTag,
	deps ...string) []Module {

	depInfos := make([]Module, 0, len(deps))
	for _, dep := range deps {
		depInfo, errs := mctx.context.addVariationDependency(mctx.module, mctx.mutator, mctx.config, variations, tag, dep, true)
		if len(errs) > 0 {
			mctx.errs = append(mctx.errs, errs...)
		}
		if !mctx.pause(depInfo) {
			// Pausing not supported by this mutator, new dependencies can't be returned.
			depInfo = nil
		}
		depInfos = append(depInfos, maybeLogicModule(depInfo))
	}
	return depInfos
}

func (mctx *mutatorContext) ReplaceDependencies(name string) {
	mctx.ReplaceDependenciesIf(name, nil)
}

type ReplaceDependencyPredicate func(from Module, tag DependencyTag, to Module) bool

func (mctx *mutatorContext) ReplaceDependenciesIf(name string, predicate ReplaceDependencyPredicate) {
	if !mctx.mutator.usesReplaceDependencies {
		panic(fmt.Errorf("method ReplaceDependenciesIf called from mutator that was not marked UsesReplaceDependencies"))
	}

	targets := mctx.context.moduleVariantsThatDependOn(name, mctx.module)

	if len(targets) == 0 {
		panic(fmt.Errorf("ReplaceDependencies could not find identical variant {%s} for module %s\n"+
			"available variants:\n  %s",
			mctx.context.prettyPrintVariant(mctx.module.variant.variations),
			name,
			mctx.context.prettyPrintGroupVariants(mctx.context.moduleGroupFromName(name, mctx.module.namespace()))))
	}

	for _, target := range targets {
		mctx.replace = append(mctx.replace, replace{target, mctx.module, predicate})
	}
}

func (mctx *mutatorContext) Rename(name string) {
	if !mctx.mutator.usesRename {
		panic(fmt.Errorf("method Rename called from mutator that was not marked UsesRename"))
	}
	mctx.rename = append(mctx.rename, rename{mctx.module.group, name})
}

func (mctx *mutatorContext) CreateModule(factory ModuleFactory, typeName string, props ...interface{}) Module {
	if !mctx.mutator.usesCreateModule {
		panic(fmt.Errorf("method CreateModule called from mutator that was not marked UsesCreateModule"))
	}

	module := newModule(factory)

	module.relBlueprintsFile = mctx.module.relBlueprintsFile
	module.pos = mctx.module.pos
	module.propertyPos = mctx.module.propertyPos
	module.createdBy = mctx.module
	module.typeName = typeName

	for _, p := range props {
		err := proptools.AppendMatchingProperties(module.properties, p, nil)
		if err != nil {
			panic(err)
		}
	}

	mctx.newModules = append(mctx.newModules, module)

	return module.logicModule
}

// pause waits until the given dependency has been visited by the mutator's parallelVisit call.
// It returns true if the pause was supported, false if the pause was not supported and did not
// occur, which will happen when the mutator is not parallelizable.  If the dependency is nil
// it returns true if pausing is supported or false if it is not.
func (mctx *mutatorContext) pause(dep *moduleInfo) bool {
	if mctx.pauseCh != nil {
		if dep != nil {
			unpause := make(unpause)
			mctx.pauseCh <- pauseSpec{
				paused:  mctx.module,
				until:   dep,
				unpause: unpause,
			}
			<-unpause
		}
		return true
	}
	return false
}

// SimpleName is an embeddable object to implement the ModuleContext.Name method using a property
// called "name".  Modules that embed it must also add SimpleName.Properties to their property
// structure list.
type SimpleName struct {
	Properties struct {
		Name string
	}
}

func (s *SimpleName) Name() string {
	return s.Properties.Name
}

// Load Hooks

type LoadHookContext interface {
	EarlyModuleContext

	// CreateModule creates a new module by calling the factory method for the specified moduleType, and applies
	// the specified property structs to it as if the properties were set in a blueprint file.
	CreateModule(ModuleFactory, string, ...interface{}) Module

	// CreateModuleInDirectory creates a new module in the specified directory by calling the
	// factory method for the specified moduleType, and applies the specified property structs
	// to it as if the properties were set in a blueprint file.
	CreateModuleInDirectory(ModuleFactory, string, string, ...interface{}) Module

	// RegisterScopedModuleType creates a new module type that is scoped to the current Blueprints
	// file.
	RegisterScopedModuleType(name string, factory ModuleFactory)
}

func (l *loadHookContext) createModule(factory ModuleFactory, typeName, moduleDir string, props ...interface{}) Module {
	module := newModule(factory)

	module.relBlueprintsFile = moduleDir
	module.pos = l.module.pos
	module.propertyPos = l.module.propertyPos
	module.createdBy = l.module
	module.typeName = typeName

	for _, p := range props {
		err := proptools.AppendMatchingProperties(module.properties, p, nil)
		if err != nil {
			panic(err)
		}
	}

	l.newModules = append(l.newModules, module)

	return module.logicModule
}

func (l *loadHookContext) CreateModule(factory ModuleFactory, typeName string, props ...interface{}) Module {
	return l.createModule(factory, typeName, l.module.relBlueprintsFile, props...)
}

func (l *loadHookContext) CreateModuleInDirectory(factory ModuleFactory, typeName, moduleDir string, props ...interface{}) Module {
	if moduleDir != filepath.Clean(moduleDir) {
		panic(fmt.Errorf("Cannot create a module in %s", moduleDir))
	}

	filePath := filepath.Join(moduleDir, "Android.bp")
	return l.createModule(factory, typeName, filePath, props...)
}

func (l *loadHookContext) RegisterScopedModuleType(name string, factory ModuleFactory) {
	if _, exists := l.context.moduleFactories[name]; exists {
		panic(fmt.Errorf("A global module type named %q already exists", name))
	}

	if _, exists := (*l.scopedModuleFactories)[name]; exists {
		panic(fmt.Errorf("A module type named %q already exists in this scope", name))
	}

	if *l.scopedModuleFactories == nil {
		*l.scopedModuleFactories = make(map[string]ModuleFactory)
	}

	(*l.scopedModuleFactories)[name] = factory
}

type loadHookContext struct {
	baseModuleContext
	newModules            []*moduleInfo
	scopedModuleFactories *map[string]ModuleFactory
}

type LoadHook func(ctx LoadHookContext)

// LoadHookWithPriority is a wrapper around LoadHook and allows hooks to be sorted by priority.
// hooks with higher value of `priority` run last.
// hooks with equal value of `priority` run in the order they were registered.
type LoadHookWithPriority struct {
	priority int
	loadHook LoadHook
}

// Load hooks need to be added by module factories, which don't have any parameter to get to the
// Context, and only produce a Module interface with no base implementation, so the load hooks
// must be stored in a global map.  The key is a pointer allocated by the module factory, so there
// is no chance of collisions even if tests are running in parallel with multiple contexts.  The
// contents should be short-lived, they are added during a module factory and removed immediately
// after the module factory returns.
var pendingHooks sync.Map

func AddLoadHook(module Module, hook LoadHook) {
	// default priority is 0
	AddLoadHookWithPriority(module, hook, 0)
}

// AddLoadhHookWithPriority adds a load hook with a specified priority.
// Hooks with higher priority run last.
// Hooks with equal priority run in the order they were registered.
func AddLoadHookWithPriority(module Module, hook LoadHook, priority int) {
	// Only one goroutine can be processing a given module, so no additional locking is required
	// for the slice stored in the sync.Map.
	v, exists := pendingHooks.Load(module)
	if !exists {
		v, _ = pendingHooks.LoadOrStore(module, new([]LoadHookWithPriority))
	}
	hooks := v.(*[]LoadHookWithPriority)
	*hooks = append(*hooks, LoadHookWithPriority{priority, hook})
}

func runAndRemoveLoadHooks(ctx *Context, config interface{}, module *moduleInfo,
	scopedModuleFactories *map[string]ModuleFactory) (newModules []*moduleInfo, deps []string, errs []error) {

	if v, exists := pendingHooks.Load(module.logicModule); exists {
		hooks := v.(*[]LoadHookWithPriority)
		// Sort the hooks by priority.
		// Use SliceStable so that hooks with equal priority run in the order they were registered.
		sort.SliceStable(*hooks, func(i, j int) bool { return (*hooks)[i].priority < (*hooks)[j].priority })

		for _, hook := range *hooks {
			mctx := &loadHookContext{
				baseModuleContext: baseModuleContext{
					context: ctx,
					config:  config,
					module:  module,
				},
				scopedModuleFactories: scopedModuleFactories,
			}
			hook.loadHook(mctx)
			newModules = append(newModules, mctx.newModules...)
			deps = append(deps, mctx.ninjaFileDeps...)
			errs = append(errs, mctx.errs...)
		}
		pendingHooks.Delete(module.logicModule)

		return newModules, deps, errs
	}

	return nil, nil, nil
}

// Check the syntax of a generated blueprint file.
//
// This is intended to perform a quick syntactic check for generated blueprint
// code, where syntactically correct means:
// * No variable definitions.
// * Valid module types.
// * Valid property names.
// * Valid values for the property type.
//
// It does not perform any semantic checking of properties, existence of referenced
// files, or dependencies.
//
// At a low level it:
// * Parses the contents.
// * Invokes relevant factory to create Module instances.
// * Unpacks the properties into the Module.
// * Does not invoke load hooks or any mutators.
//
// The filename is only used for reporting errors.
func CheckBlueprintSyntax(moduleFactories map[string]ModuleFactory, filename string, contents string) []error {
	file, errs := parser.Parse(filename, strings.NewReader(contents))
	if len(errs) != 0 {
		return errs
	}

	for _, def := range file.Defs {
		switch def := def.(type) {
		case *parser.Module:
			_, moduleErrs := processModuleDef(def, filename, moduleFactories, nil, false)
			errs = append(errs, moduleErrs...)

		default:
			panic(fmt.Errorf("unknown definition type: %T", def))
		}
	}

	return errs
}

func maybeLogicModule(module *moduleInfo) Module {
	if module != nil {
		return module.logicModule
	} else {
		return nil
	}
}
