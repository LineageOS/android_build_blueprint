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
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/ioutil"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/scanner"
	"text/template"
	"unsafe"

	"github.com/google/blueprint/metrics"
	"github.com/google/blueprint/parser"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/pool"
	"github.com/google/blueprint/proptools"
)

var ErrBuildActionsNotReady = errors.New("build actions are not ready")

const maxErrors = 10
const MockModuleListFile = "bplist"

const OutFilePermissions = 0666

const BuildActionsCacheFile = "build_actions.gob"
const OrderOnlyStringsCacheFile = "order_only_strings.gob"

// A Context contains all the state needed to parse a set of Blueprints files
// and generate a Ninja file.  The process of generating a Ninja file proceeds
// through a series of four phases.  Each phase corresponds with a some methods
// on the Context object
//
//	      Phase                            Methods
//	   ------------      -------------------------------------------
//	1. Registration         RegisterModuleType, RegisterSingletonType
//
//	2. Parse                    ParseBlueprintsFiles, Parse
//
//	3. Generate            ResolveDependencies, PrepareBuildActions
//
//	4. Write                           WriteBuildFile
//
// The registration phase prepares the context to process Blueprints files
// containing various types of modules.  The parse phase reads in one or more
// Blueprints files and validates their contents against the module types that
// have been registered.  The generate phase then analyzes the parsed Blueprints
// contents to create an internal representation for the build actions that must
// be performed.  This phase also performs validation of the module dependencies
// and property values defined in the parsed Blueprints files.  Finally, the
// write phase generates the Ninja manifest text based on the generated build
// actions.
type Context struct {
	context.Context

	// Used for metrics-related event logging.
	EventHandler *metrics.EventHandler

	BeforePrepareBuildActionsHook func() error

	moduleFactories     map[string]ModuleFactory
	nameInterface       NameInterface
	moduleGroups        []*moduleGroup
	moduleInfo          map[Module]*moduleInfo
	modulesSorted       []*moduleInfo
	singletonInfo       []*singletonInfo
	mutatorInfo         []*mutatorInfo
	variantMutatorNames []string

	variantCreatingMutatorOrder []string

	transitionMutators []*transitionMutatorImpl

	depsModified uint32 // positive if a mutator modified the dependencies

	dependenciesReady bool // set to true on a successful ResolveDependencies
	buildActionsReady bool // set to true on a successful PrepareBuildActions

	// set by SetIgnoreUnknownModuleTypes
	ignoreUnknownModuleTypes bool

	// set by SetAllowMissingDependencies
	allowMissingDependencies bool

	verifyProvidersAreUnchanged bool

	// set during PrepareBuildActions
	nameTracker     *nameTracker
	liveGlobals     *liveTracker
	globalVariables map[Variable]*ninjaString
	globalPools     map[Pool]*poolDef
	globalRules     map[Rule]*ruleDef

	// set during PrepareBuildActions
	outDir             *ninjaString // The builddir special Ninja variable
	requiredNinjaMajor int          // For the ninja_required_version variable
	requiredNinjaMinor int          // For the ninja_required_version variable
	requiredNinjaMicro int          // For the ninja_required_version variable

	subninjas []string

	// set lazily by sortedModuleGroups
	cachedSortedModuleGroups []*moduleGroup
	// cache deps modified to determine whether cachedSortedModuleGroups needs to be recalculated
	cachedDepsModified bool

	globs    map[globKey]pathtools.GlobResult
	globLock sync.Mutex

	srcDir         string
	fs             pathtools.FileSystem
	moduleListFile string

	// Mutators indexed by the ID of the provider associated with them.  Not all mutators will
	// have providers, and not all providers will have a mutator, or if they do the mutator may
	// not be registered in this Context.
	providerMutators []*mutatorInfo

	// The currently running mutator
	startedMutator *mutatorInfo
	// True for any mutators that have already run over all modules
	finishedMutators map[*mutatorInfo]bool

	// If true, RunBlueprint will skip cloning modules at the end of RunBlueprint.
	// Cloning modules intentionally invalidates some Module values after
	// mutators run (to ensure that mutators don't set such Module values in a way
	// which ruins the integrity of the graph). However, keeping Module values
	// changed by mutators may be a desirable outcome (such as for tooling or tests).
	SkipCloneModulesAfterMutators bool

	// String values that can be used to gate build graph traversal
	includeTags *IncludeTags

	sourceRootDirs *SourceRootDirs

	// True if an incremental analysis can be attempted, i.e., there is no Soong
	// code changes, no environmental variable changes and no product config
	// variable changes.
	incrementalAnalysis bool

	// True if the flag --incremental-build-actions is set, in which case Soong
	// will try to do a incremental build. Mainly two tasks will involve here:
	// caching the providers of all the participating modules, and restoring the
	// providers and skip the build action generations if there is a cache hit.
	// Enabling this flag will only guarantee the former task to be performed, the
	// latter will depend on the flag above.
	incrementalEnabled bool

	buildActionsToCache       BuildActionCache
	buildActionsToCacheLock   sync.Mutex
	buildActionsFromCache     BuildActionCache
	orderOnlyStringsFromCache OrderOnlyStringsCache
	orderOnlyStringsToCache   OrderOnlyStringsCache
}

// A container for String keys. The keys can be used to gate build graph traversal
type SourceRootDirs struct {
	dirs []string
}

func (dirs *SourceRootDirs) Add(names ...string) {
	dirs.dirs = append(dirs.dirs, names...)
}

func (dirs *SourceRootDirs) SourceRootDirAllowed(path string) (bool, string) {
	sort.Slice(dirs.dirs, func(i, j int) bool {
		return len(dirs.dirs[i]) < len(dirs.dirs[j])
	})
	last := len(dirs.dirs)
	for i := range dirs.dirs {
		// iterate from longest paths (most specific)
		prefix := dirs.dirs[last-i-1]
		disallowedPrefix := false
		if len(prefix) >= 1 && prefix[0] == '-' {
			prefix = prefix[1:]
			disallowedPrefix = true
		}
		if strings.HasPrefix(path, prefix) {
			if disallowedPrefix {
				return false, prefix
			} else {
				return true, prefix
			}
		}
	}
	return true, ""
}

func (c *Context) AddSourceRootDirs(dirs ...string) {
	c.sourceRootDirs.Add(dirs...)
}

// A container for String keys. The keys can be used to gate build graph traversal
type IncludeTags map[string]bool

func (tags *IncludeTags) Add(names ...string) {
	for _, name := range names {
		(*tags)[name] = true
	}
}

func (tags *IncludeTags) Contains(tag string) bool {
	_, exists := (*tags)[tag]
	return exists
}

func (c *Context) AddIncludeTags(names ...string) {
	c.includeTags.Add(names...)
}

func (c *Context) ContainsIncludeTag(name string) bool {
	return c.includeTags.Contains(name)
}

// An Error describes a problem that was encountered that is related to a
// particular location in a Blueprints file.
type BlueprintError struct {
	Err error            // the error that occurred
	Pos scanner.Position // the relevant Blueprints file location
}

// A ModuleError describes a problem that was encountered that is related to a
// particular module in a Blueprints file
type ModuleError struct {
	BlueprintError
	module *moduleInfo
}

// A PropertyError describes a problem that was encountered that is related to a
// particular property in a Blueprints file
type PropertyError struct {
	ModuleError
	property string
}

func (e *BlueprintError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Err)
}

func (e *ModuleError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Pos, e.module, e.Err)
}

func (e *PropertyError) Error() string {
	return fmt.Sprintf("%s: %s: %s: %s", e.Pos, e.module, e.property, e.Err)
}

type localBuildActions struct {
	variables []*localVariable
	rules     []*localRule
	buildDefs []*buildDef
}

type moduleAlias struct {
	variant variant
	target  *moduleInfo
}

func (m *moduleAlias) alias() *moduleAlias              { return m }
func (m *moduleAlias) module() *moduleInfo              { return nil }
func (m *moduleAlias) moduleOrAliasTarget() *moduleInfo { return m.target }
func (m *moduleAlias) moduleOrAliasVariant() variant    { return m.variant }

func (m *moduleInfo) alias() *moduleAlias              { return nil }
func (m *moduleInfo) module() *moduleInfo              { return m }
func (m *moduleInfo) moduleOrAliasTarget() *moduleInfo { return m }
func (m *moduleInfo) moduleOrAliasVariant() variant    { return m.variant }

type moduleOrAlias interface {
	alias() *moduleAlias
	module() *moduleInfo
	moduleOrAliasTarget() *moduleInfo
	moduleOrAliasVariant() variant
}

type modulesOrAliases []moduleOrAlias

func (l modulesOrAliases) firstModule() *moduleInfo {
	for _, moduleOrAlias := range l {
		if m := moduleOrAlias.module(); m != nil {
			return m
		}
	}
	panic(fmt.Errorf("no first module!"))
}

func (l modulesOrAliases) lastModule() *moduleInfo {
	for i := range l {
		if m := l[len(l)-1-i].module(); m != nil {
			return m
		}
	}
	panic(fmt.Errorf("no last module!"))
}

type moduleGroup struct {
	name      string
	ninjaName string

	modules modulesOrAliases

	namespace Namespace
}

func (group *moduleGroup) moduleOrAliasByVariantName(name string) moduleOrAlias {
	for _, module := range group.modules {
		if module.moduleOrAliasVariant().name == name {
			return module
		}
	}
	return nil
}

func (group *moduleGroup) moduleByVariantName(name string) *moduleInfo {
	return group.moduleOrAliasByVariantName(name).module()
}

type moduleInfo struct {
	// set during Parse
	typeName          string
	factory           ModuleFactory
	relBlueprintsFile string
	pos               scanner.Position
	propertyPos       map[string]scanner.Position
	createdBy         *moduleInfo

	variant variant

	logicModule Module
	group       *moduleGroup
	properties  []interface{}

	// set during ResolveDependencies
	missingDeps   []string
	newDirectDeps []depInfo

	// set during updateDependencies
	reverseDeps []*moduleInfo
	forwardDeps []*moduleInfo
	directDeps  []depInfo

	// used by parallelVisit
	waitingCount int

	// set during each runMutator
	splitModules           modulesOrAliases
	obsoletedByNewVariants bool

	// Used by TransitionMutator implementations
	transitionVariations     []string
	currentTransitionMutator string
	requiredVariationsLock   sync.Mutex

	// outgoingTransitionCache stores the final variation for each dependency, indexed by the source variation
	// index in transitionVariations and then by the index of the dependency in directDeps
	outgoingTransitionCache [][]string

	// set during PrepareBuildActions
	actionDefs localBuildActions

	providers                  []interface{}
	providerInitialValueHashes []uint64

	startedMutator  *mutatorInfo
	finishedMutator *mutatorInfo

	startedGenerateBuildActions  bool
	finishedGenerateBuildActions bool

	incrementalInfo
}

type incrementalInfo struct {
	incrementalRestored bool
	buildActionCacheKey *BuildActionCacheKey
	orderOnlyStrings    *[]string
}

type variant struct {
	name                 string
	variations           variationMap
	dependencyVariations variationMap
}

type depInfo struct {
	module *moduleInfo
	tag    DependencyTag
}

func (module *moduleInfo) Name() string {
	// If this is called from a LoadHook (which is run before the module has been registered)
	// then group will not be set and so the name is retrieved from logicModule.Name().
	// Usually, using that method is not safe as it does not track renames (group.name does).
	// However, when called from LoadHook it is safe as there is no way to rename a module
	// until after the LoadHook has run and the module has been registered.
	if module.group != nil {
		return module.group.name
	} else {
		return module.logicModule.Name()
	}
}

func (module *moduleInfo) String() string {
	s := fmt.Sprintf("module %q", module.Name())
	if module.variant.name != "" {
		s += fmt.Sprintf(" variant %q", module.variant.name)
	}
	if module.createdBy != nil {
		s += fmt.Sprintf(" (created by %s)", module.createdBy)
	}

	return s
}

func (module *moduleInfo) namespace() Namespace {
	return module.group.namespace
}

func (module *moduleInfo) ModuleCacheKey() string {
	variant := module.variant.name
	if variant == "" {
		variant = "none"
	}
	return fmt.Sprintf("%s-%s-%s-%s",
		strings.ReplaceAll(filepath.Dir(module.relBlueprintsFile), "/", "."),
		module.Name(), variant, module.typeName)
}

// A Variation is a way that a variant of a module differs from other variants of the same module.
// For example, two variants of the same module might have Variation{"arch","arm"} and
// Variation{"arch","arm64"}
type Variation struct {
	// Mutator is the axis on which this variation applies, i.e. "arch" or "link"
	Mutator string
	// Variation is the name of the variation on the axis, i.e. "arm" or "arm64" for arch, or
	// "shared" or "static" for link.
	Variation string
}

// A variationMap stores a map of Mutator to Variation to specify a variant of a module.
type variationMap struct {
	variations map[string]string
}

func (vm variationMap) clone() variationMap {
	return variationMap{
		variations: maps.Clone(vm.variations),
	}
}

func (vm variationMap) cloneMatching(mutators []string) variationMap {
	newVariations := make(map[string]string)
	for _, mutator := range mutators {
		if variation, ok := vm.variations[mutator]; ok {
			newVariations[mutator] = variation
		}
	}
	return variationMap{
		variations: newVariations,
	}
}

// Compare this variationMap to another one.  Returns true if the every entry in this map
// exists and has the same value in the other map.
func (vm variationMap) subsetOf(other variationMap) bool {
	for k, v1 := range vm.variations {
		if v2, ok := other.variations[k]; !ok || v1 != v2 {
			return false
		}
	}
	return true
}

func (vm variationMap) equal(other variationMap) bool {
	return maps.Equal(vm.variations, other.variations)
}

func (vm *variationMap) set(mutator, variation string) {
	if variation == "" {
		if vm.variations != nil {
			delete(vm.variations, mutator)
		}
	} else {
		if vm.variations == nil {
			vm.variations = make(map[string]string)
		}
		vm.variations[mutator] = variation
	}
}

func (vm variationMap) get(mutator string) string {
	return vm.variations[mutator]
}

func (vm variationMap) delete(mutator string) {
	delete(vm.variations, mutator)
}

func (vm variationMap) empty() bool {
	return len(vm.variations) == 0
}

// differenceKeysCount returns the count of keys that exist in this variationMap that don't exist in the argument.  It
// ignores the values.
func (vm variationMap) differenceKeysCount(other variationMap) int {
	divergence := 0
	for mutator, _ := range vm.variations {
		if _, exists := other.variations[mutator]; !exists {
			divergence += 1
		}
	}
	return divergence
}

type singletonInfo struct {
	// set during RegisterSingletonType
	factory   SingletonFactory
	singleton Singleton
	name      string
	parallel  bool

	// set during PrepareBuildActions
	actionDefs localBuildActions
}

type mutatorInfo struct {
	// set during RegisterMutator
	topDownMutator    TopDownMutator
	bottomUpMutator   BottomUpMutator
	name              string
	parallel          bool
	transitionMutator *transitionMutatorImpl
}

func newContext() *Context {
	eventHandler := metrics.EventHandler{}
	return &Context{
		Context:                     context.Background(),
		EventHandler:                &eventHandler,
		moduleFactories:             make(map[string]ModuleFactory),
		nameInterface:               NewSimpleNameInterface(),
		moduleInfo:                  make(map[Module]*moduleInfo),
		globs:                       make(map[globKey]pathtools.GlobResult),
		fs:                          pathtools.OsFs,
		finishedMutators:            make(map[*mutatorInfo]bool),
		includeTags:                 &IncludeTags{},
		sourceRootDirs:              &SourceRootDirs{},
		outDir:                      nil,
		requiredNinjaMajor:          1,
		requiredNinjaMinor:          7,
		requiredNinjaMicro:          0,
		verifyProvidersAreUnchanged: true,
		buildActionsToCache:         make(BuildActionCache),
		orderOnlyStringsToCache:     make(OrderOnlyStringsCache),
	}
}

// NewContext creates a new Context object.  The created context initially has
// no module or singleton factories registered, so the RegisterModuleFactory and
// RegisterSingletonFactory methods must be called before it can do anything
// useful.
func NewContext() *Context {
	ctx := newContext()

	ctx.RegisterBottomUpMutator("blueprint_deps", blueprintDepsMutator)

	return ctx
}

// A ModuleFactory function creates a new Module object.  See the
// Context.RegisterModuleType method for details about how a registered
// ModuleFactory is used by a Context.
type ModuleFactory func() (m Module, propertyStructs []interface{})

// RegisterModuleType associates a module type name (which can appear in a
// Blueprints file) with a Module factory function.  When the given module type
// name is encountered in a Blueprints file during parsing, the Module factory
// is invoked to instantiate a new Module object to handle the build action
// generation for the module.  If a Mutator splits a module into multiple variants,
// the factory is invoked again to create a new Module for each variant.
//
// The module type names given here must be unique for the context.  The factory
// function should be a named function so that its package and name can be
// included in the generated Ninja file for debugging purposes.
//
// The factory function returns two values.  The first is the newly created
// Module object.  The second is a slice of pointers to that Module object's
// properties structs.  Each properties struct is examined when parsing a module
// definition of this type in a Blueprints file.  Exported fields of the
// properties structs are automatically set to the property values specified in
// the Blueprints file.  The properties struct field names determine the name of
// the Blueprints file properties that are used - the Blueprints property name
// matches that of the properties struct field name with the first letter
// converted to lower-case.
//
// The fields of the properties struct must be either []string, a string, or
// bool. The Context will panic if a Module gets instantiated with a properties
// struct containing a field that is not one these supported types.
//
// Any properties that appear in the Blueprints files that are not built-in
// module properties (such as "name" and "deps") and do not have a corresponding
// field in the returned module properties struct result in an error during the
// Context's parse phase.
//
// As an example, the follow code:
//
//	type myModule struct {
//	    properties struct {
//	        Foo string
//	        Bar []string
//	    }
//	}
//
//	func NewMyModule() (blueprint.Module, []interface{}) {
//	    module := new(myModule)
//	    properties := &module.properties
//	    return module, []interface{}{properties}
//	}
//
//	func main() {
//	    ctx := blueprint.NewContext()
//	    ctx.RegisterModuleType("my_module", NewMyModule)
//	    // ...
//	}
//
// would support parsing a module defined in a Blueprints file as follows:
//
//	my_module {
//	    name: "myName",
//	    foo:  "my foo string",
//	    bar:  ["my", "bar", "strings"],
//	}
//
// The factory function may be called from multiple goroutines.  Any accesses
// to global variables must be synchronized.
func (c *Context) RegisterModuleType(name string, factory ModuleFactory) {
	if _, present := c.moduleFactories[name]; present {
		panic(fmt.Errorf("module type %q is already registered", name))
	}
	c.moduleFactories[name] = factory
}

// A SingletonFactory function creates a new Singleton object.  See the
// Context.RegisterSingletonType method for details about how a registered
// SingletonFactory is used by a Context.
type SingletonFactory func() Singleton

// RegisterSingletonType registers a singleton type that will be invoked to
// generate build actions.  Each registered singleton type is instantiated
// and invoked exactly once as part of the generate phase.
//
// Those singletons registered with parallel=true are run in parallel, after
// which the other registered singletons are run in registration order.
//
// The singleton type names given here must be unique for the context.  The
// factory function should be a named function so that its package and name can
// be included in the generated Ninja file for debugging purposes.
func (c *Context) RegisterSingletonType(name string, factory SingletonFactory, parallel bool) {
	for _, s := range c.singletonInfo {
		if s.name == name {
			panic(fmt.Errorf("singleton %q is already registered", name))
		}
	}

	c.singletonInfo = append(c.singletonInfo, &singletonInfo{
		factory:   factory,
		singleton: factory(),
		name:      name,
		parallel:  parallel,
	})
}

func (c *Context) SetNameInterface(i NameInterface) {
	c.nameInterface = i
}

func (c *Context) SetIncrementalAnalysis(incremental bool) {
	c.incrementalAnalysis = incremental
}

func (c *Context) GetIncrementalAnalysis() bool {
	return c.incrementalAnalysis
}

func (c *Context) SetIncrementalEnabled(incremental bool) {
	c.incrementalEnabled = incremental
}

func (c *Context) GetIncrementalEnabled() bool {
	return c.incrementalEnabled
}

func (c *Context) updateBuildActionsCache(key *BuildActionCacheKey, data *BuildActionCachedData) {
	if key != nil {
		c.buildActionsToCacheLock.Lock()
		defer c.buildActionsToCacheLock.Unlock()
		c.buildActionsToCache[*key] = data
	}
}

func (c *Context) getBuildActionsFromCache(key *BuildActionCacheKey) *BuildActionCachedData {
	if c.buildActionsFromCache != nil && key != nil {
		return c.buildActionsFromCache[*key]
	}
	return nil
}

func (c *Context) CacheAllBuildActions(soongOutDir string) error {
	return errors.Join(writeToCache(c, soongOutDir, BuildActionsCacheFile, &c.buildActionsToCache),
		writeToCache(c, soongOutDir, OrderOnlyStringsCacheFile, &c.orderOnlyStringsToCache))
}

func writeToCache[T any](ctx *Context, soongOutDir string, fileName string, data *T) error {
	file, err := ctx.fs.OpenFile(filepath.Join(ctx.SrcDir(), soongOutDir, fileName),
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, OutFilePermissions)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	return encoder.Encode(data)
}

func (c *Context) RestoreAllBuildActions(soongOutDir string) error {
	c.buildActionsFromCache = make(BuildActionCache)
	c.orderOnlyStringsFromCache = make(OrderOnlyStringsCache)
	return errors.Join(restoreFromCache(c, soongOutDir, BuildActionsCacheFile, &c.buildActionsFromCache),
		restoreFromCache(c, soongOutDir, OrderOnlyStringsCacheFile, &c.orderOnlyStringsFromCache))
}

func restoreFromCache[T any](ctx *Context, soongOutDir string, fileName string, data *T) error {
	file, err := ctx.fs.Open(filepath.Join(ctx.SrcDir(), soongOutDir, fileName))
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return err
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	return decoder.Decode(data)
}

func (c *Context) SetSrcDir(path string) {
	c.srcDir = path
	c.fs = pathtools.NewOsFs(path)
}

func (c *Context) SrcDir() string {
	return c.srcDir
}

func singletonPkgPath(singleton Singleton) string {
	typ := reflect.TypeOf(singleton)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.PkgPath()
}

func singletonTypeName(singleton Singleton) string {
	typ := reflect.TypeOf(singleton)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ.PkgPath() + "." + typ.Name()
}

// RegisterTopDownMutator registers a mutator that will be invoked to propagate dependency info
// top-down between Modules.  Each registered mutator is invoked in registration order (mixing
// TopDownMutators and BottomUpMutators) once per Module, and the invocation on any module will
// have returned before it is in invoked on any of its dependencies.
//
// The mutator type names given here must be unique to all top down mutators in
// the Context.
//
// Returns a MutatorHandle, on which Parallel can be called to set the mutator to visit modules in
// parallel while maintaining ordering.
func (c *Context) RegisterTopDownMutator(name string, mutator TopDownMutator) MutatorHandle {
	for _, m := range c.mutatorInfo {
		if m.name == name && m.topDownMutator != nil {
			panic(fmt.Errorf("mutator %q is already registered", name))
		}
	}

	info := &mutatorInfo{
		topDownMutator: mutator,
		name:           name,
	}

	c.mutatorInfo = append(c.mutatorInfo, info)

	return info
}

// RegisterBottomUpMutator registers a mutator that will be invoked to split Modules into variants.
// Each registered mutator is invoked in registration order (mixing TopDownMutators and
// BottomUpMutators) once per Module, will not be invoked on a module until the invocations on all
// of the modules dependencies have returned.
//
// The mutator type names given here must be unique to all bottom up or early
// mutators in the Context.
//
// Returns a MutatorHandle, on which Parallel can be called to set the mutator to visit modules in
// parallel while maintaining ordering.
func (c *Context) RegisterBottomUpMutator(name string, mutator BottomUpMutator) MutatorHandle {
	for _, m := range c.variantMutatorNames {
		if m == name {
			panic(fmt.Errorf("mutator %q is already registered", name))
		}
	}

	info := &mutatorInfo{
		bottomUpMutator: mutator,
		name:            name,
	}
	c.mutatorInfo = append(c.mutatorInfo, info)

	c.variantMutatorNames = append(c.variantMutatorNames, name)

	return info
}

// HasMutatorFinished returns true if the given mutator has finished running.
// It will panic if given an invalid mutator name.
func (c *Context) HasMutatorFinished(mutatorName string) bool {
	for _, mutator := range c.mutatorInfo {
		if mutator.name == mutatorName {
			finished, ok := c.finishedMutators[mutator]
			return ok && finished
		}
	}
	panic(fmt.Sprintf("unknown mutator %q", mutatorName))
}

type MutatorHandle interface {
	// Set the mutator to visit modules in parallel while maintaining ordering.  Calling any
	// method on the mutator context is thread-safe, but the mutator must handle synchronization
	// for any modifications to global state or any modules outside the one it was invoked on.
	Parallel() MutatorHandle

	setTransitionMutator(impl *transitionMutatorImpl) MutatorHandle
}

func (mutator *mutatorInfo) Parallel() MutatorHandle {
	mutator.parallel = true
	return mutator
}

func (mutator *mutatorInfo) setTransitionMutator(impl *transitionMutatorImpl) MutatorHandle {
	mutator.transitionMutator = impl
	return mutator
}

// SetIgnoreUnknownModuleTypes sets the behavior of the context in the case
// where it encounters an unknown module type while parsing Blueprints files. By
// default, the context will report unknown module types as an error.  If this
// method is called with ignoreUnknownModuleTypes set to true then the context
// will silently ignore unknown module types.
//
// This method should generally not be used.  It exists to facilitate the
// bootstrapping process.
func (c *Context) SetIgnoreUnknownModuleTypes(ignoreUnknownModuleTypes bool) {
	c.ignoreUnknownModuleTypes = ignoreUnknownModuleTypes
}

// SetAllowMissingDependencies changes the behavior of Blueprint to ignore
// unresolved dependencies.  If the module's GenerateBuildActions calls
// ModuleContext.GetMissingDependencies Blueprint will not emit any errors
// for missing dependencies.
func (c *Context) SetAllowMissingDependencies(allowMissingDependencies bool) {
	c.allowMissingDependencies = allowMissingDependencies
}

// SetVerifyProvidersAreUnchanged makes blueprint hash all providers immediately
// after SetProvider() is called, and then hash them again after the build finished.
// If the hashes change, it's an error. Providers are supposed to be immutable, but
// we don't have any more direct way to enforce that in go.
func (c *Context) SetVerifyProvidersAreUnchanged(verifyProvidersAreUnchanged bool) {
	c.verifyProvidersAreUnchanged = verifyProvidersAreUnchanged
}

func (c *Context) GetVerifyProvidersAreUnchanged() bool {
	return c.verifyProvidersAreUnchanged
}

func (c *Context) SetModuleListFile(listFile string) {
	c.moduleListFile = listFile
}

func (c *Context) ListModulePaths(baseDir string) (paths []string, err error) {
	reader, err := c.fs.Open(c.moduleListFile)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	bytes, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	text := string(bytes)

	text = strings.Trim(text, "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = filepath.Join(baseDir, lines[i])
	}

	return lines, nil
}

// a fileParseContext tells the status of parsing a particular file
type fileParseContext struct {
	// name of file
	fileName string

	// scope to use when resolving variables
	Scope *parser.Scope

	// pointer to the one in the parent directory
	parent *fileParseContext

	// is closed once FileHandler has completed for this file
	doneVisiting chan struct{}
}

// ParseBlueprintsFiles parses a set of Blueprints files starting with the file
// at rootFile.  When it encounters a Blueprints file with a set of subdirs
// listed it recursively parses any Blueprints files found in those
// subdirectories.
//
// If no errors are encountered while parsing the files, the list of paths on
// which the future output will depend is returned.  This list will include both
// Blueprints file paths as well as directory paths for cases where wildcard
// subdirs are found.
func (c *Context) ParseBlueprintsFiles(rootFile string,
	config interface{}) (deps []string, errs []error) {

	baseDir := filepath.Dir(rootFile)
	pathsToParse, err := c.ListModulePaths(baseDir)
	if err != nil {
		return nil, []error{err}
	}
	return c.ParseFileList(baseDir, pathsToParse, config)
}

type shouldVisitFileInfo struct {
	shouldVisitFile bool
	skippedModules  []string
	reasonForSkip   string
	errs            []error
}

// Returns a boolean for whether this file should be analyzed
// Evaluates to true if the file either
// 1. does not contain a blueprint_package_includes
// 2. contains a blueprint_package_includes and all requested tags are set
// This should be processed before adding any modules to the build graph
func shouldVisitFile(c *Context, file *parser.File) shouldVisitFileInfo {
	skippedModules := []string{}
	for _, def := range file.Defs {
		switch def := def.(type) {
		case *parser.Module:
			skippedModules = append(skippedModules, def.Name())
		}
	}

	shouldVisit, invalidatingPrefix := c.sourceRootDirs.SourceRootDirAllowed(file.Name)
	if !shouldVisit {
		return shouldVisitFileInfo{
			shouldVisitFile: shouldVisit,
			skippedModules:  skippedModules,
			reasonForSkip: fmt.Sprintf(
				"%q is a descendant of %q, and that path prefix was not included in PRODUCT_SOURCE_ROOT_DIRS",
				file.Name,
				invalidatingPrefix,
			),
		}
	}
	return shouldVisitFileInfo{shouldVisitFile: true}
}

func (c *Context) ParseFileList(rootDir string, filePaths []string,
	config interface{}) (deps []string, errs []error) {

	if len(filePaths) < 1 {
		return nil, []error{fmt.Errorf("no paths provided to parse")}
	}

	c.dependenciesReady = false

	type newModuleInfo struct {
		*moduleInfo
		deps  []string
		added chan<- struct{}
	}

	type newSkipInfo struct {
		shouldVisitFileInfo
		file string
	}

	moduleCh := make(chan newModuleInfo)
	errsCh := make(chan []error)
	doneCh := make(chan struct{})
	skipCh := make(chan newSkipInfo)
	var numErrs uint32
	var numGoroutines int32

	// handler must be reentrant
	handleOneFile := func(file *parser.File) {
		if atomic.LoadUint32(&numErrs) > maxErrors {
			return
		}

		addedCh := make(chan struct{})

		var scopedModuleFactories map[string]ModuleFactory

		var addModule func(module *moduleInfo) []error
		addModule = func(module *moduleInfo) []error {
			// Run any load hooks immediately before it is sent to the moduleCh and is
			// registered by name. This allows load hooks to set and/or modify any aspect
			// of the module (including names) using information that is not available when
			// the module factory is called.
			newModules, newDeps, errs := runAndRemoveLoadHooks(c, config, module, &scopedModuleFactories)
			if len(errs) > 0 {
				return errs
			}

			moduleCh <- newModuleInfo{module, newDeps, addedCh}
			<-addedCh
			for _, n := range newModules {
				errs = addModule(n)
				if len(errs) > 0 {
					return errs
				}
			}
			return nil
		}
		shouldVisitInfo := shouldVisitFile(c, file)
		errs := shouldVisitInfo.errs
		if len(errs) > 0 {
			atomic.AddUint32(&numErrs, uint32(len(errs)))
			errsCh <- errs
		}
		if !shouldVisitInfo.shouldVisitFile {
			skipCh <- newSkipInfo{
				file:                file.Name,
				shouldVisitFileInfo: shouldVisitInfo,
			}
			// TODO: Write a file that lists the skipped bp files
			return
		}

		for _, def := range file.Defs {
			switch def := def.(type) {
			case *parser.Module:
				module, errs := processModuleDef(def, file.Name, c.moduleFactories, scopedModuleFactories, c.ignoreUnknownModuleTypes)
				if len(errs) == 0 && module != nil {
					errs = addModule(module)
				}

				if len(errs) > 0 {
					atomic.AddUint32(&numErrs, uint32(len(errs)))
					errsCh <- errs
				}

			case *parser.Assignment:
				// Already handled via Scope object
			default:
				panic("unknown definition type")
			}

		}
	}

	atomic.AddInt32(&numGoroutines, 1)
	go func() {
		var errs []error
		deps, errs = c.WalkBlueprintsFiles(rootDir, filePaths, handleOneFile)
		if len(errs) > 0 {
			errsCh <- errs
		}
		doneCh <- struct{}{}
	}()

	var hookDeps []string
loop:
	for {
		select {
		case newErrs := <-errsCh:
			errs = append(errs, newErrs...)
		case module := <-moduleCh:
			newErrs := c.addModule(module.moduleInfo)
			hookDeps = append(hookDeps, module.deps...)
			if module.added != nil {
				module.added <- struct{}{}
			}
			if len(newErrs) > 0 {
				errs = append(errs, newErrs...)
			}
		case <-doneCh:
			n := atomic.AddInt32(&numGoroutines, -1)
			if n == 0 {
				break loop
			}
		case skipped := <-skipCh:
			nctx := newNamespaceContextFromFilename(skipped.file)
			for _, name := range skipped.skippedModules {
				c.nameInterface.NewSkippedModule(nctx, name, SkippedModuleInfo{
					filename: skipped.file,
					reason:   skipped.reasonForSkip,
				})
			}
		}
	}

	deps = append(deps, hookDeps...)
	return deps, errs
}

type FileHandler func(*parser.File)

// WalkBlueprintsFiles walks a set of Blueprints files starting with the given filepaths,
// calling the given file handler on each
//
// When WalkBlueprintsFiles encounters a Blueprints file with a set of subdirs listed,
// it recursively parses any Blueprints files found in those subdirectories.
//
// If any of the file paths is an ancestor directory of any other of file path, the ancestor
// will be parsed and visited first.
//
// the file handler will be called from a goroutine, so it must be reentrant.
//
// If no errors are encountered while parsing the files, the list of paths on
// which the future output will depend is returned.  This list will include both
// Blueprints file paths as well as directory paths for cases where wildcard
// subdirs are found.
//
// visitor will be called asynchronously, and will only be called once visitor for each
// ancestor directory has completed.
//
// WalkBlueprintsFiles will not return until all calls to visitor have returned.
func (c *Context) WalkBlueprintsFiles(rootDir string, filePaths []string,
	visitor FileHandler) (deps []string, errs []error) {

	// make a mapping from ancestors to their descendants to facilitate parsing ancestors first
	descendantsMap, err := findBlueprintDescendants(filePaths)
	if err != nil {
		panic(err.Error())
	}
	blueprintsSet := make(map[string]bool)

	// Channels to receive data back from openAndParse goroutines
	blueprintsCh := make(chan fileParseContext)
	errsCh := make(chan []error)
	depsCh := make(chan string)

	// Channel to notify main loop that a openAndParse goroutine has finished
	doneParsingCh := make(chan fileParseContext)

	// Number of outstanding goroutines to wait for
	activeCount := 0
	var pending []fileParseContext
	tooManyErrors := false

	// Limit concurrent calls to parseBlueprintFiles to 200
	// Darwin has a default limit of 256 open files
	maxActiveCount := 200

	// count the number of pending calls to visitor()
	visitorWaitGroup := sync.WaitGroup{}

	startParseBlueprintsFile := func(blueprint fileParseContext) {
		if blueprintsSet[blueprint.fileName] {
			return
		}
		blueprintsSet[blueprint.fileName] = true
		activeCount++
		deps = append(deps, blueprint.fileName)
		visitorWaitGroup.Add(1)
		go func() {
			file, blueprints, deps, errs := c.openAndParse(blueprint.fileName, blueprint.Scope, rootDir,
				&blueprint)
			if len(errs) > 0 {
				errsCh <- errs
			}
			for _, blueprint := range blueprints {
				blueprintsCh <- blueprint
			}
			for _, dep := range deps {
				depsCh <- dep
			}
			doneParsingCh <- blueprint

			if blueprint.parent != nil && blueprint.parent.doneVisiting != nil {
				// wait for visitor() of parent to complete
				<-blueprint.parent.doneVisiting
			}

			if len(errs) == 0 {
				// process this file
				visitor(file)
			}
			if blueprint.doneVisiting != nil {
				close(blueprint.doneVisiting)
			}
			visitorWaitGroup.Done()
		}()
	}

	foundParseableBlueprint := func(blueprint fileParseContext) {
		if activeCount >= maxActiveCount {
			pending = append(pending, blueprint)
		} else {
			startParseBlueprintsFile(blueprint)
		}
	}

	startParseDescendants := func(blueprint fileParseContext) {
		descendants, hasDescendants := descendantsMap[blueprint.fileName]
		if hasDescendants {
			for _, descendant := range descendants {
				foundParseableBlueprint(fileParseContext{descendant, parser.NewScope(blueprint.Scope), &blueprint, make(chan struct{})})
			}
		}
	}

	// begin parsing any files that have no ancestors
	startParseDescendants(fileParseContext{"", parser.NewScope(nil), nil, nil})

loop:
	for {
		if len(errs) > maxErrors {
			tooManyErrors = true
		}

		select {
		case newErrs := <-errsCh:
			errs = append(errs, newErrs...)
		case dep := <-depsCh:
			deps = append(deps, dep)
		case blueprint := <-blueprintsCh:
			if tooManyErrors {
				continue
			}
			foundParseableBlueprint(blueprint)
		case blueprint := <-doneParsingCh:
			activeCount--
			if !tooManyErrors {
				startParseDescendants(blueprint)
			}
			if activeCount < maxActiveCount && len(pending) > 0 {
				// start to process the next one from the queue
				next := pending[len(pending)-1]
				pending = pending[:len(pending)-1]
				startParseBlueprintsFile(next)
			}
			if activeCount == 0 {
				break loop
			}
		}
	}

	sort.Strings(deps)

	// wait for every visitor() to complete
	visitorWaitGroup.Wait()

	return
}

// MockFileSystem causes the Context to replace all reads with accesses to the provided map of
// filenames to contents stored as a byte slice.
func (c *Context) MockFileSystem(files map[string][]byte) {
	// look for a module list file
	_, ok := files[MockModuleListFile]
	if !ok {
		// no module list file specified; find every file named Blueprints
		pathsToParse := []string{}
		for candidate := range files {
			if filepath.Base(candidate) == "Android.bp" {
				pathsToParse = append(pathsToParse, candidate)
			}
		}
		if len(pathsToParse) < 1 {
			panic(fmt.Sprintf("No Blueprints files found in mock filesystem: %v\n", files))
		}
		// put the list of Blueprints files into a list file
		files[MockModuleListFile] = []byte(strings.Join(pathsToParse, "\n"))
	}
	c.SetModuleListFile(MockModuleListFile)

	// mock the filesystem
	c.fs = pathtools.MockFs(files)
}

func (c *Context) SetFs(fs pathtools.FileSystem) {
	c.fs = fs
}

// openAndParse opens and parses a single Blueprints file, and returns the results
func (c *Context) openAndParse(filename string, scope *parser.Scope, rootDir string,
	parent *fileParseContext) (file *parser.File,
	subBlueprints []fileParseContext, deps []string, errs []error) {

	f, err := c.fs.Open(filename)
	if err != nil {
		// couldn't open the file; see if we can provide a clearer error than "could not open file"
		stats, statErr := c.fs.Lstat(filename)
		if statErr == nil {
			isSymlink := stats.Mode()&os.ModeSymlink != 0
			if isSymlink {
				err = fmt.Errorf("could not open symlink %v : %v", filename, err)
				target, readlinkErr := os.Readlink(filename)
				if readlinkErr == nil {
					_, targetStatsErr := c.fs.Lstat(target)
					if targetStatsErr != nil {
						err = fmt.Errorf("could not open symlink %v; its target (%v) cannot be opened", filename, target)
					}
				}
			} else {
				err = fmt.Errorf("%v exists but could not be opened: %v", filename, err)
			}
		}
		return nil, nil, nil, []error{err}
	}

	func() {
		defer func() {
			err = f.Close()
			if err != nil {
				errs = append(errs, err)
			}
		}()
		file, subBlueprints, errs = c.parseOne(rootDir, filename, f, scope, parent)
	}()

	if len(errs) > 0 {
		return nil, nil, nil, errs
	}

	for _, b := range subBlueprints {
		deps = append(deps, b.fileName)
	}

	return file, subBlueprints, deps, nil
}

// parseOne parses a single Blueprints file from the given reader, creating Module
// objects for each of the module definitions encountered.  If the Blueprints
// file contains an assignment to the "subdirs" variable, then the
// subdirectories listed are searched for Blueprints files returned in the
// subBlueprints return value.  If the Blueprints file contains an assignment
// to the "build" variable, then the file listed are returned in the
// subBlueprints return value.
//
// rootDir specifies the path to the root directory of the source tree, while
// filename specifies the path to the Blueprints file.  These paths are used for
// error reporting and for determining the module's directory.
func (c *Context) parseOne(rootDir, filename string, reader io.Reader,
	scope *parser.Scope, parent *fileParseContext) (file *parser.File, subBlueprints []fileParseContext, errs []error) {

	relBlueprintsFile, err := filepath.Rel(rootDir, filename)
	if err != nil {
		return nil, nil, []error{err}
	}

	scope.DontInherit("subdirs")
	scope.DontInherit("optional_subdirs")
	scope.DontInherit("build")
	file, errs = parser.ParseAndEval(filename, reader, scope)
	if len(errs) > 0 {
		for i, err := range errs {
			if parseErr, ok := err.(*parser.ParseError); ok {
				err = &BlueprintError{
					Err: parseErr.Err,
					Pos: parseErr.Pos,
				}
				errs[i] = err
			}
		}

		// If there were any parse errors don't bother trying to interpret the
		// result.
		return nil, nil, errs
	}
	file.Name = relBlueprintsFile

	build, buildPos, err := getLocalStringListFromScope(scope, "build")
	if err != nil {
		errs = append(errs, err)
	}
	for _, buildEntry := range build {
		if strings.Contains(buildEntry, "/") {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("illegal value %v. The '/' character is not permitted", buildEntry),
				Pos: buildPos,
			})
		}
	}

	if err != nil {
		errs = append(errs, err)
	}

	var blueprints []string

	newBlueprints, newErrs := c.findBuildBlueprints(filepath.Dir(filename), build, buildPos)
	blueprints = append(blueprints, newBlueprints...)
	errs = append(errs, newErrs...)

	subBlueprintsAndScope := make([]fileParseContext, len(blueprints))
	for i, b := range blueprints {
		subBlueprintsAndScope[i] = fileParseContext{b, parser.NewScope(scope), parent, make(chan struct{})}
	}
	return file, subBlueprintsAndScope, errs
}

func (c *Context) findBuildBlueprints(dir string, build []string,
	buildPos scanner.Position) ([]string, []error) {

	var blueprints []string
	var errs []error

	for _, file := range build {
		pattern := filepath.Join(dir, file)
		var matches []string
		var err error

		matches, err = c.glob(pattern, nil)

		if err != nil {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: %s", pattern, err.Error()),
				Pos: buildPos,
			})
			continue
		}

		if len(matches) == 0 {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: not found", pattern),
				Pos: buildPos,
			})
		}

		for _, foundBlueprints := range matches {
			if strings.HasSuffix(foundBlueprints, "/") {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("%q: is a directory", foundBlueprints),
					Pos: buildPos,
				})
			}
			blueprints = append(blueprints, foundBlueprints)
		}
	}

	return blueprints, errs
}

func (c *Context) findSubdirBlueprints(dir string, subdirs []string, subdirsPos scanner.Position,
	subBlueprintsName string, optional bool) ([]string, []error) {

	var blueprints []string
	var errs []error

	for _, subdir := range subdirs {
		pattern := filepath.Join(dir, subdir, subBlueprintsName)
		var matches []string
		var err error

		matches, err = c.glob(pattern, nil)

		if err != nil {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: %s", pattern, err.Error()),
				Pos: subdirsPos,
			})
			continue
		}

		if len(matches) == 0 && !optional {
			errs = append(errs, &BlueprintError{
				Err: fmt.Errorf("%q: not found", pattern),
				Pos: subdirsPos,
			})
		}

		for _, subBlueprints := range matches {
			if strings.HasSuffix(subBlueprints, "/") {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("%q: is a directory", subBlueprints),
					Pos: subdirsPos,
				})
			}
			blueprints = append(blueprints, subBlueprints)
		}
	}

	return blueprints, errs
}

func getLocalStringListFromScope(scope *parser.Scope, v string) ([]string, scanner.Position, error) {
	if assignment := scope.GetLocal(v); assignment == nil {
		return nil, scanner.Position{}, nil
	} else {
		switch value := assignment.Value.(type) {
		case *parser.List:
			ret := make([]string, 0, len(value.Values))

			for _, listValue := range value.Values {
				s, ok := listValue.(*parser.String)
				if !ok {
					// The parser should not produce this.
					panic("non-string value found in list")
				}

				ret = append(ret, s.Value)
			}

			return ret, assignment.EqualsPos, nil
		case *parser.Bool, *parser.String:
			return nil, scanner.Position{}, &BlueprintError{
				Err: fmt.Errorf("%q must be a list of strings", v),
				Pos: assignment.EqualsPos,
			}
		default:
			panic(fmt.Errorf("unknown value type: %d", assignment.Value.Type()))
		}
	}
}

// Clones a build logic module by calling the factory method for its module type, and then cloning
// property values.  Any values stored in the module object that are not stored in properties
// structs will be lost.
func (c *Context) cloneLogicModule(origModule *moduleInfo) (Module, []interface{}) {
	newLogicModule, newProperties := origModule.factory()

	if len(newProperties) != len(origModule.properties) {
		panic("mismatched properties array length in " + origModule.Name())
	}

	for i := range newProperties {
		dst := reflect.ValueOf(newProperties[i])
		src := reflect.ValueOf(origModule.properties[i])

		proptools.CopyProperties(dst, src)
	}

	return newLogicModule, newProperties
}

func newVariant(module *moduleInfo, mutatorName string, variationName string,
	local bool) variant {

	newVariantName := module.variant.name
	if variationName != "" {
		if newVariantName == "" {
			newVariantName = variationName
		} else {
			newVariantName += "_" + variationName
		}
	}

	newVariations := module.variant.variations.clone()
	newVariations.set(mutatorName, variationName)

	newDependencyVariations := module.variant.dependencyVariations.clone()
	if !local {
		newDependencyVariations.set(mutatorName, variationName)
	}

	return variant{newVariantName, newVariations, newDependencyVariations}
}

func (c *Context) createVariations(origModule *moduleInfo, mutator *mutatorInfo,
	depChooser depChooser, variationNames []string, local bool) (modulesOrAliases, []error) {

	if len(variationNames) == 0 {
		panic(fmt.Errorf("mutator %q passed zero-length variation list for module %q",
			mutator.name, origModule.Name()))
	}

	var newModules modulesOrAliases

	var errs []error

	for i, variationName := range variationNames {
		var newLogicModule Module
		var newProperties []interface{}

		if i == 0 && mutator.transitionMutator == nil {
			// Reuse the existing module for the first new variant
			// This both saves creating a new module, and causes the insertion in c.moduleInfo below
			// with logicModule as the key to replace the original entry in c.moduleInfo
			newLogicModule, newProperties = origModule.logicModule, origModule.properties
		} else {
			newLogicModule, newProperties = c.cloneLogicModule(origModule)
		}

		m := *origModule
		newModule := &m
		newModule.directDeps = slices.Clone(origModule.directDeps)
		newModule.reverseDeps = nil
		newModule.forwardDeps = nil
		newModule.logicModule = newLogicModule
		newModule.variant = newVariant(origModule, mutator.name, variationName, local)
		newModule.properties = newProperties
		newModule.providers = slices.Clone(origModule.providers)
		newModule.providerInitialValueHashes = slices.Clone(origModule.providerInitialValueHashes)

		newModules = append(newModules, newModule)

		newErrs := c.convertDepsToVariation(newModule, i, depChooser)
		if len(newErrs) > 0 {
			errs = append(errs, newErrs...)
		}
	}

	// Mark original variant as invalid.  Modules that depend on this module will still
	// depend on origModule, but we'll fix it when the mutator is called on them.
	origModule.obsoletedByNewVariants = true
	origModule.splitModules = newModules

	atomic.AddUint32(&c.depsModified, 1)

	return newModules, errs
}

type depChooser func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string)

func chooseDep(candidates modulesOrAliases, mutatorName, variationName string, defaultVariationName *string) (*moduleInfo, string) {
	for _, m := range candidates {
		if m.moduleOrAliasVariant().variations.get(mutatorName) == variationName {
			return m.moduleOrAliasTarget(), ""
		}
	}

	if defaultVariationName != nil {
		// give it a second chance; match with defaultVariationName
		for _, m := range candidates {
			if m.moduleOrAliasVariant().variations.get(mutatorName) == *defaultVariationName {
				return m.moduleOrAliasTarget(), ""
			}
		}
	}

	return nil, variationName
}

func chooseDepByIndexes(mutatorName string, variations [][]string) depChooser {
	return func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string) {
		desiredVariation := variations[variationIndex][depIndex]
		return chooseDep(dep.module.splitModules, mutatorName, desiredVariation, nil)
	}
}

func chooseDepExplicit(mutatorName string,
	variationName string, defaultVariationName *string) depChooser {
	return func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string) {
		return chooseDep(dep.module.splitModules, mutatorName, variationName, defaultVariationName)
	}
}

func chooseDepInherit(mutatorName string, defaultVariationName *string) depChooser {
	return func(source *moduleInfo, variationIndex, depIndex int, dep depInfo) (*moduleInfo, string) {
		sourceVariation := source.variant.variations.get(mutatorName)
		return chooseDep(dep.module.splitModules, mutatorName, sourceVariation, defaultVariationName)
	}
}

func (c *Context) convertDepsToVariation(module *moduleInfo, variationIndex int, depChooser depChooser) (errs []error) {
	for i, dep := range module.directDeps {
		if dep.module.obsoletedByNewVariants {
			newDep, missingVariation := depChooser(module, variationIndex, i, dep)
			if newDep == nil {
				errs = append(errs, &BlueprintError{
					Err: fmt.Errorf("failed to find variation %q for module %q needed by %q",
						missingVariation, dep.module.Name(), module.Name()),
					Pos: module.pos,
				})
				continue
			}
			module.directDeps[i].module = newDep
		}
	}

	return errs
}

func (c *Context) prettyPrintVariant(variations variationMap) string {
	var names []string
	for _, m := range c.variantMutatorNames {
		if v := variations.get(m); v != "" {
			names = append(names, m+":"+v)
		}
	}
	if len(names) == 0 {
		return "<empty variant>"
	}

	return strings.Join(names, ",")
}

func (c *Context) prettyPrintGroupVariants(group *moduleGroup) string {
	var variants []string
	for _, moduleOrAlias := range group.modules {
		if mod := moduleOrAlias.module(); mod != nil {
			variants = append(variants, c.prettyPrintVariant(mod.variant.variations))
		} else if alias := moduleOrAlias.alias(); alias != nil {
			variants = append(variants, c.prettyPrintVariant(alias.variant.variations)+
				" (alias to "+c.prettyPrintVariant(alias.target.variant.variations)+")")
		}
	}
	return strings.Join(variants, "\n  ")
}

func newModule(factory ModuleFactory) *moduleInfo {
	logicModule, properties := factory()

	return &moduleInfo{
		logicModule: logicModule,
		factory:     factory,
		properties:  properties,
	}
}

func processModuleDef(moduleDef *parser.Module,
	relBlueprintsFile string, moduleFactories, scopedModuleFactories map[string]ModuleFactory, ignoreUnknownModuleTypes bool) (*moduleInfo, []error) {

	factory, ok := moduleFactories[moduleDef.Type]
	if !ok && scopedModuleFactories != nil {
		factory, ok = scopedModuleFactories[moduleDef.Type]
	}
	if !ok {
		if ignoreUnknownModuleTypes {
			return nil, nil
		}

		return nil, []error{
			&BlueprintError{
				Err: fmt.Errorf("unrecognized module type %q", moduleDef.Type),
				Pos: moduleDef.TypePos,
			},
		}
	}

	module := newModule(factory)
	module.typeName = moduleDef.Type

	module.relBlueprintsFile = relBlueprintsFile

	propertyMap, errs := proptools.UnpackProperties(moduleDef.Properties, module.properties...)
	if len(errs) > 0 {
		for i, err := range errs {
			if unpackErr, ok := err.(*proptools.UnpackError); ok {
				err = &BlueprintError{
					Err: unpackErr.Err,
					Pos: unpackErr.Pos,
				}
				errs[i] = err
			}
		}
		return nil, errs
	}

	module.pos = moduleDef.TypePos
	module.propertyPos = make(map[string]scanner.Position)
	for name, propertyDef := range propertyMap {
		module.propertyPos[name] = propertyDef.ColonPos
	}

	return module, nil
}

func (c *Context) addModule(module *moduleInfo) []error {
	name := module.logicModule.Name()
	if name == "" {
		return []error{
			&BlueprintError{
				Err: fmt.Errorf("property 'name' is missing from a module"),
				Pos: module.pos,
			},
		}
	}
	c.moduleInfo[module.logicModule] = module

	group := &moduleGroup{
		name:    name,
		modules: modulesOrAliases{module},
	}
	module.group = group
	namespace, errs := c.nameInterface.NewModule(
		newNamespaceContext(module),
		ModuleGroup{moduleGroup: group},
		module.logicModule)
	if len(errs) > 0 {
		for i := range errs {
			errs[i] = &BlueprintError{Err: errs[i], Pos: module.pos}
		}
		return errs
	}
	group.namespace = namespace

	c.moduleGroups = append(c.moduleGroups, group)

	return nil
}

// ResolveDependencies checks that the dependencies specified by all of the
// modules defined in the parsed Blueprints files are valid.  This means that
// the modules depended upon are defined and that no circular dependencies
// exist.
func (c *Context) ResolveDependencies(config interface{}) (deps []string, errs []error) {
	c.BeginEvent("resolve_deps")
	defer c.EndEvent("resolve_deps")
	return c.resolveDependencies(c.Context, config)
}

func (c *Context) resolveDependencies(ctx context.Context, config interface{}) (deps []string, errs []error) {
	pprof.Do(ctx, pprof.Labels("blueprint", "ResolveDependencies"), func(ctx context.Context) {
		c.initProviders()

		errs = c.updateDependencies()
		if len(errs) > 0 {
			return
		}

		deps, errs = c.runMutators(ctx, config)
		if len(errs) > 0 {
			return
		}

		c.BeginEvent("clone_modules")
		if !c.SkipCloneModulesAfterMutators {
			c.cloneModules()
		}
		defer c.EndEvent("clone_modules")

		c.clearTransitionMutatorInputVariants()

		c.dependenciesReady = true
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

// Default dependencies handling.  If the module implements the (deprecated)
// DynamicDependerModule interface then this set consists of the union of those
// module names returned by its DynamicDependencies method and those added by calling
// AddDependencies or AddVariationDependencies on DynamicDependencyModuleContext.
func blueprintDepsMutator(ctx BottomUpMutatorContext) {
	if dynamicDepender, ok := ctx.Module().(DynamicDependerModule); ok {
		func() {
			defer func() {
				if r := recover(); r != nil {
					ctx.error(newPanicErrorf(r, "DynamicDependencies for %s", ctx.moduleInfo()))
				}
			}()
			dynamicDeps := dynamicDepender.DynamicDependencies(ctx)

			if ctx.Failed() {
				return
			}

			ctx.AddDependency(ctx.Module(), nil, dynamicDeps...)
		}()
	}
}

// findExactVariantOrSingle searches the moduleGroup for a module with the same variant as module,
// and returns the matching module, or nil if one is not found.  A group with exactly one module
// is always considered matching.
func (c *Context) findExactVariantOrSingle(module *moduleInfo, config any, possible *moduleGroup, reverse bool) *moduleInfo {
	found, _ := c.findVariant(module, config, possible, nil, false, reverse)
	if found == nil {
		for _, moduleOrAlias := range possible.modules {
			if m := moduleOrAlias.module(); m != nil {
				if found != nil {
					// more than one possible match, give up
					return nil
				}
				found = m
			}
		}
	}
	return found
}

func (c *Context) addDependency(module *moduleInfo, config any, tag DependencyTag, depName string) (*moduleInfo, []error) {
	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	if depName == module.Name() {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on itself", depName),
			Pos: module.pos,
		}}
	}

	possibleDeps := c.moduleGroupFromName(depName, module.namespace())
	if possibleDeps == nil {
		return nil, c.discoveredMissingDependencies(module, depName, variationMap{})
	}

	if m := c.findExactVariantOrSingle(module, config, possibleDeps, false); m != nil {
		module.newDirectDeps = append(module.newDirectDeps, depInfo{m, tag})
		atomic.AddUint32(&c.depsModified, 1)
		return m, nil
	}

	if c.allowMissingDependencies {
		// Allow missing variants.
		return nil, c.discoveredMissingDependencies(module, depName, module.variant.dependencyVariations)
	}

	return nil, []error{&BlueprintError{
		Err: fmt.Errorf("dependency %q of %q missing variant:\n  %s\navailable variants:\n  %s",
			depName, module.Name(),
			c.prettyPrintVariant(module.variant.dependencyVariations),
			c.prettyPrintGroupVariants(possibleDeps)),
		Pos: module.pos,
	}}
}

func (c *Context) findReverseDependency(module *moduleInfo, config any, destName string) (*moduleInfo, []error) {
	if destName == module.Name() {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on itself", destName),
			Pos: module.pos,
		}}
	}

	possibleDeps := c.moduleGroupFromName(destName, module.namespace())
	if possibleDeps == nil {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q has a reverse dependency on undefined module %q",
				module.Name(), destName),
			Pos: module.pos,
		}}
	}

	if m := c.findExactVariantOrSingle(module, config, possibleDeps, true); m != nil {
		return m, nil
	}

	if c.allowMissingDependencies {
		// Allow missing variants.
		return module, c.discoveredMissingDependencies(module, destName, module.variant.dependencyVariations)
	}

	return nil, []error{&BlueprintError{
		Err: fmt.Errorf("reverse dependency %q of %q missing variant:\n  %s\navailable variants:\n  %s",
			destName, module.Name(),
			c.prettyPrintVariant(module.variant.dependencyVariations),
			c.prettyPrintGroupVariants(possibleDeps)),
		Pos: module.pos,
	}}
}

// applyTransitions takes a variationMap being used to add a dependency on a module in a moduleGroup
// and applies the OutgoingTransition and IncomingTransition methods of each completed TransitionMutator to
// modify the requested variation.  It finds a variant that existed before the TransitionMutator ran that is
// a subset of the requested variant to use as the module context for IncomingTransition.
func (c *Context) applyTransitions(config any, module *moduleInfo, group *moduleGroup, variant variationMap,
	requestedVariations []Variation) variationMap {
	for _, transitionMutator := range c.transitionMutators {
		explicitlyRequested := slices.ContainsFunc(requestedVariations, func(variation Variation) bool {
			return variation.Mutator == transitionMutator.name
		})

		sourceVariation := variant.get(transitionMutator.name)
		outgoingVariation := sourceVariation

		// Apply the outgoing transition if it was not explicitly requested.
		if !explicitlyRequested {
			ctx := outgoingTransitionContextPool.Get()
			*ctx = outgoingTransitionContextImpl{
				transitionContextImpl{context: c, source: module, dep: nil,
					depTag: nil, postMutator: true, config: config},
			}
			outgoingVariation = transitionMutator.mutator.OutgoingTransition(ctx, sourceVariation)
			outgoingTransitionContextPool.Put(ctx)
		}

		earlierVariantCreatingMutators := c.variantCreatingMutatorOrder[:transitionMutator.variantCreatingMutatorIndex]
		filteredVariant := variant.cloneMatching(earlierVariantCreatingMutators)

		check := func(inputVariant variationMap) bool {
			filteredInputVariant := inputVariant.cloneMatching(earlierVariantCreatingMutators)
			return filteredInputVariant.equal(filteredVariant)
		}

		// Find an appropriate module to use as the context for the IncomingTransition.  First check if any of the
		// saved inputVariants for the transition mutator match the filtered variant.
		var matchingInputVariant *moduleInfo
		for _, inputVariant := range transitionMutator.inputVariants[group] {
			if check(inputVariant.variant.variations) {
				matchingInputVariant = inputVariant
				break
			}
		}

		if matchingInputVariant == nil {
			// If no inputVariants match, check all the variants of the module for a match.  This can happen if
			// the mutator only created a single "" variant when it ran on this module.  Matching against all variants
			// is slightly worse  than checking the input variants, as the selected variant could have been modified
			// by a later mutator in a way that affects the results of IncomingTransition.
			for _, moduleOrAlias := range group.modules {
				if module := moduleOrAlias.module(); module != nil {
					if check(module.variant.variations) {
						matchingInputVariant = module
						break
					}
				}
			}
		}

		if matchingInputVariant != nil {
			// Apply the incoming transition.
			ctx := incomingTransitionContextPool.Get()
			*ctx = incomingTransitionContextImpl{
				transitionContextImpl{context: c, source: nil, dep: matchingInputVariant,
					depTag: nil, postMutator: true, config: config},
			}

			finalVariation := transitionMutator.mutator.IncomingTransition(ctx, outgoingVariation)
			variant.set(transitionMutator.name, finalVariation)
			incomingTransitionContextPool.Put(ctx)
		}

		if (matchingInputVariant == nil && !explicitlyRequested) || variant.get(transitionMutator.name) == "" {
			// The transition mutator didn't apply anything to the target variant, remove the variation unless it
			// was explicitly requested when adding the dependency.
			variant.delete(transitionMutator.name)
		}
	}

	return variant
}

func (c *Context) findVariant(module *moduleInfo, config any,
	possibleDeps *moduleGroup, requestedVariations []Variation, far bool, reverse bool) (*moduleInfo, variationMap) {

	// We can't just append variant.Variant to module.dependencyVariant.variantName and
	// compare the strings because the result won't be in mutator registration order.
	// Create a new map instead, and then deep compare the maps.
	var newVariant variationMap
	if !far {
		if !reverse {
			// For forward dependency, ignore local variants by matching against
			// dependencyVariant which doesn't have the local variants
			newVariant = module.variant.dependencyVariations.clone()
		} else {
			// For reverse dependency, use all the variants
			newVariant = module.variant.variations.clone()
		}
	}
	for _, v := range requestedVariations {
		newVariant.set(v.Mutator, v.Variation)
	}

	if !reverse {
		newVariant = c.applyTransitions(config, module, possibleDeps, newVariant, requestedVariations)
	}

	// check returns a bool for whether the requested newVariant matches the given variant from possibleDeps, and a
	// divergence score.  A score of 0 is best match, and a positive integer is a worse match.
	// For a non-far search, the score is always 0 as the match must always be exact.  For a far search,
	// the score is the number of variants that are present in the given variant but not newVariant.
	check := func(variant variationMap) (bool, int) {
		if far {
			if newVariant.subsetOf(variant) {
				return true, variant.differenceKeysCount(newVariant)
			}
		} else {
			if variant.equal(newVariant) {
				return true, 0
			}
		}
		return false, math.MaxInt
	}

	var foundDep *moduleInfo
	bestDivergence := math.MaxInt
	for _, m := range possibleDeps.modules {
		if match, divergence := check(m.moduleOrAliasVariant().variations); match && divergence < bestDivergence {
			foundDep = m.moduleOrAliasTarget()
			bestDivergence = divergence
			if !far {
				// non-far dependencies use equality, so only the first match needs to be checked.
				break
			}
		}
	}

	return foundDep, newVariant
}

func (c *Context) addVariationDependency(module *moduleInfo, config any, variations []Variation,
	tag DependencyTag, depName string, far bool) (*moduleInfo, []error) {
	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	possibleDeps := c.moduleGroupFromName(depName, module.namespace())
	if possibleDeps == nil {
		return nil, c.discoveredMissingDependencies(module, depName, variationMap{})
	}

	foundDep, newVariant := c.findVariant(module, config, possibleDeps, variations, far, false)

	if foundDep == nil {
		if c.allowMissingDependencies {
			// Allow missing variants.
			return nil, c.discoveredMissingDependencies(module, depName, newVariant)
		}
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("dependency %q of %q missing variant:\n  %s\navailable variants:\n  %s",
				depName, module.Name(),
				c.prettyPrintVariant(newVariant),
				c.prettyPrintGroupVariants(possibleDeps)),
			Pos: module.pos,
		}}
	}

	if module == foundDep {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on itself", depName),
			Pos: module.pos,
		}}
	}
	// AddVariationDependency allows adding a dependency on itself, but only if
	// that module is earlier in the module list than this one, since we always
	// run GenerateBuildActions in order for the variants of a module
	if foundDep.group == module.group && beforeInModuleList(module, foundDep, module.group.modules) {
		return nil, []error{&BlueprintError{
			Err: fmt.Errorf("%q depends on later version of itself", depName),
			Pos: module.pos,
		}}
	}
	module.newDirectDeps = append(module.newDirectDeps, depInfo{foundDep, tag})
	atomic.AddUint32(&c.depsModified, 1)
	return foundDep, nil
}

func (c *Context) addInterVariantDependency(origModule *moduleInfo, tag DependencyTag,
	from, to Module) *moduleInfo {
	if _, ok := tag.(BaseDependencyTag); ok {
		panic("BaseDependencyTag is not allowed to be used directly!")
	}

	var fromInfo, toInfo *moduleInfo
	for _, moduleOrAlias := range origModule.splitModules {
		if m := moduleOrAlias.module(); m != nil {
			if m.logicModule == from {
				fromInfo = m
			}
			if m.logicModule == to {
				toInfo = m
				if fromInfo != nil {
					panic(fmt.Errorf("%q depends on later version of itself", origModule.Name()))
				}
			}
		}
	}

	if fromInfo == nil || toInfo == nil {
		panic(fmt.Errorf("AddInterVariantDependency called for module %q on invalid variant",
			origModule.Name()))
	}

	fromInfo.newDirectDeps = append(fromInfo.newDirectDeps, depInfo{toInfo, tag})
	atomic.AddUint32(&c.depsModified, 1)
	return toInfo
}

// findBlueprintDescendants returns a map linking parent Blueprint files to child Blueprints files
// For example, if paths = []string{"a/b/c/Android.bp", "a/Android.bp"},
// then descendants = {"":[]string{"a/Android.bp"}, "a/Android.bp":[]string{"a/b/c/Android.bp"}}
func findBlueprintDescendants(paths []string) (descendants map[string][]string, err error) {
	// make mapping from dir path to file path
	filesByDir := make(map[string]string, len(paths))
	for _, path := range paths {
		dir := filepath.Dir(path)
		_, alreadyFound := filesByDir[dir]
		if alreadyFound {
			return nil, fmt.Errorf("Found two Blueprint files in directory %v : %v and %v", dir, filesByDir[dir], path)
		}
		filesByDir[dir] = path
	}

	findAncestor := func(childFile string) (ancestor string) {
		prevAncestorDir := filepath.Dir(childFile)
		for {
			ancestorDir := filepath.Dir(prevAncestorDir)
			if ancestorDir == prevAncestorDir {
				// reached the root dir without any matches; assign this as a descendant of ""
				return ""
			}

			ancestorFile, ancestorExists := filesByDir[ancestorDir]
			if ancestorExists {
				return ancestorFile
			}
			prevAncestorDir = ancestorDir
		}
	}
	// generate the descendants map
	descendants = make(map[string][]string, len(filesByDir))
	for _, childFile := range filesByDir {
		ancestorFile := findAncestor(childFile)
		descendants[ancestorFile] = append(descendants[ancestorFile], childFile)
	}
	return descendants, nil
}

type visitOrderer interface {
	// returns the number of modules that this module needs to wait for
	waitCount(module *moduleInfo) int
	// returns the list of modules that are waiting for this module
	propagate(module *moduleInfo) []*moduleInfo
	// visit modules in order
	visit(modules []*moduleInfo, visit func(*moduleInfo, chan<- pauseSpec) bool)
}

type unorderedVisitorImpl struct{}

func (unorderedVisitorImpl) waitCount(module *moduleInfo) int {
	return 0
}

func (unorderedVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return nil
}

func (unorderedVisitorImpl) visit(modules []*moduleInfo, visit func(*moduleInfo, chan<- pauseSpec) bool) {
	for _, module := range modules {
		if visit(module, nil) {
			return
		}
	}
}

type bottomUpVisitorImpl struct{}

func (bottomUpVisitorImpl) waitCount(module *moduleInfo) int {
	return len(module.forwardDeps)
}

func (bottomUpVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return module.reverseDeps
}

func (bottomUpVisitorImpl) visit(modules []*moduleInfo, visit func(*moduleInfo, chan<- pauseSpec) bool) {
	for _, module := range modules {
		if visit(module, nil) {
			return
		}
	}
}

type topDownVisitorImpl struct{}

func (topDownVisitorImpl) waitCount(module *moduleInfo) int {
	return len(module.reverseDeps)
}

func (topDownVisitorImpl) propagate(module *moduleInfo) []*moduleInfo {
	return module.forwardDeps
}

func (topDownVisitorImpl) visit(modules []*moduleInfo, visit func(*moduleInfo, chan<- pauseSpec) bool) {
	for i := 0; i < len(modules); i++ {
		module := modules[len(modules)-1-i]
		if visit(module, nil) {
			return
		}
	}
}

var (
	bottomUpVisitor bottomUpVisitorImpl
	topDownVisitor  topDownVisitorImpl
)

// pauseSpec describes a pause that a module needs to occur until another module has been visited,
// at which point the unpause channel will be closed.
type pauseSpec struct {
	paused  *moduleInfo
	until   *moduleInfo
	unpause unpause
}

type unpause chan struct{}

const parallelVisitLimit = 1000

// Calls visit on each module, guaranteeing that visit is not called on a module until visit on all
// of its dependencies has finished.  A visit function can write a pauseSpec to the pause channel
// to wait for another dependency to be visited.  If a visit function returns true to cancel
// while another visitor is paused, the paused visitor will never be resumed and its goroutine
// will stay paused forever.
func parallelVisit(modules []*moduleInfo, order visitOrderer, limit int,
	visit func(module *moduleInfo, pause chan<- pauseSpec) bool) []error {

	doneCh := make(chan *moduleInfo)
	cancelCh := make(chan bool)
	pauseCh := make(chan pauseSpec)
	cancel := false

	var backlog []*moduleInfo      // Visitors that are ready to start but backlogged due to limit.
	var unpauseBacklog []pauseSpec // Visitors that are ready to unpause but backlogged due to limit.

	active := 0  // Number of visitors running, not counting paused visitors.
	visited := 0 // Number of finished visitors.

	pauseMap := make(map[*moduleInfo][]pauseSpec)

	for _, module := range modules {
		module.waitingCount = order.waitCount(module)
	}

	// Call the visitor on a module if there are fewer active visitors than the parallelism
	// limit, otherwise add it to the backlog.
	startOrBacklog := func(module *moduleInfo) {
		if active < limit {
			active++
			go func() {
				ret := visit(module, pauseCh)
				if ret {
					cancelCh <- true
				}
				doneCh <- module
			}()
		} else {
			backlog = append(backlog, module)
		}
	}

	// Unpause the already-started but paused  visitor on a module if there are fewer active
	// visitors than the parallelism limit, otherwise add it to the backlog.
	unpauseOrBacklog := func(pauseSpec pauseSpec) {
		if active < limit {
			active++
			close(pauseSpec.unpause)
		} else {
			unpauseBacklog = append(unpauseBacklog, pauseSpec)
		}
	}

	// Start any modules in the backlog up to the parallelism limit.  Unpause paused modules first
	// since they may already be holding resources.
	unpauseOrStartFromBacklog := func() {
		for active < limit && len(unpauseBacklog) > 0 {
			unpause := unpauseBacklog[0]
			unpauseBacklog = unpauseBacklog[1:]
			unpauseOrBacklog(unpause)
		}
		for active < limit && len(backlog) > 0 {
			toVisit := backlog[0]
			backlog = backlog[1:]
			startOrBacklog(toVisit)
		}
	}

	toVisit := len(modules)

	// Start or backlog any modules that are not waiting for any other modules.
	for _, module := range modules {
		if module.waitingCount == 0 {
			startOrBacklog(module)
		}
	}

	for active > 0 {
		select {
		case <-cancelCh:
			cancel = true
			backlog = nil
		case doneModule := <-doneCh:
			active--
			if !cancel {
				// Mark this module as done.
				doneModule.waitingCount = -1
				visited++

				// Unpause or backlog any modules that were waiting for this one.
				if unpauses, ok := pauseMap[doneModule]; ok {
					delete(pauseMap, doneModule)
					for _, unpause := range unpauses {
						unpauseOrBacklog(unpause)
					}
				}

				// Start any backlogged modules up to limit.
				unpauseOrStartFromBacklog()

				// Decrement waitingCount on the next modules in the tree based
				// on propagation order, and start or backlog them if they are
				// ready to start.
				for _, module := range order.propagate(doneModule) {
					module.waitingCount--
					if module.waitingCount == 0 {
						startOrBacklog(module)
					}
				}
			}
		case pauseSpec := <-pauseCh:
			if pauseSpec.until.waitingCount == -1 {
				// Module being paused for is already finished, resume immediately.
				close(pauseSpec.unpause)
			} else {
				// Register for unpausing.
				pauseMap[pauseSpec.until] = append(pauseMap[pauseSpec.until], pauseSpec)

				// Don't count paused visitors as active so that this can't deadlock
				// if 1000 visitors are paused simultaneously.
				active--
				unpauseOrStartFromBacklog()
			}
		}
	}

	if !cancel {
		// Invariant check: no backlogged modules, these weren't waiting on anything except
		// the parallelism limit so they should have run.
		if len(backlog) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d backlogged visitors", len(backlog)))
		}

		// Invariant check: no backlogged paused modules, these weren't waiting on anything
		// except the parallelism limit so they should have run.
		if len(unpauseBacklog) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d backlogged unpaused visitors", len(unpauseBacklog)))
		}

		if len(pauseMap) > 0 {
			// Probably a deadlock due to a newly added dependency cycle. Start from each module in
			// the order of the input modules list and perform a depth-first search for the module
			// it is paused on, ignoring modules that are marked as done.  Note this traverses from
			// modules to the modules that would have been unblocked when that module finished, i.e
			// the reverse of the visitOrderer.

			// In order to reduce duplicated work, once a module has been checked and determined
			// not to be part of a cycle add it and everything that depends on it to the checked
			// map.
			checked := make(map[*moduleInfo]struct{})

			var check func(module, end *moduleInfo) []*moduleInfo
			check = func(module, end *moduleInfo) []*moduleInfo {
				if module.waitingCount == -1 {
					// This module was finished, it can't be part of a loop.
					return nil
				}
				if module == end {
					// This module is the end of the loop, start rolling up the cycle.
					return []*moduleInfo{module}
				}

				if _, alreadyChecked := checked[module]; alreadyChecked {
					return nil
				}

				for _, dep := range order.propagate(module) {
					cycle := check(dep, end)
					if cycle != nil {
						return append([]*moduleInfo{module}, cycle...)
					}
				}
				for _, depPauseSpec := range pauseMap[module] {
					cycle := check(depPauseSpec.paused, end)
					if cycle != nil {
						return append([]*moduleInfo{module}, cycle...)
					}
				}

				checked[module] = struct{}{}
				return nil
			}

			// Iterate over the modules list instead of pauseMap to provide deterministic ordering.
			for _, module := range modules {
				for _, pauseSpec := range pauseMap[module] {
					cycle := check(pauseSpec.paused, pauseSpec.until)
					if len(cycle) > 0 {
						return cycleError(cycle)
					}
				}
			}
		}

		// Invariant check: if there was no deadlock and no cancellation every module
		// should have been visited.
		if visited != toVisit {
			panic(fmt.Errorf("parallelVisit ran %d visitors, expected %d", visited, toVisit))
		}

		// Invariant check: if there was no deadlock and no cancellation  every module
		// should have been visited, so there is nothing left to be paused on.
		if len(pauseMap) > 0 {
			panic(fmt.Errorf("parallelVisit finished with %d paused visitors", len(pauseMap)))
		}
	}

	return nil
}

func cycleError(cycle []*moduleInfo) (errs []error) {
	// The cycle list is in reverse order because all the 'check' calls append
	// their own module to the list.
	errs = append(errs, &BlueprintError{
		Err: fmt.Errorf("encountered dependency cycle:"),
		Pos: cycle[len(cycle)-1].pos,
	})

	// Iterate backwards through the cycle list.
	curModule := cycle[0]
	for i := len(cycle) - 1; i >= 0; i-- {
		nextModule := cycle[i]
		errs = append(errs, &BlueprintError{
			Err: fmt.Errorf("    %s depends on %s",
				curModule, nextModule),
			Pos: curModule.pos,
		})
		curModule = nextModule
	}

	return errs
}

// updateDependencies recursively walks the module dependency graph and updates
// additional fields based on the dependencies.  It builds a sorted list of modules
// such that dependencies of a module always appear first, and populates reverse
// dependency links and counts of total dependencies.  It also reports errors when
// it encounters dependency cycles.  This should be called after resolveDependencies,
// as well as after any mutator pass has called addDependency
func (c *Context) updateDependencies() (errs []error) {
	c.cachedDepsModified = true
	visited := make(map[*moduleInfo]bool)  // modules that were already checked
	checking := make(map[*moduleInfo]bool) // modules actively being checked

	sorted := make([]*moduleInfo, 0, len(c.moduleInfo))

	var check func(group *moduleInfo) []*moduleInfo

	check = func(module *moduleInfo) []*moduleInfo {
		visited[module] = true
		checking[module] = true
		defer delete(checking, module)

		// Reset the forward and reverse deps without reducing their capacity to avoid reallocation.
		module.reverseDeps = module.reverseDeps[:0]
		module.forwardDeps = module.forwardDeps[:0]

		// Add an implicit dependency ordering on all earlier modules in the same module group
		for _, dep := range module.group.modules {
			if dep == module {
				break
			}
			if depModule := dep.module(); depModule != nil {
				module.forwardDeps = append(module.forwardDeps, depModule)
			}
		}

	outer:
		for _, dep := range module.directDeps {
			// use a loop to check for duplicates, average number of directDeps measured to be 9.5.
			for _, exists := range module.forwardDeps {
				if dep.module == exists {
					continue outer
				}
			}
			module.forwardDeps = append(module.forwardDeps, dep.module)
		}

		for _, dep := range module.forwardDeps {
			if checking[dep] {
				// This is a cycle.
				return []*moduleInfo{dep, module}
			}

			if !visited[dep] {
				cycle := check(dep)
				if cycle != nil {
					if cycle[0] == module {
						// We are the "start" of the cycle, so we're responsible
						// for generating the errors.
						errs = append(errs, cycleError(cycle)...)

						// We can continue processing this module's children to
						// find more cycles.  Since all the modules that were
						// part of the found cycle were marked as visited we
						// won't run into that cycle again.
					} else {
						// We're not the "start" of the cycle, so we just append
						// our module to the list and return it.
						return append(cycle, module)
					}
				}
			}

			dep.reverseDeps = append(dep.reverseDeps, module)
		}

		sorted = append(sorted, module)

		return nil
	}

	for _, module := range c.moduleInfo {
		if !visited[module] {
			cycle := check(module)
			if cycle != nil {
				if cycle[len(cycle)-1] != module {
					panic("inconceivable!")
				}
				errs = append(errs, cycleError(cycle)...)
			}
		}
	}

	c.modulesSorted = sorted

	return
}

type jsonVariations []Variation

type jsonModuleName struct {
	Name    string
	Variant string
}

type jsonDep struct {
	jsonModuleName
	Tag string
}

type JsonModule struct {
	jsonModuleName
	Deps      []jsonDep
	Type      string
	Blueprint string
	CreatedBy *string
	Module    map[string]interface{}
}

func jsonModuleNameFromModuleInfo(m *moduleInfo) *jsonModuleName {
	return &jsonModuleName{
		Name:    m.Name(),
		Variant: m.variant.name,
	}
}

type JSONDataSupplier interface {
	AddJSONData(d *map[string]interface{})
}

// JSONAction contains the action-related info we expose to json module graph
type JSONAction struct {
	Inputs  []string
	Outputs []string
	Desc    string
}

// JSONActionSupplier allows JSON representation of additional actions that are not registered in
// Ninja
type JSONActionSupplier interface {
	JSONActions() []JSONAction
}

func jsonModuleFromModuleInfo(m *moduleInfo) *JsonModule {
	result := &JsonModule{
		jsonModuleName: *jsonModuleNameFromModuleInfo(m),
		Deps:           make([]jsonDep, 0),
		Type:           m.typeName,
		Blueprint:      m.relBlueprintsFile,
		Module:         make(map[string]interface{}),
	}
	if m.createdBy != nil {
		n := m.createdBy.Name()
		result.CreatedBy = &n
	}
	if j, ok := m.logicModule.(JSONDataSupplier); ok {
		j.AddJSONData(&result.Module)
	}
	for _, p := range m.providers {
		if j, ok := p.(JSONDataSupplier); ok {
			j.AddJSONData(&result.Module)
		}
	}
	return result
}

func jsonModuleWithActionsFromModuleInfo(m *moduleInfo, nameTracker *nameTracker) *JsonModule {
	result := &JsonModule{
		jsonModuleName: jsonModuleName{
			Name:    m.Name(),
			Variant: m.variant.name,
		},
		Deps:      make([]jsonDep, 0),
		Type:      m.typeName,
		Blueprint: m.relBlueprintsFile,
		Module:    make(map[string]interface{}),
	}
	var actions []JSONAction
	for _, bDef := range m.actionDefs.buildDefs {
		a := JSONAction{
			Inputs: append(append(append(
				bDef.InputStrings,
				bDef.ImplicitStrings...),
				getNinjaStrings(bDef.Inputs, nameTracker)...),
				getNinjaStrings(bDef.Implicits, nameTracker)...),

			Outputs: append(append(append(
				bDef.OutputStrings,
				bDef.ImplicitOutputStrings...),
				getNinjaStrings(bDef.Outputs, nameTracker)...),
				getNinjaStrings(bDef.ImplicitOutputs, nameTracker)...),
		}
		if d, ok := bDef.Variables["description"]; ok {
			a.Desc = d.Value(nameTracker)
		}
		actions = append(actions, a)
	}

	if j, ok := m.logicModule.(JSONActionSupplier); ok {
		actions = append(actions, j.JSONActions()...)
	}
	for _, p := range m.providers {
		if j, ok := p.(JSONActionSupplier); ok {
			actions = append(actions, j.JSONActions()...)
		}
	}

	result.Module["Actions"] = actions
	return result
}

// Gets a list of strings from the given list of ninjaStrings by invoking ninjaString.Value on each.
func getNinjaStrings(nStrs []*ninjaString, nameTracker *nameTracker) []string {
	var strs []string
	for _, nstr := range nStrs {
		strs = append(strs, nstr.Value(nameTracker))
	}
	return strs
}

func (c *Context) GetWeightedOutputsFromPredicate(predicate func(*JsonModule) (bool, int)) map[string]int {
	outputToWeight := make(map[string]int)
	for _, m := range c.modulesSorted {
		jmWithActions := jsonModuleWithActionsFromModuleInfo(m, c.nameTracker)
		if ok, weight := predicate(jmWithActions); ok {
			for _, a := range jmWithActions.Module["Actions"].([]JSONAction) {
				for _, o := range a.Outputs {
					if val, ok := outputToWeight[o]; ok {
						if val > weight {
							continue
						}
					}
					outputToWeight[o] = weight
				}
			}
		}
	}
	return outputToWeight
}

// PrintJSONGraph prints info of modules in a JSON file.
func (c *Context) PrintJSONGraphAndActions(wGraph io.Writer, wActions io.Writer) {
	modulesToGraph := make([]*JsonModule, 0)
	modulesToActions := make([]*JsonModule, 0)
	for _, m := range c.modulesSorted {
		jm := jsonModuleFromModuleInfo(m)
		jmWithActions := jsonModuleWithActionsFromModuleInfo(m, c.nameTracker)
		for _, d := range m.directDeps {
			jm.Deps = append(jm.Deps, jsonDep{
				jsonModuleName: *jsonModuleNameFromModuleInfo(d.module),
				Tag:            fmt.Sprintf("%T %+v", d.tag, d.tag),
			})
			jmWithActions.Deps = append(jmWithActions.Deps, jsonDep{
				jsonModuleName: jsonModuleName{
					Name: d.module.Name(),
				},
			})

		}
		modulesToGraph = append(modulesToGraph, jm)
		modulesToActions = append(modulesToActions, jmWithActions)
	}
	writeJson(wGraph, modulesToGraph)
	writeJson(wActions, modulesToActions)
}

func writeJson(w io.Writer, modules []*JsonModule) {
	e := json.NewEncoder(w)
	e.SetIndent("", "\t")
	e.Encode(modules)
}

// PrepareBuildActions generates an internal representation of all the build
// actions that need to be performed.  This process involves invoking the
// GenerateBuildActions method on each of the Module objects created during the
// parse phase and then on each of the registered Singleton objects.
//
// If the ResolveDependencies method has not already been called it is called
// automatically by this method.
//
// The config argument is made available to all of the Module and Singleton
// objects via the Config method on the ModuleContext and SingletonContext
// objects passed to GenerateBuildActions.  It is also passed to the functions
// specified via PoolFunc, RuleFunc, and VariableFunc so that they can compute
// config-specific values.
//
// The returned deps is a list of the ninja files dependencies that were added
// by the modules and singletons via the ModuleContext.AddNinjaFileDeps(),
// SingletonContext.AddNinjaFileDeps(), and PackageContext.AddNinjaFileDeps()
// methods.

func (c *Context) PrepareBuildActions(config interface{}) (deps []string, errs []error) {
	c.BeginEvent("prepare_build_actions")
	defer c.EndEvent("prepare_build_actions")
	pprof.Do(c.Context, pprof.Labels("blueprint", "PrepareBuildActions"), func(ctx context.Context) {
		c.buildActionsReady = false

		c.liveGlobals = newLiveTracker(c, config)
		// Add all the global rules/variable/pools here because when we restore from
		// cache we don't have the build defs available to build the globals.
		// TODO(b/356414070): Revisit this logic once we have a clearer picture about
		// how the incremental build pieces fit together.
		if c.GetIncrementalEnabled() {
			for _, p := range packageContexts {
				for _, v := range p.scope.variables {
					err := c.liveGlobals.addVariable(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
				for _, v := range p.scope.rules {
					_, err := c.liveGlobals.addRule(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
				for _, v := range p.scope.pools {
					err := c.liveGlobals.addPool(v)
					if err != nil {
						errs = []error{err}
						return
					}
				}
			}
		}

		if !c.dependenciesReady {
			var extraDeps []string
			extraDeps, errs = c.resolveDependencies(ctx, config)
			if len(errs) > 0 {
				return
			}
			deps = append(deps, extraDeps...)
		}

		var depsModules []string
		depsModules, errs = c.generateModuleBuildActions(config, c.liveGlobals)
		if len(errs) > 0 {
			return
		}

		pprof.Do(c.Context, pprof.Labels("blueprint", "GC"), func(ctx context.Context) {
			runtime.GC()
		})

		var depsSingletons []string
		depsSingletons, errs = c.generateSingletonBuildActions(config, c.singletonInfo, c.liveGlobals)
		if len(errs) > 0 {
			return
		}

		deps = append(deps, depsModules...)
		deps = append(deps, depsSingletons...)

		if c.outDir != nil {
			err := c.liveGlobals.addNinjaStringDeps(c.outDir)
			if err != nil {
				errs = []error{err}
				return
			}
		}

		pkgNames, depsPackages := c.makeUniquePackageNames(c.liveGlobals)

		deps = append(deps, depsPackages...)

		nameTracker := c.memoizeFullNames(c.liveGlobals, pkgNames)

		// This will panic if it finds a problem since it's a programming error.
		c.checkForVariableReferenceCycles(c.liveGlobals.variables, nameTracker)

		c.nameTracker = nameTracker
		c.globalVariables = c.liveGlobals.variables
		c.globalPools = c.liveGlobals.pools
		c.globalRules = c.liveGlobals.rules

		c.buildActionsReady = true
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

func (c *Context) runMutators(ctx context.Context, config interface{}) (deps []string, errs []error) {
	pprof.Do(ctx, pprof.Labels("blueprint", "runMutators"), func(ctx context.Context) {
		for _, mutator := range c.mutatorInfo {
			pprof.Do(ctx, pprof.Labels("mutator", mutator.name), func(context.Context) {
				c.BeginEvent(mutator.name)
				defer c.EndEvent(mutator.name)
				var newDeps []string
				if mutator.topDownMutator != nil {
					newDeps, errs = c.runMutator(config, mutator, topDownMutator)
				} else if mutator.bottomUpMutator != nil {
					newDeps, errs = c.runMutator(config, mutator, bottomUpMutator)
				} else {
					panic("no mutator set on " + mutator.name)
				}
				if len(errs) > 0 {
					return
				}
				deps = append(deps, newDeps...)
			})
			if len(errs) > 0 {
				return
			}
		}
	})

	if len(errs) > 0 {
		return nil, errs
	}

	return deps, nil
}

type mutatorDirection interface {
	run(mutator *mutatorInfo, ctx *mutatorContext)
	orderer() visitOrderer
	fmt.Stringer
}

type bottomUpMutatorImpl struct{}

func (bottomUpMutatorImpl) run(mutator *mutatorInfo, ctx *mutatorContext) {
	mutator.bottomUpMutator(ctx)
}

func (bottomUpMutatorImpl) orderer() visitOrderer {
	return bottomUpVisitor
}

func (bottomUpMutatorImpl) String() string {
	return "bottom up mutator"
}

type topDownMutatorImpl struct{}

func (topDownMutatorImpl) run(mutator *mutatorInfo, ctx *mutatorContext) {
	mutator.topDownMutator(ctx)
}

func (topDownMutatorImpl) orderer() visitOrderer {
	return topDownVisitor
}

func (topDownMutatorImpl) String() string {
	return "top down mutator"
}

var (
	topDownMutator  topDownMutatorImpl
	bottomUpMutator bottomUpMutatorImpl
)

type reverseDep struct {
	module *moduleInfo
	dep    depInfo
}

var mutatorContextPool = pool.New[mutatorContext]()

func (c *Context) runMutator(config interface{}, mutator *mutatorInfo,
	direction mutatorDirection) (deps []string, errs []error) {

	newModuleInfo := make(map[Module]*moduleInfo)
	for k, v := range c.moduleInfo {
		newModuleInfo[k] = v
	}

	type globalStateChange struct {
		reverse    []reverseDep
		rename     []rename
		replace    []replace
		newModules []*moduleInfo
		deps       []string
	}

	type newVariationPair struct {
		newVariations   modulesOrAliases
		origLogicModule Module
	}

	reverseDeps := make(map[*moduleInfo][]depInfo)
	var rename []rename
	var replace []replace
	var newModules []*moduleInfo

	errsCh := make(chan []error)
	globalStateCh := make(chan globalStateChange)
	newVariationsCh := make(chan newVariationPair)
	done := make(chan bool)

	c.depsModified = 0

	visit := func(module *moduleInfo, pause chan<- pauseSpec) bool {
		if module.splitModules != nil {
			panic("split module found in sorted module list")
		}

		mctx := mutatorContextPool.Get()
		*mctx = mutatorContext{
			baseModuleContext: baseModuleContext{
				context: c,
				config:  config,
				module:  module,
			},
			mutator: mutator,
			pauseCh: pause,
		}

		origLogicModule := module.logicModule

		module.startedMutator = mutator

		func() {
			defer func() {
				if r := recover(); r != nil {
					in := fmt.Sprintf("%s %q for %s", direction, mutator.name, module)
					if err, ok := r.(panicError); ok {
						err.addIn(in)
						mctx.error(err)
					} else {
						mctx.error(newPanicErrorf(r, in))
					}
				}
			}()
			direction.run(mutator, mctx)
		}()

		module.finishedMutator = mutator

		hasErrors := false
		if len(mctx.errs) > 0 {
			errsCh <- mctx.errs
			hasErrors = true
		} else {
			if len(mctx.newVariations) > 0 {
				newVariationsCh <- newVariationPair{mctx.newVariations, origLogicModule}
			}

			if len(mctx.reverseDeps) > 0 || len(mctx.replace) > 0 || len(mctx.rename) > 0 || len(mctx.newModules) > 0 || len(mctx.ninjaFileDeps) > 0 {
				globalStateCh <- globalStateChange{
					reverse:    mctx.reverseDeps,
					replace:    mctx.replace,
					rename:     mctx.rename,
					newModules: mctx.newModules,
					deps:       mctx.ninjaFileDeps,
				}
			}
		}
		mutatorContextPool.Put(mctx)
		mctx = nil

		return hasErrors
	}

	createdVariations := false
	var obsoleteLogicModules []Module

	// Process errs and reverseDeps in a single goroutine
	go func() {
		for {
			select {
			case newErrs := <-errsCh:
				errs = append(errs, newErrs...)
			case globalStateChange := <-globalStateCh:
				for _, r := range globalStateChange.reverse {
					reverseDeps[r.module] = append(reverseDeps[r.module], r.dep)
				}
				replace = append(replace, globalStateChange.replace...)
				rename = append(rename, globalStateChange.rename...)
				newModules = append(newModules, globalStateChange.newModules...)
				deps = append(deps, globalStateChange.deps...)
			case newVariations := <-newVariationsCh:
				if newVariations.origLogicModule != newVariations.newVariations[0].module().logicModule {
					obsoleteLogicModules = append(obsoleteLogicModules, newVariations.origLogicModule)
				}
				for _, moduleOrAlias := range newVariations.newVariations {
					if m := moduleOrAlias.module(); m != nil {
						newModuleInfo[m.logicModule] = m
					}
				}
				createdVariations = true
			case <-done:
				return
			}
		}
	}()

	c.startedMutator = mutator

	var visitErrs []error
	if mutator.parallel {
		visitErrs = parallelVisit(c.modulesSorted, direction.orderer(), parallelVisitLimit, visit)
	} else {
		direction.orderer().visit(c.modulesSorted, visit)
	}

	if len(visitErrs) > 0 {
		return nil, visitErrs
	}

	c.finishedMutators[mutator] = true

	done <- true

	if len(errs) > 0 {
		return nil, errs
	}

	for _, obsoleteLogicModule := range obsoleteLogicModules {
		delete(newModuleInfo, obsoleteLogicModule)
	}

	c.moduleInfo = newModuleInfo

	isTransitionMutator := mutator.transitionMutator != nil

	var transitionMutatorInputVariants map[*moduleGroup][]*moduleInfo
	if isTransitionMutator {
		transitionMutatorInputVariants = make(map[*moduleGroup][]*moduleInfo)
	}

	for _, group := range c.moduleGroups {
		for i := 0; i < len(group.modules); i++ {
			module := group.modules[i].module()
			if module == nil {
				// Existing alias, skip it
				continue
			}

			// Update module group to contain newly split variants
			if module.splitModules != nil {
				if isTransitionMutator {
					// For transition mutators, save the pre-split variant for reusing later in applyTransitions.
					transitionMutatorInputVariants[group] = append(transitionMutatorInputVariants[group], module)
				}
				group.modules, i = spliceModules(group.modules, i, module.splitModules)
			}

			// Fix up any remaining dependencies on modules that were split into variants
			// by replacing them with the first variant
			for j, dep := range module.directDeps {
				if dep.module.obsoletedByNewVariants {
					module.directDeps[j].module = dep.module.splitModules.firstModule()
				}
			}

			if module.createdBy != nil && module.createdBy.obsoletedByNewVariants {
				module.createdBy = module.createdBy.splitModules.firstModule()
			}

			// Add in any new direct dependencies that were added by the mutator
			module.directDeps = append(module.directDeps, module.newDirectDeps...)
			module.newDirectDeps = nil
		}

		findAliasTarget := func(oldVariant variant) *moduleInfo {
			for _, moduleOrAlias := range group.modules {
				module := moduleOrAlias.moduleOrAliasTarget()
				if module.splitModules != nil {
					// Ignore any old aliases that are pointing to modules that were obsoleted.
					continue
				}
				if alias := moduleOrAlias.alias(); alias != nil {
					if alias.variant.variations.equal(oldVariant.variations) {
						return alias.target
					}
				}
				if module.variant.variations.equal(oldVariant.variations) {
					return module
				}
			}
			return nil
		}

		// Forward or delete any dangling aliases.
		// Use a manual loop instead of range because len(group.modules) can
		// change inside the loop
		for i := 0; i < len(group.modules); i++ {
			if alias := group.modules[i].alias(); alias != nil {
				if alias.target.obsoletedByNewVariants {
					newTarget := findAliasTarget(alias.target.variant)
					if newTarget != nil {
						alias.target = newTarget
					} else {
						// The alias was left dangling, remove it.
						group.modules = append(group.modules[:i], group.modules[i+1:]...)
						i--
					}
				}
			}
		}
	}

	if isTransitionMutator {
		mutator.transitionMutator.inputVariants = transitionMutatorInputVariants
		mutator.transitionMutator.variantCreatingMutatorIndex = len(c.variantCreatingMutatorOrder)
		c.transitionMutators = append(c.transitionMutators, mutator.transitionMutator)
	}

	if createdVariations {
		c.variantCreatingMutatorOrder = append(c.variantCreatingMutatorOrder, mutator.name)
	}

	// Add in any new reverse dependencies that were added by the mutator
	for module, deps := range reverseDeps {
		sort.Sort(depSorter(deps))
		module.directDeps = append(module.directDeps, deps...)
		c.depsModified++
	}

	for _, module := range newModules {
		errs = c.addModule(module)
		if len(errs) > 0 {
			return nil, errs
		}
		atomic.AddUint32(&c.depsModified, 1)
	}

	errs = c.handleRenames(rename)
	if len(errs) > 0 {
		return nil, errs
	}

	errs = c.handleReplacements(replace)
	if len(errs) > 0 {
		return nil, errs
	}

	if c.depsModified > 0 {
		errs = c.updateDependencies()
		if len(errs) > 0 {
			return nil, errs
		}
	}

	return deps, errs
}

// clearTransitionMutatorInputVariants removes the inputVariants field from every
// TransitionMutator now that all dependencies have been resolved.
func (c *Context) clearTransitionMutatorInputVariants() {
	for _, mutator := range c.transitionMutators {
		mutator.inputVariants = nil
	}
}

// Replaces every build logic module with a clone of itself.  Prevents introducing problems where
// a mutator sets a non-property member variable on a module, which works until a later mutator
// creates variants of that module.
func (c *Context) cloneModules() {
	type update struct {
		orig  Module
		clone *moduleInfo
	}
	ch := make(chan update)
	doneCh := make(chan bool)
	go func() {
		errs := parallelVisit(c.modulesSorted, unorderedVisitorImpl{}, parallelVisitLimit,
			func(m *moduleInfo, pause chan<- pauseSpec) bool {
				origLogicModule := m.logicModule
				m.logicModule, m.properties = c.cloneLogicModule(m)
				ch <- update{origLogicModule, m}
				return false
			})
		if len(errs) > 0 {
			panic(errs)
		}
		doneCh <- true
	}()

	done := false
	for !done {
		select {
		case <-doneCh:
			done = true
		case update := <-ch:
			delete(c.moduleInfo, update.orig)
			c.moduleInfo[update.clone.logicModule] = update.clone
		}
	}
}

// Removes modules[i] from the list and inserts newModules... where it was located, returning
// the new slice and the index of the last inserted element
func spliceModules(modules modulesOrAliases, i int, newModules modulesOrAliases) (modulesOrAliases, int) {
	spliceSize := len(newModules)
	newLen := len(modules) + spliceSize - 1
	var dest modulesOrAliases
	if cap(modules) >= len(modules)-1+len(newModules) {
		// We can fit the splice in the existing capacity, do everything in place
		dest = modules[:newLen]
	} else {
		dest = make(modulesOrAliases, newLen)
		copy(dest, modules[:i])
	}

	// Move the end of the slice over by spliceSize-1
	copy(dest[i+spliceSize:], modules[i+1:])

	// Copy the new modules into the slice
	copy(dest[i:], newModules)

	return dest, i + spliceSize - 1
}

func (c *Context) generateModuleBuildActions(config interface{},
	liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateModuleBuildActions")
	defer c.EndEvent("generateModuleBuildActions")
	var deps []string
	var errs []error

	cancelCh := make(chan struct{})
	errsCh := make(chan []error)
	depsCh := make(chan []string)

	go func() {
		for {
			select {
			case <-cancelCh:
				close(cancelCh)
				return
			case newErrs := <-errsCh:
				errs = append(errs, newErrs...)
			case newDeps := <-depsCh:
				deps = append(deps, newDeps...)

			}
		}
	}()

	visitErrs := parallelVisit(c.modulesSorted, bottomUpVisitor, parallelVisitLimit,
		func(module *moduleInfo, pause chan<- pauseSpec) bool {
			uniqueName := c.nameInterface.UniqueName(newNamespaceContext(module), module.group.name)
			sanitizedName := toNinjaName(uniqueName)
			sanitizedVariant := toNinjaName(module.variant.name)

			prefix := moduleNamespacePrefix(sanitizedName + "_" + sanitizedVariant)

			// The parent scope of the moduleContext's local scope gets overridden to be that of the
			// calling Go package on a per-call basis.  Since the initial parent scope doesn't matter we
			// just set it to nil.
			scope := newLocalScope(nil, prefix)

			mctx := &moduleContext{
				baseModuleContext: baseModuleContext{
					context: c,
					config:  config,
					module:  module,
				},
				scope:              scope,
				handledMissingDeps: module.missingDeps == nil,
			}

			mctx.module.startedGenerateBuildActions = true

			func() {
				defer func() {
					if r := recover(); r != nil {
						in := fmt.Sprintf("GenerateBuildActions for %s", module)
						if err, ok := r.(panicError); ok {
							err.addIn(in)
							mctx.error(err)
						} else {
							mctx.error(newPanicErrorf(r, in))
						}
					}
				}()
				restored, cacheKey := mctx.restoreModuleBuildActions()
				if !restored {
					mctx.module.logicModule.GenerateBuildActions(mctx)
				}
				if cacheKey != nil {
					mctx.cacheModuleBuildActions(cacheKey)
				}
			}()

			mctx.module.finishedGenerateBuildActions = true

			if len(mctx.errs) > 0 {
				errsCh <- mctx.errs
				return true
			}

			if module.missingDeps != nil && !mctx.handledMissingDeps {
				var errs []error
				for _, depName := range module.missingDeps {
					errs = append(errs, c.missingDependencyError(module, depName))
				}
				errsCh <- errs
				return true
			}

			depsCh <- mctx.ninjaFileDeps

			newErrs := c.processLocalBuildActions(&module.actionDefs,
				&mctx.actionDefs, liveGlobals)
			if len(newErrs) > 0 {
				errsCh <- newErrs
				return true
			}
			return false
		})

	cancelCh <- struct{}{}
	<-cancelCh

	errs = append(errs, visitErrs...)

	return deps, errs
}

func (c *Context) generateOneSingletonBuildActions(config interface{},
	info *singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	var deps []string
	var errs []error

	// The parent scope of the singletonContext's local scope gets overridden to be that of the
	// calling Go package on a per-call basis.  Since the initial parent scope doesn't matter we
	// just set it to nil.
	scope := newLocalScope(nil, singletonNamespacePrefix(info.name))

	sctx := &singletonContext{
		name:    info.name,
		context: c,
		config:  config,
		scope:   scope,
		globals: liveGlobals,
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				in := fmt.Sprintf("GenerateBuildActions for singleton %s", info.name)
				if err, ok := r.(panicError); ok {
					err.addIn(in)
					sctx.error(err)
				} else {
					sctx.error(newPanicErrorf(r, in))
				}
			}
		}()
		info.singleton.GenerateBuildActions(sctx)
	}()

	if len(sctx.errs) > 0 {
		errs = append(errs, sctx.errs...)
		return deps, errs
	}

	deps = append(deps, sctx.ninjaFileDeps...)

	newErrs := c.processLocalBuildActions(&info.actionDefs,
		&sctx.actionDefs, liveGlobals)
	errs = append(errs, newErrs...)
	return deps, errs
}

func (c *Context) generateParallelSingletonBuildActions(config interface{},
	singletons []*singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateParallelSingletonBuildActions")
	defer c.EndEvent("generateParallelSingletonBuildActions")

	var deps []string
	var errs []error

	wg := sync.WaitGroup{}
	cancelCh := make(chan struct{})
	depsCh := make(chan []string)
	errsCh := make(chan []error)

	go func() {
		for {
			select {
			case <-cancelCh:
				close(cancelCh)
				return
			case dep := <-depsCh:
				deps = append(deps, dep...)
			case newErrs := <-errsCh:
				if len(errs) <= maxErrors {
					errs = append(errs, newErrs...)
				}
			}
		}
	}()

	for _, info := range singletons {
		if !info.parallel {
			// Skip any singletons registered with parallel=false.
			continue
		}
		wg.Add(1)
		go func(inf *singletonInfo) {
			defer wg.Done()
			newDeps, newErrs := c.generateOneSingletonBuildActions(config, inf, liveGlobals)
			depsCh <- newDeps
			errsCh <- newErrs
		}(info)
	}
	wg.Wait()

	cancelCh <- struct{}{}
	<-cancelCh

	return deps, errs
}

func (c *Context) generateSingletonBuildActions(config interface{},
	singletons []*singletonInfo, liveGlobals *liveTracker) ([]string, []error) {

	c.BeginEvent("generateSingletonBuildActions")
	defer c.EndEvent("generateSingletonBuildActions")

	var deps []string
	var errs []error

	// Run one singleton.  Use a variable to simplify manual validation testing.
	var runSingleton = func(info *singletonInfo) {
		c.BeginEvent("singleton:" + info.name)
		defer c.EndEvent("singleton:" + info.name)
		newDeps, newErrs := c.generateOneSingletonBuildActions(config, info, liveGlobals)
		deps = append(deps, newDeps...)
		errs = append(errs, newErrs...)
	}

	// Force a resort of the module groups before running singletons so that two singletons running in parallel
	// don't cause a data race when they trigger a resort in VisitAllModules.
	c.sortedModuleGroups()

	// First, take care of any singletons that want to run in parallel.
	deps, errs = c.generateParallelSingletonBuildActions(config, singletons, liveGlobals)

	for _, info := range singletons {
		if !info.parallel {
			runSingleton(info)
			if len(errs) > maxErrors {
				break
			}
		}
	}

	return deps, errs
}

func (c *Context) processLocalBuildActions(out, in *localBuildActions,
	liveGlobals *liveTracker) []error {

	var errs []error

	// First we go through and add everything referenced by the module's
	// buildDefs to the live globals set.  This will end up adding the live
	// locals to the set as well, but we'll take them out after.
	for _, def := range in.buildDefs {
		err := liveGlobals.AddBuildDefDeps(def)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errs
	}

	out.buildDefs = append(out.buildDefs, in.buildDefs...)

	// We use the now-incorrect set of live "globals" to determine which local
	// definitions are live.  As we go through copying those live locals to the
	// moduleGroup we remove them from the live globals set.
	for _, v := range in.variables {
		isLive := liveGlobals.RemoveVariableIfLive(v)
		if isLive {
			out.variables = append(out.variables, v)
		}
	}

	for _, r := range in.rules {
		isLive := liveGlobals.RemoveRuleIfLive(r)
		if isLive {
			out.rules = append(out.rules, r)
		}
	}

	return nil
}

func (c *Context) walkDeps(topModule *moduleInfo, allowDuplicates bool,
	visitDown func(depInfo, *moduleInfo) bool, visitUp func(depInfo, *moduleInfo)) {

	visited := make(map[*moduleInfo]bool)
	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "WalkDeps(%s, %s, %s) for dependency %s",
				topModule, funcName(visitDown), funcName(visitUp), visiting))
		}
	}()

	var walk func(module *moduleInfo)
	walk = func(module *moduleInfo) {
		for _, dep := range module.directDeps {
			if allowDuplicates || !visited[dep.module] {
				visiting = dep.module
				recurse := true
				if visitDown != nil {
					recurse = visitDown(dep, module)
				}
				if recurse && !visited[dep.module] {
					walk(dep.module)
					visited[dep.module] = true
				}
				if visitUp != nil {
					visitUp(dep, module)
				}
			}
		}
	}

	walk(topModule)
}

type replace struct {
	from, to  *moduleInfo
	predicate ReplaceDependencyPredicate
}

type rename struct {
	group *moduleGroup
	name  string
}

// moduleVariantsThatDependOn takes the name of a module and a dependency and returns the all the variants of the
// module that depends on the dependency.
func (c *Context) moduleVariantsThatDependOn(name string, dep *moduleInfo) []*moduleInfo {
	group := c.moduleGroupFromName(name, dep.namespace())
	var variants []*moduleInfo

	if group == nil {
		return nil
	}

	for _, module := range group.modules {
		m := module.module()
		if m == nil {
			continue
		}
		for _, moduleDep := range m.directDeps {
			if moduleDep.module == dep {
				variants = append(variants, m)
			}
		}
	}

	return variants
}

func (c *Context) handleRenames(renames []rename) []error {
	var errs []error
	for _, rename := range renames {
		group, name := rename.group, rename.name
		if name == group.name || len(group.modules) < 1 {
			continue
		}

		errs = append(errs, c.nameInterface.Rename(group.name, rename.name, group.namespace)...)
	}

	return errs
}

func (c *Context) handleReplacements(replacements []replace) []error {
	var errs []error
	changedDeps := false
	for _, replace := range replacements {
		for _, m := range replace.from.reverseDeps {
			for i, d := range m.directDeps {
				if d.module == replace.from {
					// If the replacement has a predicate then check it.
					if replace.predicate == nil || replace.predicate(m.logicModule, d.tag, d.module.logicModule) {
						m.directDeps[i].module = replace.to
						changedDeps = true
					}
				}
			}
		}

	}

	if changedDeps {
		atomic.AddUint32(&c.depsModified, 1)
	}
	return errs
}

func (c *Context) discoveredMissingDependencies(module *moduleInfo, depName string, depVariations variationMap) (errs []error) {
	if !depVariations.empty() {
		depName = depName + "{" + c.prettyPrintVariant(depVariations) + "}"
	}
	if c.allowMissingDependencies {
		module.missingDeps = append(module.missingDeps, depName)
		return nil
	}
	return []error{c.missingDependencyError(module, depName)}
}

func (c *Context) missingDependencyError(module *moduleInfo, depName string) (errs error) {
	guess := namesLike(depName, module.Name(), c.moduleGroups)
	err := c.nameInterface.MissingDependencyError(module.Name(), module.namespace(), depName, guess)
	return &BlueprintError{
		Err: err,
		Pos: module.pos,
	}
}

func (c *Context) moduleGroupFromName(name string, namespace Namespace) *moduleGroup {
	group, exists := c.nameInterface.ModuleFromName(name, namespace)
	if exists {
		return group.moduleGroup
	}
	return nil
}

func (c *Context) sortedModuleGroups() []*moduleGroup {
	if c.cachedSortedModuleGroups == nil || c.cachedDepsModified {
		unwrap := func(wrappers []ModuleGroup) []*moduleGroup {
			result := make([]*moduleGroup, 0, len(wrappers))
			for _, group := range wrappers {
				result = append(result, group.moduleGroup)
			}
			return result
		}

		c.cachedSortedModuleGroups = unwrap(c.nameInterface.AllModules())
		c.cachedDepsModified = false
	}

	return c.cachedSortedModuleGroups
}

func (c *Context) visitAllModules(visit func(Module)) {
	var module *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModules(%s) for %s",
				funcName(visit), module))
		}
	}()

	for _, moduleGroup := range c.sortedModuleGroups() {
		for _, moduleOrAlias := range moduleGroup.modules {
			if module = moduleOrAlias.module(); module != nil {
				visit(module.logicModule)
			}
		}
	}
}

func (c *Context) visitAllModulesIf(pred func(Module) bool,
	visit func(Module)) {

	var module *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModulesIf(%s, %s) for %s",
				funcName(pred), funcName(visit), module))
		}
	}()

	for _, moduleGroup := range c.sortedModuleGroups() {
		for _, moduleOrAlias := range moduleGroup.modules {
			if module = moduleOrAlias.module(); module != nil {
				if pred(module.logicModule) {
					visit(module.logicModule)
				}
			}
		}
	}
}

func (c *Context) visitAllModuleVariants(module *moduleInfo,
	visit func(Module)) {

	var variant *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModuleVariants(%s, %s) for %s",
				module, funcName(visit), variant))
		}
	}()

	for _, moduleOrAlias := range module.group.modules {
		if variant = moduleOrAlias.module(); variant != nil {
			visit(variant.logicModule)
		}
	}
}

func (c *Context) visitAllModuleInfos(visit func(*moduleInfo)) {
	var module *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitAllModules(%s) for %s",
				funcName(visit), module))
		}
	}()

	for _, moduleGroup := range c.sortedModuleGroups() {
		for _, moduleOrAlias := range moduleGroup.modules {
			if module = moduleOrAlias.module(); module != nil {
				visit(module)
			}
		}
	}
}

func (c *Context) requireNinjaVersion(major, minor, micro int) {
	if major != 1 {
		panic("ninja version with major version != 1 not supported")
	}
	if c.requiredNinjaMinor < minor {
		c.requiredNinjaMinor = minor
		c.requiredNinjaMicro = micro
	}
	if c.requiredNinjaMinor == minor && c.requiredNinjaMicro < micro {
		c.requiredNinjaMicro = micro
	}
}

func (c *Context) setOutDir(value *ninjaString) {
	if c.outDir == nil {
		c.outDir = value
	}
}

func (c *Context) makeUniquePackageNames(
	liveGlobals *liveTracker) (map[*packageContext]string, []string) {

	pkgs := make(map[string]*packageContext)
	pkgNames := make(map[*packageContext]string)
	longPkgNames := make(map[*packageContext]bool)

	processPackage := func(pctx *packageContext) {
		if pctx == nil {
			// This is a built-in rule and has no package.
			return
		}
		if _, ok := pkgNames[pctx]; ok {
			// We've already processed this package.
			return
		}

		otherPkg, present := pkgs[pctx.shortName]
		if present {
			// Short name collision.  Both this package and the one that's
			// already there need to use their full names.  We leave the short
			// name in pkgNames for now so future collisions still get caught.
			longPkgNames[pctx] = true
			longPkgNames[otherPkg] = true
		} else {
			// No collision so far.  Tentatively set the package's name to be
			// its short name.
			pkgNames[pctx] = pctx.shortName
			pkgs[pctx.shortName] = pctx
		}
	}

	// We try to give all packages their short name, but when we get collisions
	// we need to use the full unique package name.
	for v, _ := range liveGlobals.variables {
		processPackage(v.packageContext())
	}
	for p, _ := range liveGlobals.pools {
		processPackage(p.packageContext())
	}
	for r, _ := range liveGlobals.rules {
		processPackage(r.packageContext())
	}

	// Add the packages that had collisions using their full unique names.  This
	// will overwrite any short names that were added in the previous step.
	for pctx := range longPkgNames {
		pkgNames[pctx] = pctx.fullName
	}

	// Create deps list from calls to PackageContext.AddNinjaFileDeps
	deps := []string{}
	for _, pkg := range pkgs {
		deps = append(deps, pkg.ninjaFileDeps...)
	}

	return pkgNames, deps
}

// memoizeFullNames stores the full name of each live global variable, rule and pool since each is
// guaranteed to be used at least twice, once in the definition and once for each usage, and many
// are used much more than once.
func (c *Context) memoizeFullNames(liveGlobals *liveTracker, pkgNames map[*packageContext]string) *nameTracker {
	nameTracker := &nameTracker{
		pkgNames:  pkgNames,
		variables: make(map[Variable]string),
		rules:     make(map[Rule]string),
		pools:     make(map[Pool]string),
	}
	for v := range liveGlobals.variables {
		nameTracker.variables[v] = v.fullName(pkgNames)
	}
	for r := range liveGlobals.rules {
		nameTracker.rules[r] = r.fullName(pkgNames)
	}
	for p := range liveGlobals.pools {
		nameTracker.pools[p] = p.fullName(pkgNames)
	}
	return nameTracker
}

func (c *Context) checkForVariableReferenceCycles(
	variables map[Variable]*ninjaString, nameTracker *nameTracker) {

	visited := make(map[Variable]bool)  // variables that were already checked
	checking := make(map[Variable]bool) // variables actively being checked

	var check func(v Variable) []Variable

	check = func(v Variable) []Variable {
		visited[v] = true
		checking[v] = true
		defer delete(checking, v)

		value := variables[v]
		for _, dep := range value.Variables() {
			if checking[dep] {
				// This is a cycle.
				return []Variable{dep, v}
			}

			if !visited[dep] {
				cycle := check(dep)
				if cycle != nil {
					if cycle[0] == v {
						// We are the "start" of the cycle, so we're responsible
						// for generating the errors.  The cycle list is in
						// reverse order because all the 'check' calls append
						// their own module to the list.
						msgs := []string{"detected variable reference cycle:"}

						// Iterate backwards through the cycle list.
						curName := nameTracker.Variable(v)
						curValue := value.Value(nameTracker)
						for i := len(cycle) - 1; i >= 0; i-- {
							next := cycle[i]
							nextName := nameTracker.Variable(next)
							nextValue := variables[next].Value(nameTracker)

							msgs = append(msgs, fmt.Sprintf(
								"    %q depends on %q", curName, nextName))
							msgs = append(msgs, fmt.Sprintf(
								"    [%s = %s]", curName, curValue))

							curName = nextName
							curValue = nextValue
						}

						// Variable reference cycles are a programming error,
						// not the fault of the Blueprint file authors.
						panic(strings.Join(msgs, "\n"))
					} else {
						// We're not the "start" of the cycle, so we just append
						// our module to the list and return it.
						return append(cycle, v)
					}
				}
			}
		}

		return nil
	}

	for v := range variables {
		if !visited[v] {
			cycle := check(v)
			if cycle != nil {
				panic("inconceivable!")
			}
		}
	}
}

// ModuleTypePropertyStructs returns a mapping from module type name to a list of pointers to
// property structs returned by the factory for that module type.
func (c *Context) ModuleTypePropertyStructs() map[string][]interface{} {
	ret := make(map[string][]interface{}, len(c.moduleFactories))
	for moduleType, factory := range c.moduleFactories {
		_, ret[moduleType] = factory()
	}

	return ret
}

func (c *Context) ModuleTypeFactories() map[string]ModuleFactory {
	ret := make(map[string]ModuleFactory)
	for k, v := range c.moduleFactories {
		ret[k] = v
	}
	return ret
}

func (c *Context) ModuleName(logicModule Module) string {
	module := c.moduleInfo[logicModule]
	return module.Name()
}

func (c *Context) ModuleDir(logicModule Module) string {
	return filepath.Dir(c.BlueprintFile(logicModule))
}

func (c *Context) ModuleSubDir(logicModule Module) string {
	module := c.moduleInfo[logicModule]
	return module.variant.name
}

func (c *Context) ModuleType(logicModule Module) string {
	module := c.moduleInfo[logicModule]
	return module.typeName
}

// ModuleProvider returns the value, if any, for the provider for a module.  If the value for the
// provider was not set it returns nil and false.  The return value should always be considered read-only.
// It panics if called before the appropriate mutator or GenerateBuildActions pass for the provider on the
// module.  The value returned may be a deep copy of the value originally passed to SetProvider.
func (c *Context) ModuleProvider(logicModule Module, provider AnyProviderKey) (any, bool) {
	module := c.moduleInfo[logicModule]
	return c.provider(module, provider.provider())
}

func (c *Context) BlueprintFile(logicModule Module) string {
	module := c.moduleInfo[logicModule]
	return module.relBlueprintsFile
}

func (c *Context) ModuleErrorf(logicModule Module, format string,
	args ...interface{}) error {

	module := c.moduleInfo[logicModule]
	if module == nil {
		// This can happen if ModuleErrorf is called from a load hook
		return &BlueprintError{
			Err: fmt.Errorf(format, args...),
		}
	}

	return &ModuleError{
		BlueprintError: BlueprintError{
			Err: fmt.Errorf(format, args...),
			Pos: module.pos,
		},
		module: module,
	}
}

func (c *Context) PropertyErrorf(logicModule Module, property string, format string,
	args ...interface{}) error {

	module := c.moduleInfo[logicModule]
	if module == nil {
		// This can happen if PropertyErrorf is called from a load hook
		return &BlueprintError{
			Err: fmt.Errorf(format, args...),
		}
	}

	pos := module.propertyPos[property]
	if !pos.IsValid() {
		pos = module.pos
	}

	return &PropertyError{
		ModuleError: ModuleError{
			BlueprintError: BlueprintError{
				Err: fmt.Errorf(format, args...),
				Pos: pos,
			},
			module: module,
		},
		property: property,
	}
}

func (c *Context) VisitAllModules(visit func(Module)) {
	c.visitAllModules(visit)
}

func (c *Context) VisitAllModulesIf(pred func(Module) bool,
	visit func(Module)) {

	c.visitAllModulesIf(pred, visit)
}

func (c *Context) VisitDirectDeps(module Module, visit func(Module)) {
	c.VisitDirectDepsWithTags(module, func(m Module, _ DependencyTag) {
		visit(m)
	})
}

func (c *Context) VisitDirectDepsWithTags(module Module, visit func(Module, DependencyTag)) {
	topModule := c.moduleInfo[module]

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDeps(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		visit(dep.module.logicModule, dep.tag)
	}
}

func (c *Context) VisitDirectDepsIf(module Module, pred func(Module) bool, visit func(Module)) {
	topModule := c.moduleInfo[module]

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDirectDepsIf(%s, %s, %s) for dependency %s",
				topModule, funcName(pred), funcName(visit), visiting))
		}
	}()

	for _, dep := range topModule.directDeps {
		visiting = dep.module
		if pred(dep.module.logicModule) {
			visit(dep.module.logicModule)
		}
	}
}

func (c *Context) VisitDepsDepthFirst(module Module, visit func(Module)) {
	topModule := c.moduleInfo[module]

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirst(%s, %s) for dependency %s",
				topModule, funcName(visit), visiting))
		}
	}()

	c.walkDeps(topModule, false, nil, func(dep depInfo, parent *moduleInfo) {
		visiting = dep.module
		visit(dep.module.logicModule)
	})
}

func (c *Context) VisitDepsDepthFirstIf(module Module, pred func(Module) bool, visit func(Module)) {
	topModule := c.moduleInfo[module]

	var visiting *moduleInfo

	defer func() {
		if r := recover(); r != nil {
			panic(newPanicErrorf(r, "VisitDepsDepthFirstIf(%s, %s, %s) for dependency %s",
				topModule, funcName(pred), funcName(visit), visiting))
		}
	}()

	c.walkDeps(topModule, false, nil, func(dep depInfo, parent *moduleInfo) {
		if pred(dep.module.logicModule) {
			visiting = dep.module
			visit(dep.module.logicModule)
		}
	})
}

func (c *Context) PrimaryModule(module Module) Module {
	return c.moduleInfo[module].group.modules.firstModule().logicModule
}

func (c *Context) FinalModule(module Module) Module {
	return c.moduleInfo[module].group.modules.lastModule().logicModule
}

func (c *Context) VisitAllModuleVariants(module Module,
	visit func(Module)) {

	c.visitAllModuleVariants(c.moduleInfo[module], visit)
}

// Singletons returns a list of all registered Singletons.
func (c *Context) Singletons() []Singleton {
	var ret []Singleton
	for _, s := range c.singletonInfo {
		ret = append(ret, s.singleton)
	}
	return ret
}

// SingletonName returns the name that the given singleton was registered with.
func (c *Context) SingletonName(singleton Singleton) string {
	for _, s := range c.singletonInfo {
		if s.singleton == singleton {
			return s.name
		}
	}
	return ""
}

// Checks that the hashes of all the providers match the hashes from when they were first set.
// Does nothing on success, returns a list of errors otherwise. It's recommended to run this
// in a goroutine.
func (c *Context) VerifyProvidersWereUnchanged() []error {
	if !c.buildActionsReady {
		return []error{ErrBuildActionsNotReady}
	}
	toProcess := make(chan *moduleInfo)
	errorCh := make(chan []error)
	var wg sync.WaitGroup
	go func() {
		for _, m := range c.modulesSorted {
			toProcess <- m
		}
		close(toProcess)
	}()
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			var errors []error
			for m := range toProcess {
				for i, provider := range m.providers {
					if provider != nil {
						hash, err := proptools.CalculateHash(provider)
						if err != nil {
							errors = append(errors, fmt.Errorf("provider %q on module %q was modified after being set, and no longer hashable afterwards: %s", providerRegistry[i].typ, m.Name(), err.Error()))
							continue
						}
						if m.providerInitialValueHashes[i] != hash {
							errors = append(errors, fmt.Errorf("provider %q on module %q was modified after being set", providerRegistry[i].typ, m.Name()))
						}
					} else if m.providerInitialValueHashes[i] != 0 {
						// This should be unreachable, because in setProvider we check if the provider has already been set.
						errors = append(errors, fmt.Errorf("provider %q on module %q was unset somehow, this is an internal error", providerRegistry[i].typ, m.Name()))
					}
				}
			}
			if errors != nil {
				errorCh <- errors
			}
			wg.Done()
		}()
	}
	go func() {
		wg.Wait()
		close(errorCh)
	}()

	var errors []error
	for newErrors := range errorCh {
		errors = append(errors, newErrors...)
	}
	return errors
}

// WriteBuildFile writes the Ninja manifest text for the generated build
// actions to w.  If this is called before PrepareBuildActions successfully
// completes then ErrBuildActionsNotReady is returned.
func (c *Context) WriteBuildFile(w StringWriterWriter, shardNinja bool, ninjaFileName string) error {
	var err error
	pprof.Do(c.Context, pprof.Labels("blueprint", "WriteBuildFile"), func(ctx context.Context) {
		if !c.buildActionsReady {
			err = ErrBuildActionsNotReady
			return
		}

		nw := newNinjaWriter(w)

		if err = c.writeBuildFileHeader(nw); err != nil {
			return
		}

		if err = c.writeNinjaRequiredVersion(nw); err != nil {
			return
		}

		if err = c.writeSubninjas(nw); err != nil {
			return
		}

		// TODO: Group the globals by package.

		if err = c.writeGlobalVariables(nw); err != nil {
			return
		}

		if err = c.writeGlobalPools(nw); err != nil {
			return
		}

		if err = c.writeBuildDir(nw); err != nil {
			return
		}

		if err = c.writeGlobalRules(nw); err != nil {
			return
		}

		if err = c.writeAllModuleActions(nw, shardNinja, ninjaFileName); err != nil {
			return
		}

		if err = c.writeAllSingletonActions(nw); err != nil {
			return
		}
	})

	return err
}

type pkgAssociation struct {
	PkgName string
	PkgPath string
}

type pkgAssociationSorter struct {
	pkgs []pkgAssociation
}

func (s *pkgAssociationSorter) Len() int {
	return len(s.pkgs)
}

func (s *pkgAssociationSorter) Less(i, j int) bool {
	iName := s.pkgs[i].PkgName
	jName := s.pkgs[j].PkgName
	return iName < jName
}

func (s *pkgAssociationSorter) Swap(i, j int) {
	s.pkgs[i], s.pkgs[j] = s.pkgs[j], s.pkgs[i]
}

func (c *Context) writeBuildFileHeader(nw *ninjaWriter) error {
	headerTemplate := template.New("fileHeader")
	_, err := headerTemplate.Parse(fileHeaderTemplate)
	if err != nil {
		// This is a programming error.
		panic(err)
	}

	var pkgs []pkgAssociation
	maxNameLen := 0
	for pkg, name := range c.nameTracker.pkgNames {
		pkgs = append(pkgs, pkgAssociation{
			PkgName: name,
			PkgPath: pkg.pkgPath,
		})
		if len(name) > maxNameLen {
			maxNameLen = len(name)
		}
	}

	for i := range pkgs {
		pkgs[i].PkgName += strings.Repeat(" ", maxNameLen-len(pkgs[i].PkgName))
	}

	sort.Sort(&pkgAssociationSorter{pkgs})

	params := map[string]interface{}{
		"Pkgs": pkgs,
	}

	buf := bytes.NewBuffer(nil)
	err = headerTemplate.Execute(buf, params)
	if err != nil {
		return err
	}

	return nw.Comment(buf.String())
}

func (c *Context) writeNinjaRequiredVersion(nw *ninjaWriter) error {
	value := fmt.Sprintf("%d.%d.%d", c.requiredNinjaMajor, c.requiredNinjaMinor,
		c.requiredNinjaMicro)

	err := nw.Assign("ninja_required_version", value)
	if err != nil {
		return err
	}

	return nw.BlankLine()
}

func (c *Context) writeSubninjas(nw *ninjaWriter) error {
	for _, subninja := range c.subninjas {
		err := nw.Subninja(subninja)
		if err != nil {
			return err
		}
	}
	return nw.BlankLine()
}

func (c *Context) writeBuildDir(nw *ninjaWriter) error {
	if c.outDir != nil {
		err := nw.Assign("builddir", c.outDir.Value(c.nameTracker))
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Context) writeGlobalVariables(nw *ninjaWriter) error {
	visited := make(map[Variable]bool)

	var walk func(v Variable) error
	walk = func(v Variable) error {
		visited[v] = true

		// First visit variables on which this variable depends.
		value := c.globalVariables[v]
		for _, dep := range value.Variables() {
			if !visited[dep] {
				err := walk(dep)
				if err != nil {
					return err
				}
			}
		}

		err := nw.Assign(c.nameTracker.Variable(v), value.Value(c.nameTracker))
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}

		return nil
	}

	globalVariables := make([]Variable, 0, len(c.globalVariables))
	for variable := range c.globalVariables {
		globalVariables = append(globalVariables, variable)
	}

	slices.SortFunc(globalVariables, func(a, b Variable) int {
		return cmp.Compare(c.nameTracker.Variable(a), c.nameTracker.Variable(b))
	})

	for _, v := range globalVariables {
		if !visited[v] {
			err := walk(v)
			if err != nil {
				return nil
			}
		}
	}

	return nil
}

func (c *Context) writeGlobalPools(nw *ninjaWriter) error {
	globalPools := make([]Pool, 0, len(c.globalPools))
	for pool := range c.globalPools {
		globalPools = append(globalPools, pool)
	}

	slices.SortFunc(globalPools, func(a, b Pool) int {
		return cmp.Compare(c.nameTracker.Pool(a), c.nameTracker.Pool(b))
	})

	for _, pool := range globalPools {
		name := c.nameTracker.Pool(pool)
		def := c.globalPools[pool]
		err := def.WriteTo(nw, name)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Context) writeGlobalRules(nw *ninjaWriter) error {
	globalRules := make([]Rule, 0, len(c.globalRules))
	for rule := range c.globalRules {
		globalRules = append(globalRules, rule)
	}

	slices.SortFunc(globalRules, func(a, b Rule) int {
		return cmp.Compare(c.nameTracker.Rule(a), c.nameTracker.Rule(b))
	})

	for _, rule := range globalRules {
		name := c.nameTracker.Rule(rule)
		def := c.globalRules[rule]
		err := def.WriteTo(nw, name, c.nameTracker)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	return nil
}

type depSorter []depInfo

func (s depSorter) Len() int {
	return len(s)
}

func (s depSorter) Less(i, j int) bool {
	iName := s[i].module.Name()
	jName := s[j].module.Name()
	if iName == jName {
		iName = s[i].module.variant.name
		jName = s[j].module.variant.name
	}
	return iName < jName
}

func (s depSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type moduleSorter struct {
	modules       []*moduleInfo
	nameInterface NameInterface
}

func (s moduleSorter) Len() int {
	return len(s.modules)
}

func (s moduleSorter) Less(i, j int) bool {
	iMod := s.modules[i]
	jMod := s.modules[j]
	iName := s.nameInterface.UniqueName(newNamespaceContext(iMod), iMod.group.name)
	jName := s.nameInterface.UniqueName(newNamespaceContext(jMod), jMod.group.name)
	if iName == jName {
		iVariantName := s.modules[i].variant.name
		jVariantName := s.modules[j].variant.name
		if iVariantName == jVariantName {
			panic(fmt.Sprintf("duplicate module name: %s %s: %#v and %#v\n",
				iName, iVariantName, iMod.variant.variations, jMod.variant.variations))
		} else {
			return iVariantName < jVariantName
		}
	} else {
		return iName < jName
	}
}

func (s moduleSorter) Swap(i, j int) {
	s.modules[i], s.modules[j] = s.modules[j], s.modules[i]
}

func GetNinjaShardFiles(ninjaFile string) []string {
	suffix := ".ninja"
	if !strings.HasSuffix(ninjaFile, suffix) {
		panic(fmt.Errorf("ninja file name in wrong format : %s", ninjaFile))
	}
	base := strings.TrimSuffix(ninjaFile, suffix)
	ninjaShardCnt := 10
	fileNames := make([]string, ninjaShardCnt)

	for i := 0; i < ninjaShardCnt; i++ {
		fileNames[i] = fmt.Sprintf("%s.%d%s", base, i, suffix)
	}
	return fileNames
}

func (c *Context) writeAllModuleActions(nw *ninjaWriter, shardNinja bool, ninjaFileName string) error {
	c.BeginEvent("modules")
	defer c.EndEvent("modules")

	modules := make([]*moduleInfo, 0, len(c.moduleInfo))
	incrementalModules := make([]*moduleInfo, 0, 200)

	for _, module := range c.moduleInfo {
		if module.buildActionCacheKey != nil {
			incrementalModules = append(incrementalModules, module)
			continue
		}
		modules = append(modules, module)
	}
	sort.Sort(moduleSorter{modules, c.nameInterface})

	phonys := c.deduplicateOrderOnlyDeps(append(modules, incrementalModules...))
	if err := orderOnlyForIncremental(c, incrementalModules, phonys); err != nil {
		return err
	}

	c.EventHandler.Do("sort_phony_builddefs", func() {
		// sorting for determinism, the phony output names are stable
		sort.Slice(phonys.buildDefs, func(i int, j int) bool {
			return phonys.buildDefs[i].OutputStrings[0] < phonys.buildDefs[j].OutputStrings[0]
		})
	})

	if err := c.writeLocalBuildActions(nw, phonys); err != nil {
		return err
	}

	headerTemplate := template.New("moduleHeader")
	if _, err := headerTemplate.Parse(moduleHeaderTemplate); err != nil {
		// This is a programming error.
		panic(err)
	}

	if shardNinja {
		var wg sync.WaitGroup
		errorCh := make(chan error)
		files := GetNinjaShardFiles(ninjaFileName)
		shardedModules := proptools.ShardByCount(modules, len(files))
		for i, batchModules := range shardedModules {
			file := files[i]
			wg.Add(1)
			go func(file string, batchModules []*moduleInfo) {
				defer wg.Done()
				f, err := c.fs.OpenFile(JoinPath(c.SrcDir(), file), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, OutFilePermissions)
				if err != nil {
					errorCh <- fmt.Errorf("error opening Ninja file shard: %s", err)
					return
				}
				defer func() {
					err := f.Close()
					if err != nil {
						errorCh <- err
					}
				}()
				buf := bufio.NewWriterSize(f, 16*1024*1024)
				defer func() {
					err := buf.Flush()
					if err != nil {
						errorCh <- err
					}
				}()
				writer := newNinjaWriter(buf)
				err = c.writeModuleAction(batchModules, writer, headerTemplate)
				if err != nil {
					errorCh <- err
				}
			}(file, batchModules)
			nw.Subninja(file)
		}

		if c.GetIncrementalEnabled() {
			file := fmt.Sprintf("%s.incremental", ninjaFileName)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := writeIncrementalModules(c, file, incrementalModules, headerTemplate)
				if err != nil {
					errorCh <- err
				}
			}()
			nw.Subninja(file)
		}

		go func() {
			wg.Wait()
			close(errorCh)
		}()

		var errors []error
		for newErrors := range errorCh {
			errors = append(errors, newErrors)
		}
		if len(errors) > 0 {
			return proptools.MergeErrors(errors)
		}
		return nil
	} else {
		return c.writeModuleAction(modules, nw, headerTemplate)
	}
}

func orderOnlyForIncremental(c *Context, modules []*moduleInfo, phonys *localBuildActions) error {
	for _, mod := range modules {
		// find the order only strings of the incremental module, it can come from
		// the cache or from buildDefs depending on if the module was skipped or not.
		var orderOnlyStrings *[]string
		if mod.incrementalRestored {
			orderOnlyStrings = mod.orderOnlyStrings
		} else {
			orderOnlyStrings = new([]string)
			for _, b := range mod.actionDefs.buildDefs {
				// We do similar check when creating phonys in deduplicateOrderOnlyDeps as well
				if len(b.OrderOnly) > 0 {
					return fmt.Errorf("order only shouldn't be used: %s", mod.Name())
				}
				for _, str := range b.OrderOnlyStrings {
					if strings.HasPrefix(str, "dedup-") {
						*orderOnlyStrings = append(*orderOnlyStrings, str)
					}
				}
			}
		}

		if orderOnlyStrings == nil || len(*orderOnlyStrings) == 0 {
			continue
		}

		// update the order only string cache with the info found above.
		if data, ok := c.buildActionsToCache[*mod.buildActionCacheKey]; ok {
			data.OrderOnlyStrings = orderOnlyStrings
		}

		if !mod.incrementalRestored {
			continue
		}

		// if the module is skipped, the order only string that we restored from the
		// cache might not exist anymore. For example, if two modules shared the same
		// set of order only strings initially, deduplicateOrderOnlyDeps would create
		// a dedup-* phony and replace the order only string with this phony for these
		// two modules. If one of the module had its order only strings changed, and
		// we skip the other module in the next build, the dedup-* phony would not
		// in the phony list anymore, so we need to add it here in order to avoid
		// writing the ninja statements for the skipped module, otherwise it would
		// reference a dedup-* phony that no longer exists.
		for _, dep := range *orderOnlyStrings {
			// nothing changed to this phony, the cached value is still valid
			if _, ok := c.orderOnlyStringsToCache[dep]; ok {
				continue
			}
			orderOnlyStrings, ok := c.orderOnlyStringsFromCache[dep]
			if !ok {
				return fmt.Errorf("no cached value found for order only dep: %s", dep)
			}
			phony := buildDef{
				Rule:          Phony,
				OutputStrings: []string{dep},
				InputStrings:  orderOnlyStrings,
				Optional:      true,
			}
			phonys.buildDefs = append(phonys.buildDefs, &phony)
			c.orderOnlyStringsToCache[dep] = orderOnlyStrings
		}
	}
	return nil
}
func writeIncrementalModules(c *Context, baseFile string, modules []*moduleInfo, headerTemplate *template.Template) error {
	bf, err := c.fs.OpenFile(JoinPath(c.SrcDir(), baseFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, OutFilePermissions)
	if err != nil {
		return err
	}
	defer bf.Close()
	bBuf := bufio.NewWriterSize(bf, 16*1024*1024)
	defer bBuf.Flush()
	bWriter := newNinjaWriter(bBuf)
	ninjaPath := filepath.Join(filepath.Dir(baseFile), strings.ReplaceAll(filepath.Base(baseFile), ".", "_"))
	err = os.MkdirAll(JoinPath(c.SrcDir(), ninjaPath), 0755)
	if err != nil {
		return err
	}
	for _, module := range modules {
		moduleFile := filepath.Join(ninjaPath, module.ModuleCacheKey()+".ninja")
		if !module.incrementalRestored {
			err := func() error {
				mf, err := c.fs.OpenFile(JoinPath(c.SrcDir(), moduleFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, OutFilePermissions)
				if err != nil {
					return err
				}
				defer mf.Close()
				mBuf := bufio.NewWriterSize(mf, 4*1024*1024)
				defer mBuf.Flush()
				mWriter := newNinjaWriter(mBuf)
				return c.writeModuleAction([]*moduleInfo{module}, mWriter, headerTemplate)
			}()
			if err != nil {
				return err
			}
		}
		bWriter.Subninja(moduleFile)
	}
	return nil
}

func (c *Context) writeModuleAction(modules []*moduleInfo, nw *ninjaWriter, headerTemplate *template.Template) error {
	buf := bytes.NewBuffer(nil)

	for _, module := range modules {
		if len(module.actionDefs.variables)+len(module.actionDefs.rules)+len(module.actionDefs.buildDefs) == 0 {
			continue
		}
		buf.Reset()

		// In order to make the bootstrap build manifest independent of the
		// build dir we need to output the Blueprints file locations in the
		// comments as paths relative to the source directory.
		relPos := module.pos
		relPos.Filename = module.relBlueprintsFile

		// Get the name and location of the factory function for the module.
		factoryFunc := runtime.FuncForPC(reflect.ValueOf(module.factory).Pointer())
		factoryName := factoryFunc.Name()

		infoMap := map[string]interface{}{
			"name":      module.Name(),
			"typeName":  module.typeName,
			"goFactory": factoryName,
			"pos":       relPos,
			"variant":   module.variant.name,
		}
		if err := headerTemplate.Execute(buf, infoMap); err != nil {
			return err
		}

		if err := nw.Comment(buf.String()); err != nil {
			return err
		}

		if err := nw.BlankLine(); err != nil {
			return err
		}

		if err := c.writeLocalBuildActions(nw, &module.actionDefs); err != nil {
			return err
		}

		if err := nw.BlankLine(); err != nil {
			return err
		}
	}

	return nil
}

func (c *Context) writeAllSingletonActions(nw *ninjaWriter) error {
	c.BeginEvent("singletons")
	defer c.EndEvent("singletons")
	headerTemplate := template.New("singletonHeader")
	_, err := headerTemplate.Parse(singletonHeaderTemplate)
	if err != nil {
		// This is a programming error.
		panic(err)
	}

	buf := bytes.NewBuffer(nil)

	for _, info := range c.singletonInfo {
		if len(info.actionDefs.variables)+len(info.actionDefs.rules)+len(info.actionDefs.buildDefs) == 0 {
			continue
		}

		// Get the name of the factory function for the module.
		factory := info.factory
		factoryFunc := runtime.FuncForPC(reflect.ValueOf(factory).Pointer())
		factoryName := factoryFunc.Name()

		buf.Reset()
		infoMap := map[string]interface{}{
			"name":      info.name,
			"goFactory": factoryName,
		}
		err = headerTemplate.Execute(buf, infoMap)
		if err != nil {
			return err
		}

		err = nw.Comment(buf.String())
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}

		err = c.writeLocalBuildActions(nw, &info.actionDefs)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Context) GetEventHandler() *metrics.EventHandler {
	return c.EventHandler
}

func (c *Context) BeginEvent(name string) {
	c.EventHandler.Begin(name)
}

func (c *Context) EndEvent(name string) {
	c.EventHandler.End(name)
}

func (c *Context) SetBeforePrepareBuildActionsHook(hookFn func() error) {
	c.BeforePrepareBuildActionsHook = hookFn
}

// phonyCandidate represents the state of a set of deps that decides its eligibility
// to be extracted as a phony output
type phonyCandidate struct {
	sync.Once
	phony             *buildDef // the phony buildDef that wraps the set
	first             *buildDef // the first buildDef that uses this set
	orderOnlyStrings  []string  // the original OrderOnlyStrings of the first buildDef that uses this set
	usedByIncremental bool      // if the phony is used by any incremental module
}

// keyForPhonyCandidate gives a unique identifier for a set of deps.
func keyForPhonyCandidate(stringDeps []string) uint64 {
	hasher := fnv.New64a()
	write := func(s string) {
		// The hasher doesn't retain or modify the input slice, so pass the string data directly to avoid
		// an extra allocation and copy.
		_, err := hasher.Write(unsafe.Slice(unsafe.StringData(s), len(s)))
		if err != nil {
			panic(fmt.Errorf("write failed: %w", err))
		}
	}
	for _, d := range stringDeps {
		write(d)
	}
	return hasher.Sum64()
}

// scanBuildDef is called for every known buildDef `b` that has a non-empty `b.OrderOnly`.
// If `b.OrderOnly` is not present in `candidates`, it gets stored.
// But if `b.OrderOnly` already exists in `candidates`, then `b.OrderOnly`
// (and phonyCandidate#first.OrderOnly) will be replaced with phonyCandidate#phony.Outputs
func scanBuildDef(candidates *sync.Map, b *buildDef, incremental bool) {
	key := keyForPhonyCandidate(b.OrderOnlyStrings)
	if v, loaded := candidates.LoadOrStore(key, &phonyCandidate{
		first:             b,
		orderOnlyStrings:  b.OrderOnlyStrings,
		usedByIncremental: incremental,
	}); loaded {
		m := v.(*phonyCandidate)
		if slices.Equal(m.orderOnlyStrings, b.OrderOnlyStrings) {
			m.Do(func() {
				// this is the second occurrence and hence it makes sense to
				// extract it as a phony output
				m.phony = &buildDef{
					Rule:          Phony,
					OutputStrings: []string{fmt.Sprintf("dedup-%x", key)},
					InputStrings:  m.first.OrderOnlyStrings,
					Optional:      true,
				}
				// the previously recorded build-def, which first had these deps as its
				// order-only deps, should now use this phony output instead
				m.first.OrderOnlyStrings = m.phony.OutputStrings
				m.first = nil
			})
			b.OrderOnlyStrings = m.phony.OutputStrings
			// don't override the value with false if it was set to true already
			if incremental {
				m.usedByIncremental = incremental
			}
		}
	}
}

// deduplicateOrderOnlyDeps searches for common sets of order-only dependencies across all
// buildDef instances in the provided moduleInfo instances. Each such
// common set forms a new buildDef representing a phony output that then becomes
// the sole order-only dependency of those buildDef instances
func (c *Context) deduplicateOrderOnlyDeps(modules []*moduleInfo) *localBuildActions {
	c.BeginEvent("deduplicate_order_only_deps")
	defer c.EndEvent("deduplicate_order_only_deps")

	candidates := sync.Map{} //used as map[key]*candidate
	parallelVisit(modules, unorderedVisitorImpl{}, parallelVisitLimit,
		func(m *moduleInfo, pause chan<- pauseSpec) bool {
			incremental := m.buildActionCacheKey != nil
			for _, b := range m.actionDefs.buildDefs {
				// The dedup logic doesn't handle the case where OrderOnly is not empty
				if len(b.OrderOnly) == 0 && len(b.OrderOnlyStrings) > 0 {
					scanBuildDef(&candidates, b, incremental)
				}
			}
			return false
		})

	// now collect all created phonys to return
	var phonys []*buildDef
	candidates.Range(func(_ any, v any) bool {
		candidate := v.(*phonyCandidate)
		if candidate.phony != nil {
			phonys = append(phonys, candidate.phony)
			if candidate.usedByIncremental {
				c.orderOnlyStringsToCache[candidate.phony.OutputStrings[0]] =
					candidate.phony.InputStrings
			}
		}
		return true
	})

	return &localBuildActions{buildDefs: phonys}
}

func (c *Context) writeLocalBuildActions(nw *ninjaWriter,
	defs *localBuildActions) error {

	// Write the local variable assignments.
	for _, v := range defs.variables {
		// A localVariable doesn't need the package names or config to
		// determine its name or value.
		name := v.fullName(nil)
		value, err := v.value(nil, nil)
		if err != nil {
			panic(err)
		}
		err = nw.Assign(name, value.Value(c.nameTracker))
		if err != nil {
			return err
		}
	}

	if len(defs.variables) > 0 {
		err := nw.BlankLine()
		if err != nil {
			return err
		}
	}

	// Write the local rules.
	for _, r := range defs.rules {
		// A localRule doesn't need the package names or config to determine
		// its name or definition.
		name := r.fullName(nil)
		def, err := r.def(nil)
		if err != nil {
			panic(err)
		}

		err = def.WriteTo(nw, name, c.nameTracker)
		if err != nil {
			return err
		}

		err = nw.BlankLine()
		if err != nil {
			return err
		}
	}

	// Write the build definitions.
	for _, buildDef := range defs.buildDefs {
		err := buildDef.WriteTo(nw, c.nameTracker)
		if err != nil {
			return err
		}

		if len(buildDef.Args) > 0 {
			err = nw.BlankLine()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func beforeInModuleList(a, b *moduleInfo, list modulesOrAliases) bool {
	found := false
	if a == b {
		return false
	}
	for _, l := range list {
		if l.module() == a {
			found = true
		} else if l.module() == b {
			return found
		}
	}

	missing := a
	if found {
		missing = b
	}
	panic(fmt.Errorf("element %v not found in list %v", missing, list))
}

type panicError struct {
	panic interface{}
	stack []byte
	in    string
}

func newPanicErrorf(panic interface{}, in string, a ...interface{}) error {
	buf := make([]byte, 4096)
	count := runtime.Stack(buf, false)
	return panicError{
		panic: panic,
		in:    fmt.Sprintf(in, a...),
		stack: buf[:count],
	}
}

func (p panicError) Error() string {
	return fmt.Sprintf("panic in %s\n%s\n%s\n", p.in, p.panic, p.stack)
}

func (p *panicError) addIn(in string) {
	p.in += " in " + in
}

func funcName(f interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// json representation of a dependency
type depJson struct {
	Name    string      `json:"name"`
	Variant string      `json:"variant"`
	TagType string      `json:"tag_type"`
	TagData interface{} `json:"tag_data"`
}

// json representation of a provider
type providerJson struct {
	Type   string      `json:"type"`
	Debug  string      `json:"debug"` // from GetDebugString on the provider data
	Fields interface{} `json:"fields"`
}

// interface for getting debug info from various data.
// TODO: Consider having this return a json object instead
type Debuggable interface {
	GetDebugString() string
}

// Convert a slice in a reflect.Value to a value suitable for outputting to json
func debugSlice(value reflect.Value) interface{} {
	size := value.Len()
	if size == 0 {
		return nil
	}
	result := make([]interface{}, size)
	for i := 0; i < size; i++ {
		result[i] = debugValue(value.Index(i))
	}
	return result
}

// Convert a map in a reflect.Value to a value suitable for outputting to json
func debugMap(value reflect.Value) interface{} {
	if value.IsNil() {
		return nil
	}
	result := make(map[string]interface{})
	iter := value.MapRange()
	for iter.Next() {
		// In the (hopefully) rare case of a key collision (which will happen when multiple
		// go-typed keys have the same string representation, we'll just overwrite the last
		// value.
		result[debugKey(iter.Key())] = debugValue(iter.Value())
	}
	return result
}

// Convert a value into a string, suitable for being a json map key.
func debugKey(value reflect.Value) string {
	return fmt.Sprintf("%v", value)
}

// Convert a single value (possibly a map or slice too) in a reflect.Value to a value suitable for outputting to json
func debugValue(value reflect.Value) interface{} {
	// Remember if we originally received a reflect.Interface.
	wasInterface := value.Kind() == reflect.Interface
	// Dereference pointers down to the real type
	for value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface {
		// If it's nil, return nil
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	// Skip private fields, maybe other weird corner cases of go's bizarre type system.
	if !value.CanInterface() {
		return nil
	}

	switch kind := value.Kind(); kind {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Uint:
		return value.Interface()
	case reflect.Slice:
		return debugSlice(value)
	case reflect.Struct:
		// If we originally received an interface, and there is a String() method, call that.
		// TODO: figure out why Path doesn't work correctly otherwise (in aconfigPropagatingDeclarationsInfo)
		if s, ok := value.Interface().(interface{ String() string }); wasInterface && ok {
			return s.String()
		}
		return debugStruct(value)
	case reflect.Map:
		return debugMap(value)
	default:
		// TODO: add cases as we find them.
		return fmt.Sprintf("debugValue(Kind=%v, wasInterface=%v)", kind, wasInterface)
	}

	return nil
}

// Convert an object in a reflect.Value to a value suitable for outputting to json
func debugStruct(value reflect.Value) interface{} {
	result := make(map[string]interface{})
	debugStructAppend(value, &result)
	if len(result) == 0 {
		return nil
	}
	return result
}

// Convert an object to a value suiable for outputting to json
func debugStructAppend(value reflect.Value, result *map[string]interface{}) {
	for value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	if value.IsZero() {
		return
	}

	if value.Kind() != reflect.Struct {
		// TODO: could maybe support other types
		return
	}

	structType := value.Type()
	for i := 0; i < value.NumField(); i++ {
		v := debugValue(value.Field(i))
		if v != nil {
			(*result)[structType.Field(i).Name] = v
		}
	}
}

func debugPropertyStruct(props interface{}, result *map[string]interface{}) {
	if props == nil {
		return
	}
	debugStructAppend(reflect.ValueOf(props), result)
}

// Get the debug json for a single module. Returns thae data as
// flattened json text for easy concatenation by GenerateModuleDebugInfo.
func getModuleDebugJson(module *moduleInfo) []byte {
	info := struct {
		Name       string                 `json:"name"`
		SourceFile string                 `json:"source_file"`
		SourceLine int                    `json:"source_line"`
		Type       string                 `json:"type"`
		Variant    string                 `json:"variant"`
		Deps       []depJson              `json:"deps"`
		Providers  []providerJson         `json:"providers"`
		Debug      string                 `json:"debug"` // from GetDebugString on the module
		Properties map[string]interface{} `json:"properties"`
	}{
		Name:       module.logicModule.Name(),
		SourceFile: module.pos.Filename,
		SourceLine: module.pos.Line,
		Type:       module.typeName,
		Variant:    module.variant.name,
		Deps: func() []depJson {
			result := make([]depJson, len(module.directDeps))
			for i, dep := range module.directDeps {
				result[i] = depJson{
					Name:    dep.module.logicModule.Name(),
					Variant: dep.module.variant.name,
				}
				t := reflect.TypeOf(dep.tag)
				if t != nil {
					result[i].TagType = t.PkgPath() + "." + t.Name()
					result[i].TagData = debugStruct(reflect.ValueOf(dep.tag))
				}
			}
			return result
		}(),
		Providers: func() []providerJson {
			result := make([]providerJson, 0, len(module.providers))
			for _, p := range module.providers {
				pj := providerJson{}
				include := false

				t := reflect.TypeOf(p)
				if t != nil {
					pj.Type = t.PkgPath() + "." + t.Name()
					include = true
				}

				if dbg, ok := p.(Debuggable); ok {
					pj.Debug = dbg.GetDebugString()
					if pj.Debug != "" {
						include = true
					}
				}

				if p != nil {
					pj.Fields = debugValue(reflect.ValueOf(p))
					include = true
				}

				if include {
					result = append(result, pj)
				}
			}
			return result
		}(),
		Debug: func() string {
			if dbg, ok := module.logicModule.(Debuggable); ok {
				return dbg.GetDebugString()
			} else {
				return ""
			}
		}(),
		Properties: func() map[string]interface{} {
			result := make(map[string]interface{})
			for _, props := range module.properties {
				debugPropertyStruct(props, &result)
			}
			return result
		}(),
	}
	buf, _ := json.Marshal(info)
	return buf
}

// Generate out/soong/soong-debug-info.json Called if GENERATE_SOONG_DEBUG=true.
func (this *Context) GenerateModuleDebugInfo(filename string) {
	err := os.MkdirAll(filepath.Dir(filename), 0777)
	if err != nil {
		// We expect this to be writable
		panic(fmt.Sprintf("couldn't create directory for soong module debug file %s: %s", filepath.Dir(filename), err))
	}

	f, err := os.Create(filename)
	if err != nil {
		// We expect this to be writable
		panic(fmt.Sprintf("couldn't create soong module debug file %s: %s", filename, err))
	}
	defer f.Close()

	needComma := false
	f.WriteString("{\n\"modules\": [\n")

	// TODO: Optimize this (parallel execution, etc) if it gets slow.
	this.visitAllModuleInfos(func(module *moduleInfo) {
		if needComma {
			f.WriteString(",\n")
		} else {
			needComma = true
		}

		moduleData := getModuleDebugJson(module)
		f.Write(moduleData)
	})

	f.WriteString("\n]\n}")
}

var fileHeaderTemplate = `******************************************************************************
***            This file is generated and should not be edited             ***
******************************************************************************
{{if .Pkgs}}
This file contains variables, rules, and pools with name prefixes indicating
they were generated by the following Go packages:
{{range .Pkgs}}
    {{.PkgName}} [from Go package {{.PkgPath}}]{{end}}{{end}}

`

var moduleHeaderTemplate = `# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #
Module:  {{.name}}
Variant: {{.variant}}
Type:    {{.typeName}}
Factory: {{.goFactory}}
Defined: {{.pos}}
`

var singletonHeaderTemplate = `# # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # # #
Singleton: {{.name}}
Factory:   {{.goFactory}}
`

func JoinPath(base, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}
