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

type CommitInfo struct {
	Author struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Date  string `json:"date"`
	} `json:"author"`
	Committer struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Date  string `json:"date"`
	} `json:"committer"`
	Message string `json:"message"`
}

type CommitItem struct {
	SHA     string     `json:"sha"`
	HTMLURL string     `json:"html_url"`
	Commit  CommitInfo `json:"commit"`
}

type SearchResponse struct {
	Items []CommitItem `json:"items"`
}

type Repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Fork     bool   `json:"fork"`
}

// ========================== Globals ==========================

var githubToken string

// ========================== HTTP Helpers ==========================

func makeRequest(url string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	if githubToken != "" {
		req.Header.Set("Authorization", "token "+githubToken)
	}
	req.Header.Set("Accept", "application/vnd.github.cloak-preview+json")
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
		if strings.HasPrefix(line, "GITHUB_TOKEN=") {
			return strings.TrimPrefix(line, "GITHUB_TOKEN=")
		}
	}
	return ""
}

// ========================== Commit Processing ==========================

func ProcessCommits(items []CommitItem, cfg *Config, blacklist []*regexp.Regexp) {
	for _, c := range items {
		commitJSON, _ := json.Marshal(c)
		commitText := string(commitJSON)

		commitDate := c.Commit.Author.Date
		if t, err := time.Parse(time.RFC3339, commitDate); err == nil {
			commitDate = t.Format("2006-01-02 15:04:05 MST")
		}

		// Emails (with names)
		for _, who := range []struct {
			Name  string
			Email string
		}{
			{c.Commit.Author.Name, c.Commit.Author.Email},
			{c.Commit.Committer.Name, c.Commit.Committer.Email},
		} {
			if IsValidEmail(who.Email) && !IsBlacklisted(who.Email, blacklist) {
				fmt.Printf("Email: %s\n", who.Email)
				fmt.Printf("Name: %s\n", who.Name)
				fmt.Printf("Date: %s\n", commitDate)
				fmt.Printf("Location: %s\n\n", c.HTMLURL)
			}
		}

		// Operating systems
		for _, m := range SearchPatterns(commitText, cfg.OperatingSystems) {
			fmt.Printf("Operating System(s): %s\n", m)
			fmt.Printf("Email: %s\n", c.Commit.Author.Email)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Location: %s\n\n", c.HTMLURL)
		}

		// Utilities
		for _, m := range SearchPatterns(commitText, cfg.Utilities) {
			fmt.Printf("Detected Utility: %s\n", m)
			fmt.Printf("Committer: %s\n", c.Commit.Author.Email)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Location: %s\n\n", c.HTMLURL)
		}
	}
}

// ========================== Global Commits Mode ==========================

func ScanGlobalCommits(username string, cfg *Config, blacklist []*regexp.Regexp, ascending bool) {
	order := "asc"
	if !ascending {
		order = "desc"
	}
	page := 1
	for {
		url := fmt.Sprintf(
			"https://api.github.com/search/commits?q=author:%s&sort=author-date&order=%s&per_page=100&page=%d",
			username, order, page,
		)
		body, status, err := makeRequest(url)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if status != 200 {
			if status == 422 {
				fmt.Println("Reached 1000-result limit for search API.")
				return
			}
			fmt.Printf("GitHub API error %d\n%s\n", status, string(body))
			return
		}

		var searchResp SearchResponse
		if err := json.Unmarshal(body, &searchResp); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}

		if len(searchResp.Items) == 0 {
			break
		}

		ProcessCommits(searchResp.Items, cfg, blacklist)
		page++
	}
}

// ========================== Repo Commits Mode ==========================

func GetUserRepos(username string) ([]Repo, error) {
	page := 1
	var repos []Repo
	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&page=%d", username, page)
		body, status, err := makeRequest(url)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("GitHub API error %d\n%s", status, string(body))
		}

		var tmp []Repo
		if err := json.Unmarshal(body, &tmp); err != nil {
			return nil, err
		}
		if len(tmp) == 0 {
			break
		}
		repos = append(repos, tmp...)
		page++
	}
	return repos, nil
}

func ScanRepoCommits(repoFullName string, cfg *Config, blacklist []*regexp.Regexp) {
	page := 1
	for {
		url := fmt.Sprintf("https://api.github.com/repos/%s/commits?per_page=100&page=%d", repoFullName, page)
		body, status, err := makeRequest(url)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if status != 200 {
			fmt.Printf("GitHub API error %d\n%s\n", status, string(body))
			return
		}

		var commits []CommitItem
		if err := json.Unmarshal(body, &commits); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}
		if len(commits) == 0 {
			break
		}

		ProcessCommits(commits, cfg, blacklist)
		page++
	}
}

// ========================== Main ==========================

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <github-username>")
		os.Exit(1)
	}
	username := os.Args[1]

	githubToken = LoadEnvToken(".env")
	if githubToken != "" {
		fmt.Println("🔑 Found GitHub token in .env!")
	} else {
		fmt.Println("⚠️  No GitHub personal access token found in env, running unauthenticated (with rate limits)")
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

	fmt.Printf("Scanning commits for user: %s\n\n", username)

	// 1. First 1000 commits (ascending)
	fmt.Println("=== First 1000 commits (oldest) ===")
	ScanGlobalCommits(username, cfg, blacklist, true)

	// 2. Last 1000 commits (descending)
	fmt.Println("=== Last 1000 commits (newest) ===")
	ScanGlobalCommits(username, cfg, blacklist, false)

	// 3. Repo-by-repo scanning (full)
	fmt.Println("=== Per-repo scan (all commits) ===")
	repos, err := GetUserRepos(username)
	if err != nil {
		fmt.Println("Error fetching repos:", err)
		os.Exit(1)
	}
	for _, r := range repos {
		if r.Fork {
			continue // skip forks by default
		}
		fmt.Printf("Scanning repo: %s\n", r.FullName)
		ScanRepoCommits(r.FullName, cfg, blacklist)
	}
}
