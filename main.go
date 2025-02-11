package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/google/go-github/github"
	"github.com/kelseyhightower/envconfig"
	"golang.org/x/oauth2"
)

type env struct {
	GithubToken     string   `envconfig:"GITHUB_TOKEN"`
	Owner           string   `envconfig:"OWNER"`
	Repo            string   `envconfig:"REPO"`
	PRNumber        int      `envconfig:"PR_NUMBER"`
	Comment         string   `envconfig:"COMMENT"`
	MergeMethod     string   `envconfig:"MERGE_METHOD" default:"merge"`
	Mergers         []string `envconfig:"MERGERS"`
	Actor           string   `envconfig:"GITHUB_ACTOR"` // github user who initiated the workflow.
	EnableAutoMerge bool     `envconfig:"ENABLE_AUTO_MERGE" default:"false"`
}

const (
	mergeComment = "/merge"
	jobTimeout   = 10 * 60 * time.Second
)

func main() {
	var e env
	err := envconfig.Process("INPUT", &e)
	if err != nil {
		fmt.Printf("failed to load inputs: %s\n", err.Error())
		panic(err.Error())
	}
	ctx, f := context.WithTimeout(context.Background(), jobTimeout)
	defer f()
	client := newGHClient(e.GithubToken)
	if err := validateEnv(e); err != nil {
		if serr := client.sendMsg(ctx, e.Owner, e.Repo, e.PRNumber, errMsg(err)); serr != nil {
			fmt.Printf("failed to send message: %v original: %v", serr, err)
			panic(serr.Error())
		}
		fmt.Printf("failed to validate env: %v", err)
		panic(err.Error())
	}
	if err := client.merge(ctx, e.Owner, e.Repo, e.PRNumber, e.MergeMethod, e.EnableAutoMerge); err != nil {
		if serr := client.sendMsg(ctx, e.Owner, e.Repo, e.PRNumber, errMsg(err)); serr != nil {
			fmt.Printf("failed to send message: %v original: %v", serr, err)
			panic(serr.Error())
		}
		fmt.Printf("failed to merge: %v", err)
		panic(err.Error())
	}
	successMsg := "Merged PR #" + fmt.Sprintf("%d", e.PRNumber) + " successfully!"
	if err := client.sendMsg(ctx, e.Owner, e.Repo, e.PRNumber, successMsg); err != nil {
		fmt.Printf("failed to send message: %v", err)
		panic(err.Error())
	}
	fmt.Printf(successMsg)
}

func validateEnv(e env) error {
	if e.Comment != mergeComment {
		return fmt.Errorf("comment must be %s, got %s", mergeComment, e.Comment)
	}
	if len(e.Mergers) == 0 {
		return nil
	}
	for _, m := range e.Mergers {
		if e.Actor == m {
			// if actor matches specified mergers, then valid workflow run.
			return nil
		}
	}
	return fmt.Errorf("actor %s is not in mergers list", e.Actor)
}

type ghClient struct {
	client *github.Client
}

func newGHClient(token string) *ghClient {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	return &ghClient{
		client: client,
	}
}

func (gh *ghClient) merge(ctx context.Context, owner, repo string, prNumber int, mergeMethod string, enableAutoMerge bool) error {
	pr, _, err := gh.client.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return fmt.Errorf("failed to get pull request: %w", err)
	}
	commitMsg, err := generateCommitBody(pr)
	if err != nil {
		return fmt.Errorf("failed to generate template: %w", err)
	}

	if enableAutoMerge {
		// GitHub API docs: https://cli.github.com/manual/gh_pr_merge
		err = exec.Command("gh", "pr", "merge", strconv.Itoa(prNumber), fmt.Sprintf("--%s", mergeMethod), "--auto", "--subject", generateCommitSubject(pr), "--body", commitMsg, "--repo", fmt.Sprintf("%s/%s", owner, repo)).Run()
	} else {
		_, _, err = gh.client.PullRequests.Merge(ctx, owner, repo, prNumber, commitMsg, &github.PullRequestOptions{
			CommitTitle: generateCommitSubject(pr),
			MergeMethod: mergeMethod,
		})
	}
	if err != nil {
		return fmt.Errorf("failed to merge pull request: %w", err)
	}
	return nil
}

func generateCommitSubject(pr *github.PullRequest) string {
	return fmt.Sprintf("%s (#%d)", pr.GetTitle(), pr.GetNumber())
}

func generateCommitBody(pr *github.PullRequest) (string, error) {
	body := newCommitBody(pr)
	o := new(bytes.Buffer)
	if err := bodyTpl.Execute(o, body); err != nil {
		return "", err
	}
	return o.String(), nil
}

func (gh *ghClient) sendMsg(ctx context.Context, owner, repo string, prNumber int, msg string) error {
	_, _, err := gh.client.Issues.CreateComment(ctx, owner, repo, prNumber, &github.IssueComment{
		Body: &msg,
	})
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

func newCommitBody(pr *github.PullRequest) commitBody {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.GetName())
	}
	description, releaseNote := splitReleaseNote(pr.GetBody())
	return commitBody{
		Message:     description,
		Labels:      labels,
		ReleaseNote: releaseNote,
	}
}

type commitBody struct {
	Labels      []string
	Message     string
	ReleaseNote string
}

var bodyTpl = template.Must(template.New("commit").Parse(`
{{- if .Message }}
{{ .Message }}
{{- end }}
{{if .Labels}}
Labels:
{{- range .Labels }}
  * {{ . }}
{{- end -}}
{{- end -}}
` +
	"\n\n```release-note\n* {{ .ReleaseNote }}\n```",
))

var (
	needApproveRegexp = regexp.MustCompile("At least ([0-9]+) approving review is required by reviewers with write access")
	releaseNoteRegexp = regexp.MustCompile("```release-note\n(.+?)\n```")
)

// errMsg returns error message to post from error.
// Especially handing error from github. go-github does not have error type for some cases.
func errMsg(err error) string {
	if err == nil {
		return "Succeeded!"
	}
	ss := needApproveRegexp.FindStringSubmatch(err.Error())
	if len(ss) == 2 {
		return fmt.Sprintf("Need %s approving review", ss[1])
	}
	return err.Error()
}

// splitReleaseNote returns description and release note from commit body.
// if release note is empty, return whole body and "NONE"
func splitReleaseNote(body string) (description, releaseNote string) {
	ss := releaseNoteRegexp.FindStringSubmatch(body)
	if len(ss) != 2 {
		return body, "NONE"
	}
	if rn := strings.TrimSpace(ss[1]); rn != "" {
		return strings.ReplaceAll(body, ss[0], ""), rn
	}
	return body, "NONE"
}
