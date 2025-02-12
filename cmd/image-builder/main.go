package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/osbuild/bootc-image-builder/bib/pkg/progress"
	"github.com/osbuild/images/pkg/arch"
	"github.com/osbuild/images/pkg/imagefilter"
	"github.com/osbuild/images/pkg/osbuild"
	"github.com/osbuild/images/pkg/ostree"

	"github.com/osbuild/image-builder-cli/internal/blueprintload"
)

var (
	osStdout io.Writer = os.Stdout
	osStderr io.Writer = os.Stderr
)

// generate the default output directory name for the given image
func outputNameFor(img *imagefilter.Result) string {
	return fmt.Sprintf("%s-%s-%s", img.Distro.Name(), img.ImgType.Name(), img.Arch.Name())
}

func cmdListImages(cmd *cobra.Command, args []string) error {
	filter, err := cmd.Flags().GetStringArray("filter")
	if err != nil {
		return err
	}
	output, err := cmd.Flags().GetString("output")
	if err != nil {
		return err
	}
	dataDir, err := cmd.Flags().GetString("datadir")
	if err != nil {
		return err
	}
	extraRepos, err := cmd.Flags().GetStringArray("extra-repo")
	if err != nil {
		return err
	}

	return listImages(dataDir, extraRepos, output, filter)
}

func ostreeImageOptions(cmd *cobra.Command) (*ostree.ImageOptions, error) {
	imageRef, err := cmd.Flags().GetString("ostree-ref")
	if err != nil {
		return nil, err
	}
	parentRef, err := cmd.Flags().GetString("ostree-parent")
	if err != nil {
		return nil, err
	}
	url, err := cmd.Flags().GetString("ostree-url")
	if err != nil {
		return nil, err
	}
	if imageRef == "" && parentRef == "" && url == "" {
		return nil, nil
	}

	// XXX: how to add RHSM?
	return &ostree.ImageOptions{
		ImageRef:  imageRef,
		ParentRef: parentRef,
		URL:       url,
	}, nil
}

func cmdManifestWrapper(pbar progress.ProgressBar, cmd *cobra.Command, args []string, w io.Writer, archChecker func(string) error) (*imagefilter.Result, error) {
	dataDir, err := cmd.Flags().GetString("datadir")
	if err != nil {
		return nil, err
	}
	extraRepos, err := cmd.Flags().GetStringArray("extra-repo")
	if err != nil {
		return nil, err
	}
	forceRepos, err := cmd.Flags().GetStringArray("force-repo")
	if err != nil {
		return nil, err
	}
	archStr, err := cmd.Flags().GetString("arch")
	if err != nil {
		return nil, err
	}
	if archStr == "" {
		archStr = arch.Current().String()
	}
	distroStr, err := cmd.Flags().GetString("distro")
	if err != nil {
		return nil, err
	}
	withSBOM, err := cmd.Flags().GetBool("with-sbom")
	if err != nil {
		return nil, err
	}
	outputDir, err := cmd.Flags().GetString("output-dir")
	if err != nil {
		return nil, err
	}
	ostreeImgOpts, err := ostreeImageOptions(cmd)
	if err != nil {
		return nil, err
	}
	useLibrepo, err := cmd.Flags().GetBool("use-librepo")
	if err != nil {
		return nil, err
	}
	var rpmDownloader osbuild.RpmDownloader
	if useLibrepo {
		rpmDownloader = osbuild.RpmDownloaderLibrepo
	}

	blueprintPath, err := cmd.Flags().GetString("blueprint")
	if err != nil {
		return nil, err
	}

	bp, err := blueprintload.Load(blueprintPath)
	if err != nil {
		return nil, err
	}

	distroStr, err = findDistro(distroStr, bp.Distro)
	if err != nil {
		return nil, err
	}
	imgTypeStr := args[0]
	pbar.SetPulseMsgf("Manifest generation step")
	pbar.SetMessagef("Building manifest for %s-%s", distroStr, imgTypeStr)

	repoOpts := &repoOptions{
		DataDir:    dataDir,
		ExtraRepos: extraRepos,
		ForceRepos: forceRepos,
	}
	img, err := getOneImage(distroStr, imgTypeStr, archStr, repoOpts)
	if err != nil {
		return nil, err
	}
	if archChecker != nil {
		if err := archChecker(img.Arch.Name()); err != nil {
			return nil, err
		}
	}

	opts := &manifestOptions{
		OutputDir:     outputDir,
		BlueprintPath: blueprintPath,
		Ostree:        ostreeImgOpts,
		RpmDownloader: rpmDownloader,
		WithSBOM:      withSBOM,

		ForceRepos: forceRepos,
	}
	err = generateManifest(dataDir, extraRepos, img, w, opts)
	return img, err
}

func cmdManifest(cmd *cobra.Command, args []string) error {
	pbar, err := progress.New("")
	if err != nil {
		return err
	}
	_, err = cmdManifestWrapper(pbar, cmd, args, osStdout, nil)
	return err
}

func progressFromCmd(cmd *cobra.Command) (progress.ProgressBar, error) {
	progressType, err := cmd.Flags().GetString("progress")
	if err != nil {
		return nil, err
	}
	verbose, err := cmd.Flags().GetBool("verbose")
	if err != nil {
		return nil, err
	}
	if progressType == "auto" && verbose {
		progressType = "verbose"
	}

	return progress.New(progressType)
}

func cmdBuild(cmd *cobra.Command, args []string) error {
	cacheDir, err := cmd.Flags().GetString("cache")
	if err != nil {
		return err
	}
	outputDir, err := cmd.Flags().GetString("output-dir")
	if err != nil {
		return err
	}
	withManifest, err := cmd.Flags().GetBool("with-manifest")
	if err != nil {
		return err
	}
	pbar, err := progressFromCmd(cmd)
	if err != nil {
		return err
	}
	pbar.Start()
	defer pbar.Stop()
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		pbar.Stop()
	}()

	var mf bytes.Buffer
	// XXX: check env here, i.e. if user is root and osbuild is installed
	res, err := cmdManifestWrapper(pbar, cmd, args, &mf, func(archStr string) error {
		if archStr != arch.Current().String() {
			return fmt.Errorf("cannot build for arch %q from %q", archStr, arch.Current().String())
		}
		return nil
	})
	if err != nil {
		return err
	}

	var uploadUnsupported *UploadTypeUnsupportedError
	var missingUploadConfig *MissingUploadConfigError
	uploader, err := uploaderFor(cmd, res.ImgType.Name())
	if err != nil && !errors.As(err, &missingUploadConfig) && !errors.As(err, &uploadUnsupported) {
		return err
	}
	if missingUploadConfig != nil && !missingUploadConfig.allMissing {
		return fmt.Errorf("partial upload config provided: %w", err)
	}

	if uploader != nil {
		pbar.SetPulseMsgf("Checking cloud access")
		if err := uploaderCheckWithProgress(pbar, uploader); err != nil {
			return err
		}
	}

	// XXX: support output filename via commandline (c.f.
	//   https://github.com/osbuild/images/pull/1039)
	if outputDir == "" {
		outputDir = outputNameFor(res)
	}
	buildOpts := &buildOptions{
		OutputDir:     outputDir,
		StoreDir:      cacheDir,
		WriteManifest: withManifest,
	}
	pbar.SetPulseMsgf("Image building step")
	if err := buildImage(pbar, res, mf.Bytes(), buildOpts); err != nil {
		return err
	}

	if uploader != nil {
		// XXX: integrate better into the progress, see bib
		pbar.Stop()
		imagePath := filepath.Join(outputDir, res.ImgType.Name(), res.ImgType.Filename())

		if err := uploadImageWithProgress(uploader, imagePath); err != nil {
			return err
		}
	}

	return nil
}

func cmdDescribeImg(cmd *cobra.Command, args []string) error {
	// XXX: boilderplate identical to cmdManifest() above
	dataDir, err := cmd.Flags().GetString("datadir")
	if err != nil {
		return err
	}
	distroStr, err := cmd.Flags().GetString("distro")
	if err != nil {
		return err
	}
	archStr, err := cmd.Flags().GetString("arch")
	if err != nil {
		return err
	}
	if archStr == "" {
		archStr = arch.Current().String()
	}
	imgTypeStr := args[0]
	res, err := getOneImage(distroStr, imgTypeStr, archStr, &repoOptions{DataDir: dataDir})
	if err != nil {
		return err
	}

	return describeImage(res, osStdout)
}

func run() error {
	// images generates a lot of noisy logs a bunch of stuff to
	// Debug/Info that is distracting the user (at least by
	// default, like what repos being loaded)
	//
	// Disable for now until we can filter out the usless log
	// messages.
	logrus.SetOutput(io.Discard)

	rootCmd := &cobra.Command{
		Use:   "image-builder",
		Short: "Build operating system images from a given distro/image-type/blueprint",
		Long: `Build operating system images from a given distribution,
image-type and blueprint.

Image-builder builds operating system images for a range of predefined
operating systems like Fedora, CentOS and RHEL with easy customizations support.`,
		SilenceErrors: true,
	}
	rootCmd.PersistentFlags().String("datadir", "", `Override the default data directory for e.g. custom repositories/*.json data`)
	rootCmd.PersistentFlags().StringArray("extra-repo", nil, `Add an extra repository during build (will *not* be gpg checked and not be part of the final image)`)
	rootCmd.PersistentFlags().StringArray("force-repo", nil, `Override the base repositories during build (these will not be part of the final image)`)
	rootCmd.PersistentFlags().String("output-dir", "", `Put output into the specified directory`)
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, `Switch to verbose mode`)
	rootCmd.SetOut(osStdout)
	rootCmd.SetErr(osStderr)

	listImagesCmd := &cobra.Command{
		Use:          "list-images",
		Short:        "List buildable images, use --filter to limit further",
		RunE:         cmdListImages,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
	}
	listImagesCmd.Flags().StringArray("filter", nil, `Filter distributions by a specific criteria (e.g. "type:iot*")`)
	listImagesCmd.Flags().String("output", "", "Output in a specific format (text, json)")
	rootCmd.AddCommand(listImagesCmd)

	manifestCmd := &cobra.Command{
		Use:          "manifest <image-type>",
		Short:        "Build manifest for the given image-type, e.g. qcow2 (tip: combine with --distro, --arch)",
		RunE:         cmdManifest,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Hidden:       true,
	}
	manifestCmd.Flags().String("blueprint", "", `filename of a blueprint to customize an image`)
	manifestCmd.Flags().String("arch", "", `build manifest for a different architecture`)
	manifestCmd.Flags().String("distro", "", `build manifest for a different distroname (e.g. centos-9)`)
	manifestCmd.Flags().String("ostree-ref", "", `OSTREE reference`)
	manifestCmd.Flags().String("ostree-parent", "", `OSTREE parent`)
	manifestCmd.Flags().String("ostree-url", "", `OSTREE url`)
	manifestCmd.Flags().Bool("use-librepo", true, `use librepo to download packages (disable if you use old versions of osbuild)`)
	manifestCmd.Flags().Bool("with-sbom", false, `export SPDX SBOM document`)
	rootCmd.AddCommand(manifestCmd)

	uploadCmd := &cobra.Command{
		Use:          "upload <image-path>",
		Short:        "Upload the given image from <image-path>",
		RunE:         cmdUpload,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
	}
	uploadCmd.Flags().String("aws-ami-name", "", "name for the AMI in AWS (only for type=ami)")
	uploadCmd.Flags().String("aws-bucket", "", "target S3 bucket name for intermediate storage when creating AMI (only for type=ami)")
	uploadCmd.Flags().String("aws-region", "", "target region for AWS uploads (only for type=ami)")
	rootCmd.AddCommand(uploadCmd)

	buildCmd := &cobra.Command{
		Use:          "build <image-type>",
		Short:        "Build the given image-type, e.g. qcow2 (tip: combine with --distro, --arch)",
		RunE:         cmdBuild,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
	}
	buildCmd.Flags().AddFlagSet(manifestCmd.Flags())
	buildCmd.Flags().Bool("with-manifest", false, `export osbuild manifest`)
	// XXX: add --rpmmd cache too and put under /var/cache/image-builder/dnf
	buildCmd.Flags().String("cache", "/var/cache/image-builder/store", `osbuild directory to cache intermediate build artifacts"`)
	// XXX: add "--verbose" here, similar to how bib is doing this
	// (see https://github.com/osbuild/bootc-image-builder/pull/790/commits/5cec7ffd8a526e2ca1e8ada0ea18f927695dfe43)
	buildCmd.Flags().String("progress", "auto", "type of progress bar to use (e.g. verbose,term)")
	rootCmd.AddCommand(buildCmd)
	buildCmd.Flags().AddFlagSet(uploadCmd.Flags())
	// add after the rest of the uploadCmd flag set is added to avoid
	// that build gets a "--to" parameter
	uploadCmd.Flags().String("to", "", "upload to the given cloud")

	// XXX: add --format=json too?
	describeImgCmd := &cobra.Command{
		Use:          "describe-image <image-type>",
		Short:        "Describe the given image-type, e.g. qcow2 (tip: combine with --distro,--arch)",
		RunE:         cmdDescribeImg,
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		Hidden:       true,
	}
	describeImgCmd.Flags().String("arch", "", `use the different architecture`)
	describeImgCmd.Flags().String("distro", "", `build manifest for a different distroname (e.g. centos-9)`)

	rootCmd.AddCommand(describeImgCmd)

	return rootCmd.Execute()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(osStderr, "error: %s\n", err)
		os.Exit(1)
	}
}
