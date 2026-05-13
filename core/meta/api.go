package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ImageMaster/core/jmbridge"
)

const (
	latestReleaseAPI = "https://api.github.com/repos/TyrEamon/ImageMaster/releases/latest"
	releasesPageURL  = "https://github.com/TyrEamon/ImageMaster/releases"
)

type ConfigProvider interface {
	GetProxy() string
}

type VersionInfo struct {
	Version    string `json:"version"`
	Display    string `json:"display"`
	Commit     string `json:"commit"`
	BuildTime  string `json:"buildTime"`
	IsDevBuild bool   `json:"isDevBuild"`
}

type JmRuntimeInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Engine       string `json:"engine"`
	Upstream     string `json:"upstream"`
	BuildTime    string `json:"buildTime"`
	ManifestPath string `json:"manifestPath"`
	HelperPath   string `json:"helperPath"`
	Available    bool   `json:"available"`
	Source       string `json:"source"`
}

type UpdateCheckResult struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	LatestTag      string `json:"latestTag"`
	HasUpdate      bool   `json:"hasUpdate"`
	ReleaseURL     string `json:"releaseUrl"`
	AssetName      string `json:"assetName"`
	AssetURL       string `json:"assetUrl"`
	PublishedAt    string `json:"publishedAt"`
}

type API struct {
	version   string
	commit    string
	buildTime string
	cfg       ConfigProvider
}

func NewAPI(version string, commit string, buildTime string, cfg ConfigProvider) *API {
	return &API{
		version:   strings.TrimSpace(version),
		commit:    strings.TrimSpace(commit),
		buildTime: strings.TrimSpace(buildTime),
		cfg:       cfg,
	}
}

func (a *API) GetVersionInfo() VersionInfo {
	version := a.version
	if version == "" {
		version = "0.0.0-dev"
	}

	display := version
	if !strings.HasPrefix(strings.ToLower(display), "v") {
		display = "v" + display
	}

	commit := a.commit
	if commit == "" {
		commit = "local"
	}

	return VersionInfo{
		Version:    version,
		Display:    display,
		Commit:     commit,
		BuildTime:  a.buildTime,
		IsDevBuild: strings.Contains(strings.ToLower(version), "dev"),
	}
}

func (a *API) GetJmRuntimeInfo() JmRuntimeInfo {
	info := jmbridge.GetRuntimeInfo()

	version := strings.TrimSpace(info.Version)
	if version == "" {
		version = "unversioned"
	}

	return JmRuntimeInfo{
		Name:         info.Name,
		Version:      version,
		Engine:       info.Engine,
		Upstream:     info.Upstream,
		BuildTime:    info.BuildTime,
		ManifestPath: info.ManifestPath,
		HelperPath:   info.HelperPath,
		Available:    info.Available,
		Source:       info.Source,
	}
}

func (a *API) CheckForUpdates() (UpdateCheckResult, error) {
	current := a.GetVersionInfo()
	release, err := a.fetchLatestRelease()
	if err != nil {
		return UpdateCheckResult{}, err
	}

	latestVersion := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	if latestVersion == "" {
		latestVersion = strings.TrimSpace(release.Name)
	}

	assetName, assetURL := pickWindowsReleaseAsset(release.Assets)
	releaseURL := strings.TrimSpace(release.HTMLURL)
	if releaseURL == "" {
		releaseURL = releasesPageURL
	}

	return UpdateCheckResult{
		CurrentVersion: current.Display,
		LatestVersion:  latestVersion,
		LatestTag:      release.TagName,
		HasUpdate:      isNewerVersion(current.Version, latestVersion),
		ReleaseURL:     releaseURL,
		AssetName:      assetName,
		AssetURL:       assetURL,
		PublishedAt:    release.PublishedAt,
	}, nil
}

func (a *API) fetchLatestRelease() (githubRelease, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseAPI, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ImageMaster/"+a.GetVersionInfo().Version)

	client := &http.Client{Timeout: 20 * time.Second}
	if a.cfg != nil {
		if proxy := strings.TrimSpace(a.cfg.GetProxy()); proxy != "" {
			proxyURL, err := url.Parse(proxy)
			if err != nil {
				return githubRelease{}, fmt.Errorf("invalid proxy url: %w", err)
			}
			client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("github release api returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return githubRelease{}, err
	}
	if strings.TrimSpace(release.TagName) == "" && strings.TrimSpace(release.Name) == "" {
		return githubRelease{}, fmt.Errorf("github release api returned empty release")
	}
	return release, nil
}

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	Name        string               `json:"name"`
	HTMLURL     string               `json:"html_url"`
	PublishedAt string               `json:"published_at"`
	Assets      []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func pickWindowsReleaseAsset(assets []githubReleaseAsset) (string, string) {
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, "windows") && strings.Contains(name, "amd64") && strings.HasSuffix(name, ".zip") {
			return asset.Name, asset.BrowserDownloadURL
		}
	}
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if strings.Contains(name, "windows") && strings.HasSuffix(name, ".zip") {
			return asset.Name, asset.BrowserDownloadURL
		}
	}
	return "", ""
}

func isNewerVersion(current string, latest string) bool {
	currentParts, currentOK := versionParts(current)
	latestParts, latestOK := versionParts(latest)
	if !currentOK || !latestOK {
		return normalizeVersion(latest) != "" && normalizeVersion(latest) != normalizeVersion(current)
	}

	for i := 0; i < len(currentParts) || i < len(latestParts); i++ {
		currentPart := 0
		latestPart := 0
		if i < len(currentParts) {
			currentPart = currentParts[i]
		}
		if i < len(latestParts) {
			latestPart = latestParts[i]
		}
		if latestPart > currentPart {
			return true
		}
		if latestPart < currentPart {
			return false
		}
	}

	return strings.Contains(strings.ToLower(current), "dev") && !strings.Contains(strings.ToLower(latest), "dev")
}

func versionParts(value string) ([]int, bool) {
	normalized := normalizeVersion(value)
	if normalized == "" {
		return nil, false
	}

	rawParts := strings.Split(normalized, ".")
	parts := make([]int, 0, len(rawParts))
	for _, raw := range rawParts {
		if raw == "" {
			return nil, false
		}
		value := 0
		for _, ch := range raw {
			if ch < '0' || ch > '9' {
				return nil, false
			}
			value = value*10 + int(ch-'0')
		}
		parts = append(parts, value)
	}
	return parts, len(parts) > 0
}

func normalizeVersion(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "v")
	end := len(value)
	for i, ch := range value {
		if (ch < '0' || ch > '9') && ch != '.' {
			end = i
			break
		}
	}
	return strings.Trim(value[:end], ".")
}
