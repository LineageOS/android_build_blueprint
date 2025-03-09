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

package bootstrap

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/pathtools"
	"github.com/google/blueprint/proptools"
)

var (
	pctx = blueprint.NewPackageContext("github.com/google/blueprint/bootstrap")

	goTestMainCmd   = pctx.StaticVariable("goTestMainCmd", filepath.Join("$ToolDir", "gotestmain"))
	goTestRunnerCmd = pctx.StaticVariable("goTestRunnerCmd", filepath.Join("$ToolDir", "gotestrunner"))
	pluginGenSrcCmd = pctx.StaticVariable("pluginGenSrcCmd", filepath.Join("$ToolDir", "loadplugins"))

	parallelCompile = pctx.StaticVariable("parallelCompile", func() string {
		numCpu := runtime.NumCPU()
		// This will cause us to recompile all go programs if the
		// number of cpus changes. We don't get a lot of benefit from
		// higher values, so cap this to make it cheaper to move trees
		// between machines.
		if numCpu > 8 {
			numCpu = 8
		}
		return fmt.Sprintf("-c %d", numCpu)
	}())

	compile = pctx.StaticRule("compile",
		blueprint.RuleParams{
			Command: "GOROOT='$goRoot' $compileCmd $parallelCompile -o $out.tmp " +
				"$debugFlags -p $pkgPath -complete $incFlags $embedFlags -pack $in && " +
				"if cmp --quiet $out.tmp $out; then rm $out.tmp; else mv -f $out.tmp $out; fi",
			CommandDeps: []string{"$compileCmd"},
			Description: "compile $out",
			Restat:      true,
		},
		"pkgPath", "incFlags", "embedFlags")

	link = pctx.StaticRule("link",
		blueprint.RuleParams{
			Command: "GOROOT='$goRoot' $linkCmd -o $out.tmp $libDirFlags $in && " +
				"if cmp --quiet $out.tmp $out; then rm $out.tmp; else mv -f $out.tmp $out; fi",
			CommandDeps: []string{"$linkCmd"},
			Description: "link $out",
			Restat:      true,
		},
		"libDirFlags")

	goTestMain = pctx.StaticRule("gotestmain",
		blueprint.RuleParams{
			Command:     "$goTestMainCmd -o $out -pkg $pkg $in",
			CommandDeps: []string{"$goTestMainCmd"},
			Description: "gotestmain $out",
		},
		"pkg")

	pluginGenSrc = pctx.StaticRule("pluginGenSrc",
		blueprint.RuleParams{
			Command:     "$pluginGenSrcCmd -o $out -p $pkg $plugins",
			CommandDeps: []string{"$pluginGenSrcCmd"},
			Description: "create $out",
		},
		"pkg", "plugins")

	test = pctx.StaticRule("test",
		blueprint.RuleParams{
			Command:     "$goTestRunnerCmd -p $pkgSrcDir -f $out -- $in -test.short",
			CommandDeps: []string{"$goTestRunnerCmd"},
			Description: "test $pkg",
		},
		"pkg", "pkgSrcDir")

	cp = pctx.StaticRule("cp",
		blueprint.RuleParams{
			Command:     "cp $in $out",
			Description: "cp $out",
		},
		"generator")

	touch = pctx.StaticRule("touch",
		blueprint.RuleParams{
			Command:     "touch $out",
			Description: "touch $out",
		},
		"depfile", "generator")

	cat = pctx.StaticRule("Cat",
		blueprint.RuleParams{
			Command:     "rm -f $out && cat $in > $out",
			Description: "concatenate files to $out",
		})

	// ubuntu 14.04 offcially use dash for /bin/sh, and its builtin echo command
	// doesn't support -e option. Therefore we force to use /bin/bash when writing out
	// content to file.
	writeFile = pctx.StaticRule("writeFile",
		blueprint.RuleParams{
			Command:     `rm -f $out && /bin/bash -c 'echo -e -n "$$0" > $out' $content`,
			Description: "writing file $out",
		},
		"content")

	generateBuildNinja = pctx.StaticRule("build.ninja",
		blueprint.RuleParams{
			// TODO: it's kinda ugly that some parameters are computed from
			// environment variables and some from Ninja parameters, but it's probably
			// better to not to touch that while Blueprint and Soong are separate
			// NOTE: The spaces at EOL are important because otherwise Ninja would
			// omit all spaces between the different options.
			Command: `cd "$$(dirname "$builder")" && ` +
				`BUILDER="$$PWD/$$(basename "$builder")" && ` +
				`cd / && ` +
				`env -i $env "$$BUILDER" ` +
				`    --top "$$TOP" ` +
				`    --soong_out "$soongOutDir" ` +
				`    --out "$outDir" ` +
				`    $extra`,
			CommandDeps: []string{"$builder"},
			Description: "$builder $out",
			Deps:        blueprint.DepsGCC,
			Depfile:     "$out.d",
			Restat:      true,
		},
		"builder", "env", "extra", "pool")

	// Work around a Ninja issue.  See https://github.com/martine/ninja/pull/634
	phony = pctx.StaticRule("phony",
		blueprint.RuleParams{
			Command:     "# phony $out",
			Description: "phony $out",
			Generator:   true,
		},
		"depfile")

	_ = pctx.VariableFunc("ToolDir", func(ctx blueprint.VariableFuncContext, config interface{}) (string, error) {
		return config.(BootstrapConfig).HostToolDir(), nil
	})
)

var (
	// echoEscaper escapes a string such that passing it to "echo -e" will produce the input value.
	echoEscaper = strings.NewReplacer(
		`\`, `\\`, // First escape existing backslashes so they aren't interpreted by `echo -e`.
		"\n", `\n`, // Then replace newlines with \n
	)
)

// shardString takes a string and returns a slice of strings where the length of each one is
// at most shardSize.
func shardString(s string, shardSize int) []string {
	if len(s) == 0 {
		return nil
	}
	ret := make([]string, 0, (len(s)+shardSize-1)/shardSize)
	for len(s) > shardSize {
		ret = append(ret, s[0:shardSize])
		s = s[shardSize:]
	}
	if len(s) > 0 {
		ret = append(ret, s)
	}
	return ret
}

// writeFileRule creates a ninja rule to write contents to a file.  The contents will be
// escaped so that the file contains exactly the contents passed to the function.
func writeFileRule(ctx blueprint.ModuleContext, outputFile string, content string) {
	// This is MAX_ARG_STRLEN subtracted with some safety to account for shell escapes
	const SHARD_SIZE = 131072 - 10000

	buildWriteFileRule := func(outputFile string, content string) {
		content = echoEscaper.Replace(content)
		content = proptools.NinjaEscape(proptools.ShellEscapeIncludingSpaces(content))
		if content == "" {
			content = "''"
		}
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:        writeFile,
			Outputs:     []string{outputFile},
			Description: "write " + outputFile,
			Args: map[string]string{
				"content": content,
			},
		})
	}

	if len(content) > SHARD_SIZE {
		var chunks []string
		for i, c := range shardString(content, SHARD_SIZE) {
			tempPath := fmt.Sprintf("%s.%d", outputFile, i)
			buildWriteFileRule(tempPath, c)
			chunks = append(chunks, tempPath)
		}
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:        cat,
			Inputs:      chunks,
			Outputs:     []string{outputFile},
			Description: "Merging to " + outputFile,
		})
		return
	}
	buildWriteFileRule(outputFile, content)
}

type pluginDependencyTag struct {
	blueprint.BaseDependencyTag
}

type bootstrapDependencies interface {
	bootstrapDeps(ctx blueprint.BottomUpMutatorContext)
}

var pluginDepTag = pluginDependencyTag{}

func BootstrapDeps(ctx blueprint.BottomUpMutatorContext) {
	if pkg, ok := ctx.Module().(bootstrapDependencies); ok {
		pkg.bootstrapDeps(ctx)
	}
}

type PackageInfo struct {
	PkgPath       string
	PkgRoot       string
	PackageTarget string
	TestTargets   []string
}

var PackageProvider = blueprint.NewProvider[*PackageInfo]()

type BinaryInfo struct {
	IntermediatePath string
	InstallPath      string
	TestTargets      []string
}

var BinaryProvider = blueprint.NewProvider[*BinaryInfo]()

type DocsPackageInfo struct {
	PkgPath string
	Srcs    []string
}

var DocsPackageProvider = blueprint.NewMutatorProvider[*DocsPackageInfo]("bootstrap_deps")

// A GoPackage is a module for building Go packages.
type GoPackage struct {
	blueprint.SimpleName
	properties struct {
		Deps      []string
		PkgPath   string
		Srcs      []string
		TestSrcs  []string
		TestData  []string
		PluginFor []string
		EmbedSrcs []string
		// The visibility property is unused in blueprint, but exists so that soong
		// can add one and not have the bp files fail to parse during the bootstrap build.
		Visibility []string

		Darwin struct {
			Srcs     []string
			TestSrcs []string
		}
		Linux struct {
			Srcs     []string
			TestSrcs []string
		}
	}
}

func newGoPackageModuleFactory() func() (blueprint.Module, []interface{}) {
	return func() (blueprint.Module, []interface{}) {
		module := &GoPackage{}
		return module, []interface{}{&module.properties, &module.SimpleName.Properties}
	}
}

// Properties returns the list of property structs to be used for registering a wrapped module type.
func (g *GoPackage) Properties() []interface{} {
	return []interface{}{&g.properties}
}

func (g *GoPackage) DynamicDependencies(ctx blueprint.DynamicDependerModuleContext) []string {
	return g.properties.Deps
}

func (g *GoPackage) bootstrapDeps(ctx blueprint.BottomUpMutatorContext) {
	for _, plugin := range g.properties.PluginFor {
		ctx.AddReverseDependency(ctx.Module(), pluginDepTag, plugin)
	}
	blueprint.SetProvider(ctx, DocsPackageProvider, &DocsPackageInfo{
		PkgPath: g.properties.PkgPath,
		Srcs:    g.properties.Srcs,
	})
}

func (g *GoPackage) GenerateBuildActions(ctx blueprint.ModuleContext) {
	var (
		name       = ctx.ModuleName()
		hasPlugins = false
		pluginSrc  = ""
		genSrcs    = []string{}
	)

	if g.properties.PkgPath == "" {
		ctx.ModuleErrorf("module %s did not specify a valid pkgPath", name)
		return
	}

	pkgRoot := packageRoot(ctx)
	archiveFile := filepath.Join(pkgRoot,
		filepath.FromSlash(g.properties.PkgPath)+".a")

	ctx.VisitDepsDepthFirst(func(module blueprint.Module) {
		if ctx.OtherModuleDependencyTag(module) == pluginDepTag {
			hasPlugins = true
		}
	})
	if hasPlugins {
		pluginSrc = filepath.Join(moduleGenSrcDir(ctx), "plugin.go")
		genSrcs = append(genSrcs, pluginSrc)
	}

	if hasPlugins && !buildGoPluginLoader(ctx, g.properties.PkgPath, pluginSrc) {
		return
	}

	var srcs, testSrcs []string
	if runtime.GOOS == "darwin" {
		srcs = append(g.properties.Srcs, g.properties.Darwin.Srcs...)
		testSrcs = append(g.properties.TestSrcs, g.properties.Darwin.TestSrcs...)
	} else if runtime.GOOS == "linux" {
		srcs = append(g.properties.Srcs, g.properties.Linux.Srcs...)
		testSrcs = append(g.properties.TestSrcs, g.properties.Linux.TestSrcs...)
	}

	testArchiveFile := filepath.Join(testRoot(ctx),
		filepath.FromSlash(g.properties.PkgPath)+".a")
	testResultFile := buildGoTest(ctx, testRoot(ctx), testArchiveFile,
		g.properties.PkgPath, srcs, genSrcs, testSrcs, g.properties.EmbedSrcs)

	// Don't build for test-only packages
	if len(srcs) == 0 && len(genSrcs) == 0 {
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:     touch,
			Outputs:  []string{archiveFile},
			Optional: true,
		})
		return
	}

	buildGoPackage(ctx, pkgRoot, g.properties.PkgPath, archiveFile,
		srcs, genSrcs, g.properties.EmbedSrcs)
	blueprint.SetProvider(ctx, blueprint.SrcsFileProviderKey, blueprint.SrcsFileProviderData{SrcPaths: srcs})
	blueprint.SetProvider(ctx, PackageProvider, &PackageInfo{
		PkgPath:       g.properties.PkgPath,
		PkgRoot:       pkgRoot,
		PackageTarget: archiveFile,
		TestTargets:   testResultFile,
	})
}

// A GoBinary is a module for building executable binaries from Go sources.
type GoBinary struct {
	blueprint.SimpleName
	properties struct {
		Deps           []string
		Srcs           []string
		TestSrcs       []string
		TestData       []string
		EmbedSrcs      []string
		PrimaryBuilder bool
		Default        bool
		// The visibility property is unused in blueprint, but exists so that soong
		// can add one and not have the bp files fail to parse during the bootstrap build.
		Visibility []string

		Darwin struct {
			Srcs     []string
			TestSrcs []string
		}
		Linux struct {
			Srcs     []string
			TestSrcs []string
		}
	}

	installPath string

	// skipInstall can be set to true by a module type that wraps GoBinary to skip the install rule,
	// allowing the wrapping module type to create the install rule itself.
	skipInstall bool

	// outputFile is set to the path to the intermediate output file.
	outputFile string
}

func newGoBinaryModuleFactory() func() (blueprint.Module, []interface{}) {
	return func() (blueprint.Module, []interface{}) {
		module := &GoBinary{}
		return module, []interface{}{&module.properties, &module.SimpleName.Properties}
	}
}

func (g *GoBinary) DynamicDependencies(ctx blueprint.DynamicDependerModuleContext) []string {
	return g.properties.Deps
}

func (g *GoBinary) bootstrapDeps(ctx blueprint.BottomUpMutatorContext) {
	if g.properties.PrimaryBuilder {
		blueprint.SetProvider(ctx, PrimaryBuilderProvider, PrimaryBuilderInfo{})
	}
}

// IntermediateFile returns the path to the final linked intermedate file.
func (g *GoBinary) IntermediateFile() string {
	return g.outputFile
}

// SetSkipInstall is called by module types that wrap GoBinary to skip the install rule,
// allowing the wrapping module type to create the install rule itself.
func (g *GoBinary) SetSkipInstall() {
	g.skipInstall = true
}

// Properties returns the list of property structs to be used for registering a wrapped module type.
func (g *GoBinary) Properties() []interface{} {
	return []interface{}{&g.properties}
}

func (g *GoBinary) GenerateBuildActions(ctx blueprint.ModuleContext) {
	var (
		name            = ctx.ModuleName()
		objDir          = moduleObjDir(ctx)
		archiveFile     = filepath.Join(objDir, name+".a")
		testArchiveFile = filepath.Join(testRoot(ctx), name+".a")
		aoutFile        = filepath.Join(objDir, name)
		hasPlugins      = false
		pluginSrc       = ""
		genSrcs         = []string{}
	)

	if !g.skipInstall {
		g.installPath = filepath.Join(ctx.Config().(BootstrapConfig).HostToolDir(), name)
	}

	ctx.VisitDirectDeps(func(module blueprint.Module) {
		if ctx.OtherModuleDependencyTag(module) == pluginDepTag {
			hasPlugins = true
		}
	})
	if hasPlugins {
		pluginSrc = filepath.Join(moduleGenSrcDir(ctx), "plugin.go")
		genSrcs = append(genSrcs, pluginSrc)
	}

	var testDeps []string

	if hasPlugins && !buildGoPluginLoader(ctx, "main", pluginSrc) {
		return
	}

	var srcs, testSrcs []string
	if runtime.GOOS == "darwin" {
		srcs = append(g.properties.Srcs, g.properties.Darwin.Srcs...)
		testSrcs = append(g.properties.TestSrcs, g.properties.Darwin.TestSrcs...)
	} else if runtime.GOOS == "linux" {
		srcs = append(g.properties.Srcs, g.properties.Linux.Srcs...)
		testSrcs = append(g.properties.TestSrcs, g.properties.Linux.TestSrcs...)
	}

	testResultFile := buildGoTest(ctx, testRoot(ctx), testArchiveFile,
		name, srcs, genSrcs, testSrcs, g.properties.EmbedSrcs)
	testDeps = append(testDeps, testResultFile...)

	buildGoPackage(ctx, objDir, "main", archiveFile, srcs, genSrcs, g.properties.EmbedSrcs)

	var linkDeps []string
	var libDirFlags []string
	ctx.VisitDepsDepthFirst(func(module blueprint.Module) {
		if info, ok := blueprint.OtherModuleProvider(ctx, module, PackageProvider); ok {
			linkDeps = append(linkDeps, info.PackageTarget)
			libDir := info.PkgRoot
			libDirFlags = append(libDirFlags, "-L "+libDir)
			testDeps = append(testDeps, info.TestTargets...)
		}
	})

	linkArgs := map[string]string{}
	if len(libDirFlags) > 0 {
		linkArgs["libDirFlags"] = strings.Join(libDirFlags, " ")
	}

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:      link,
		Outputs:   []string{aoutFile},
		Inputs:    []string{archiveFile},
		Implicits: linkDeps,
		Args:      linkArgs,
		Optional:  true,
	})

	g.outputFile = aoutFile

	var validations []string
	if ctx.Config().(BootstrapConfig).RunGoTests() {
		validations = testDeps
	}

	if !g.skipInstall {
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:        cp,
			Outputs:     []string{g.installPath},
			Inputs:      []string{aoutFile},
			Validations: validations,
			Optional:    !g.properties.Default,
		})
	}

	blueprint.SetProvider(ctx, blueprint.SrcsFileProviderKey, blueprint.SrcsFileProviderData{SrcPaths: srcs})
	blueprint.SetProvider(ctx, BinaryProvider, &BinaryInfo{
		IntermediatePath: g.outputFile,
		InstallPath:      g.installPath,
		TestTargets:      testResultFile,
	})
}

func buildGoPluginLoader(ctx blueprint.ModuleContext, pkgPath, pluginSrc string) bool {
	ret := true

	var pluginPaths []string
	ctx.VisitDirectDeps(func(module blueprint.Module) {
		if ctx.OtherModuleDependencyTag(module) == pluginDepTag {
			if info, ok := blueprint.OtherModuleProvider(ctx, module, PackageProvider); ok {
				pluginPaths = append(pluginPaths, info.PkgPath)
			}
		}
	})

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:    pluginGenSrc,
		Outputs: []string{pluginSrc},
		Args: map[string]string{
			"pkg":     pkgPath,
			"plugins": strings.Join(pluginPaths, " "),
		},
		Optional: true,
	})

	return ret
}

func generateEmbedcfgFile(ctx blueprint.ModuleContext, files []string, srcDir string, embedcfgFile string) {
	embedcfg := struct {
		Patterns map[string][]string
		Files    map[string]string
	}{
		make(map[string][]string, len(files)),
		make(map[string]string, len(files)),
	}

	for _, file := range files {
		embedcfg.Patterns[file] = []string{file}
		embedcfg.Files[file] = filepath.Join(srcDir, file)
	}

	embedcfgData, err := json.Marshal(&embedcfg)
	if err != nil {
		ctx.ModuleErrorf("Failed to marshal embedcfg data: %s", err.Error())
	}

	writeFileRule(ctx, embedcfgFile, string(embedcfgData))
}

func buildGoPackage(ctx blueprint.ModuleContext, pkgRoot string,
	pkgPath string, archiveFile string, srcs []string, genSrcs []string, embedSrcs []string) {

	srcDir := moduleSrcDir(ctx)
	srcFiles := pathtools.PrefixPaths(srcs, srcDir)
	srcFiles = append(srcFiles, genSrcs...)

	var incFlags []string
	var deps []string
	ctx.VisitDepsDepthFirst(func(module blueprint.Module) {
		if info, ok := blueprint.OtherModuleProvider(ctx, module, PackageProvider); ok {
			incDir := info.PkgRoot
			target := info.PackageTarget
			incFlags = append(incFlags, "-I "+incDir)
			deps = append(deps, target)
		}
	})

	compileArgs := map[string]string{
		"pkgPath": pkgPath,
	}

	if len(incFlags) > 0 {
		compileArgs["incFlags"] = strings.Join(incFlags, " ")
	}

	if len(embedSrcs) > 0 {
		embedcfgFile := archiveFile + ".embedcfg"
		generateEmbedcfgFile(ctx, embedSrcs, srcDir, embedcfgFile)
		compileArgs["embedFlags"] = "-embedcfg " + embedcfgFile
		deps = append(deps, embedcfgFile)
	}

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:      compile,
		Outputs:   []string{archiveFile},
		Inputs:    srcFiles,
		Implicits: deps,
		Args:      compileArgs,
		Optional:  true,
	})
}

func buildGoTest(ctx blueprint.ModuleContext, testRoot, testPkgArchive,
	pkgPath string, srcs, genSrcs, testSrcs []string, embedSrcs []string) []string {

	if len(testSrcs) == 0 {
		return nil
	}

	srcDir := moduleSrcDir(ctx)
	testFiles := pathtools.PrefixPaths(testSrcs, srcDir)

	mainFile := filepath.Join(testRoot, "test.go")
	testArchive := filepath.Join(testRoot, "test.a")
	testFile := filepath.Join(testRoot, "test")
	testPassed := filepath.Join(testRoot, "test.passed")

	buildGoPackage(ctx, testRoot, pkgPath, testPkgArchive,
		append(srcs, testSrcs...), genSrcs, embedSrcs)

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:    goTestMain,
		Outputs: []string{mainFile},
		Inputs:  testFiles,
		Args: map[string]string{
			"pkg": pkgPath,
		},
		Optional: true,
	})

	linkDeps := []string{testPkgArchive}
	libDirFlags := []string{"-L " + testRoot}
	testDeps := []string{}
	ctx.VisitDepsDepthFirst(func(module blueprint.Module) {
		if info, ok := blueprint.OtherModuleProvider(ctx, module, PackageProvider); ok {
			linkDeps = append(linkDeps, info.PackageTarget)
			libDir := info.PkgRoot
			libDirFlags = append(libDirFlags, "-L "+libDir)
			testDeps = append(testDeps, info.TestTargets...)
		}
	})

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:      compile,
		Outputs:   []string{testArchive},
		Inputs:    []string{mainFile},
		Implicits: []string{testPkgArchive},
		Args: map[string]string{
			"pkgPath":  "main",
			"incFlags": "-I " + testRoot,
		},
		Optional: true,
	})

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:      link,
		Outputs:   []string{testFile},
		Inputs:    []string{testArchive},
		Implicits: linkDeps,
		Args: map[string]string{
			"libDirFlags": strings.Join(libDirFlags, " "),
		},
		Optional: true,
	})

	ctx.Build(pctx, blueprint.BuildParams{
		Rule:        test,
		Outputs:     []string{testPassed},
		Inputs:      []string{testFile},
		Validations: testDeps,
		Args: map[string]string{
			"pkg":       pkgPath,
			"pkgSrcDir": filepath.Dir(testFiles[0]),
		},
		Optional: true,
	})

	return []string{testPassed}
}

var PrimaryBuilderProvider = blueprint.NewMutatorProvider[PrimaryBuilderInfo]("bootstrap_deps")

type PrimaryBuilderInfo struct{}

type singleton struct {
}

func newSingletonFactory() func() blueprint.Singleton {
	return func() blueprint.Singleton {
		return &singleton{}
	}
}

func (s *singleton) GenerateBuildActions(ctx blueprint.SingletonContext) {
	// Find the module that's marked as the "primary builder", which means it's
	// creating the binary that we'll use to generate the non-bootstrap
	// build.ninja file.
	var primaryBuilders []string
	// blueprintTools contains blueprint go binaries that will be built in StageMain
	var blueprintTools []string
	// blueprintTools contains the test outputs of go tests that can be run in StageMain
	var blueprintTests []string
	// blueprintGoPackages contains all blueprint go packages that can be built in StageMain
	var blueprintGoPackages []string
	ctx.VisitAllModules(func(module blueprint.Module) {
		if ctx.PrimaryModule(module) == module {
			if binaryInfo, ok := blueprint.SingletonModuleProvider(ctx, module, BinaryProvider); ok {
				if binaryInfo.InstallPath != "" {
					blueprintTools = append(blueprintTools, binaryInfo.InstallPath)
				}
				blueprintTests = append(blueprintTests, binaryInfo.TestTargets...)
				if _, ok := blueprint.SingletonModuleProvider(ctx, module, PrimaryBuilderProvider); ok {
					primaryBuilders = append(primaryBuilders, binaryInfo.InstallPath)
				}
			}

			if packageInfo, ok := blueprint.SingletonModuleProvider(ctx, module, PackageProvider); ok {
				blueprintGoPackages = append(blueprintGoPackages, packageInfo.PackageTarget)
				blueprintTests = append(blueprintTests, packageInfo.TestTargets...)
			}
		}
	})

	var primaryBuilderCmdlinePrefix []string
	var primaryBuilderFile string

	if len(primaryBuilders) == 0 {
		ctx.Errorf("no primary builder module present")
		return
	} else if len(primaryBuilders) > 1 {
		ctx.Errorf("multiple primary builder modules present: %q", primaryBuilders)
		return
	} else {
		primaryBuilderFile = primaryBuilders[0]
	}

	ctx.SetOutDir(pctx, "${outDir}")

	for _, subninja := range ctx.Config().(BootstrapConfig).Subninjas() {
		ctx.AddSubninja(subninja)
	}

	for _, i := range ctx.Config().(BootstrapConfig).PrimaryBuilderInvocations() {
		flags := make([]string, 0)
		flags = append(flags, primaryBuilderCmdlinePrefix...)
		flags = append(flags, i.Args...)

		pool := ""
		if i.Console {
			pool = "console"
		}

		envAssignments := ""
		for k, v := range i.Env {
			// NB: This is rife with quoting issues but we don't care because we trust
			// soong_ui to not abuse this facility too much
			envAssignments += k + "=" + v + " "
		}

		// Build the main build.ninja
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:      generateBuildNinja,
			Outputs:   i.Outputs,
			Inputs:    i.Inputs,
			Implicits: i.Implicits,
			OrderOnly: i.OrderOnlyInputs,
			Args: map[string]string{
				"builder": primaryBuilderFile,
				"env":     envAssignments,
				"extra":   strings.Join(flags, " "),
				"pool":    pool,
			},
			// soong_ui explicitly requests what it wants to be build. This is
			// because the same Ninja file contains instructions to run
			// soong_build, run bp2build and to generate the JSON module graph.
			Optional:    true,
			Description: i.Description,
		})
	}

	// Add a phony target for building various tools that are part of blueprint
	if len(blueprintTools) > 0 {
		ctx.Build(pctx, blueprint.BuildParams{
			Rule:    blueprint.Phony,
			Outputs: []string{"blueprint_tools"},
			Inputs:  blueprintTools,
		})
	}

	// Add a phony target for running various tests that are part of blueprint
	ctx.Build(pctx, blueprint.BuildParams{
		Rule:    blueprint.Phony,
		Outputs: []string{"blueprint_tests"},
		Inputs:  blueprintTests,
	})

	// Add a phony target for running go tests
	ctx.Build(pctx, blueprint.BuildParams{
		Rule:     blueprint.Phony,
		Outputs:  []string{"blueprint_go_packages"},
		Inputs:   blueprintGoPackages,
		Optional: true,
	})
}

// packageRoot returns the module-specific package root directory path.  This
// directory is where the final package .a files are output and where dependant
// modules search for this package via -I arguments.
func packageRoot(ctx blueprint.ModuleContext) string {
	toolDir := ctx.Config().(BootstrapConfig).HostToolDir()
	return filepath.Join(toolDir, "go", ctx.ModuleName(), ctx.ModuleSubDir(), "pkg")
}

// testRoot returns the module-specific package root directory path used for
// building tests. The .a files generated here will include everything from
// packageRoot, plus the test-only code.
func testRoot(ctx blueprint.ModuleContext) string {
	toolDir := ctx.Config().(BootstrapConfig).HostToolDir()
	return filepath.Join(toolDir, "go", ctx.ModuleName(), ctx.ModuleSubDir(), "test")
}

// moduleSrcDir returns the path of the directory that all source file paths are
// specified relative to.
func moduleSrcDir(ctx blueprint.ModuleContext) string {
	return ctx.ModuleDir()
}

// moduleObjDir returns the module-specific object directory path.
func moduleObjDir(ctx blueprint.ModuleContext) string {
	toolDir := ctx.Config().(BootstrapConfig).HostToolDir()
	return filepath.Join(toolDir, "go", ctx.ModuleName(), ctx.ModuleSubDir(), "obj")
}

// moduleGenSrcDir returns the module-specific generated sources path.
func moduleGenSrcDir(ctx blueprint.ModuleContext) string {
	toolDir := ctx.Config().(BootstrapConfig).HostToolDir()
	return filepath.Join(toolDir, "go", ctx.ModuleName(), ctx.ModuleSubDir(), "gen")
}
