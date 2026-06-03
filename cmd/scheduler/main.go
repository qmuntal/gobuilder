package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/qmuntal/gobuilder/internal/githubactions"
	"github.com/qmuntal/gobuilder/internal/scheduler"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scheduler: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		return err
	}

	jobQueue, err := scheduler.NewFixedQueue(options.fixedJobs)
	if err != nil {
		return err
	}

	workflowDispatcher := githubactions.Dispatcher{
		Token:      options.githubToken,
		APIURL:     options.githubAPIURL,
		Repository: options.repository,
		Ref:        options.ref,
	}

	_, err = scheduler.Run(context.Background(), scheduler.Config{
		Queue:          jobQueue,
		Dispatcher:     workflowDispatcher,
		MaxJobs:        options.maxJobs,
		Workflow:       "builder.yml",
		SchedulerRunID: options.schedulerRunID,
		DryRun:         options.dryRun,
		Output:         os.Stdout,
	})
	return err
}

type options struct {
	fixedJobs      int
	maxJobs        int
	dryRun         bool
	githubAPIURL   string
	githubToken    string
	repository     string
	ref            string
	schedulerRunID string
}

func parseOptions(args []string) (options, error) {
	parsedOptions := options{
		fixedJobs:    2,
		maxJobs:      5,
		githubAPIURL: githubactions.DefaultAPIURL,
	}

	flags := flag.NewFlagSet("scheduler", flag.ContinueOnError)
	flags.IntVar(&parsedOptions.fixedJobs, "fixed-jobs", parsedOptions.fixedJobs, "queue depth returned by the fixed queue")
	flags.IntVar(&parsedOptions.maxJobs, "max-jobs", parsedOptions.maxJobs, "maximum builder workflow runs to start")
	flags.BoolVar(&parsedOptions.dryRun, "dry-run", parsedOptions.dryRun, "print dispatch count without starting builder workflows")
	flags.StringVar(&parsedOptions.githubAPIURL, "github-api-url", parsedOptions.githubAPIURL, "GitHub API base URL")
	flags.StringVar(&parsedOptions.githubToken, "github-token", parsedOptions.githubToken, "GitHub token used to dispatch builder workflows")
	flags.StringVar(&parsedOptions.repository, "repository", parsedOptions.repository, "repository in owner/name format")
	flags.StringVar(&parsedOptions.ref, "ref", parsedOptions.ref, "git ref used for builder workflow dispatches")
	flags.StringVar(&parsedOptions.schedulerRunID, "scheduler-run-id", parsedOptions.schedulerRunID, "GitHub run ID of the scheduler workflow")

	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	return parsedOptions, nil
}
