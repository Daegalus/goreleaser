// Package nfpm implements the Pipe interface providing nFPM bindings.
package nfpm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caarlos0/log"
	"github.com/goreleaser/goreleaser/internal/artifact"
	"github.com/goreleaser/goreleaser/internal/deprecate"
	"github.com/goreleaser/goreleaser/internal/ids"
	"github.com/goreleaser/goreleaser/internal/pipe"
	"github.com/goreleaser/goreleaser/internal/semerrgroup"
	"github.com/goreleaser/goreleaser/internal/tmpl"
	"github.com/goreleaser/goreleaser/pkg/config"
	"github.com/goreleaser/goreleaser/pkg/context"
	"github.com/goreleaser/nfpm/v2"
	"github.com/goreleaser/nfpm/v2/deprecation"
	"github.com/goreleaser/nfpm/v2/files"
	"github.com/imdario/mergo"

	_ "github.com/goreleaser/nfpm/v2/apk"  // blank import to register the format
	_ "github.com/goreleaser/nfpm/v2/arch" // blank import to register the format
	_ "github.com/goreleaser/nfpm/v2/deb"  // blank import to register the format
	_ "github.com/goreleaser/nfpm/v2/rpm"  // blank import to register the format
)

const (
	defaultNameTemplate = `{{ .PackageName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}{{ with .Mips }}_{{ . }}{{ end }}{{ if not (eq .Amd64 "v1") }}{{ .Amd64 }}{{ end }}`
	extraFiles          = "Files"
)

// Pipe for nfpm packaging.
type Pipe struct{}

func (Pipe) String() string                 { return "linux packages" }
func (Pipe) Skip(ctx *context.Context) bool { return len(ctx.Config.NFPMs) == 0 }

// Default sets the pipe defaults.
func (Pipe) Default(ctx *context.Context) error {
	ids := ids.New("nfpms")
	for i := range ctx.Config.NFPMs {
		fpm := &ctx.Config.NFPMs[i]
		if fpm.ID == "" {
			fpm.ID = "default"
		}
		if fpm.Bindir == "" {
			fpm.Bindir = "/usr/bin"
		}
		if fpm.PackageName == "" {
			fpm.PackageName = ctx.Config.ProjectName
		}
		if fpm.FileNameTemplate == "" {
			fpm.FileNameTemplate = defaultNameTemplate
		}
		if fpm.Maintainer == "" {
			deprecate.NoticeCustom(ctx, "nfpms.maintainer", "`{{ .Property }}` should always be set, check {{ .URL }} for more info")
		}
		if len(fpm.Replacements) != 0 {
			deprecate.Notice(ctx, "nfpms.replacements")
		}
		ids.Inc(fpm.ID)
	}

	deprecation.Noticer = io.Discard
	return ids.Validate()
}

// Run the pipe.
func (Pipe) Run(ctx *context.Context) error {
	for _, nfpm := range ctx.Config.NFPMs {
		if len(nfpm.Formats) == 0 {
			// FIXME: this assumes other nfpm configs will fail too...
			return pipe.Skip("no output formats configured")
		}
		if err := doRun(ctx, nfpm); err != nil {
			return err
		}
	}
	return nil
}

func doRun(ctx *context.Context, fpm config.NFPM) error {
	filters := []artifact.Filter{
		artifact.ByType(artifact.Binary),
		artifact.Or(
			artifact.ByGoos("linux"),
			artifact.ByGoos("ios"),
		),
	}
	if len(fpm.Builds) > 0 {
		filters = append(filters, artifact.ByIDs(fpm.Builds...))
	}
	linuxBinaries := ctx.Artifacts.
		Filter(artifact.And(filters...)).
		GroupByPlatform()
	if len(linuxBinaries) == 0 {
		return fmt.Errorf("no linux binaries found for builds %v", fpm.Builds)
	}
	g := semerrgroup.New(ctx.Parallelism)
	for _, format := range fpm.Formats {
		for _, artifacts := range linuxBinaries {
			format := format
			artifacts := artifacts
			g.Go(func() error {
				return create(ctx, fpm, format, artifacts)
			})
		}
	}
	return g.Wait()
}

func mergeOverrides(fpm config.NFPM, format string) (*config.NFPMOverridables, error) {
	var overridden config.NFPMOverridables
	if err := mergo.Merge(&overridden, fpm.NFPMOverridables); err != nil {
		return nil, err
	}
	perFormat, ok := fpm.Overrides[format]
	if ok {
		err := mergo.Merge(&overridden, perFormat, mergo.WithOverride)
		if err != nil {
			return nil, err
		}
	}
	return &overridden, nil
}

const termuxFormat = "termux.deb"

func isSupportedTermuxArch(arch string) bool {
	for _, a := range []string{"amd64", "arm64", "386"} {
		if strings.HasPrefix(arch, a) {
			return true
		}
	}
	return false
}

func create(ctx *context.Context, fpm config.NFPM, format string, binaries []*artifact.Artifact) error {
	// TODO: improve mips handling on nfpm
	infoArch := binaries[0].Goarch + binaries[0].Goarm + binaries[0].Gomips // key used for the ConventionalFileName et al
	arch := infoArch + binaries[0].Goamd64                                  // unique arch key
	infoPlatform := binaries[0].Goos
	if infoPlatform == "ios" {
		if format == "deb" {
			infoPlatform = "iphoneos-arm64"
		} else {
			return nil
		}
	}

	bindDir := fpm.Bindir
	if format == termuxFormat {
		if !isSupportedTermuxArch(arch) {
			log.Debugf("skipping termux.deb for %s as its not supported by termux", arch)
			return nil
		}

		replacer := strings.NewReplacer(
			"386", "i686",
			"amd64", "x86_64",
			"arm64", "aarch64",
		)
		infoArch = replacer.Replace(infoArch)
		arch = replacer.Replace(arch)
		bindDir = filepath.Join("/data/data/com.termux/files", bindDir)
	}

	overridden, err := mergeOverrides(fpm, format)
	if err != nil {
		return err
	}
	// nolint:staticcheck
	t := tmpl.New(ctx).
		WithArtifactReplacements(binaries[0], overridden.Replacements).
		WithExtraFields(tmpl.Fields{
			"Release":     fpm.Release,
			"Epoch":       fpm.Epoch,
			"PackageName": fpm.PackageName,
		})

	binDir, err := t.Apply(bindDir)
	if err != nil {
		return err
	}

	homepage, err := t.Apply(fpm.Homepage)
	if err != nil {
		return err
	}

	description, err := t.Apply(fpm.Description)
	if err != nil {
		return err
	}

	maintainer, err := t.Apply(fpm.Maintainer)
	if err != nil {
		return err
	}

	debKeyFile, err := t.Apply(overridden.Deb.Signature.KeyFile)
	if err != nil {
		return err
	}

	rpmKeyFile, err := t.Apply(overridden.RPM.Signature.KeyFile)
	if err != nil {
		return err
	}

	apkKeyFile, err := t.Apply(overridden.APK.Signature.KeyFile)
	if err != nil {
		return err
	}

	apkKeyName, err := t.Apply(overridden.APK.Signature.KeyName)
	if err != nil {
		return err
	}

	contents := files.Contents{}
	for _, content := range overridden.Contents {
		src, err := t.Apply(content.Source)
		if err != nil {
			return err
		}
		dst, err := t.Apply(content.Destination)
		if err != nil {
			return err
		}
		contents = append(contents, &files.Content{
			Source:      src,
			Destination: dst,
			Type:        content.Type,
			Packager:    content.Packager,
			FileInfo:    content.FileInfo,
		})
	}

	if len(fpm.Deb.Lintian) > 0 {
		lines := make([]string, 0, len(fpm.Deb.Lintian))
		for _, ov := range fpm.Deb.Lintian {
			lines = append(lines, fmt.Sprintf("%s: %s", fpm.PackageName, ov))
		}
		lintianPath := filepath.Join(ctx.Config.Dist, "deb", fpm.PackageName+"_"+arch, ".lintian")
		if err := os.MkdirAll(filepath.Dir(lintianPath), 0o755); err != nil {
			return fmt.Errorf("failed to write lintian file: %w", err)
		}
		if err := os.WriteFile(lintianPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return fmt.Errorf("failed to write lintian file: %w", err)
		}

		log.Debugf("creating %q", lintianPath)
		contents = append(contents, &files.Content{
			Source:      lintianPath,
			Destination: filepath.Join("./usr/share/lintian/overrides", fpm.PackageName),
			Packager:    "deb",
			FileInfo: &files.ContentFileInfo{
				Mode: 0o644,
			},
		})
	}

	log := log.WithField("package", fpm.PackageName).WithField("format", format).WithField("arch", arch)

	// FPM meta package should not contain binaries at all
	if !fpm.Meta {
		for _, binary := range binaries {
			src := binary.Path
			dst := filepath.Join(binDir, binary.Name)
			log.WithField("src", src).WithField("dst", dst).Debug("adding binary to package")
			contents = append(contents, &files.Content{
				Source:      filepath.ToSlash(src),
				Destination: filepath.ToSlash(dst),
				FileInfo: &files.ContentFileInfo{
					Mode: 0o755,
				},
			})
		}
	}

	log.WithField("files", destinations(contents)).Debug("all archive files")

	info := &nfpm.Info{
		Arch:            infoArch,
		Platform:        infoPlatform,
		Name:            fpm.PackageName,
		Version:         ctx.Version,
		Section:         fpm.Section,
		Priority:        fpm.Priority,
		Epoch:           fpm.Epoch,
		Release:         fpm.Release,
		Prerelease:      fpm.Prerelease,
		VersionMetadata: fpm.VersionMetadata,
		Maintainer:      maintainer,
		Description:     description,
		Vendor:          fpm.Vendor,
		Homepage:        homepage,
		License:         fpm.License,
		Changelog:       fpm.Changelog,
		Overridables: nfpm.Overridables{
			Conflicts:  overridden.Conflicts,
			Depends:    overridden.Dependencies,
			Recommends: overridden.Recommends,
			Provides:   overridden.Provides,
			Suggests:   overridden.Suggests,
			Replaces:   overridden.Replaces,
			Contents:   contents,
			Scripts: nfpm.Scripts{
				PreInstall:  overridden.Scripts.PreInstall,
				PostInstall: overridden.Scripts.PostInstall,
				PreRemove:   overridden.Scripts.PreRemove,
				PostRemove:  overridden.Scripts.PostRemove,
			},
			Deb: nfpm.Deb{
				// TODO: Compression, Fields
				Scripts: nfpm.DebScripts{
					Rules:     overridden.Deb.Scripts.Rules,
					Templates: overridden.Deb.Scripts.Templates,
				},
				Triggers: nfpm.DebTriggers{
					Interest:        overridden.Deb.Triggers.Interest,
					InterestAwait:   overridden.Deb.Triggers.InterestAwait,
					InterestNoAwait: overridden.Deb.Triggers.InterestNoAwait,
					Activate:        overridden.Deb.Triggers.Activate,
					ActivateAwait:   overridden.Deb.Triggers.ActivateAwait,
					ActivateNoAwait: overridden.Deb.Triggers.ActivateNoAwait,
				},
				Breaks: overridden.Deb.Breaks,
				Signature: nfpm.DebSignature{
					PackageSignature: nfpm.PackageSignature{
						KeyFile:       debKeyFile,
						KeyPassphrase: getPassphraseFromEnv(ctx, "DEB", fpm.ID),
						// TODO: Method, Type, KeyID
					},
					Type: overridden.Deb.Signature.Type,
				},
			},
			RPM: nfpm.RPM{
				Summary:     overridden.RPM.Summary,
				Group:       overridden.RPM.Group,
				Compression: overridden.RPM.Compression,
				Signature: nfpm.RPMSignature{
					PackageSignature: nfpm.PackageSignature{
						KeyFile:       rpmKeyFile,
						KeyPassphrase: getPassphraseFromEnv(ctx, "RPM", fpm.ID),
						// TODO: KeyID
					},
				},
				Scripts: nfpm.RPMScripts{
					PreTrans:  overridden.RPM.Scripts.PreTrans,
					PostTrans: overridden.RPM.Scripts.PostTrans,
				},
			},
			APK: nfpm.APK{
				Signature: nfpm.APKSignature{
					PackageSignature: nfpm.PackageSignature{
						KeyFile:       apkKeyFile,
						KeyPassphrase: getPassphraseFromEnv(ctx, "APK", fpm.ID),
					},
					KeyName: apkKeyName,
				},
				Scripts: nfpm.APKScripts{
					PreUpgrade:  overridden.APK.Scripts.PreUpgrade,
					PostUpgrade: overridden.APK.Scripts.PostUpgrade,
				},
			},
			ArchLinux: nfpm.ArchLinux{
				Pkgbase:  overridden.ArchLinux.Pkgbase,
				Packager: overridden.ArchLinux.Packager,
				Scripts: nfpm.ArchLinuxScripts{
					PreUpgrade:  overridden.ArchLinux.Scripts.PreUpgrade,
					PostUpgrade: overridden.ArchLinux.Scripts.PostUpgrade,
				},
			},
		},
	}

	if ctx.SkipSign {
		info.APK.Signature = nfpm.APKSignature{}
		info.RPM.Signature = nfpm.RPMSignature{}
		info.Deb.Signature = nfpm.DebSignature{}
	}

	packager, err := nfpm.Get(strings.Replace(format, "termux.", "", 1))
	if err != nil {
		return err
	}

	info = nfpm.WithDefaults(info)
	name, err := t.WithExtraFields(tmpl.Fields{
		"ConventionalFileName": packager.ConventionalFileName(info),
	}).Apply(overridden.FileNameTemplate)
	if err != nil {
		return err
	}

	ext := "." + format
	if packager, ok := packager.(nfpm.PackagerWithExtension); ok {
		if format != "termux.deb" {
			ext = packager.ConventionalExtension()
		}
	}

	if !strings.HasSuffix(name, ext) {
		name = name + ext
	}

	path := filepath.Join(ctx.Config.Dist, name)
	log.WithField("file", path).Info("creating")
	w, err := os.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()

	if err := packager.Package(info, w); err != nil {
		return fmt.Errorf("nfpm failed for %s: %w", name, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("could not close package file: %w", err)
	}
	ctx.Artifacts.Add(&artifact.Artifact{
		Type:    artifact.LinuxPackage,
		Name:    name,
		Path:    path,
		Goos:    binaries[0].Goos,
		Goarch:  binaries[0].Goarch,
		Goarm:   binaries[0].Goarm,
		Gomips:  binaries[0].Gomips,
		Goamd64: binaries[0].Goamd64,
		Extra: map[string]interface{}{
			artifact.ExtraBuilds: binaries,
			artifact.ExtraID:     fpm.ID,
			artifact.ExtraFormat: format,
			extraFiles:           contents,
		},
	})
	return nil
}

func destinations(contents files.Contents) []string {
	result := make([]string, 0, len(contents))
	for _, f := range contents {
		result = append(result, f.Destination)
	}
	return result
}

func getPassphraseFromEnv(ctx *context.Context, packager string, nfpmID string) string {
	var passphrase string

	nfpmID = strings.ToUpper(nfpmID)
	packagerSpecificPassphrase := ctx.Env[fmt.Sprintf(
		"NFPM_%s_%s_PASSPHRASE",
		nfpmID,
		packager,
	)]
	if packagerSpecificPassphrase != "" {
		passphrase = packagerSpecificPassphrase
	} else {
		generalPassphrase := ctx.Env[fmt.Sprintf("NFPM_%s_PASSPHRASE", nfpmID)]
		passphrase = generalPassphrase
	}

	return passphrase
}
