package pkgfetcher

import (
	"fmt"
	"path/filepath"
	"strings"

	"gonum.org/v1/gonum/graph"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/packagerepo/repocloner/rpmrepocloner"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/packagerepo/repoutils"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkggraph"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/rpm"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/scheduler/schedulerutils"
)

func (cfg *Config) ResolvePackages() error {
	dependencyGraph := pkggraph.NewPkgGraph()

	err := pkggraph.ReadDOTGraphFile(dependencyGraph, cfg.InputGraph)
	if err != nil {
		logger.Log.Panicf("Failed to read graph to file. Error: %s", err)
	}

	var toolchainPackages []string
	toolchainManifest := cfg.ToolchainManifest
	if len(toolchainManifest) > 0 {
		toolchainPackages, err = schedulerutils.ReadReservedFilesList(toolchainManifest)
		if err != nil {
			logger.Log.Fatalf("unable to read toolchain manifest file '%s': %s", toolchainManifest, err)
		}
	}
	cloner, err := initializeCloner(cfg)
	if err != nil {
		logger.Log.Errorf("Failed to initialize RPM repo cloner. Error: %s", err)
		return err
	}
	defer cloner.Close()

	if hasUnresolvedNodes(dependencyGraph) {
		err = cfg.resolveGraphNodes(dependencyGraph, cfg.InputSummaryFile, cfg.OutputSummaryFile, toolchainPackages, cloner, cfg.OutDir, cfg.DisableUpstreamRepos, cfg.StopOnFailure)
		if err != nil {
			logger.Log.Panicf("Failed to resolve graph. Error: %s", err)
		}
	} else {
		logger.Log.Info("No unresolved packages to cache")
	}

	err = pkggraph.WriteDOTGraphFile(dependencyGraph, cfg.OutputGraph)
	return err
}

func initializeCloner(cfg *Config) (*rpmrepocloner.RpmRepoCloner, error) {
	// Create the worker environment
	cloner := rpmrepocloner.New()
	err := cloner.Initialize(cfg.OutDir, cfg.TmpDir, cfg.WorkerTar, cfg.ExistingRpmDir, cfg.UsePreviewRepo, cfg.RepoFiles)
	//if err != nil {
	//	logger.Log.Errorf("Failed to initialize RPM repo cloner. Error: %s", err)
	//	return
	//}
	// defer cloner.Close()

	if !cfg.DisableUpstreamRepos {
		tlsKey, tlsCert := strings.TrimSpace(cfg.TlsClientKey), strings.TrimSpace(cfg.TlsClientCert)
		err = cloner.AddNetworkFiles(tlsCert, tlsKey)
		if err != nil {
			logger.Log.Panicf("Failed to customize RPM repo cloner. Error: %s", err)
		}
	}
	return cloner, err
}

// hasUnresolvedNodes scans through the graph to see if there is anything to do
func hasUnresolvedNodes(graph *pkggraph.PkgGraph) bool {
	for _, n := range graph.AllRunNodes() {
		if n.State == pkggraph.StateUnresolved {
			return true
		}
	}
	return false
}

// resolveGraphNodes scans a graph and for each unresolved node in the graph clones the RPMs needed
// to satisfy it.
func (cfg *Config) resolveGraphNodes(dependencyGraph *pkggraph.PkgGraph, inputSummaryFile, outputSummaryFile string, toolchainPackages []string, cloner *rpmrepocloner.RpmRepoCloner, outDir string, disableUpstreamRepos, stopOnFailure bool) (err error) {

	cachingSucceeded := true
	if strings.TrimSpace(inputSummaryFile) == "" {
		// Cache an RPM for each unresolved node in the graph.
		fetchedPackages := make(map[string]bool)
		prebuiltPackages := make(map[string]bool)
		for _, n := range dependencyGraph.AllRunNodes() {
			if n.State == pkggraph.StateUnresolved {
				resolveErr := cfg.resolveSingleNode(cloner, n, toolchainPackages, fetchedPackages, prebuiltPackages, outDir)
				// Failing to clone a dependency should not halt a build.
				// The build should continue and attempt best effort to build as many packages as possible.
				if resolveErr != nil {
					cachingSucceeded = false
					errorMessage := strings.Builder{}
					errorMessage.WriteString(fmt.Sprintf("Failed to resolve all nodes in the graph while resolving '%s'\n", n))
					errorMessage.WriteString("Nodes which have this as a dependency:\n")
					for _, dependant := range graph.NodesOf(dependencyGraph.To(n.ID())) {
						errorMessage.WriteString(fmt.Sprintf("\t'%s' depends on '%s'\n", dependant.(*pkggraph.PkgNode), n))
					}
					logger.Log.Debugf(errorMessage.String())
				}
			}
		}
	} else {
		// If an input summary file was provided, simply restore the cache using the file.
		err = repoutils.RestoreClonedRepoContents(cloner, inputSummaryFile)
		cachingSucceeded = err == nil
	}
	if stopOnFailure && !cachingSucceeded {
		return fmt.Errorf("failed to cache unresolved nodes")
	}

	logger.Log.Info("Configuring downloaded RPMs as a local repository")
	err = cloner.ConvertDownloadedPackagesIntoRepo()
	if err != nil {
		logger.Log.Errorf("Failed to convert downloaded RPMs into a repo. Error: %s", err)
		return
	}

	if strings.TrimSpace(outputSummaryFile) != "" {
		err = repoutils.SaveClonedRepoContents(cloner, outputSummaryFile)
		if err != nil {
			logger.Log.Errorf("Failed to save cloned repo contents.")
			return
		}
	}

	return
}

// resolveSingleNode caches the RPM for a single node.
// It will modify fetchedPackages on a successful package clone.
func (cfg *Config) resolveSingleNode(cloner *rpmrepocloner.RpmRepoCloner, node *pkggraph.PkgNode, toolchainPackages []string, fetchedPackages, prebuiltPackages map[string]bool, outDir string) (err error) {
	const cloneDeps = true
	logger.Log.Debugf("Adding node %s to the cache", node.FriendlyName())

	logger.Log.Debugf("Searching for a package which supplies: %s", node.VersionedPkg.Name)
	// Resolve nodes to exact package names so they can be referenced in the graph.
	resolvedPackages, err := cloner.WhatProvides(node.VersionedPkg)
	if err != nil {
		msg := fmt.Sprintf("Failed to resolve (%s) to a package. Error: %s", node.VersionedPkg, err)
		// It is not an error if an implicit node could not be resolved as it may become available later in the build.
		// If it does not become available scheduler will print an error at the end of the build.
		if node.Implicit {
			logger.Log.Debug(msg)
		} else {
			logger.Log.Error(msg)
		}
		return
	}

	if len(resolvedPackages) == 0 {
		return fmt.Errorf("failed to find any packages providing '%v'", node.VersionedPkg)
	}

	preBuilt := false
	for _, resolvedPackage := range resolvedPackages {
		if !fetchedPackages[resolvedPackage] {
			desiredPackage := &pkgjson.PackageVer{
				Name: resolvedPackage,
			}

			preBuilt, err = cloner.Clone(cloneDeps, desiredPackage)
			if err != nil {
				logger.Log.Errorf("Failed to clone '%s' from RPM repo. Error: %s", resolvedPackage, err)
				return
			}
			fetchedPackages[resolvedPackage] = true
			prebuiltPackages[resolvedPackage] = preBuilt

			logger.Log.Debugf("Fetched '%s' as potential candidate (is pre-built: %v).", resolvedPackage, prebuiltPackages[resolvedPackage])
		}
	}

	err = cfg.assignRPMPath(node, outDir, resolvedPackages)
	if err != nil {
		logger.Log.Errorf("Failed to find an RPM to provide '%s'. Error: %s", node.VersionedPkg.Name, err)
		return
	}

	// If a package is  available locally, and it is part of the toolchain, mark it as a prebuilt so the scheduler knows it can use it
	// immediately (especially for dynamic generator created capabilities)
	if (preBuilt || prebuiltPackages[node.RpmPath]) && isToolchainPackage(node.RpmPath, toolchainPackages) {
		logger.Log.Debugf("Using a prebuilt toolchain package to resolve this dependency")
		prebuiltPackages[node.RpmPath] = true
		node.State = pkggraph.StateUpToDate
		node.Type = pkggraph.TypePreBuilt
	} else {
		node.State = pkggraph.StateCached
	}

	logger.Log.Infof("Choosing '%s' to provide '%s'.", filepath.Base(node.RpmPath), node.VersionedPkg.Name)

	return
}

func (cfg *Config) assignRPMPath(node *pkggraph.PkgNode, outDir string, resolvedPackages []string) (err error) {
	rpmPaths := []string{}
	for _, resolvedPackage := range resolvedPackages {
		rpmPaths = append(rpmPaths, rpmPackageToRPMPath(resolvedPackage, outDir))
	}

	node.RpmPath = rpmPaths[0]
	if len(rpmPaths) > 1 {
		var resolvedRPMs []string
		logger.Log.Debugf("Found %d candidates. Resolving.", len(rpmPaths))

		resolvedRPMs, err = rpm.ResolveCompetingPackages(cfg.TmpDir, rpmPaths...)
		if err != nil {
			logger.Log.Errorf("Failed while trying to pick an RPM providing '%s' from the following RPMs: %v", node.VersionedPkg.Name, rpmPaths)
			return
		}

		resolvedRPMsCount := len(resolvedRPMs)
		if resolvedRPMsCount == 0 {
			logger.Log.Errorf("Failed while trying to pick an RPM providing '%s'. No RPM can be installed from the following: %v", node.VersionedPkg.Name, rpmPaths)
			return
		}

		if resolvedRPMsCount > 1 {
			logger.Log.Warnf("Found %d candidates to provide '%s'. Picking the first one.", resolvedRPMsCount, node.VersionedPkg.Name)
		}

		node.RpmPath = rpmPackageToRPMPath(resolvedRPMs[0], outDir)
	}

	return
}

func rpmPackageToRPMPath(rpmPackage, outDir string) string {
	// Construct the rpm path of the cloned package.
	rpmName := fmt.Sprintf("%s.rpm", rpmPackage)
	return filepath.Join(outDir, rpmName)
}

func isToolchainPackage(rpmPath string, toolchainRPMs []string) bool {
	base := filepath.Base(rpmPath)
	for _, t := range toolchainRPMs {
		if t == base {
			return true
		}
	}
	return false
}
