package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
	"gopkg.in/yaml.v3"
)

// ========================== Structs ==========================

type Pattern struct {
	ID    string `yaml:"id"`
	Regex string `yaml:"regex"`
}

type Config struct {
	OperatingSystems []Pattern `yaml:"operating_systems"`
	Utilities        []Pattern `yaml:"utilities"`
}

type GitLabCommit struct {
	ID           string `json:"id"`
	ShortID      string `json:"short_id"`
	Title        string `json:"title"`
	AuthorName   string `json:"author_name"`
	AuthorEmail  string `json:"author_email"`
	AuthoredDate string `json:"authored_date"`
	WebURL       string `json:"web_url,omitempty"` // GitLab doesn’t always include this
}

type GitLabProject struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	Path              string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	ForkedFromProject *struct {
		ID int `json:"id"`
	} `json:"forked_from_project"`
}

type GitLabUser struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// ========================== Globals ==========================

var gitlabToken string

// ========================== HTTP Helpers ==========================

func makeRequest(url string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	if gitlabToken != "" {
		req.Header.Set("PRIVATE-TOKEN", gitlabToken)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// ========================== YAML / Config ==========================

func LoadPatterns(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SearchPatterns(text string, patterns []Pattern) []string {
	var matches []string
	for _, pat := range patterns {
		re := regexp.MustCompile(pat.Regex)
		if re.MatchString(text) {
			matches = append(matches, pat.ID)
		}
	}
	return matches
}

// ========================== Blacklist ==========================

func LoadBlacklist(filename string) ([]*regexp.Regexp, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var regexes []*regexp.Regexp
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re := regexp.MustCompile(line)
		regexes = append(regexes, re)
	}
	return regexes, scanner.Err()
}

func IsBlacklisted(email string, blacklist []*regexp.Regexp) bool {
	for _, re := range blacklist {
		if re.MatchString(email) {
			return true
		}
	}
	return false
}

// ========================== Email Validation ==========================

func IsValidEmail(addr string) bool {
	if !strings.Contains(addr, "@") {
		return false
	}

	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return false
	}

	at := strings.LastIndex(parsed.Address, "@")
	if at < 0 {
		return false
	}
	domain := parsed.Address[at+1:]

	if !strings.Contains(domain, ".") {
		return false
	}

	eTLD, icann := publicsuffix.PublicSuffix(domain)
	if eTLD == "" || !icann {
		return false
	}

	if _, err := url.Parse("http://" + domain); err != nil {
		return false
	}

	return true
}

// ========================== Env Loader ==========================

func LoadEnvToken(filename string) string {
	file, err := os.Open(filename)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "GITLAB_TOKEN=") {
			return strings.TrimPrefix(line, "GITLAB_TOKEN=")
		}
	}
	return ""
}

// ========================== Commit Processing ==========================

func ProcessCommits(commits []GitLabCommit, cfg *Config, blacklist []*regexp.Regexp, projectURL string) {
	for _, c := range commits {
		commitDate := c.AuthoredDate
		if t, err := time.Parse(time.RFC3339, commitDate); err == nil {
			commitDate = t.Format("2006-01-02 15:04:05 MST")
		}

		if IsValidEmail(c.AuthorEmail) && !IsBlacklisted(c.AuthorEmail, blacklist) {
			fmt.Printf("Email: %s\n", c.AuthorEmail)
			fmt.Printf("Name: %s\n", c.AuthorName)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Project: %s\n\n", projectURL)
		}

		for _, m := range SearchPatterns(c.Title, cfg.OperatingSystems) {
			fmt.Printf("Operating System: %s\n", m)
			fmt.Printf("Committer: %s <%s>\n", c.AuthorName, c.AuthorEmail)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Project: %s\n\n", projectURL)
		}

		for _, m := range SearchPatterns(c.Title, cfg.Utilities) {
			fmt.Printf("Utility: %s\n", m)
			fmt.Printf("Committer: %s <%s>\n", c.AuthorName, c.AuthorEmail)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Project: %s\n\n", projectURL)
		}
	}
}

// ========================== Repo Commits Mode ==========================

func GetUserID(username string) (int, error) {
	url := fmt.Sprintf("https://gitlab.com/api/v4/users?username=%s", username)
	body, status, err := makeRequest(url)
	if err != nil {
		return 0, err
	}
	if status != 200 {
		return 0, fmt.Errorf("GitLab API error %d\n%s", status, string(body))
	}
	var users []GitLabUser
	if err := json.Unmarshal(body, &users); err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, fmt.Errorf("No GitLab user found for %s", username)
	}
	return users[0].ID, nil
}

func GetUserProjects(userID int) ([]GitLabProject, error) {
	page := 1
	var projects []GitLabProject
	for {
		url := fmt.Sprintf("https://gitlab.com/api/v4/users/%d/projects?per_page=100&page=%d", userID, page)
		body, status, err := makeRequest(url)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("GitLab API error %d\n%s", status, string(body))
		}

		var tmp []GitLabProject
		if err := json.Unmarshal(body, &tmp); err != nil {
			return nil, err
		}
		if len(tmp) == 0 {
			break
		}
		projects = append(projects, tmp...)
		page++
	}
	return projects, nil
}

func ScanProjectCommits(project GitLabProject, cfg *Config, blacklist []*regexp.Regexp, ascending bool) {
	page := 1
	var allCommits []GitLabCommit

	for {
		url := fmt.Sprintf("https://gitlab.com/api/v4/projects/%d/repository/commits?per_page=100&page=%d", project.ID, page)
		body, status, err := makeRequest(url)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if status != 200 {
			fmt.Printf("GitLab API error %d\n%s\n", status, string(body))
			return
		}

		var commits []GitLabCommit
		if err := json.Unmarshal(body, &commits); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}
		if len(commits) == 0 {
			break
		}

		allCommits = append(allCommits, commits...)
		page++
	}

	// Reverse order if user requested ascending (oldest first)
	if ascending {
		for i, j := 0, len(allCommits)-1; i < j; i, j = i+1, j-1 {
			allCommits[i], allCommits[j] = allCommits[j], allCommits[i]
		}
	}

	ProcessCommits(allCommits, cfg, blacklist, project.WebURL)
}


// ========================== Main ==========================

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <gitlab-username>")
		os.Exit(1)
	}
	username := os.Args[1]

	gitlabToken = LoadEnvToken(".env")
	if gitlabToken != "" {
		fmt.Println("🔑 Found GitLab token in .env!")
	} else {
		fmt.Println("⚠️  No GitLab personal access token found in env, running unauthenticated (with rate limits)")
	}

	cfg, err := LoadPatterns("signatures.yaml")
	if err != nil {
		fmt.Println("Error reading YAML:", err)
		os.Exit(1)
	}

	blacklist, err := LoadBlacklist("blacklist.txt")
	if err != nil {
		fmt.Println("Error reading blacklist:", err)
		os.Exit(1)
	}

	fmt.Printf("Scanning GitLab commits for user: %s\n\n", username)

	userID, err := GetUserID(username)
	if err != nil {
		fmt.Println("Error fetching user:", err)
		os.Exit(1)
	}

	projects, err := GetUserProjects(userID)
	if err != nil {
		fmt.Println("Error fetching projects:", err)
		os.Exit(1)
	}

	for _, p := range projects {
		if p.ForkedFromProject != nil {
			continue // skip forks
		}
		fmt.Printf("Scanning project: %s\n", p.Path)
		ScanProjectCommits(p, cfg, blacklist, true)  // oldest first
		ScanProjectCommits(p, cfg, blacklist, false) // newest first
	}
}
