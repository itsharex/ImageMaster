package parsers

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ImageMaster/core/request"
	"ImageMaster/core/types"
)

var (
	kemonoPostPathPattern  = regexp.MustCompile(`^/([^/]+)/user/([^/]+)/post/([^/]+)`)
	kemonoUnsafeNameChars  = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	kemonoBrTagPattern     = regexp.MustCompile(`(?i)<br\s*/?>`)
	kemonoParagraphPattern = regexp.MustCompile(`(?i)</p>`)
	kemonoTagPattern       = regexp.MustCompile(`<[^>]+>`)
)

type KemonoFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type KemonoPost struct {
	ID          string       `json:"id"`
	User        string       `json:"user"`
	Service     string       `json:"service"`
	Title       string       `json:"title"`
	Content     string       `json:"content"`
	File        *KemonoFile  `json:"file"`
	Attachments []KemonoFile `json:"attachments"`
}

type kemonoAPIResponse struct {
	Post KemonoPost `json:"post"`
}

type kemonoExportLink struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	URL  string `json:"url"`
}

type kemonoImageEntry struct {
	Type         string
	Name         string
	Path         string
	OriginalURL  string
	ThumbnailURL string
	FileName     string
}

type KemonoParser struct {
	cfg        types.ConfigProvider
	lastPost   *KemonoPost
	lastImages []kemonoImageEntry
}

func (p *KemonoParser) GetName() string {
	return "Kemono"
}

func (p *KemonoParser) Parse(reqClient *request.Client, rawURL string) (*ParseResult, error) {
	service, userID, postID, pageURL, err := parseKemonoPostURL(rawURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://kemono.cr/api/v1/%s/user/%s/post/%s", service, userID, postID)
	headers := map[string]string{
		"Accept":  "text/css",
		"Referer": pageURL,
	}
	if p.cfg != nil {
		if cookie := strings.TrimSpace(p.cfg.GetKemonoCookie()); cookie != "" {
			if !strings.Contains(cookie, "=") {
				cookie = "session=" + cookie
			}
			headers["Cookie"] = cookie
		}
	}

	resp, err := reqClient.DoRequest(http.MethodGet, apiURL, nil, headers)
	if err != nil {
		return nil, fmt.Errorf("request kemono api failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kemono api returned status %d", resp.StatusCode)
	}

	var payload kemonoAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode kemono api response failed: %w", err)
	}

	post := payload.Post
	post.Title = sanitizeKemonoName(post.Title)
	if post.Title == "" {
		post.Title = fmt.Sprintf("%s-%s-%s", service, userID, postID)
	}

	images := collectKemonoImages(post)
	if len(images) == 0 {
		return nil, fmt.Errorf("no images found in kemono post")
	}

	imageURLs := make([]string, 0, len(images))
	filePaths := make([]string, 0, len(images))
	useOriginalImages := p.cfg != nil && p.cfg.GetKemonoUseOriginalImages()
	for _, image := range images {
		if useOriginalImages {
			imageURLs = append(imageURLs, image.OriginalURL)
		} else {
			imageURLs = append(imageURLs, image.ThumbnailURL)
		}
		filePaths = append(filePaths, image.FileName)
	}

	postCopy := post
	p.lastPost = &postCopy
	p.lastImages = images

	return &ParseResult{
		Name:      post.Title,
		ImageURLs: imageURLs,
		FilePaths: filePaths,
	}, nil
}

type KemonoCrawler struct {
	*BaseCrawler
	parser *KemonoParser
}

func NewKemonoCrawler(reqClient *request.Client, cfg types.ConfigProvider) types.ImageCrawler {
	parser := &KemonoParser{}
	parser.cfg = cfg
	return &KemonoCrawler{
		BaseCrawler: NewBaseCrawler(reqClient, parser),
		parser:      parser,
	}
}

func (c *KemonoCrawler) Crawl(rawURL string, savePath string) (string, error) {
	if err := c.CrawlWithParser(rawURL, savePath); err != nil {
		return "", err
	}
	return savePath, nil
}

func (c *KemonoCrawler) CrawlWithParser(rawURL string, savePath string) error {
	if err := SetupRequestClient(c.reqClient, c.downloader); err != nil {
		return fmt.Errorf("setup request client failed: %w", err)
	}

	result, err := c.parser.Parse(c.reqClient, rawURL)
	if err != nil {
		return fmt.Errorf("parse kemono post failed: %w", err)
	}

	UpdateTaskName(c.downloader, result.Name)
	UpdateTaskStatus(c.downloader, types.StatusParsing, "")

	if err := ValidateDownloader(c.downloader, c.parser.GetName()); err != nil {
		return err
	}

	contentPath := filepath.Join(savePath, result.Name)
	if err := os.MkdirAll(contentPath, 0o755); err != nil {
		return fmt.Errorf("create kemono output directory failed: %w", err)
	}

	if err := c.writeKemonoMetadata(contentPath); err != nil {
		return err
	}

	filePaths := make([]string, 0, len(result.FilePaths))
	for _, path := range result.FilePaths {
		filePaths = append(filePaths, filepath.Join(contentPath, filepath.Base(path)))
	}

	return BatchDownloadWithProgress(c.downloader, result.ImageURLs, filePaths)
}

func (c *KemonoCrawler) writeKemonoMetadata(contentPath string) error {
	if c.parser.lastPost == nil {
		return nil
	}

	contentText := kemonoHTMLToText(c.parser.lastPost.Content)
	if err := os.WriteFile(filepath.Join(contentPath, "content.txt"), []byte(contentText+"\n"), 0o644); err != nil {
		return fmt.Errorf("write kemono content failed: %w", err)
	}

	originalLinks := make([]kemonoExportLink, 0, len(c.parser.lastImages))
	thumbnailLinks := make([]kemonoExportLink, 0, len(c.parser.lastImages))
	for _, image := range c.parser.lastImages {
		originalLinks = append(originalLinks, kemonoExportLink{
			Type: image.Type,
			Name: image.Name,
			Path: image.Path,
			URL:  image.OriginalURL,
		})
		thumbnailLinks = append(thumbnailLinks, kemonoExportLink{
			Type: image.Type,
			Name: image.Name,
			Path: image.Path,
			URL:  image.ThumbnailURL,
		})
	}

	if err := writeKemonoJSON(filepath.Join(contentPath, "original-image-links.json"), originalLinks); err != nil {
		return err
	}
	if err := writeKemonoJSON(filepath.Join(contentPath, "thumbnail-image-links.json"), thumbnailLinks); err != nil {
		return err
	}

	return nil
}

func writeKemonoJSON(path string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json failed: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write json file failed: %w", err)
	}
	return nil
}

func parseKemonoPostURL(rawURL string) (service string, userID string, postID string, pageURL string, err error) {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return "", "", "", "", fmt.Errorf("invalid url: %w", err)
	}

	match := kemonoPostPathPattern.FindStringSubmatch(parsed.Path)
	if len(match) < 4 {
		return "", "", "", "", fmt.Errorf("unsupported kemono post url: %s", rawURL)
	}

	service = match[1]
	userID = match[2]
	postID = match[3]
	pageURL = fmt.Sprintf("https://kemono.cr/%s/user/%s/post/%s", service, userID, postID)
	return service, userID, postID, pageURL, nil
}

func collectKemonoImages(post KemonoPost) []kemonoImageEntry {
	images := make([]kemonoImageEntry, 0, len(post.Attachments)+1)
	index := 1

	appendImage := func(imageType string, file *KemonoFile) {
		if file == nil || !isKemonoImage(file.Name) || file.Path == "" {
			return
		}

		ext := strings.ToLower(filepath.Ext(file.Name))
		if ext == "" {
			ext = ".jpg"
		}

		fileName := fmt.Sprintf("%03d%s", index, ext)
		index++
		images = append(images, kemonoImageEntry{
			Type:         imageType,
			Name:         file.Name,
			Path:         file.Path,
			OriginalURL:  buildKemonoOriginalURL(file.Path),
			ThumbnailURL: buildKemonoThumbnailURL(file.Path),
			FileName:     fileName,
		})
	}

	appendImage("main", post.File)
	for i := range post.Attachments {
		file := post.Attachments[i]
		appendImage("attachment", &file)
	}

	return images
}

func isKemonoImage(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".jpg") ||
		strings.HasSuffix(name, ".jpeg") ||
		strings.HasSuffix(name, ".png") ||
		strings.HasSuffix(name, ".webp") ||
		strings.HasSuffix(name, ".gif") ||
		strings.HasSuffix(name, ".bmp") ||
		strings.HasSuffix(name, ".avif")
}

func buildKemonoOriginalURL(path string) string {
	return "https://kemono.cr/data" + path
}

func buildKemonoThumbnailURL(path string) string {
	return "https://img.kemono.cr/thumbnail/data" + path
}

func sanitizeKemonoName(value string) string {
	value = strings.TrimSpace(value)
	value = kemonoUnsafeNameChars.ReplaceAllString(value, "_")
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 120 {
		value = value[:120]
	}
	return value
}

func kemonoHTMLToText(input string) string {
	text := kemonoBrTagPattern.ReplaceAllString(input, "\n")
	text = kemonoParagraphPattern.ReplaceAllString(text, "\n\n")
	text = kemonoTagPattern.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return text
}

func init() {
	Register(SiteTypeKemono, func(reqClient *request.Client, cfg types.ConfigProvider) types.ImageCrawler {
		return NewKemonoCrawler(reqClient, cfg)
	})
	RegisterHostContains(SiteTypeKemono, "kemono.cr")
}
