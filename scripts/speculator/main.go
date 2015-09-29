// speculator allows you to preview pull requests to the matrix.org specification.
// It serves the following HTTP endpoints:
//  - / lists open pull requests
//  - /spec/123 which renders the spec as html at pull request 123.
//  - /diff/rst/123 which gives a diff of the spec's rst at pull request 123.
//  - /diff/html/123 which gives a diff of the spec's HTML at pull request 123.
// It is currently woefully inefficient, and there is a lot of low hanging fruit for improvement.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
)

type PullRequest struct {
	Number  int
	Base    Commit
	Head    Commit
	Title   string
	User    User
	HTMLURL string `json:"html_url"`
}

type Commit struct {
	SHA  string
	Repo RequestRepo
}

type RequestRepo struct {
	CloneURL string `json:"clone_url"`
}

type User struct {
	Login   string
	HTMLURL string `json:"html_url"`
}

var (
	port           = flag.Int("port", 9000, "Port on which to listen for HTTP")
	allowedMembers map[string]bool
)

func (u *User) IsTrusted() bool {
	return allowedMembers[u.Login]
}

const pullsPrefix = "https://api.github.com/repos/matrix-org/matrix-doc/pulls"

func gitClone(url string) (string, error) {
	dst := path.Join("/tmp/matrix-doc", strconv.FormatInt(rand.Int63(), 10))
	cmd := exec.Command("git", "clone", url, dst)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error cloning repo: %v", err)
	}
	return dst, nil
}

func gitCheckout(path, sha string) error {
	cmd := exec.Command("git", "checkout", sha)
	cmd.Dir = path
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error checking out repo: %v", err)
	}
	return nil
}

func lookupPullRequest(url url.URL, pathPrefix string) (*PullRequest, error) {
	if !strings.HasPrefix(url.Path, pathPrefix+"/") {
		return nil, fmt.Errorf("invalid path passed: %s expect %s/123", url.Path, pathPrefix)
	}
	prNumber := url.Path[len(pathPrefix)+1:]
	if strings.Contains(prNumber, "/") {
		return nil, fmt.Errorf("invalid path passed: %s expect %s/123", url.Path, pathPrefix)
	}

	resp, err := http.Get(fmt.Sprintf("%s/%s", pullsPrefix, prNumber))
	defer resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("error getting pulls: %v", err)
	}
	dec := json.NewDecoder(resp.Body)
	var pr PullRequest
	if err := dec.Decode(&pr); err != nil {
		return nil, fmt.Errorf("error decoding pulls: %v", err)
	}
	return &pr, nil
}

func generate(dir string) error {
	cmd := exec.Command("python", "gendoc.py", "--nodelete")
	cmd.Dir = path.Join(dir, "scripts")
	var b bytes.Buffer
	cmd.Stderr = &b
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error generating spec: %v\nOutput from gendoc:\n%v", err, b.String())
	}
	return nil
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.WriteHeader(code)
	io.WriteString(w, fmt.Sprintf("%v\n", err))
}

// generateAt generates spec from repo at sha.
// Returns the path where the generation was done.
func generateAt(repo, sha string) (dst string, err error) {
	dst, err = gitClone(repo)
	if err != nil {
		return
	}

	if err = gitCheckout(dst, sha); err != nil {
		return
	}

	err = generate(dst)
	return
}

func serveSpec(w http.ResponseWriter, req *http.Request) {
	var cloneURL string
	var sha string

	if strings.ToLower(req.URL.Path) == "/spec/head" {
		cloneURL = "https://github.com/matrix-org/matrix-doc.git"
		sha = "HEAD"
	} else {
		pr, err := lookupPullRequest(*req.URL, "/spec")
		if err != nil {
			writeError(w, 400, err)
			return
		}

		// We're going to run whatever Python is specified in the pull request, which
		// may do bad things, so only trust people we trust.
		if err := checkAuth(pr); err != nil {
			writeError(w, 403, err)
			return
		}
		cloneURL = pr.Head.Repo.CloneURL
		sha = pr.Head.SHA
	}

	dst, err := generateAt(cloneURL, sha)
	defer os.RemoveAll(dst)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	b, err := ioutil.ReadFile(path.Join(dst, "scripts/gen/specification.html"))
	if err != nil {
		writeError(w, 500, fmt.Errorf("Error reading spec: %v", err))
		return
	}
	w.Write(b)
}

func checkAuth(pr *PullRequest) error {
	if !pr.User.IsTrusted() {
		return fmt.Errorf("%q is not a trusted pull requester", pr.User.Login)
	}
	return nil
}

func serveRSTDiff(w http.ResponseWriter, req *http.Request) {
	pr, err := lookupPullRequest(*req.URL, "/diff/rst")
	if err != nil {
		writeError(w, 400, err)
		return
	}

	// We're going to run whatever Python is specified in the pull request, which
	// may do bad things, so only trust people we trust.
	if err := checkAuth(pr); err != nil {
		writeError(w, 403, err)
		return
	}

	base, err := generateAt(pr.Base.Repo.CloneURL, pr.Base.SHA)
	defer os.RemoveAll(base)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	head, err := generateAt(pr.Head.Repo.CloneURL, pr.Head.SHA)
	defer os.RemoveAll(head)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	diffCmd := exec.Command("diff", "-u", path.Join(base, "scripts", "tmp", "full_spec.rst"), path.Join(head, "scripts", "tmp", "full_spec.rst"))
	var diff bytes.Buffer
	diffCmd.Stdout = &diff
	if err := ignoreExitCodeOne(diffCmd.Run()); err != nil {
		writeError(w, 500, fmt.Errorf("error running diff: %v", err))
		return
	}
	w.Write(diff.Bytes())
}

func serveHTMLDiff(w http.ResponseWriter, req *http.Request) {
	pr, err := lookupPullRequest(*req.URL, "/diff/html")
	if err != nil {
		writeError(w, 400, err)
		return
	}

	// We're going to run whatever Python is specified in the pull request, which
	// may do bad things, so only trust people we trust.
	if err := checkAuth(pr); err != nil {
		writeError(w, 403, err)
		return
	}

	base, err := generateAt(pr.Base.Repo.CloneURL, pr.Base.SHA)
	defer os.RemoveAll(base)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	head, err := generateAt(pr.Head.Repo.CloneURL, pr.Head.SHA)
	defer os.RemoveAll(head)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	htmlDiffer, err := findHTMLDiffer()
	if err != nil {
		writeError(w, 500, fmt.Errorf("could not find HTML differ"))
		return
	}

	cmd := exec.Command(htmlDiffer, path.Join(base, "scripts", "gen", "specification.html"), path.Join(head, "scripts", "gen", "specification.html"))
	var b bytes.Buffer
	cmd.Stdout = &b
	if err := cmd.Run(); err != nil {
		writeError(w, 500, fmt.Errorf("error running HTML differ: %v", err))
		return
	}
	w.Write(b.Bytes())
}

func findHTMLDiffer() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	differ := path.Join(wd, "htmldiff.pl")
	if _, err := os.Stat(differ); err == nil {
		return differ, nil
	}
	return "", fmt.Errorf("unable to find htmldiff.pl")
}

func listPulls(w http.ResponseWriter, req *http.Request) {
	resp, err := http.Get(pullsPrefix)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var pulls []PullRequest
	if err := dec.Decode(&pulls); err != nil {
		writeError(w, 500, err)
		return
	}
	if len(pulls) == 0 {
		io.WriteString(w, "No pull requests found")
		return
	}
	s := "<body><ul>"
	for _, pull := range pulls {
		s += fmt.Sprintf(`<li>%d: <a href="%s">%s</a>: <a href="%s">%s</a>: <a href="spec/%d">spec</a> <a href="diff/html/%d">spec diff</a> <a href="diff/rst/%d">rst diff</a></li>`,
			pull.Number, pull.User.HTMLURL, pull.User.Login, pull.HTMLURL, pull.Title, pull.Number, pull.Number, pull.Number)
	}
	s += `</ul><div><a href="spec/head">View the spec at head</a></div></body>`
	io.WriteString(w, s)
}

func ignoreExitCodeOne(err error) error {
	if err == nil {
		return err
	}

	if exiterr, ok := err.(*exec.ExitError); ok {
		if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
			if status.ExitStatus() == 1 {
				return nil
			}
		}
	}
	return err
}

func main() {
	flag.Parse()
	// It would be great to read this from github, but there's no convenient way to do so.
	// Most of these memberships are "private", so would require some kind of auth.
	allowedMembers = map[string]bool{
		"dbkr":          true,
		"erikjohnston":  true,
		"illicitonion":  true,
		"Kegsay":        true,
		"NegativeMjark": true,
	}
	http.HandleFunc("/spec/", serveSpec)
	http.HandleFunc("/diff/rst/", serveRSTDiff)
	http.HandleFunc("/diff/html/", serveHTMLDiff)
	http.HandleFunc("/healthz", serveText("ok"))
	http.HandleFunc("/", listPulls)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

func serveText(s string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, s)
	}
}
