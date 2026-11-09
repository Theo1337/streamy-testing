package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"streamy/internal/version"
)

// GitHubAsset is a downloadable release asset.
type GitHubAsset struct {
	Name        string `json:"name"`
	BrowserURL  string `json:"browser_download_url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// GitHubRelease represents a GitHub release.
type GitHubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	PublishedAt string        `json:"published_at"`
	HTMLURL     string        `json:"html_url"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []GitHubAsset `json:"assets"`
}

// Checker checks for updates from GitHub releases.
type Checker struct {
	repo       string
	httpClient *http.Client
	apiBase    string
}

// NewChecker creates a new update checker for the given GitHub repository.
func NewChecker(repo string) *Checker {
	return NewCheckerWithClient(repo, &http.Client{Timeout: 10 * time.Second}, "https://api.github.com")
}

// NewCheckerWithClient is useful for tests and for deployments that proxy the
// GitHub API. apiBase should contain the scheme and host, without a trailing
// slash.
func NewCheckerWithClient(repo string, client *http.Client, apiBase string) *Checker {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Checker{
		repo:       strings.Trim(repo, "/ "),
		httpClient: client,
		apiBase:    strings.TrimRight(strings.TrimSpace(apiBase), "/"),
	}
}

// CheckResult holds the result of an update check.
type CheckResult struct {
	UpdateAvailable bool
	CurrentVersion  string
	LatestVersion   string
	ReleaseNotes    string
	DownloadURL     string
	DownloadSize    int64
	ReleaseURL      string
	PublishedAt     string
	Required        bool // If true, update is critical and should override disabled settings
}

// Check checks for updates from GitHub releases.
func (c *Checker) Check(ctx context.Context) (CheckResult, error) {
	current := normalizeVersion(version.Version)
	if current == "" {
		return CheckResult{}, fmt.Errorf("current version is empty")
	}
	currentVersion, err := parseVersion(current)
	if err != nil {
		return CheckResult{}, fmt.Errorf("current version %q is invalid: %w", version.Version, err)
	}
	if c.apiBase == "" {
		c.apiBase = "https://api.github.com"
	}

	requestURL := fmt.Sprintf("%s/repos/%s/releases", c.apiBase, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return CheckResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Streamy-UpdateChecker")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CheckResult{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return CheckResult{}, fmt.Errorf("repository not found or no releases")
	}
	if resp.StatusCode != http.StatusOK {
		return CheckResult{}, fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	var releases []GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return CheckResult{}, fmt.Errorf("decode response: %w", err)
	}

	// GitHub normally returns newest first, but do not rely on that ordering.
	// A manually created older release or a pagination change must not make the
	// updater install the wrong version.
	var latestRelease *GitHubRelease
	var latestVersion semanticVersion
	for i := range releases {
		release := &releases[i]
		if release.Draft {
			continue
		}
		candidate, err := parseVersion(normalizeVersion(release.TagName))
		if err != nil {
			continue
		}
		if latestRelease == nil || compareSemantic(candidate, latestVersion) > 0 {
			latestRelease = release
			latestVersion = candidate
		}
	}
	if latestRelease == nil {
		return CheckResult{}, fmt.Errorf("no non-draft releases with valid versions found")
	}

	available := compareSemantic(currentVersion, latestVersion) < 0
	asset, hasAsset := findWindowsInstallerAsset(latestRelease.Assets)
	if available && !hasAsset {
		return CheckResult{}, fmt.Errorf("release %q has no Windows installer asset", latestRelease.TagName)
	}
	if available {
		parsed, err := url.Parse(asset.BrowserURL)
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(path.Ext(parsed.Path), ".exe") {
			return CheckResult{}, fmt.Errorf("release %q has an unsafe installer URL", latestRelease.TagName)
		}
	}

	return CheckResult{
		UpdateAvailable: available,
		CurrentVersion:  version.Version,
		LatestVersion:   latestRelease.TagName,
		ReleaseNotes:    latestRelease.Body,
		DownloadURL:     asset.BrowserURL,
		DownloadSize:    asset.Size,
		ReleaseURL:      latestRelease.HTMLURL,
		PublishedAt:     latestRelease.PublishedAt,
		Required:        isRequiredRelease(latestRelease),
	}, nil
}

// normalizeVersion removes the common release-tag prefix.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

type semanticVersion struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease []string
}

var versionPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

func parseVersion(value string) (semanticVersion, error) {
	match := versionPattern.FindStringSubmatch(normalizeVersion(value))
	if match == nil {
		return semanticVersion{}, fmt.Errorf("expected semantic version")
	}
	parseNumber := func(raw string) (uint64, error) {
		if raw == "" {
			return 0, nil
		}
		return strconv.ParseUint(raw, 10, 64)
	}
	major, err := parseNumber(match[1])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid major number: %w", err)
	}
	minor, err := parseNumber(match[2])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid minor number: %w", err)
	}
	patch, err := parseNumber(match[3])
	if err != nil {
		return semanticVersion{}, fmt.Errorf("invalid patch number: %w", err)
	}
	var prerelease []string
	if match[4] != "" {
		prerelease = strings.Split(match[4], ".")
		for _, identifier := range prerelease {
			if identifier == "" {
				return semanticVersion{}, fmt.Errorf("empty prerelease identifier")
			}
			if len(identifier) > 1 && identifier[0] == '0' {
				numeric := true
				for _, r := range identifier {
					if r < '0' || r > '9' {
						numeric = false
						break
					}
				}
				if numeric {
					return semanticVersion{}, fmt.Errorf("numeric prerelease identifier %q has leading zero", identifier)
				}
			}
		}
	}
	return semanticVersion{major: major, minor: minor, patch: patch, prerelease: prerelease}, nil
}

func compareSemantic(a, b semanticVersion) int {
	for _, pair := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(a.prerelease) == 0 && len(b.prerelease) != 0 {
		return 1
	}
	if len(a.prerelease) != 0 && len(b.prerelease) == 0 {
		return -1
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		left, right := a.prerelease[i], b.prerelease[i]
		leftNumber, leftErr := strconv.ParseUint(left, 10, 64)
		rightNumber, rightErr := strconv.ParseUint(right, 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}

// compareVersions compares two semantic version strings. Invalid values compare
// lexically for backwards compatibility with callers that only need ordering;
// Check uses parseVersion and rejects invalid release tags before installing.
func compareVersions(a, b string) int {
	left, leftErr := parseVersion(a)
	right, rightErr := parseVersion(b)
	if leftErr != nil || rightErr != nil {
		return strings.Compare(normalizeVersion(a), normalizeVersion(b))
	}
	return compareSemantic(left, right)
}

func findWindowsInstallerAsset(assets []GitHubAsset) (GitHubAsset, bool) {
	// Only the published Windows setup executable is supported. A release
	// page, ZIP, or MSI cannot safely be passed to exec.Command as an installer.
	for _, asset := range assets {
		name := strings.ToLower(strings.TrimSpace(asset.Name))
		if asset.BrowserURL == "" || !strings.HasSuffix(name, ".exe") {
			continue
		}
		if strings.HasSuffix(name, "-setup.exe") || strings.HasSuffix(name, "-installer.exe") || strings.Contains(name, "streamy") {
			return asset, true
		}
	}
	return GitHubAsset{}, false
}

// findWindowsInstaller retains the small helper API used by older callers.
func findWindowsInstaller(assets []GitHubAsset) string {
	asset, ok := findWindowsInstallerAsset(assets)
	if !ok {
		return ""
	}
	return asset.BrowserURL
}

// isRequiredRelease checks if a release is marked as required.
// A release is considered required if its body contains "[REQUIRED]" or "required: true".
func isRequiredRelease(release *GitHubRelease) bool {
	body := strings.ToLower(release.Body)
	return strings.Contains(body, "[required]") || strings.Contains(body, "required: true")
}
