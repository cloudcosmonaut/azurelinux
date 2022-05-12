package validatechroot

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/file"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/safechroot"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/shell"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/logger"
)

func (cfg *Config) Validate() error {
	return cfg.validateWorker(cfg.ToolchainRpmsDir, cfg.TmpDir, cfg.WorkerTar, cfg.WorkerManifest)
}

func (cfg *Config) validateWorker(rpmsDir, chrootDir, workerTarPath, manifestPath string) (err error) {
	const (
		chrootToolchainRpmsDir = "/toolchainrpms"
		isExistingDir          = false
	)

	var (
		chroot *safechroot.Chroot
		// Every valid line will be of the form: <package>-<version>.<arch>.rpm
		packageArchLookupRegex = regexp.MustCompile(`^.+(?P<arch>x86_64|aarch64|noarch)\.rpm$`)
	)

	// Ensure that if initialization fails, the chroot is closed
	defer func() {
		if chroot != nil {
			closeErr := chroot.Close(cfg.LeaveChrootFilesOnDisk)
			if closeErr != nil {
				logger.Log.Panicf("Unable to close chroot on failed initialization. Error: %s", closeErr)
			}
		}
	}()

	logger.Log.Infof("Creating chroot environment to validate '%s' against '%s'", workerTarPath, manifestPath)

	chroot = safechroot.NewChroot(chrootDir, isExistingDir)
	rpmMount := safechroot.NewMountPoint(rpmsDir, chrootToolchainRpmsDir, "", safechroot.BindMountPointFlags, "")
	extraDirectories := []string{chrootToolchainRpmsDir}
	rpmMounts := []*safechroot.MountPoint{rpmMount}
	err = chroot.Initialize(workerTarPath, extraDirectories, rpmMounts)
	if err != nil {
		chroot = nil
		return
	}

	manifestEntries, err := file.ReadLines(manifestPath)
	if err != nil {
		return
	}
	badEntries := make(map[string]string)

	err = chroot.Run(func() (err error) {
		for _, rpm := range manifestEntries {
			archMatches := packageArchLookupRegex.FindStringSubmatch(rpm)
			if len(archMatches) != 2 {
				logger.Log.Errorf("%v", archMatches)
				return fmt.Errorf("'%s' is an invalid rpm file path", rpm)
			}
			arch := archMatches[1]
			rpmPath := path.Join(chrootToolchainRpmsDir, arch, rpm)

			// --replacepkgs instructs RPM to gracefully re-install a package, including checking dependencies
			args := []string{
				"-ihv",
				"--replacepkgs",
				"--nosignature",
				rpmPath,
			}
			logger.Log.Infof("Validating %s", filepath.Base(rpmPath))
			stdout, stderr, err := shell.Execute("rpm", args...)

			logger.Log.Debug(stdout)

			if err != nil || len(stderr) > 0 {
				logger.Log.Warn(stderr)
				if len(stderr) > 0 {
					badEntries[rpm] = stderr
				} else {
					badEntries[rpm] = err.Error()
				}
			}
		}
		return
	})

	if len(badEntries) > 0 {
		for rpm, errMsg := range badEntries {
			logger.Log.Errorf("%s:\n %s", rpm, errMsg)
		}
		err = fmt.Errorf("found invalid packages in the worker chroot")
	}
	return
}
