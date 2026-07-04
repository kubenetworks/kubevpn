package upgrade

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mockTransportWithBody returns a canned HTTP response with the given body.
type mockTransportWithBody struct {
	statusCode int
	body       string
}

func (m *mockTransportWithBody) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: m.statusCode,
		Body:       io.NopCloser(strings.NewReader(m.body)),
		Header:     make(http.Header),
	}, nil
}

// githubReleaseJSON builds a mock GitHub releases API response with the given
// tag and a single asset for the current OS/arch.
func githubReleaseJSON(tag string) string {
	assetName := fmt.Sprintf("kubevpn_%s_%s_%s.zip", tag, runtime.GOOS, runtime.GOARCH)
	downloadURL := fmt.Sprintf("https://example.com/download/%s", assetName)
	return fmt.Sprintf(`{
		"tag_name": %q,
		"assets": [
			{
				"name": %q,
				"browser_download_url": %q
			}
		]
	}`, tag, assetName, downloadURL)
}

func Test_needsUpgrade(t *testing.T) {
	cases := []struct {
		name           string
		currentVersion string
		latestTag      string
		wantUpgrade    bool
	}{
		{
			name:           "current older than latest",
			currentVersion: "v1.0.0",
			latestTag:      "v2.0.0",
			wantUpgrade:    true,
		},
		{
			name:           "current equal to latest",
			currentVersion: "v2.0.0",
			latestTag:      "v2.0.0",
			wantUpgrade:    false,
		},
		{
			name:           "current newer than latest",
			currentVersion: "v3.0.0",
			latestTag:      "v2.0.0",
			wantUpgrade:    false,
		},
		{
			name:           "patch version behind",
			currentVersion: "v2.0.0",
			latestTag:      "v2.0.1",
			wantUpgrade:    true,
		},
		{
			name:           "minor version behind",
			currentVersion: "v2.0.5",
			latestTag:      "v2.1.0",
			wantUpgrade:    true,
		},
		{
			name:           "pre-release current with newer stable",
			currentVersion: "v1.9.0-rc1",
			latestTag:      "v2.0.0",
			wantUpgrade:    true,
		},
		{
			name:           "same major minor different patch",
			currentVersion: "v1.2.3",
			latestTag:      "v1.2.4",
			wantUpgrade:    true,
		},
		{
			name:           "same version no v prefix on current",
			currentVersion: "2.0.0",
			latestTag:      "v2.0.0",
			wantUpgrade:    false,
		},
		{
			name:           "major version ahead",
			currentVersion: "v3.1.0",
			latestTag:      "v2.9.9",
			wantUpgrade:    false,
		},
		{
			name:           "both pre-release current older",
			currentVersion: "v2.0.0-alpha",
			latestTag:      "v2.0.0-beta",
			wantUpgrade:    true,
		},
		{
			name:           "pre-release vs stable same base",
			currentVersion: "v2.0.0-rc1",
			latestTag:      "v2.0.0",
			wantUpgrade:    true,
		},
		{
			name:           "high patch number",
			currentVersion: "v1.99.99",
			latestTag:      "v2.0.0",
			wantUpgrade:    true,
		},
		{
			name:           "zero versions equal",
			currentVersion: "v0.0.0",
			latestTag:      "v0.0.0",
			wantUpgrade:    false,
		},
		{
			name:           "zero to first release",
			currentVersion: "v0.0.0",
			latestTag:      "v0.0.1",
			wantUpgrade:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{
				Transport: &mockTransportWithBody{
					statusCode: http.StatusOK,
					body:       githubReleaseJSON(tc.latestTag),
				},
			}

			url, latestVersion, upgrade, err := needsUpgrade(context.Background(), client, tc.currentVersion)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if upgrade != tc.wantUpgrade {
				t.Errorf("upgrade: got %v, want %v (current=%s, latest=%s)", upgrade, tc.wantUpgrade, tc.currentVersion, latestVersion)
			}
			if latestVersion != tc.latestTag {
				t.Errorf("latestVersion: got %q, want %q", latestVersion, tc.latestTag)
			}
			if tc.wantUpgrade && url == "" {
				t.Errorf("expected non-empty download URL when upgrade is needed")
			}
		})
	}
}

func Test_needsUpgrade_InvalidCurrentVersion(t *testing.T) {
	client := &http.Client{
		Transport: &mockTransportWithBody{
			statusCode: http.StatusOK,
			body:       githubReleaseJSON("v2.0.0"),
		},
	}

	_, _, _, err := needsUpgrade(context.Background(), client, "not-a-version")
	if err == nil {
		t.Fatal("expected error for invalid current version, got nil")
	}
}

func Test_needsUpgrade_DownloadURL(t *testing.T) {
	tag := "v2.5.0"
	client := &http.Client{
		Transport: &mockTransportWithBody{
			statusCode: http.StatusOK,
			body:       githubReleaseJSON(tag),
		},
	}

	url, _, upgrade, err := needsUpgrade(context.Background(), client, "v1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !upgrade {
		t.Fatal("expected upgrade=true")
	}
	expectedAsset := fmt.Sprintf("kubevpn_%s_%s_%s.zip", tag, runtime.GOOS, runtime.GOARCH)
	want := fmt.Sprintf("https://example.com/download/%s", expectedAsset)
	if url != want {
		t.Errorf("download URL: got %q, want %q", url, want)
	}
}

func Test_needsUpgrade_NoUpgradeReturnEmptyURL(t *testing.T) {
	tag := "v1.0.0"
	client := &http.Client{
		Transport: &mockTransportWithBody{
			statusCode: http.StatusOK,
			body:       githubReleaseJSON(tag),
		},
	}

	url, latestVersion, upgrade, err := needsUpgrade(context.Background(), client, "v2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upgrade {
		t.Error("expected upgrade=false when current is newer")
	}
	if latestVersion != tag {
		t.Errorf("latestVersion: got %q, want %q", latestVersion, tag)
	}
	// URL should still be populated (it's the download URL from the manifest).
	if url == "" {
		t.Error("expected non-empty URL even when no upgrade needed")
	}
}

func Test_needsUpgrade_MultipleAssets(t *testing.T) {
	// Simulate a release with assets for multiple platforms. Only the one
	// matching runtime.GOOS and runtime.GOARCH should be returned.
	body := fmt.Sprintf(`{
		"tag_name": "v3.0.0",
		"assets": [
			{"name": "kubevpn_v3.0.0_freebsd_386.zip", "browser_download_url": "https://example.com/freebsd_386"},
			{"name": "kubevpn_v3.0.0_freebsd_arm64.zip", "browser_download_url": "https://example.com/freebsd_arm64"},
			{"name": "kubevpn_v3.0.0_%s_%s.zip", "browser_download_url": "https://example.com/correct"}
		]
	}`, runtime.GOOS, runtime.GOARCH)

	client := &http.Client{
		Transport: &mockTransportWithBody{
			statusCode: http.StatusOK,
			body:       body,
		},
	}

	url, _, upgrade, err := needsUpgrade(context.Background(), client, "v1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !upgrade {
		t.Fatal("expected upgrade=true")
	}
	if url != "https://example.com/correct" {
		t.Errorf("expected correct platform URL, got %q", url)
	}
}

func TestElevatePermission_WritableDir(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Skipf("cannot determine executable path: %v", err)
	}

	dir := filepath.Dir(executable)
	testFile := filepath.Join(dir, ".test")

	// Verify the directory is writable before testing.
	f, createErr := os.Create(testFile)
	if f != nil {
		_ = f.Close()
		_ = os.Remove(testFile)
	}
	if createErr != nil {
		t.Skipf("executable directory not writable, skipping: %v", createErr)
	}

	err = elevatePermission()
	if err != nil {
		t.Fatalf("elevatePermission failed in writable dir: %v", err)
	}
}
