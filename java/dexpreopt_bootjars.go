// Copyright 2019 Google Inc. All rights reserved.
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

package java

import (
	"path/filepath"
	"sort"
	"strings"

	"android/soong/android"
	"android/soong/dexpreopt"

	"github.com/google/blueprint/proptools"
)

func init() {
	android.RegisterSingletonType("dex_bootjars", dexpreoptBootJarsFactory)
}

// Target-independent description of pre-compiled boot image.
type bootImageConfig struct {
	// If this image is an extension, the image that it extends.
	extends *bootImageConfig

	// Image name (used in directory names and ninja rule names).
	name string

	// Basename of the image: the resulting filenames are <stem>[-<jar>].{art,oat,vdex}.
	stem string

	// Output directory for the image files.
	dir android.OutputPath

	// Output directory for the image files with debug symbols.
	symbolsDir android.OutputPath

	// Subdirectory where the image files are installed.
	installSubdir string

	// The names of jars that constitute this image.
	modules []string

	// File paths to jars.
	dexPaths     android.WritablePaths // for this image
	dexPathsDeps android.WritablePaths // for the dependency images and in this image

	// File path to a zip archive with all image files (or nil, if not needed).
	zip android.WritablePath

	// Rules which should be used in make to install the outputs.
	profileInstalls android.RuleBuilderInstalls

	// Target-dependent fields.
	variants []*bootImageVariant
}

// Target-dependent description of pre-compiled boot image.
type bootImageVariant struct {
	*bootImageConfig

	// Target for which the image is generated.
	target android.Target

	// The "locations" of jars.
	dexLocations     []string // for this image
	dexLocationsDeps []string // for the dependency images and in this image

	// Paths to image files.
	images     android.OutputPath  // first image file
	imagesDeps android.OutputPaths // all files

	// Only for extensions, paths to the primary boot images.
	primaryImages android.OutputPath

	// Rules which should be used in make to install the outputs.
	installs           android.RuleBuilderInstalls
	vdexInstalls       android.RuleBuilderInstalls
	unstrippedInstalls android.RuleBuilderInstalls
}

func (image bootImageConfig) getVariant(target android.Target) *bootImageVariant {
	for _, variant := range image.variants {
		if variant.target.Os == target.Os && variant.target.Arch.ArchType == target.Arch.ArchType {
			return variant
		}
	}
	return nil
}

// Return any (the first) variant which is for the device (as opposed to for the host)
func (image bootImageConfig) getAnyAndroidVariant() *bootImageVariant {
	for _, variant := range image.variants {
		if variant.target.Os == android.Android {
			return variant
		}
	}
	return nil
}

func (image bootImageConfig) moduleName(idx int) string {
	// Dexpreopt on the boot class path produces multiple files. The first dex file
	// is converted into 'name'.art (to match the legacy assumption that 'name'.art
	// exists), and the rest are converted to 'name'-<jar>.art.
	m := image.modules[idx]
	name := image.stem
	if idx != 0 || image.extends != nil {
		name += "-" + stemOf(m)
	}
	return name
}

func (image bootImageConfig) firstModuleNameOrStem() string {
	if len(image.modules) > 0 {
		return image.moduleName(0)
	} else {
		return image.stem
	}
}

func (image bootImageConfig) moduleFiles(ctx android.PathContext, dir android.OutputPath, exts ...string) android.OutputPaths {
	ret := make(android.OutputPaths, 0, len(image.modules)*len(exts))
	for i := range image.modules {
		name := image.moduleName(i)
		for _, ext := range exts {
			ret = append(ret, dir.Join(ctx, name+ext))
		}
	}
	return ret
}

// The image "location" is a symbolic path that, with multiarchitecture support, doesn't really
// exist on the device. Typically it is /apex/com.android.art/javalib/boot.art and should be the
// same for all supported architectures on the device. The concrete architecture specific files
// actually end up in architecture-specific sub-directory such as arm, arm64, x86, or x86_64.
//
// For example a physical file
// "/apex/com.android.art/javalib/x86/boot.art" has "image location"
// "/apex/com.android.art/javalib/boot.art" (which is not an actual file).
//
// The location is passed as an argument to the ART tools like dex2oat instead of the real path.
// ART tools will then reconstruct the architecture-specific real path.
func (image *bootImageVariant) imageLocations() (imageLocations []string) {
	if image.extends != nil {
		imageLocations = image.extends.getVariant(image.target).imageLocations()
	}
	return append(imageLocations, dexpreopt.PathToLocation(image.images, image.target.Arch.ArchType))
}

func concat(lists ...[]string) []string {
	var size int
	for _, l := range lists {
		size += len(l)
	}
	ret := make([]string, 0, size)
	for _, l := range lists {
		ret = append(ret, l...)
	}
	return ret
}

func dexpreoptBootJarsFactory() android.Singleton {
	return &dexpreoptBootJars{}
}

func skipDexpreoptBootJars(ctx android.PathContext) bool {
	if dexpreopt.GetGlobalConfig(ctx).DisablePreopt {
		return true
	}

	if ctx.Config().UnbundledBuild() {
		return true
	}

	return false
}

type dexpreoptBootJars struct {
	defaultBootImage *bootImageConfig
	otherImages      []*bootImageConfig

	dexpreoptConfigForMake android.WritablePath
}

// Accessor function for the apex package. Returns nil if dexpreopt is disabled.
func DexpreoptedArtApexJars(ctx android.BuilderContext) map[android.ArchType]android.OutputPaths {
	if skipDexpreoptBootJars(ctx) {
		return nil
	}
	// Include dexpreopt files for the primary boot image.
	files := map[android.ArchType]android.OutputPaths{}
	for _, variant := range artBootImageConfig(ctx).variants {
		// We also generate boot images for host (for testing), but we don't need those in the apex.
		if variant.target.Os == android.Android {
			files[variant.target.Arch.ArchType] = variant.imagesDeps
		}
	}
	return files
}

// dexpreoptBoot singleton rules
func (d *dexpreoptBootJars) GenerateBuildActions(ctx android.SingletonContext) {
	if skipDexpreoptBootJars(ctx) {
		return
	}
	if dexpreopt.GetCachedGlobalSoongConfig(ctx) == nil {
		// No module has enabled dexpreopting, so we assume there will be no boot image to make.
		return
	}

	d.dexpreoptConfigForMake = android.PathForOutput(ctx, ctx.Config().DeviceName(), "dexpreopt.config")
	writeGlobalConfigForMake(ctx, d.dexpreoptConfigForMake)

	global := dexpreopt.GetGlobalConfig(ctx)

	// Skip recompiling the boot image for the second sanitization phase. We'll get separate paths
	// and invalidate first-stage artifacts which are crucial to SANITIZE_LITE builds.
	// Note: this is technically incorrect. Compiled code contains stack checks which may depend
	//       on ASAN settings.
	if len(ctx.Config().SanitizeDevice()) == 1 &&
		ctx.Config().SanitizeDevice()[0] == "address" &&
		global.SanitizeLite {
		return
	}

	// Always create the default boot image first, to get a unique profile rule for all images.
	d.defaultBootImage = buildBootImage(ctx, defaultBootImageConfig(ctx))
	// Create boot image for the ART apex (build artifacts are accessed via the global boot image config).
	d.otherImages = append(d.otherImages, buildBootImage(ctx, artBootImageConfig(ctx)))

	dumpOatRules(ctx, d.defaultBootImage)
}

// buildBootImage takes a bootImageConfig, creates rules to build it, and returns the image.
func buildBootImage(ctx android.SingletonContext, image *bootImageConfig) *bootImageConfig {
	bootDexJars := make(android.Paths, len(image.modules))
	ctx.VisitAllModules(func(module android.Module) {
		// Collect dex jar paths for the modules listed above.
		if j, ok := module.(interface{ DexJar() android.Path }); ok {
			name := ctx.ModuleName(module)
			if i := android.IndexList(name, image.modules); i != -1 {
				bootDexJars[i] = j.DexJar()
			}
		}
	})

	var missingDeps []string
	// Ensure all modules were converted to paths
	for i := range bootDexJars {
		if bootDexJars[i] == nil {
			if ctx.Config().AllowMissingDependencies() {
				missingDeps = append(missingDeps, image.modules[i])
				bootDexJars[i] = android.PathForOutput(ctx, "missing")
			} else {
				ctx.Errorf("failed to find dex jar path for module %q",
					image.modules[i])
			}
		}
	}

	// The path to bootclasspath dex files needs to be known at module GenerateAndroidBuildAction time, before
	// the bootclasspath modules have been compiled.  Copy the dex jars there so the module rules that have
	// already been set up can find them.
	for i := range bootDexJars {
		ctx.Build(pctx, android.BuildParams{
			Rule:   android.Cp,
			Input:  bootDexJars[i],
			Output: image.dexPaths[i],
		})
	}

	profile := bootImageProfileRule(ctx, image, missingDeps)
	bootFrameworkProfileRule(ctx, image, missingDeps)
	updatableBcpPackagesRule(ctx, image, missingDeps)

	var allFiles android.Paths
	for _, variant := range image.variants {
		files := buildBootImageVariant(ctx, variant, profile, missingDeps)
		allFiles = append(allFiles, files.Paths()...)
	}

	if image.zip != nil {
		rule := android.NewRuleBuilder()
		rule.Command().
			BuiltTool(ctx, "soong_zip").
			FlagWithOutput("-o ", image.zip).
			FlagWithArg("-C ", image.dir.String()).
			FlagWithInputList("-f ", allFiles, " -f ")

		rule.Build(pctx, ctx, "zip_"+image.name, "zip "+image.name+" image")
	}

	return image
}

func buildBootImageVariant(ctx android.SingletonContext, image *bootImageVariant,
	profile android.Path, missingDeps []string) android.WritablePaths {

	globalSoong := dexpreopt.GetCachedGlobalSoongConfig(ctx)
	global := dexpreopt.GetGlobalConfig(ctx)

	arch := image.target.Arch.ArchType
	os := image.target.Os.String() // We need to distinguish host-x86 and device-x86.
	symbolsDir := image.symbolsDir.Join(ctx, os, image.installSubdir, arch.String())
	symbolsFile := symbolsDir.Join(ctx, image.stem+".oat")
	outputDir := image.dir.Join(ctx, os, image.installSubdir, arch.String())
	outputPath := outputDir.Join(ctx, image.stem+".oat")
	oatLocation := dexpreopt.PathToLocation(outputPath, arch)
	imagePath := outputPath.ReplaceExtension(ctx, "art")

	rule := android.NewRuleBuilder()
	rule.MissingDeps(missingDeps)

	rule.Command().Text("mkdir").Flag("-p").Flag(symbolsDir.String())
	rule.Command().Text("rm").Flag("-f").
		Flag(symbolsDir.Join(ctx, "*.art").String()).
		Flag(symbolsDir.Join(ctx, "*.oat").String()).
		Flag(symbolsDir.Join(ctx, "*.invocation").String())
	rule.Command().Text("rm").Flag("-f").
		Flag(outputDir.Join(ctx, "*.art").String()).
		Flag(outputDir.Join(ctx, "*.oat").String()).
		Flag(outputDir.Join(ctx, "*.invocation").String())

	cmd := rule.Command()

	extraFlags := ctx.Config().Getenv("ART_BOOT_IMAGE_EXTRA_ARGS")
	if extraFlags == "" {
		// Use ANDROID_LOG_TAGS to suppress most logging by default...
		cmd.Text(`ANDROID_LOG_TAGS="*:e"`)
	} else {
		// ...unless the boot image is generated specifically for testing, then allow all logging.
		cmd.Text(`ANDROID_LOG_TAGS="*:v"`)
	}

	invocationPath := outputPath.ReplaceExtension(ctx, "invocation")

	cmd.Tool(globalSoong.Dex2oat).
		Flag("--avoid-storing-invocation").
		FlagWithOutput("--write-invocation-to=", invocationPath).ImplicitOutput(invocationPath).
		Flag("--runtime-arg").FlagWithArg("-Xms", global.Dex2oatImageXms).
		Flag("--runtime-arg").FlagWithArg("-Xmx", global.Dex2oatImageXmx)

	if profile != nil {
		cmd.FlagWithArg("--compiler-filter=", "speed-profile")
		cmd.FlagWithInput("--profile-file=", profile)
	}

	if global.DirtyImageObjects.Valid() {
		cmd.FlagWithInput("--dirty-image-objects=", global.DirtyImageObjects.Path())
	}

	if image.extends != nil {
		artImage := image.primaryImages
		cmd.
			Flag("--runtime-arg").FlagWithInputList("-Xbootclasspath:", image.dexPathsDeps.Paths(), ":").
			Flag("--runtime-arg").FlagWithList("-Xbootclasspath-locations:", image.dexLocationsDeps, ":").
			FlagWithArg("--boot-image=", dexpreopt.PathToLocation(artImage, arch)).Implicit(artImage)
	} else {
		cmd.FlagWithArg("--base=", ctx.Config().LibartImgDeviceBaseAddress())
	}

	cmd.
		FlagForEachInput("--dex-file=", image.dexPaths.Paths()).
		FlagForEachArg("--dex-location=", image.dexLocations).
		Flag("--generate-debug-info").
		Flag("--generate-build-id").
		Flag("--image-format=lz4hc").
		FlagWithArg("--oat-symbols=", symbolsFile.String()).
		Flag("--strip").
		FlagWithArg("--oat-file=", outputPath.String()).
		FlagWithArg("--oat-location=", oatLocation).
		FlagWithArg("--image=", imagePath.String()).
		FlagWithArg("--instruction-set=", arch.String()).
		FlagWithArg("--android-root=", global.EmptyDirectory).
		FlagWithArg("--no-inline-from=", "core-oj.jar").
		Flag("--force-determinism").
		Flag("--abort-on-hard-verifier-error")

	// Use the default variant/features for host builds.
	// The map below contains only device CPU info (which might be x86 on some devices).
	if image.target.Os == android.Android {
		cmd.FlagWithArg("--instruction-set-variant=", global.CpuVariant[arch])
		cmd.FlagWithArg("--instruction-set-features=", global.InstructionSetFeatures[arch])
	}

	if global.BootFlags != "" {
		cmd.Flag(global.BootFlags)
	}

	if extraFlags != "" {
		cmd.Flag(extraFlags)
	}

	cmd.Textf(`|| ( echo %s ; false )`, proptools.ShellEscape(failureMessage))

	installDir := filepath.Join("/", image.installSubdir, arch.String())

	var vdexInstalls android.RuleBuilderInstalls
	var unstrippedInstalls android.RuleBuilderInstalls

	var zipFiles android.WritablePaths

	for _, artOrOat := range image.moduleFiles(ctx, outputDir, ".art", ".oat") {
		cmd.ImplicitOutput(artOrOat)
		zipFiles = append(zipFiles, artOrOat)

		// Install the .oat and .art files
		rule.Install(artOrOat, filepath.Join(installDir, artOrOat.Base()))
	}

	for _, vdex := range image.moduleFiles(ctx, outputDir, ".vdex") {
		cmd.ImplicitOutput(vdex)
		zipFiles = append(zipFiles, vdex)

		// Note that the vdex files are identical between architectures.
		// Make rules will create symlinks to share them between architectures.
		vdexInstalls = append(vdexInstalls,
			android.RuleBuilderInstall{vdex, filepath.Join(installDir, vdex.Base())})
	}

	for _, unstrippedOat := range image.moduleFiles(ctx, symbolsDir, ".oat") {
		cmd.ImplicitOutput(unstrippedOat)

		// Install the unstripped oat files.  The Make rules will put these in $(TARGET_OUT_UNSTRIPPED)
		unstrippedInstalls = append(unstrippedInstalls,
			android.RuleBuilderInstall{unstrippedOat, filepath.Join(installDir, unstrippedOat.Base())})
	}

	rule.Build(pctx, ctx, image.name+"JarsDexpreopt_"+image.target.String(), "dexpreopt "+image.name+" jars "+arch.String())

	// save output and installed files for makevars
	image.installs = rule.Installs()
	image.vdexInstalls = vdexInstalls
	image.unstrippedInstalls = unstrippedInstalls

	return zipFiles
}

const failureMessage = `ERROR: Dex2oat failed to compile a boot image.
It is likely that the boot classpath is inconsistent.
Rebuild with ART_BOOT_IMAGE_EXTRA_ARGS="--runtime-arg -verbose:verifier" to see verification errors.`

func bootImageProfileRule(ctx android.SingletonContext, image *bootImageConfig, missingDeps []string) android.WritablePath {
	globalSoong := dexpreopt.GetCachedGlobalSoongConfig(ctx)
	global := dexpreopt.GetGlobalConfig(ctx)

	if global.DisableGenerateProfile || ctx.Config().IsPdkBuild() || ctx.Config().UnbundledBuild() {
		return nil
	}
	profile := ctx.Config().Once(bootImageProfileRuleKey, func() interface{} {
		defaultProfile := "frameworks/base/config/boot-image-profile.txt"

		rule := android.NewRuleBuilder()
		rule.MissingDeps(missingDeps)

		var bootImageProfile android.Path
		if len(global.BootImageProfiles) > 1 {
			combinedBootImageProfile := image.dir.Join(ctx, "boot-image-profile.txt")
			rule.Command().Text("cat").Inputs(global.BootImageProfiles).Text(">").Output(combinedBootImageProfile)
			bootImageProfile = combinedBootImageProfile
		} else if len(global.BootImageProfiles) == 1 {
			bootImageProfile = global.BootImageProfiles[0]
		} else if path := android.ExistentPathForSource(ctx, defaultProfile); path.Valid() {
			bootImageProfile = path.Path()
		} else {
			// No profile (not even a default one, which is the case on some branches
			// like master-art-host that don't have frameworks/base).
			// Return nil and continue without profile.
			return nil
		}

		profile := image.dir.Join(ctx, "boot.prof")

		rule.Command().
			Text(`ANDROID_LOG_TAGS="*:e"`).
			Tool(globalSoong.Profman).
			FlagWithInput("--create-profile-from=", bootImageProfile).
			FlagForEachInput("--apk=", image.dexPathsDeps.Paths()).
			FlagForEachArg("--dex-location=", image.getAnyAndroidVariant().dexLocationsDeps).
			FlagWithOutput("--reference-profile-file=", profile)

		rule.Install(profile, "/system/etc/boot-image.prof")

		rule.Build(pctx, ctx, "bootJarsProfile", "profile boot jars")

		image.profileInstalls = rule.Installs()

		return profile
	})
	if profile == nil {
		return nil // wrap nil into a typed pointer with value nil
	}
	return profile.(android.WritablePath)
}

var bootImageProfileRuleKey = android.NewOnceKey("bootImageProfileRule")

func bootFrameworkProfileRule(ctx android.SingletonContext, image *bootImageConfig, missingDeps []string) android.WritablePath {
	globalSoong := dexpreopt.GetCachedGlobalSoongConfig(ctx)
	global := dexpreopt.GetGlobalConfig(ctx)

	if global.DisableGenerateProfile || ctx.Config().IsPdkBuild() || ctx.Config().UnbundledBuild() {
		return nil
	}
	return ctx.Config().Once(bootFrameworkProfileRuleKey, func() interface{} {
		rule := android.NewRuleBuilder()
		rule.MissingDeps(missingDeps)

		// Some branches like master-art-host don't have frameworks/base, so manually
		// handle the case that the default is missing.  Those branches won't attempt to build the profile rule,
		// and if they do they'll get a missing deps error.
		defaultProfile := "frameworks/base/config/boot-profile.txt"
		path := android.ExistentPathForSource(ctx, defaultProfile)
		var bootFrameworkProfile android.Path
		if path.Valid() {
			bootFrameworkProfile = path.Path()
		} else {
			missingDeps = append(missingDeps, defaultProfile)
			bootFrameworkProfile = android.PathForOutput(ctx, "missing")
		}

		profile := image.dir.Join(ctx, "boot.bprof")

		rule.Command().
			Text(`ANDROID_LOG_TAGS="*:e"`).
			Tool(globalSoong.Profman).
			Flag("--generate-boot-profile").
			FlagWithInput("--create-profile-from=", bootFrameworkProfile).
			FlagForEachInput("--apk=", image.dexPathsDeps.Paths()).
			FlagForEachArg("--dex-location=", image.getAnyAndroidVariant().dexLocationsDeps).
			FlagWithOutput("--reference-profile-file=", profile)

		rule.Install(profile, "/system/etc/boot-image.bprof")
		rule.Build(pctx, ctx, "bootFrameworkProfile", "profile boot framework jars")
		image.profileInstalls = append(image.profileInstalls, rule.Installs()...)

		return profile
	}).(android.WritablePath)
}

var bootFrameworkProfileRuleKey = android.NewOnceKey("bootFrameworkProfileRule")

func updatableBcpPackagesRule(ctx android.SingletonContext, image *bootImageConfig, missingDeps []string) android.WritablePath {
	if ctx.Config().IsPdkBuild() || ctx.Config().UnbundledBuild() {
		return nil
	}

	return ctx.Config().Once(updatableBcpPackagesRuleKey, func() interface{} {
		global := dexpreopt.GetGlobalConfig(ctx)
		updatableModules := dexpreopt.GetJarsFromApexJarPairs(global.UpdatableBootJars)

		// Collect `permitted_packages` for updatable boot jars.
		var updatablePackages []string
		ctx.VisitAllModules(func(module android.Module) {
			if j, ok := module.(*Library); ok {
				name := ctx.ModuleName(module)
				if i := android.IndexList(name, updatableModules); i != -1 {
					pp := j.properties.Permitted_packages
					if len(pp) > 0 {
						updatablePackages = append(updatablePackages, pp...)
					} else {
						ctx.Errorf("Missing permitted_packages for %s", name)
					}
					// Do not match the same library repeatedly.
					updatableModules = append(updatableModules[:i], updatableModules[i+1:]...)
				}
			}
		})

		// Sort updatable packages to ensure deterministic ordering.
		sort.Strings(updatablePackages)

		updatableBcpPackagesName := "updatable-bcp-packages.txt"
		updatableBcpPackages := image.dir.Join(ctx, updatableBcpPackagesName)

		ctx.Build(pctx, android.BuildParams{
			Rule:   android.WriteFile,
			Output: updatableBcpPackages,
			Args: map[string]string{
				// WriteFile automatically adds the last end-of-line.
				"content": strings.Join(updatablePackages, "\\n"),
			},
		})

		rule := android.NewRuleBuilder()
		rule.MissingDeps(missingDeps)
		rule.Install(updatableBcpPackages, "/system/etc/"+updatableBcpPackagesName)
		// TODO: Rename `profileInstalls` to `extraInstalls`?
		// Maybe even move the field out of the bootImageConfig into some higher level type?
		image.profileInstalls = append(image.profileInstalls, rule.Installs()...)

		return updatableBcpPackages
	}).(android.WritablePath)
}

var updatableBcpPackagesRuleKey = android.NewOnceKey("updatableBcpPackagesRule")

func dumpOatRules(ctx android.SingletonContext, image *bootImageConfig) {
	var allPhonies android.Paths
	for _, image := range image.variants {
		arch := image.target.Arch.ArchType
		suffix := arch.String()
		// Host and target might both use x86 arch. We need to ensure the names are unique.
		if image.target.Os.Class == android.Host {
			suffix = "host-" + suffix
		}
		// Create a rule to call oatdump.
		output := android.PathForOutput(ctx, "boot."+suffix+".oatdump.txt")
		rule := android.NewRuleBuilder()
		rule.Command().
			// TODO: for now, use the debug version for better error reporting
			BuiltTool(ctx, "oatdumpd").
			FlagWithInputList("--runtime-arg -Xbootclasspath:", image.dexPathsDeps.Paths(), ":").
			FlagWithList("--runtime-arg -Xbootclasspath-locations:", image.dexLocationsDeps, ":").
			FlagWithArg("--image=", strings.Join(image.imageLocations(), ":")).Implicits(image.imagesDeps.Paths()).
			FlagWithOutput("--output=", output).
			FlagWithArg("--instruction-set=", arch.String())
		rule.Build(pctx, ctx, "dump-oat-boot-"+suffix, "dump oat boot "+arch.String())

		// Create a phony rule that depends on the output file and prints the path.
		phony := android.PathForPhony(ctx, "dump-oat-boot-"+suffix)
		rule = android.NewRuleBuilder()
		rule.Command().
			Implicit(output).
			ImplicitOutput(phony).
			Text("echo").FlagWithArg("Output in ", output.String())
		rule.Build(pctx, ctx, "phony-dump-oat-boot-"+suffix, "dump oat boot "+arch.String())

		allPhonies = append(allPhonies, phony)
	}

	phony := android.PathForPhony(ctx, "dump-oat-boot")
	ctx.Build(pctx, android.BuildParams{
		Rule:        android.Phony,
		Output:      phony,
		Inputs:      allPhonies,
		Description: "dump-oat-boot",
	})

}

func writeGlobalConfigForMake(ctx android.SingletonContext, path android.WritablePath) {
	data := dexpreopt.GetGlobalConfigRawData(ctx)

	ctx.Build(pctx, android.BuildParams{
		Rule:   android.WriteFile,
		Output: path,
		Args: map[string]string{
			"content": string(data),
		},
	})
}

// Export paths for default boot image to Make
func (d *dexpreoptBootJars) MakeVars(ctx android.MakeVarsContext) {
	if d.dexpreoptConfigForMake != nil {
		ctx.Strict("DEX_PREOPT_CONFIG_FOR_MAKE", d.dexpreoptConfigForMake.String())
		ctx.Strict("DEX_PREOPT_SOONG_CONFIG_FOR_MAKE", android.PathForOutput(ctx, "dexpreopt_soong.config").String())
	}

	image := d.defaultBootImage
	if image != nil {
		ctx.Strict("DEXPREOPT_IMAGE_PROFILE_BUILT_INSTALLED", image.profileInstalls.String())
		ctx.Strict("DEXPREOPT_BOOTCLASSPATH_DEX_FILES", strings.Join(image.dexPathsDeps.Strings(), " "))
		ctx.Strict("DEXPREOPT_BOOTCLASSPATH_DEX_LOCATIONS", strings.Join(image.getAnyAndroidVariant().dexLocationsDeps, " "))

		var imageNames []string
		for _, current := range append(d.otherImages, image) {
			imageNames = append(imageNames, current.name)
			for _, variant := range current.variants {
				suffix := ""
				if variant.target.Os.Class == android.Host {
					suffix = "_host"
				}
				sfx := variant.name + suffix + "_" + variant.target.Arch.ArchType.String()
				ctx.Strict("DEXPREOPT_IMAGE_VDEX_BUILT_INSTALLED_"+sfx, variant.vdexInstalls.String())
				ctx.Strict("DEXPREOPT_IMAGE_"+sfx, variant.images.String())
				ctx.Strict("DEXPREOPT_IMAGE_DEPS_"+sfx, strings.Join(variant.imagesDeps.Strings(), " "))
				ctx.Strict("DEXPREOPT_IMAGE_BUILT_INSTALLED_"+sfx, variant.installs.String())
				ctx.Strict("DEXPREOPT_IMAGE_UNSTRIPPED_BUILT_INSTALLED_"+sfx, variant.unstrippedInstalls.String())
			}
			imageLocations := current.getAnyAndroidVariant().imageLocations()
			ctx.Strict("DEXPREOPT_IMAGE_LOCATIONS_"+current.name, strings.Join(imageLocations, ":"))
			ctx.Strict("DEXPREOPT_IMAGE_ZIP_"+current.name, current.zip.String())
		}
		ctx.Strict("DEXPREOPT_IMAGE_NAMES", strings.Join(imageNames, " "))
	}
}
