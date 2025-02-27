// Copyright 2020 OpenSSF Scorecard Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package pkg defines fns for running Scorecard checks on a Repo.
package pkg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/release-utils/version"

	"github.com/ossf/scorecard/v5/checker"
	"github.com/ossf/scorecard/v5/clients"
	"github.com/ossf/scorecard/v5/clients/githubrepo"
	"github.com/ossf/scorecard/v5/clients/gitlabrepo"
	"github.com/ossf/scorecard/v5/clients/localdir"
	"github.com/ossf/scorecard/v5/clients/ossfuzz"
	"github.com/ossf/scorecard/v5/config"
	sce "github.com/ossf/scorecard/v5/errors"
	"github.com/ossf/scorecard/v5/finding"
	"github.com/ossf/scorecard/v5/internal/packageclient"
	proberegistration "github.com/ossf/scorecard/v5/internal/probes"
	sclog "github.com/ossf/scorecard/v5/log"
	"github.com/ossf/scorecard/v5/options"
	"github.com/ossf/scorecard/v5/policy"
)

// errEmptyRepository indicates the repository is empty.
var errEmptyRepository = errors.New("repository empty")

func runEnabledChecks(ctx context.Context,
	repo clients.Repo,
	request *checker.CheckRequest,
	checksToRun checker.CheckNameToFnMap,
	resultsCh chan<- checker.CheckResult,
) {
	wg := sync.WaitGroup{}
	for checkName, checkFn := range checksToRun {
		checkName := checkName
		checkFn := checkFn
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner := checker.NewRunner(
				checkName,
				repo.URI(),
				request,
			)

			resultsCh <- runner.Run(ctx, checkFn)
		}()
	}
	wg.Wait()
	close(resultsCh)
}

func getRepoCommitHash(r clients.RepoClient) (string, error) {
	commits, err := r.ListCommits()
	if err != nil {
		// allow --local repos to still process
		if errors.Is(err, clients.ErrUnsupportedFeature) {
			return "unknown", nil
		}
		return "", sce.WithMessage(sce.ErrScorecardInternal, fmt.Sprintf("ListCommits:%v", err.Error()))
	}

	if len(commits) == 0 {
		return "", errEmptyRepository
	}
	return commits[0].SHA, nil
}

func runScorecard(ctx context.Context,
	repo clients.Repo,
	commitSHA string,
	commitDepth int,
	checksToRun checker.CheckNameToFnMap,
	probesToRun []string,
	repoClient clients.RepoClient,
	ossFuzzRepoClient clients.RepoClient,
	ciiClient clients.CIIBestPracticesClient,
	vulnsClient clients.VulnerabilitiesClient,
	projectClient packageclient.ProjectPackageClient,
) (ScorecardResult, error) {
	if err := repoClient.InitRepo(repo, commitSHA, commitDepth); err != nil {
		// No need to call sce.WithMessage() since InitRepo will do that for us.
		//nolint:wrapcheck
		return ScorecardResult{}, err
	}
	defer repoClient.Close()

	versionInfo := version.GetVersionInfo()
	ret := ScorecardResult{
		Repo: RepoInfo{
			Name:      repo.URI(),
			CommitSHA: commitSHA,
		},
		Scorecard: ScorecardInfo{
			Version:   versionInfo.GitVersion,
			CommitSHA: versionInfo.GitCommit,
		},
		Date: time.Now(),
	}

	commitSHA, err := getRepoCommitHash(repoClient)

	if errors.Is(err, errEmptyRepository) {
		return ret, nil
	} else if err != nil {
		return ScorecardResult{}, err
	}
	ret.Repo.CommitSHA = commitSHA

	defaultBranch, err := repoClient.GetDefaultBranchName()
	if err != nil {
		if !errors.Is(err, clients.ErrUnsupportedFeature) {
			return ScorecardResult{},
				sce.WithMessage(sce.ErrScorecardInternal, fmt.Sprintf("GetDefaultBranchName:%v", err.Error()))
		}
		defaultBranch = "unknown"
	}

	resultsCh := make(chan checker.CheckResult)

	// Set metadata for all checks to use. This is necessary
	// to create remediations from the probe yaml files.
	ret.RawResults.Metadata.Metadata = map[string]string{
		"repository.host":          repo.Host(),
		"repository.name":          strings.TrimPrefix(repo.URI(), repo.Host()+"/"),
		"repository.uri":           repo.URI(),
		"repository.sha1":          commitSHA,
		"repository.defaultBranch": defaultBranch,
	}

	request := &checker.CheckRequest{
		Ctx:                   ctx,
		RepoClient:            repoClient,
		OssFuzzRepo:           ossFuzzRepoClient,
		CIIClient:             ciiClient,
		VulnerabilitiesClient: vulnsClient,
		ProjectClient:         projectClient,
		Repo:                  repo,
		RawResults:            &ret.RawResults,
	}

	// If the user runs probes
	if len(probesToRun) > 0 {
		err = runEnabledProbes(request, probesToRun, &ret)
		if err != nil {
			return ScorecardResult{}, err
		}
		return ret, nil
	}

	// If the user runs checks
	go runEnabledChecks(ctx, repo, request, checksToRun, resultsCh)

	if os.Getenv(options.EnvVarScorecardExperimental) == "1" {
		r, path := findConfigFile(repoClient)
		logger := sclog.NewLogger(sclog.DefaultLevel)

		if r != nil {
			defer r.Close()
			logger.Info(fmt.Sprintf("using maintainer annotations: %s", path))
			c, err := config.Parse(r)
			if err != nil {
				logger.Info(fmt.Sprintf("couldn't parse maintainer annotations: %v", err))
			}
			ret.Config = c
		}
	}

	for result := range resultsCh {
		ret.Checks = append(ret.Checks, result)
		ret.Findings = append(ret.Findings, result.Findings...)
	}
	return ret, nil
}

func findConfigFile(rc clients.RepoClient) (io.ReadCloser, string) {
	// Look for a config file. Return first one regardless of validity
	locs := []string{"scorecard.yml", ".scorecard.yml", ".github/scorecard.yml"}

	for i := range locs {
		cfr, err := rc.GetFileReader(locs[i])
		if err != nil {
			continue
		}
		return cfr, locs[i]
	}

	return nil, ""
}

func runEnabledProbes(request *checker.CheckRequest,
	probesToRun []string,
	ret *ScorecardResult,
) error {
	// Add RawResults to request
	err := populateRawResults(request, probesToRun, ret)
	if err != nil {
		return err
	}

	probeFindings := make([]finding.Finding, 0)
	for _, probeName := range probesToRun {
		probe, err := proberegistration.Get(probeName)
		if err != nil {
			return fmt.Errorf("getting probe %q: %w", probeName, err)
		}
		// Run probe
		var findings []finding.Finding
		if probe.IndependentImplementation != nil {
			findings, _, err = probe.IndependentImplementation(request)
		} else {
			findings, _, err = probe.Implementation(&ret.RawResults)
		}
		if err != nil {
			return sce.WithMessage(sce.ErrScorecardInternal, "ending run")
		}
		probeFindings = append(probeFindings, findings...)
	}
	ret.Findings = probeFindings
	return nil
}

// RunScorecard runs enabled Scorecard checks on a Repo.
func RunScorecard(ctx context.Context,
	repo clients.Repo,
	commitSHA string,
	commitDepth int,
	checksToRun checker.CheckNameToFnMap,
	repoClient clients.RepoClient,
	ossFuzzRepoClient clients.RepoClient,
	ciiClient clients.CIIBestPracticesClient,
	vulnsClient clients.VulnerabilitiesClient,
	projectClient packageclient.ProjectPackageClient,
) (ScorecardResult, error) {
	return runScorecard(ctx,
		repo,
		commitSHA,
		commitDepth,
		checksToRun,
		[]string{},
		repoClient,
		ossFuzzRepoClient,
		ciiClient,
		vulnsClient,
		projectClient,
	)
}

// ExperimentalRunProbes is experimental. Do not depend on it, it may be removed at any point.
func ExperimentalRunProbes(ctx context.Context,
	repo clients.Repo,
	commitSHA string,
	commitDepth int,
	checksToRun checker.CheckNameToFnMap,
	probesToRun []string,
	repoClient clients.RepoClient,
	ossFuzzRepoClient clients.RepoClient,
	ciiClient clients.CIIBestPracticesClient,
	vulnsClient clients.VulnerabilitiesClient,
	projectClient packageclient.ProjectPackageClient,
) (ScorecardResult, error) {
	return runScorecard(ctx,
		repo,
		commitSHA,
		commitDepth,
		checksToRun,
		probesToRun,
		repoClient,
		ossFuzzRepoClient,
		ciiClient,
		vulnsClient,
		projectClient,
	)
}

type runConfig struct {
	client        clients.RepoClient
	vulnClient    clients.VulnerabilitiesClient
	ciiClient     clients.CIIBestPracticesClient
	projectClient packageclient.ProjectPackageClient
	ossfuzzClient clients.RepoClient
	checks        []string
	commit        string
	probes        []string
	commitDepth   int
}

type Option func(*runConfig) error

func WithCommitDepth(depth int) Option {
	return func(c *runConfig) error {
		c.commitDepth = depth
		return nil
	}
}

func WithCommitSHA(sha string) Option {
	return func(c *runConfig) error {
		c.commit = sha
		return nil
	}
}

func WithChecks(checks []string) Option {
	return func(c *runConfig) error {
		c.checks = checks
		return nil
	}
}

func WithProbes(probes []string) Option {
	return func(c *runConfig) error {
		c.probes = probes
		return nil
	}
}

func WithRepoClient(client clients.RepoClient) Option {
	return func(c *runConfig) error {
		c.client = client
		return nil
	}
}

func WithOSSFuzzClient(client clients.RepoClient) Option {
	return func(c *runConfig) error {
		c.ossfuzzClient = client
		return nil
	}
}

func WithVulnerabilitiesClient(client clients.VulnerabilitiesClient) Option {
	return func(c *runConfig) error {
		c.vulnClient = client
		return nil
	}
}

func WithOpenSSFBestPraticesClient(client clients.CIIBestPracticesClient) Option {
	return func(c *runConfig) error {
		c.ciiClient = client
		return nil
	}
}

func Run(ctx context.Context, repo clients.Repo, opts ...Option) (ScorecardResult, error) {
	// TODO logger
	logger := sclog.NewLogger(sclog.InfoLevel)
	c := runConfig{
		commit: clients.HeadSHA,
	}
	for _, option := range opts {
		if err := option(&c); err != nil {
			return ScorecardResult{}, err
		}
	}
	if c.ciiClient == nil {
		c.ciiClient = clients.DefaultCIIBestPracticesClient()
	}
	if c.ossfuzzClient == nil {
		c.ossfuzzClient = ossfuzz.CreateOSSFuzzClient(ossfuzz.StatusURL)
	}
	if c.vulnClient == nil {
		c.vulnClient = clients.DefaultVulnerabilitiesClient()
	}
	if c.projectClient == nil {
		c.projectClient = packageclient.CreateDepsDevClient()
	}

	var requiredRequestTypes []checker.RequestType
	var err error
	switch repo.(type) {
	case *localdir.Repo:
		requiredRequestTypes = append(requiredRequestTypes, checker.FileBased)
		if c.client == nil {
			c.client = localdir.CreateLocalDirClient(ctx, logger)
		}
	case *githubrepo.Repo:
		if c.client == nil {
			c.client = githubrepo.CreateGithubRepoClient(ctx, logger)
		}
	case *gitlabrepo.Repo:
		if c.client == nil {
			c.client, err = gitlabrepo.CreateGitlabClient(ctx, repo.Host())
			if err != nil {
				return ScorecardResult{}, fmt.Errorf("creating gitlab client: %w", err)
			}
		}
	}

	if !strings.EqualFold(c.commit, clients.HeadSHA) {
		requiredRequestTypes = append(requiredRequestTypes, checker.CommitBased)
	}

	checksToRun, err := policy.GetEnabled(nil, c.checks, requiredRequestTypes)
	if err != nil {
		return ScorecardResult{}, fmt.Errorf("getting enabled checks: %w", err)
	}

	return runScorecard(ctx, repo, c.commit, c.commitDepth, checksToRun, c.probes,
		c.client, c.ossfuzzClient, c.ciiClient, c.vulnClient, c.projectClient)
}
