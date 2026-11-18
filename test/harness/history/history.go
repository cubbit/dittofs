// Package history reads past dt runs, from a checkout's results or from what
// the develop runs of the tests workflow uploaded, and counts how each suite
// and each test went over them (docs/internals/rfcs/rfc-29-test-harness.md).
package history

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cubbit/dittofs/test/harness/report"
)

// reRun matches the name of a job's re-run, RUN-a2: its time, its commit and
// its attempt. The first attempt's run is the one history reads.
var reRun = regexp.MustCompile(`^[^-]+-[^-]+-a[0-9]+$`)

// Local returns the last n runs in results, oldest first, a re-run's left
// out.
func Local(results string, n int) ([]report.Run, error) {
	entries, err := os.ReadDir(results)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !reRun.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	var runs []report.Run
	for _, name := range names[max(0, len(names)-n):] {
		r, err := report.Read(filepath.Join(results, name))
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// CI returns the last n finished runs of the tests workflow on branch,
// develop when it is "", oldest first, as the runs their jobs made, each with
// its own tier and OS, and the workflow run's name as dt names a run, by when
// it started and its commit; only each job's first attempt is read. A run of
// a pull request is left out, since one from a fork's branch called develop
// is listed as develop's too, and so is a cancelled run, as a push replaced
// while it waited is, with no results. Each job uploads its cell's results as
// the artifact dt-JOB-CELL-ATTEMPT, the cell's name with - for /, so with
// suite only the artifacts of its cells are read.
//
// An artifact that can't be read, as one gone or cut short, is skipped, and
// warn says so. GITHUB_API_URL and GITHUB_REPOSITORY say where the API is
// and which repository it's asked about, DittoFS's by default. Reading an
// artifact needs a token, GITHUB_TOKEN.
func CI(ctx context.Context, suite string, n int, branch string, warn io.Writer) ([]report.Run, error) {
	c, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	branch = url.QueryEscape(cmp.Or(branch, "develop"))
	var found []workflowRun
	for page := 1; len(found) < n; page++ {
		var got struct {
			Runs []workflowRun `json:"workflow_runs"`
		}
		if err := c.getJSON(fmt.Sprintf("%s/actions/workflows/tests.yml/runs?branch=%s&status=completed&per_page=100&page=%d", c.base, branch, page), &got); err != nil {
			return nil, err
		}
		if len(got.Runs) == 0 {
			break
		}
		for _, r := range got.Runs {
			if r.Event != "pull_request" && r.Conclusion != "cancelled" && len(found) < n {
				found = append(found, r)
			}
		}
	}

	var runs []report.Run
	for _, w := range slices.Backward(found) {
		got, err := c.readRun(w, suite, warn)
		if err != nil {
			return nil, err
		}
		runs = append(runs, got...)
	}
	return runs, nil
}

// Workflow returns the runs of one run of the tests workflow, by its ID, as
// CI returns a listed one's, whatever its branch and its event. A run that
// isn't there is an error.
func Workflow(ctx context.Context, suite string, id int64, warn io.Writer) ([]report.Run, error) {
	c, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	var w workflowRun
	if err := c.getJSON(fmt.Sprintf("%s/actions/runs/%d", c.base, id), &w); err != nil {
		return nil, err
	}
	return c.readRun(w, suite, warn)
}

// client asks the GitHub API of the repository history is about, with the
// token.
type client struct {
	ctx         context.Context
	base, token string
}

func newClient(ctx context.Context) (*client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("reading the runs' artifacts needs a token in GITHUB_TOKEN, as GITHUB_TOKEN=$(gh auth token)")
	}
	return &client{ctx, cmp.Or(os.Getenv("GITHUB_API_URL"), "https://api.github.com") + "/repos/" + cmp.Or(os.Getenv("GITHUB_REPOSITORY"), "cubbit/dittofs"), token}, nil
}

func (c *client) get(url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (c *client) getJSON(url string, v any) error {
	data, err := c.get(url)
	if err == nil {
		err = json.Unmarshal(data, v)
	}
	return err
}

type workflowRun struct {
	ID         int64     `json:"id"`
	Event      string    `json:"event"`
	Conclusion string    `json:"conclusion"`
	HeadSHA    string    `json:"head_sha"`
	CreatedAt  time.Time `json:"created_at"`
}

// readRun reads the cells of one workflow run's artifacts, with suite only
// those of its cells, each job's run under the workflow run's name. An
// artifact that can't be read is skipped and warn says so.
func (c *client) readRun(w workflowRun, suite string, warn io.Writer) ([]report.Run, error) {
	prefix := "" // of the cells' names the artifacts read are for
	if suite != "" {
		prefix = suite + "-"
	}
	type artifact struct {
		Name    string `json:"name"`
		Expired bool   `json:"expired"`
		URL     string `json:"archive_download_url"`
	}
	name := w.CreatedAt.UTC().Format("20060102T150405Z") + "-" + w.HeadSHA[:min(8, len(w.HeadSHA))]
	var artifacts []artifact
	for page, total := 1, 1; len(artifacts) < total; page++ {
		var got struct {
			Total     int        `json:"total_count"`
			Artifacts []artifact `json:"artifacts"`
		}
		if err := c.getJSON(fmt.Sprintf("%s/actions/runs/%d/artifacts?per_page=100&page=%d", c.base, w.ID, page), &got); err != nil {
			return nil, err
		}
		if len(got.Artifacts) == 0 {
			break
		}
		artifacts, total = append(artifacts, got.Artifacts...), got.Total
	}
	var runs []report.Run
	for _, a := range artifacts {
		if a.Expired || !strings.HasPrefix(a.Name, "dt-run-"+prefix) && !strings.HasPrefix(a.Name, "dt-non-blocking-"+prefix) {
			continue
		}
		data, err := c.get(a.URL)
		var jobs []report.Run
		if err == nil {
			jobs, err = jobRuns(a.Name, data)
		}
		if err != nil {
			_, _ = fmt.Fprintf(warn, "skipped the artifact %s of run %s: %v\n", a.Name, name, err)
			continue
		}
		for _, r := range jobs {
			r.Name = name
			runs = append(runs, r)
		}
	}
	return runs, nil
}

// jobRuns are the runs an artifact holds: a job's results directory, with
// the one run it made, unless it's a re-run's.
func jobRuns(name string, data []byte) ([]report.Run, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var runs []report.Run
	for _, f := range z.File {
		dir, file := path.Split(f.Name)
		if file != "summary.json" || strings.Count(dir, "/") != 1 || reRun.MatchString(strings.TrimSuffix(dir, "/")) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		r, err := report.Parse(name+"/"+f.Name, data)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}
