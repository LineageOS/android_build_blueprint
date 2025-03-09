package bootstrap

import (
	"fmt"
	"maps"
	"path/filepath"
	"reflect"

	"github.com/google/blueprint"
	"github.com/google/blueprint/bootstrap/bpdoc"
	"github.com/google/blueprint/pathtools"
)

// ModuleTypeDocs returns a list of bpdoc.ModuleType objects that contain information relevant
// to generating documentation for module types supported by the primary builder.
func ModuleTypeDocs(ctx *blueprint.Context, factories map[string]reflect.Value) ([]*bpdoc.Package, error) {
	// Find the module that's marked as the "primary builder", which means it's
	// creating the binary that we'll use to generate the non-bootstrap
	// build.ninja file.
	var primaryBuilders []blueprint.Module
	ctx.VisitAllModules(func(module blueprint.Module) {
		if ctx.PrimaryModule(module) == module {
			if _, ok := blueprint.SingletonModuleProvider(ctx, module, PrimaryBuilderProvider); ok {
				primaryBuilders = append(primaryBuilders, module)
			}
		}
	})

	var primaryBuilder blueprint.Module
	switch len(primaryBuilders) {
	case 0:
		return nil, fmt.Errorf("no primary builder module present")

	case 1:
		primaryBuilder = primaryBuilders[0]

	default:
		return nil, fmt.Errorf("multiple primary builder modules present")
	}

	pkgFiles := make(map[string][]string)
	ctx.VisitDepsDepthFirst(primaryBuilder, func(module blueprint.Module) {
		if info, ok := blueprint.SingletonModuleProvider(ctx, module, DocsPackageProvider); ok {
			pkgFiles[info.PkgPath] = pathtools.PrefixPaths(info.Srcs,
				filepath.Join(ctx.SrcDir(), ctx.ModuleDir(module)))
		}
	})

	mergedFactories := maps.Clone(factories)

	for moduleType, factory := range ctx.ModuleTypeFactories() {
		if _, exists := mergedFactories[moduleType]; !exists {
			mergedFactories[moduleType] = reflect.ValueOf(factory)
		}
	}

	return bpdoc.AllPackages(pkgFiles, mergedFactories, ctx.ModuleTypePropertyStructs())
}
