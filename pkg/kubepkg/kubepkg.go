/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubepkg

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"k8s.io/release/pkg/command"
	"k8s.io/release/pkg/github"
	"k8s.io/release/pkg/kubepkg/options"
	"k8s.io/release/pkg/release"
	"k8s.io/release/pkg/util"
)

type ChannelType string

const (
	ChannelRelease ChannelType = "release"
	ChannelTesting ChannelType = "testing"
	ChannelNightly ChannelType = "nightly"

	minimumKubernetesVersion = "1.13.0"
	minimumCNIVersion        = "0.7.5"
	pre117CNIVersion         = "0.7.5"

	kubeadmConf = "10-kubeadm.conf"
)

var (
	minimumCRIToolsVersion = minimumKubernetesVersion

	buildArchMap = map[string]map[options.BuildType]string{
		"amd64": {
			"deb": "amd64",
			"rpm": "x86_64",
		},
		"arm": {
			"deb": "armhf",
			"rpm": "armhfp",
		},
		"arm64": {
			"deb": "arm64",
			"rpm": "aarch64",
		},
		"ppc64le": {
			"deb": "ppc64el",
			"rpm": "ppc64le",
		},
		"s390x": {
			"deb": "s390x",
			"rpm": "s390x",
		},
	}

	builtins = map[string]interface{}{
		"date": func() string {
			return time.Now().Format(time.RFC1123Z)
		},
	}
)

type Client struct {
	options *options.Options
	version *release.Version
	github  *github.GitHub
}

func New(o *options.Options) *Client {
	return &Client{
		options: o,
		version: release.NewVersion(),
		github:  github.New(),
	}
}

type Build struct {
	Type        options.BuildType
	Package     string
	Definitions []*PackageDefinition
	TemplateDir string
}

type PackageDefinition struct {
	Name     string
	Version  string
	Revision string

	Channel ChannelType

	KubernetesVersion string
	Dependencies      map[string]string

	DownloadLinkBase         string
	KubeadmKubeletConfigFile string

	CNIDownloadLink string
}

type buildConfig struct {
	*PackageDefinition
	Type      options.BuildType
	GoArch    string
	BuildArch string
	Package   string

	TemplateDir string
	workspace   string
	specOnly    bool
}

func (c *Client) ConstructBuilds() ([]Build, error) {
	logrus.Infof("Constructing builds...")

	builds := []Build{}

	for _, pkg := range c.options.Packages() {
		// TODO: Get package directory for any version once package definitions are broken out
		packageTemplateDir := filepath.Join(c.options.TemplateDir(), string(c.options.BuildType()), pkg)
		if _, err := os.Stat(packageTemplateDir); err != nil {
			return nil, errors.Wrap(err, "finding package template dir")
		}

		b := &Build{
			Type:        c.options.BuildType(),
			Package:     pkg,
			TemplateDir: packageTemplateDir,
		}

		for _, channel := range c.options.Channels() {
			packageDef := &PackageDefinition{
				Revision: c.options.Revision(),
				Channel:  ChannelType(channel),
			}

			packageDef.KubernetesVersion = c.options.KubeVersion()

			switch b.Package {
			case "kubernetes-cni":
				packageDef.Version = c.options.CNIVersion()
			case "cri-tools":
				packageDef.Version = c.options.CRIToolsVersion()
			}

			b.Definitions = append(b.Definitions, packageDef)
		}

		builds = append(builds, *b)
	}

	logrus.Infof("Successfully constructed builds")
	return builds, nil
}

func (c *Client) WalkBuilds(builds []Build) (err error) {
	logrus.Infof("Walking builds...")

	workingDir := os.Getenv("KUBEPKG_WORKING_DIR")
	if workingDir == "" {
		workingDir, err = ioutil.TempDir("", "kubepkg")
		if err != nil {
			return err
		}
	}

	for _, arch := range c.options.Architectures() {
		for _, build := range builds {
			for _, packageDef := range build.Definitions {
				if err := c.buildPackage(build, packageDef, arch, workingDir); err != nil {
					return err
				}
			}
		}
	}
	if c.options.SpecOnly() {
		logrus.Infof("Package specs have been saved in %s", workingDir)
	}
	logrus.Infof("Successfully walked builds")
	return nil
}

func (c *Client) buildPackage(build Build, packageDef *PackageDefinition, arch, tmpDir string) error {
	if packageDef == nil {
		return errors.New("package definition cannot be nil")
	}

	pd := &PackageDefinition{}
	*pd = *packageDef

	bc := &buildConfig{
		PackageDefinition: pd,
		Type:              build.Type,
		Package:           build.Package,
		GoArch:            arch,
		TemplateDir:       build.TemplateDir,
		workspace:         tmpDir,
		specOnly:          c.options.SpecOnly(),
	}

	bc.Name = build.Package

	var err error

	if bc.KubernetesVersion != "" {
		logrus.Infof("Checking if user-supplied Kubernetes version (%s) is valid semver...", bc.KubernetesVersion)
		kubeSemver, err := util.TagStringToSemver(bc.KubernetesVersion)
		if err != nil {
			return errors.Wrap(err, "user-supplied Kubernetes version is not valid semver")
		}

		kubeVersionString := kubeSemver.String()
		kubeVersionParts := strings.Split(kubeVersionString, ".")

		switch {
		case len(kubeVersionParts) > 4:
			logrus.Info("User-supplied Kubernetes version is a CI version")
			logrus.Info("Setting channel to nightly")
			bc.Channel = ChannelNightly
		case len(kubeVersionParts) == 4:
			logrus.Info("User-supplied Kubernetes version is a pre-release version")
			logrus.Info("Setting channel to testing")
			bc.Channel = ChannelTesting
		default:
			logrus.Info("User-supplied Kubernetes version is a release version")
			logrus.Info("Setting channel to release")
			bc.Channel = ChannelRelease
		}
	}

	bc.KubernetesVersion, err = c.getKubernetesVersion(pd)
	if err != nil {
		return errors.Wrap(err, "getting Kubernetes version")
	}

	bc.DownloadLinkBase, err = c.getDownloadLinkBase(pd)
	if err != nil {
		return errors.Wrap(err, "getting Kubernetes download link base")
	}

	logrus.Infof("Kubernetes download link base: %s", bc.DownloadLinkBase)

	// For cases where a CI build version of Kubernetes is retrieved, replace instances
	// of "+" with "-", so that we build with a valid Debian package version.
	bc.KubernetesVersion = strings.Replace(bc.KubernetesVersion, "+", "-", 1)

	bc.Version, err = c.getPackageVersion(pd)
	if err != nil {
		return errors.Wrap(err, "getting package version")
	}

	logrus.Infof("%s package version: %s", bc.Name, bc.Version)

	bc.Dependencies, err = getDependencies(pd)
	if err != nil {
		return errors.Wrap(err, "getting dependencies")
	}

	bc.KubeadmKubeletConfigFile = kubeadmConf

	bc.BuildArch = getBuildArch(bc.GoArch, bc.Type)

	bc.CNIDownloadLink, err = getCNIDownloadLink(pd, bc.GoArch)
	if err != nil {
		return errors.Wrap(err, "getting CNI download link")
	}

	logrus.Infof("Building %s package for %s/%s architecture...", bc.Package, bc.GoArch, bc.BuildArch)
	return bc.run()
}

func (bc *buildConfig) run() error {
	workspaceInfo, err := os.Stat(bc.workspace)
	if err != nil {
		return err
	}

	specDir := filepath.Join(bc.workspace, string(bc.Channel), bc.Package)
	specDirWithArch := filepath.Join(specDir, bc.GoArch)

	if err := os.MkdirAll(specDirWithArch, workspaceInfo.Mode()); err != nil {
		return err
	}

	// TODO: keepTmp/cleanup needs to defined in kubepkg root
	if !bc.specOnly {
		defer os.RemoveAll(specDirWithArch)
	}

	if _, err := buildSpecs(bc, specDirWithArch); err != nil {
		return err
	}

	if bc.specOnly {
		logrus.Info("Spec-only mode was selected; kubepkg will now exit without building packages")
		return nil
	}

	// TODO: Move OS-specific logic into their own files
	switch bc.Type {
	case options.BuildDeb:
		logrus.Infof("Running dpkg-buildpackage for %s (%s/%s)", bc.Package, bc.GoArch, bc.BuildArch)

		dpkgErr := command.NewWithWorkDir(
			specDirWithArch,
			"dpkg-buildpackage",
			"--unsigned-source",
			"--unsigned-changes",
			"--build=binary",
			"--host-arch",
			bc.BuildArch,
		).RunSuccess()

		if dpkgErr != nil {
			return dpkgErr
		}

		fileName := fmt.Sprintf("%s_%s-%s_%s.deb", bc.Package, bc.Version, bc.Revision, bc.BuildArch)
		dstParts := []string{"bin", string(bc.Channel), fileName}

		dstPath := filepath.Join(dstParts...)
		if mkdirErr := os.MkdirAll(dstPath, os.FileMode(0777)); mkdirErr != nil {
			return mkdirErr
		}

		mvErr := command.New("mv", filepath.Join(specDir, fileName), dstPath).RunSuccess()
		if mvErr != nil {
			return mvErr
		}

		logrus.Infof("Successfully built %s", dstPath)
	case options.BuildRpm:
		logrus.Info("Building rpms via kubepkg is not currently supported")
	}

	return nil
}

func (c *Client) getPackageVersion(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	logrus.Infof("Setting version for %s package...", packageDef.Name)
	switch packageDef.Name {
	case "kubernetes-cni":
		return getCNIVersion(packageDef)
	case "cri-tools":
		return c.getCRIToolsVersion(packageDef)
	}

	logrus.Infof(
		"Using Kubernetes version %s for %s package",
		packageDef.KubernetesVersion, packageDef.Name,
	)
	return util.TrimTagPrefix(packageDef.KubernetesVersion), nil
}

func (c *Client) getKubernetesVersion(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	if packageDef.KubernetesVersion != "" {
		logrus.Infof("Using Kubernetes version (%s) for %s package", packageDef.KubernetesVersion, packageDef.Name)
		return packageDef.KubernetesVersion, nil
	}
	switch packageDef.Channel {
	case ChannelTesting:
		return c.version.GetKubeVersion(release.VersionTypeStablePreRelease)
	case ChannelNightly:
		return c.version.GetKubeVersion(release.VersionTypeCILatest)
	}

	return c.version.GetKubeVersion(release.VersionTypeStable)
}

func getCNIVersion(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	logrus.Infof("Getting CNI version...")

	kubeSemver, err := util.TagStringToSemver(packageDef.KubernetesVersion)
	if err != nil {
		return "", err
	}

	v117, err := semver.Make("1.17.0-alpha.0")
	if err != nil {
		return "", err
	}

	if packageDef.Version != "" {
		if kubeSemver.LT(v117) {
			logrus.Infof("Kubernetes version earlier than 1.17 must use CNI version <= %s", pre117CNIVersion)
			logrus.Infof("Setting CNI version to %s", pre117CNIVersion)
			return pre117CNIVersion, nil
		}

		logrus.Infof("Setting CNI version to %s", packageDef.Version)
		return packageDef.Version, nil
	}

	logrus.Infof("Setting CNI version to %s", minimumCNIVersion)
	return minimumCNIVersion, nil
}

func (c *Client) getCRIToolsVersion(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	if packageDef.Version != "" {
		return packageDef.Version, nil
	}

	kubeSemver, err := util.TagStringToSemver(packageDef.KubernetesVersion)
	if err != nil {
		return "", err
	}

	logrus.Infof("Getting CRI version...")

	kubeVersionString := kubeSemver.String()
	kubeVersionParts := strings.Split(kubeVersionString, ".")

	criToolsMajor := kubeVersionParts[0]
	criToolsMinor := kubeVersionParts[1]

	// CRI tools releases are not published until after the corresponding Kubernetes release.
	// In instances where the Kubernetes version selected is a pre-release or CI build version, // we need to build from the previous minor version of CRI tools instead.
	//
	// Example:
	// Kubernetes version: 1.18.0-alpha.1
	// Initial CRI tools version: 1.18.0-alpha.1
	// Modified CRI tools version: 1.17.0
	if len(kubeVersionParts) >= 4 {
		criToolsMinorInt, err := strconv.Atoi(criToolsMinor)
		if err != nil {
			return "", err
		}

		criToolsMinorInt--
		criToolsMinor = strconv.Itoa(criToolsMinorInt)
	}

	criToolsVersion := fmt.Sprintf("%s.%s.0", criToolsMajor, criToolsMinor)

	releases, err := c.github.Releases("kubernetes-sigs", "cri-tools", false)
	if err != nil {
		return "", err
	}

	var tags []string
	for _, release := range releases {
		criToolsReleaseTag := util.TrimTagPrefix(*release.TagName)
		criToolsReleaseVersionParts := strings.Split(criToolsReleaseTag, ".")
		criToolsReleaseMinor := criToolsReleaseVersionParts[1]

		if criToolsReleaseMinor == criToolsMinor {
			tags = append(tags, criToolsReleaseTag)
		}
	}

	for _, tag := range tags {
		tagSemver, err := semver.Parse(tag)
		if err != nil {
			return "", errors.Wrap(err, "could not parse tag semver")
		}

		criToolsSemver, err := semver.Parse(criToolsVersion)
		if err != nil {
			return "", errors.Wrap(err, "could not parse CRI tools semver")
		}

		if tagSemver.GTE(criToolsSemver) {
			criToolsVersion = tag
		}
	}

	logrus.Infof("Setting CRI tools version to %s", criToolsVersion)
	return criToolsVersion, nil
}

func (c *Client) getDownloadLinkBase(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	if packageDef.Channel == ChannelNightly {
		return c.getCIBuildsDownloadLinkBase(packageDef)
	}

	return getDefaultReleaseDownloadLinkBase(packageDef)
}

func (c *Client) getCIBuildsDownloadLinkBase(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	ciVersion := packageDef.KubernetesVersion
	if ciVersion == "" {
		var err error
		ciVersion, err = c.version.GetKubeVersion(release.VersionTypeCILatest)
		if err != nil {
			return "", err
		}
	}

	return fmt.Sprintf("https://dl.k8s.io/ci/%s", util.AddTagPrefix(ciVersion)), nil
}

func getDefaultReleaseDownloadLinkBase(packageDef *PackageDefinition) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	return fmt.Sprintf(
		"%s/%s",
		options.DefaultReleaseDownloadLinkBase,
		util.AddTagPrefix(packageDef.KubernetesVersion),
	), nil
}

func getDependencies(packageDef *PackageDefinition) (map[string]string, error) {
	if packageDef == nil {
		return nil, errors.New("package definition cannot be nil")
	}

	deps := make(map[string]string)

	switch packageDef.Name {
	case "kubelet":
		deps["kubernetes-cni"] = minimumCNIVersion
	case "kubeadm":
		deps["kubelet"] = minimumKubernetesVersion
		deps["kubectl"] = minimumKubernetesVersion
		deps["kubernetes-cni"] = minimumCNIVersion
		deps["cri-tools"] = minimumCRIToolsVersion
	}

	return deps, nil
}

func getBuildArch(goArch string, buildType options.BuildType) string {
	return buildArchMap[goArch][buildType]
}

func getCNIDownloadLink(packageDef *PackageDefinition, arch string) (string, error) {
	if packageDef == nil {
		return "", errors.New("package definition cannot be nil")
	}

	sv, err := util.TagStringToSemver(packageDef.Version)
	if err != nil {
		return "", err
	}

	v075, err := semver.Make(pre117CNIVersion)
	if err != nil {
		return "", err
	}

	if sv.LTE(v075) {
		return fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-%s-v%s.tgz", packageDef.Version, arch, packageDef.Version), nil
	}

	return fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz", packageDef.Version, arch, packageDef.Version), nil
}
