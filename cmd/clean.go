package cmd

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"twig/branch"
	"twig/common"
	"twig/config"
	"twig/log"
	"twig/network"
)

const (
	requestLimit      = 5
	itemsPerRequest   = 100
	itemsThreshold    = 5
	doneStatusId    = 3
	cleanCmdName      = "clean"
	cleanAllCmdName   = "all"
	cleanLocalCmdName = "local"
)

var (
	rate     = time.Tick(time.Second / time.Duration(requestLimit))
	mu       sync.Mutex
	assignee string
	cleanCmd = &cobra.Command{
		Use:   cleanCmdName,
		Short: "Deletes branches which have Jira tickets in 'Done' state",
		Args:  cobra.NoArgs,
	}
	cleanLocalCmd = &cobra.Command{
		Use:   cleanLocalCmdName,
		Short: "Deletes only local branches which have Jira tickets in 'Done' state",
		Args:  cobra.NoArgs,
		Run: runClean,
	}
	cleanAllCmd = &cobra.Command{
		Use:   cleanAllCmdName,
		Short: "Deletes remote and local branches which have Jira tickets in 'Done' state",
		Args:  cobra.NoArgs,
		Run: runClean,
	}
)

func runClean(cmd *cobra.Command, args []string) {
	log.Debug().Println("clean: executing command")

	httpClient := &http.Client{}
	client := network.NewClient(httpClient)

	fetchCommand, err := common.ExecuteFetchPrune()
	if err != nil {
		logCmdFatal(err)
	}

	if fetchCommand != "" {
		log.Info().Println(fetchCommand)
	}

	if err := common.BranchStatus(); err != nil {
		logCmdFatal(err)
	}

	devBranch := config.GetString(config.BranchDefault)
	hasBranch := common.HasBranch(devBranch)

	checkoutCommand, err := common.Checkout(devBranch, hasBranch)
	if err != nil {
		logCmdFatal(err)
	}

	if checkoutCommand != "" {
		log.Info().Println(checkoutCommand)
	}

	localBranches, err := common.GetLocalBranches()
	if err != nil {
		logCmdFatal(err)
	}

	issues, err := pairBranchesWithIssues(localBranches)
	if err != nil {
		logCmdFatal(err)
	}

	statuses, err := pairBranchesWithStatuses(client, issues)
	if err != nil {
		logCmdFatal(err)
	}

	remote := config.GetString(config.BranchOrigin)
	if remote == "" {
		logCmdFatal(fmt.Errorf("%q is not set", config.BranchOrigin))
	}

	if err := deleteBranchesIfAny(cmd.Name(), remote, statuses); err != nil {
		logCmdFatal(err)
	}
}

func init() {
	cleanCmd.PersistentFlags().StringVarP(
		&assignee,
		"assignee",
		"a",
		"",
		"(optional) provides assignee to delete branch",
	)

	cleanCmd.AddCommand(
		cleanLocalCmd,
		cleanAllCmd,
	)
}

func deleteBranchesIfAny(cmdName, remote string, statuses map[string]network.IssueStatusCategory) error {
	anyInDoneStatus := false

	for branchName, status := range statuses {
		if status.Id == doneStatusId {
			deleteLocalBranch(branchName)

			if cmdName == cleanAllCmdName {
				deleteRemoteBranch(remote, branchName)
			}

			anyInDoneStatus = true
		}
	}

	if !anyInDoneStatus {
		return errors.New("no associated Jira issues in DONE status")
	}

	return nil
}

func deleteLocalBranch(branchName string) {
	deleteCommand, err := common.DeleteLocalBranch(branchName)
	if err != nil {
		log.Error().Print(deleteCommand)
		log.Error().Print(fmt.Errorf("local branch: [%s] %w\n", branchName, err).Error())
	} else {
		log.Info().Print(deleteCommand)
	}
}

func deleteRemoteBranch(remote, branchName string) {
	deleteCommand, err := common.DeleteRemoteBranch(remote, branchName)
	if err != nil {
		log.Error().Print(deleteCommand)
		log.Error().Print(fmt.Errorf("remote branch: [%s] %w\n", branchName, err).Error())
	} else {
		log.Info().Print(deleteCommand)
	}
}

func pairBranchesWithStatuses(client network.Client, issues map[string]string) (map[string]network.IssueStatusCategory, error) {
	statuses := make(map[string]network.IssueStatusCategory)

	size := len(issues)
	if size <= itemsThreshold {
		queryIssues(client, issues, statuses)
	} else {
		bulkQueryIssues(client, issues, statuses)
	}

	if len(statuses) == 0 {
		return nil, errors.New("no Jira issues match")
	}

	return statuses, nil
}

func queryIssues(client network.Client, issues map[string]string, statuses map[string]network.IssueStatusCategory) {
	hasAssignee := assignee != ""

	for localBranch, issue := range issues {
		jiraIssue, err := client.GetJiraIssueStatus(issue, hasAssignee)

		if err != nil {
			log.Debug().Println(fmt.Sprintf("Branch with status %s", err.Error()))
			continue
		}

		if hasAssignee {
			email := jiraIssue.Fields.Assignee.Email

			if err = validateJiraIssue(jiraIssue.Key, email, assignee); err != nil {
				log.Debug().Println(fmt.Sprintf("Validae issue: %s", err.Error()))
				continue
			}
		}

		log.Debug().Println(fmt.Sprintf("Branch %q with status %q", localBranch, jiraIssue.Fields.Status.Category.Name))
		statuses[localBranch] = jiraIssue.Fields.Status.Category
	}
}

func bulkQueryIssues(client network.Client, issues map[string]string, statuses map[string]network.IssueStatusCategory) {
	hasAssignee := assignee != ""
	size := len(issues)

	jiraIssues := make([]network.JiraIssue, len(issues))
	values := slices.Collect(maps.Values(issues))
	attemptsNeeded := calculateAttempts(size)

	var wg sync.WaitGroup
	wg.Add(attemptsNeeded)

	for i := 0; i < attemptsNeeded; i++ {
		go func(batch int) {
			mu.Lock()
			jiraIssues = append(jiraIssues, getJiraIssueStatusBulk(batch, client, values, hasAssignee)...)
			mu.Unlock()

			wg.Done()
		}(i)
	}

	wg.Wait()

	jiraKeyToIssueMap := make(map[string]network.JiraIssue)
	for _, jiraIssue := range jiraIssues {
		jiraKeyToIssueMap[jiraIssue.Key] = jiraIssue
	}

	for localBranch, issue := range issues {
		jiraIssue := jiraKeyToIssueMap[issue]

		if hasAssignee {
			email := jiraIssue.Fields.Assignee.Email

			if err := validateJiraIssue(jiraIssue.Key, email, assignee); err != nil {
				log.Debug().Println(fmt.Sprintf("Validate issue: %s", err.Error()))
				continue
			}
		}

		log.Debug().Println(fmt.Sprintf("Branch %q with status %q", localBranch, jiraIssue.Fields.Status.Category.Name))
		statuses[localBranch] = jiraIssue.Fields.Status.Category
	}
}

func getJiraIssueStatusBulk(batch int, client network.Client, values []string, hasAssignee bool) []network.JiraIssue {
	<-rate

	size := len(values)

	start := batch * itemsPerRequest
	end := start + itemsPerRequest
	if end > size {
		end = size
	}

	jiraIssues, err := client.GetJiraIssueStatusBulk(values[start:end-1], hasAssignee)
	if err != nil {
		log.Debug().Println(fmt.Sprintf("Bulk issue: %s", err.Error()))
	}

	return jiraIssues
}

func calculateAttempts(size int) int {
	return (size + itemsPerRequest - 1) / itemsPerRequest
}

func validateJiraIssue(issueKey, email, assignee string) error {
	at := strings.Index(email, "@")
	if at == -1 {
		return fmt.Errorf("validate: email %q pulled from Jira issue is either invalid or corrupted", email)
	}

	username := strings.TrimSpace(email[:at])
	if assignee != username {
		return fmt.Errorf("validate: issue %q has assignee %q but looking for %q", issueKey, username, assignee)
	}

	return nil
}

func pairBranchesWithIssues(rawBranches string) (map[string]string, error) {
	localBranches := strings.Split(rawBranches, "\n")
	issues := make(map[string]string)

	for _, localBranch := range localBranches {
		trimmedBranchName := strings.Join(strings.Fields(localBranch), "")

		issue, err := branch.ExtractIssueNameFromBranch(trimmedBranchName)
		if err != nil || issue == "" {
			continue
		}

		log.Debug().Println(fmt.Sprintf("Branch %q with issue %q", issue, trimmedBranchName))
		issues[trimmedBranchName] = issue
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("pair branch with issue: no relation to Jira issues found")
	}

	return issues, nil
}
