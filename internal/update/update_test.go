package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckerSelectsHighestValidWindowsRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/streamy/releases" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitHubRelease{
			{TagName: "v0.9.0", HTMLURL: "https://example.invalid/old", Assets: []GitHubAsset{{Name: "streamy-setup.exe", BrowserURL: "https://example.invalid/old.exe"}}},
			{TagName: "v1.2.0", HTMLURL: "https://example.invalid/new", Assets: []GitHubAsset{{Name: "streamy-1.2.0-setup.exe", BrowserURL: "https://example.invalid/new.exe", Size: 42}}},
			{TagName: "v1.1.0", Draft: true, Assets: []GitHubAsset{{Name: "streamy-1.1.0-setup.exe", BrowserURL: "https://example.invalid/draft.exe"}}},
		})
	}))
	defer server.Close()

	checker := NewCheckerWithClient("acme/streamy", server.Client(), server.URL)
	result, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.UpdateAvailable || result.LatestVersion != "v1.2.0" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.DownloadURL != "https://example.invalid/new.exe" || result.DownloadSize != 42 {
		t.Fatalf("unexpected asset: %+v", result)
	}
}

func TestCheckerRejectsUnsupportedAssetInsteadOfReleasePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]GitHubRelease{{
			TagName: "v9.0.0",
			HTMLURL: "https://example.invalid/release",
			Assets:  []GitHubAsset{{Name: "streamy.zip", BrowserURL: "https://example.invalid/streamy.zip"}},
		}})
	}))
	defer server.Close()

	_, err := NewCheckerWithClient("acme/streamy", server.Client(), server.URL).Check(context.Background())
	if err == nil {
		t.Fatal("Check() accepted a release without a Windows installer")
	}
}

func TestCompareSemanticPrereleaseOrdering(t *testing.T) {
	cases := [][3]string{
		{"1.0.0-alpha", "1.0.0-beta", "-1"},
		{"1.0.0-rc.1", "1.0.0-rc.2", "-1"},
		{"1.0.0", "1.0.0-rc.9", "1"},
		{"1.10.0", "1.9.99", "1"},
	}
	for _, tc := range cases {
		left, err := parseVersion(tc[0])
		if err != nil {
			t.Fatalf("parse %q: %v", tc[0], err)
		}
		right, err := parseVersion(tc[1])
		if err != nil {
			t.Fatalf("parse %q: %v", tc[1], err)
		}
		want := 0
		if tc[2] == "-1" {
			want = -1
		} else if tc[2] == "1" {
			want = 1
		}
		if got := compareSemantic(left, right); got != want {
			t.Errorf("compareSemantic(%q, %q) = %d, want %d", tc[0], tc[1], got, want)
		}
	}
}

func TestFindWindowsInstallerRejectsMSIAndHTML(t *testing.T) {
	if got, ok := findWindowsInstallerAsset([]GitHubAsset{
		{Name: "release.msi", BrowserURL: "https://example.invalid/release.msi"},
		{Name: "release.html", BrowserURL: "https://example.invalid/release.html"},
	}); ok || got.BrowserURL != "" {
		t.Fatalf("unsupported assets selected: %+v, %v", got, ok)
	}
}
