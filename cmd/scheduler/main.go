package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/qmuntal/gobuilder/internal/buildbucket"
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

	jobQueue, err := buildbucket.NewQueue(buildbucket.QueueConfig{
		BaseURL:     options.buildbucketURL,
		Project:     options.buildbucketProject,
		Bucket:      options.buildbucketBucket,
		Builder:     options.buildbucketBuilder,
		BuilderName: options.buildbucketBuilderName,
	})
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
	maxJobs                int
	dryRun                 bool
	githubAPIURL           string
	githubToken            string
	repository             string
	ref                    string
	schedulerRunID         string
	buildbucketURL         string
	buildbucketProject     string
	buildbucketBucket      string
	buildbucketBuilder     string
	buildbucketBuilderName string
}

func parseOptions(args []string) (options, error) {
	parsedOptions := options{
		maxJobs:            5,
		githubAPIURL:       githubactions.DefaultAPIURL,
		buildbucketURL:     buildbucket.DefaultURL,
		buildbucketProject: "golang",
		buildbucketBucket:  "ci",
	}

	flags := flag.NewFlagSet("scheduler", flag.ContinueOnError)
	flags.Var(optionalInt{value: &parsedOptions.maxJobs}, "max-jobs", "maximum builder workflow runs to start")
	flags.BoolVar(&parsedOptions.dryRun, "dry-run", parsedOptions.dryRun, "print dispatch count without starting builder workflows")
	flags.StringVar(&parsedOptions.githubAPIURL, "github-api-url", parsedOptions.githubAPIURL, "GitHub API base URL")
	flags.StringVar(&parsedOptions.githubToken, "github-token", parsedOptions.githubToken, "GitHub token used to dispatch builder workflows")
	flags.StringVar(&parsedOptions.repository, "repository", parsedOptions.repository, "repository in owner/name format")
	flags.StringVar(&parsedOptions.ref, "ref", parsedOptions.ref, "git ref used for builder workflow dispatches")
	flags.StringVar(&parsedOptions.schedulerRunID, "scheduler-run-id", parsedOptions.schedulerRunID, "GitHub run ID of the scheduler workflow")
	flags.StringVar(&parsedOptions.buildbucketURL, "buildbucket-url", parsedOptions.buildbucketURL, "Buildbucket base URL")
	flags.StringVar(&parsedOptions.buildbucketProject, "buildbucket-project", parsedOptions.buildbucketProject, "LUCI project to query")
	flags.StringVar(&parsedOptions.buildbucketBucket, "buildbucket-bucket", parsedOptions.buildbucketBucket, "LUCI bucket to query")
	flags.StringVar(&parsedOptions.buildbucketBuilder, "buildbucket-builder", parsedOptions.buildbucketBuilder, "optional exact LUCI builder to query")
	flags.StringVar(&parsedOptions.buildbucketBuilderName, "buildbucket-builder-name", parsedOptions.buildbucketBuilderName, "only count queued jobs whose builder name contains this substring")

	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	return parsedOptions, nil
}

type optionalInt struct {
	value *int
}

func (option optionalInt) Set(rawValue string) error {
	if rawValue == "" {
		return nil
	}

	parsedValue, err := strconv.Atoi(rawValue)
	if err != nil {
		return err
	}

	*option.value = parsedValue
	return nil
}

func (option optionalInt) String() string {
	if option.value == nil {
		return ""
	}
	return strconv.Itoa(*option.value)
}
