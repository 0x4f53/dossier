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

type BitbucketCommit struct {
	Hash    string `json:"hash"`
	Date    string `json:"date"`
	Message string `json:"message"`
	Author  struct {
		Raw string `json:"raw"` // e.g. "John Doe <john@example.com>"
	} `json:"author"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type BitbucketCommitPage struct {
	Values []BitbucketCommit `json:"values"`
	Next   string            `json:"next"`
}

type Repo struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

type RepoPage struct {
	Values []Repo `json:"values"`
	Next   string `json:"next"`
}

// ========================== Globals ==========================

var bitbucketUser string

// ========================== HTTP Helpers ==========================

func makeRequest(url string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
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

// ========================== Commit Processing ==========================

func ProcessCommits(commits []BitbucketCommit, cfg *Config, blacklist []*regexp.Regexp, repoName string) {
	for _, c := range commits {
		commitDate := c.Date
		if t, err := time.Parse(time.RFC3339, commitDate); err == nil {
			commitDate = t.Format("2006-01-02 15:04:05 MST")
		}

		// Parse "John Doe <email>" from Raw
		name, email := parseRawAuthor(c.Author.Raw)

		if IsValidEmail(email) && !IsBlacklisted(email, blacklist) {
			fmt.Printf("Email: %s\n", email)
			fmt.Printf("Name: %s\n", name)
			fmt.Printf("Date: %s\n", commitDate)
			fmt.Printf("Repo: %s\n", repoName)
			fmt.Printf("Location: %s\n\n", c.Links.HTML.Href)
		}

		commitText := fmt.Sprintf("%s %s %s", c.Message, c.Author.Raw, repoName)

		for _, m := range SearchPatterns(commitText, cfg.OperatingSystems) {
			fmt.Printf("Operating System: %s\nDate: %s\nRepo: %s\nLocation: %s\n\n",
				m, commitDate, repoName, c.Links.HTML.Href)
		}

		for _, m := range SearchPatterns(commitText, cfg.Utilities) {
			fmt.Printf("Utility: %s\nDate: %s\nRepo: %s\nLocation: %s\n\n",
				m, commitDate, repoName, c.Links.HTML.Href)
		}
	}
}

func parseRawAuthor(raw string) (string, string) {
	if strings.Contains(raw, "<") && strings.Contains(raw, ">") {
		parts := strings.SplitN(raw, "<", 2)
		name := strings.TrimSpace(parts[0])
		email := strings.TrimSuffix(parts[1], ">")
		return name, email
	}
	return raw, ""
}

// ========================== Repos and Commits ==========================

func GetUserRepos(username string) ([]Repo, error) {
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s?pagelen=100", username)
	var repos []Repo

	for url != "" {
		body, status, err := makeRequest(url)
		if err != nil {
			return nil, err
		}
		if status != 200 {
			return nil, fmt.Errorf("Bitbucket API error %d\n%s", status, string(body))
		}

		var page RepoPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		repos = append(repos, page.Values...)
		url = page.Next
	}
	return repos, nil
}

func ScanRepoCommits(username, repoSlug, repoName string, cfg *Config, blacklist []*regexp.Regexp, ascending bool) {
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/commits?pagelen=100", username, repoSlug)
	var allCommits []BitbucketCommit

	for url != "" {
		body, status, err := makeRequest(url)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if status != 200 {
			fmt.Printf("Bitbucket API error %d\n%s\n", status, string(body))
			return
		}

		var page BitbucketCommitPage
		if err := json.Unmarshal(body, &page); err != nil {
			fmt.Printf("Error parsing response: %v\n", err)
			return
		}

		allCommits = append(allCommits, page.Values...)
		url = page.Next
	}

	// Reverse if ascending
	if ascending {
		for i, j := 0, len(allCommits)-1; i < j; i, j = i+1, j-1 {
			allCommits[i], allCommits[j] = allCommits[j], allCommits[i]
		}
	}

	ProcessCommits(allCommits, cfg, blacklist, repoName)
}

// ========================== Main ==========================

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run bitbucket.go <bitbucket-username>")
		os.Exit(1)
	}
	bitbucketUser = os.Args[1]

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

	fmt.Printf("Scanning Bitbucket commits for user: %s\n\n", bitbucketUser)

	repos, err := GetUserRepos(bitbucketUser)
	if err != nil {
		fmt.Println("Error fetching repos:", err)
		os.Exit(1)
	}

	for _, r := range repos {
		fmt.Printf("Scanning repo: %s\n", r.Name)
		ScanRepoCommits(bitbucketUser, r.Slug, r.Name, cfg, blacklist, true) // ascending (oldest first)
	}
}
