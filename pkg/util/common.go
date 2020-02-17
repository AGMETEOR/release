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

package util

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/sirupsen/logrus"

	"k8s.io/release/pkg/command"
)

const (
	TagPrefix = "v"
)

// PackagesAvailable takes a slice of packages and determines if they are installed
// on the host OS. Replaces common::check_packages.
func PackagesAvailable(packages ...string) (bool, error) {
	hostOS, osErr := getOS()
	if osErr != nil {
		return false, osErr
	}

	var pkgMgr string
	missingPkgs := []string{}

	ok := true
	switch hostOS {
	case "Ubuntu", "Debian", "LinuxMint":
		pkgMgr = "apt"
		logrus.Infof("Assuming %s as the host OS package manager", pkgMgr)

		for _, pkg := range packages {
			checkCmd := command.New(
				"dpkg",
				"-l",
				pkg,
			)

			logrus.Infof("Checking if %s has been installed via %s...", pkg, pkgMgr)
			checkCmdStatus, checkCmdErr := checkCmd.RunSilent()
			if checkCmdErr != nil {
				return false, checkCmdErr
			}

			if !checkCmdStatus.Success() {
				logrus.Infof("Adding %s to missing packages", pkg)
				missingPkgs = append(missingPkgs, pkg)
				ok = false
			}
		}
	case "Fedora":
		pkgMgr = "dnf"
		logrus.Infof("Assuming %s as the host OS package manager", pkgMgr)

		for _, pkg := range packages {
			checkCmd := command.New(
				"rpm",
				"--quiet",
				"-q",
				pkg,
			)

			logrus.Infof("Checking if %s has been installed via %s...", pkg, pkgMgr)
			checkCmdStatus, checkCmdErr := checkCmd.RunSilent()
			if checkCmdErr != nil {
				return false, checkCmdErr
			}

			if !checkCmdStatus.Success() {
				missingPkgs = append(missingPkgs, pkg)
				ok = false
			}
		}
	default:
		ok = false
		return ok, errors.New("cannot continue; running tool on an unsupported OS")
	}

	installInstructionsPrefix := fmt.Sprintf("sudo %s install ", pkgMgr)

	if len(missingPkgs) > 0 {
		missingPkgsString := strings.Join(missingPkgs, ",")

		logrus.Warnf("The following packages are not installed via %s: %s", pkgMgr, missingPkgsString)

		for _, pkg := range missingPkgs {
			installInstructions := fmt.Sprintf("'%s%s'", installInstructionsPrefix, pkg)

			logrus.Infof("Install %s with: %s", pkg, installInstructions)
		}
	}

	return ok, nil
}

func getOS() (string, error) {
	logrus.Info("Checking host OS...")

	get := command.New("lsb_release", "-si")
	getStream, getErr := get.RunSilentSuccessOutput()
	if getErr != nil {
		return "", getErr
	}

	osOutput := getStream.OutputTrimNL()
	logrus.Infof("Host OS is %s", osOutput)

	return osOutput, nil
}

/*
#############################################################################
# Simple yes/no prompt
#
# @optparam default -n(default)/-y/-e (default to n, y or make (e)xplicit)
# @param message
common::askyorn () {
  local yorn
  local def=n
  local msg="y/N"

  case $1 in
  -y) # yes default
      def="y" msg="Y/n"
      shift
      ;;
  -e) # Explicit
      def="" msg="y/n"
      shift
      ;;
  -n) shift
      ;;
  esac

  while [[ $yorn != [yYnN] ]]; do
    logecho -n "$*? ($msg): "
    read yorn
    : ${yorn:=$def}
  done

  # Final test to set return code
  [[ $yorn == [yY] ]]
}
*/

func Ask(question, expectedResponse string, retries int) (answer string, success bool, err error) {
	attempts := 1

	if retries < 0 {
		fmt.Printf("Retries was set to a number less than zero (%d). Please specify a positive number of retries or zero, if you want to ask unconditionally.", retries)
	}

	for attempts <= retries {
		scanner := bufio.NewScanner(os.Stdin)
		fmt.Printf("%s (%d/%d) ", question, attempts, retries)

		scanner.Scan()
		answer = scanner.Text()

		if answer == expectedResponse {
			return answer, true, nil
		}

		fmt.Printf("Expected '%s', but got '%s'", expectedResponse, answer)

		attempts++
	}

	return answer, false, errors.New("expected response was not input. Retries exceeded")
}

// FakeGOPATH creates a temp directory, links the base directory into it and
// sets the GOPATH environment variable to it.
func FakeGOPATH(srcDir string) (string, error) {
	logrus.Debug("Linking repository into temp dir")
	baseDir, err := ioutil.TempDir("", "ff-")
	if err != nil {
		return "", err
	}

	logrus.Infof("New working directory is %q", baseDir)

	os.Setenv("GOPATH", baseDir)
	logrus.Debugf("GOPATH: %s", os.Getenv("GOPATH"))

	gitRoot := fmt.Sprintf("%s/src/k8s.io", baseDir)
	if err := os.MkdirAll(gitRoot, os.FileMode(0755)); err != nil {
		return "", err
	}
	gitRoot = filepath.Join(gitRoot, "kubernetes")

	// link the repo into the working directory
	logrus.Debugf("Creating symlink from %q to %q", srcDir, gitRoot)
	if err := os.Symlink(srcDir, gitRoot); err != nil {
		return "", err
	}

	logrus.Infof("Changing working directory to %s", gitRoot)
	if err := os.Chdir(gitRoot); err != nil {
		return "", err
	}

	return gitRoot, nil
}

// ReadFileFromGzippedTar opens a tarball and reads contents of a file inside.
func ReadFileFromGzippedTar(tarPath, filePath string) (io.Reader, error) {
	file, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}

	archive, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(archive)

	for {
		h, err := tr.Next()
		if err == io.EOF {
			break // End of archive
		}

		if h.Name == filePath {
			return tr, nil
		}
	}

	return nil, errors.New("unable to find file in tarball")
}

// MoreRecent determines if file at path a was modified more recently than file
// at path b. If one file does not exist, the other will be treated as most
// recent. If both files do not exist or an error occurs, an error is returned.
func MoreRecent(a, b string) (bool, error) {
	fileA, errA := os.Stat(a)
	if errA != nil && !os.IsNotExist(errA) {
		return false, errA
	}

	fileB, errB := os.Stat(b)
	if errB != nil && !os.IsNotExist(errB) {
		return false, errB
	}

	switch {
	case os.IsNotExist(errA) && os.IsNotExist(errB):
		return false, errors.New("neither file exists")
	case os.IsNotExist(errA):
		return false, nil
	case os.IsNotExist(errB):
		return true, nil
	}

	return (fileA.ModTime().Unix() >= fileB.ModTime().Unix()), nil
}

func AddTagPrefix(tag string) string {
	return TagPrefix + tag
}

func TrimTagPrefix(tag string) string {
	return strings.TrimPrefix(tag, TagPrefix)
}

func TagStringToSemver(tag string) (semver.Version, error) {
	return semver.Make(TrimTagPrefix(tag))
}

func SemverToTagString(tag semver.Version) string {
	return AddTagPrefix(tag.String())
}
