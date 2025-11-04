package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"gopkg.in/yaml.v3"
)

type config struct {
	contentDir  string
	outputDir   string
	templateDir string
	assetDir    string
	baseURL     string
}

type frontMatter struct {
	Title       string    `yaml:"title"`
	Date        time.Time `yaml:"date"`
	Tags        []string  `yaml:"tags"`
	Summary     string    `yaml:"summary"`
	Description string    `yaml:"description"`
	Draft       bool      `yaml:"draft"`
}

type post struct {
	Slug        string
	Title       string
	Date        time.Time
	Tags        []string
	Summary     string
	Description string
	Draft       bool
	ContentHTML template.HTML
	ContentRaw  []byte
	SourcePath  string
}

type templateBundle struct {
	layout *template.Template
	index  *template.Template
	post   *template.Template
	tags   *template.Template
	tag    *template.Template
}

type tagGroup struct {
	Name  string
	Slug  string
	Posts []post
}

type rssFeed struct {
	XMLName   xml.Name   `xml:"rss"`
	Version   string     `xml:"version,attr"`
	XMLNSAtom string     `xml:"xmlns:atom,attr"`
	Channel   rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language,omitempty"`
	LastBuildDate string    `xml:"lastBuildDate,omitempty"`
	AtomLink      atomLink  `xml:"atom:link"`
	Items         []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
	Description string  `xml:"description"`
}

type rssGUID struct {
	IsPermaLink string `xml:"isPermaLink,attr,omitempty"`
	Value       string `xml:",chardata"`
}

const githubRepo = "yoonhyunwoo/blog"

func main() {
	cfg := config{}
	flag.StringVar(&cfg.contentDir, "content", "content", "Markdown content directory")
	flag.StringVar(&cfg.templateDir, "templates", "templates", "HTML template directory")
	flag.StringVar(&cfg.assetDir, "assets", "assets", "Static asset directory")
	flag.StringVar(&cfg.outputDir, "out", "public", "Build output directory")
	flag.StringVar(&cfg.baseURL, "baseURL", "https://example.com", "Base URL used for absolute links in RSS (e.g. https://thumbgo.dev)")
	flag.Parse()

	cfg.baseURL = strings.TrimRight(cfg.baseURL, "/")
	if cfg.baseURL == "" {
		cfg.baseURL = "https://example.com"
	}

	if err := run(context.Background(), cfg); err != nil {
		log.Fatalf("generate: %v", err)
	}
}

func run(ctx context.Context, cfg config) error {
	if err := ensureDir(cfg.outputDir); err != nil {
		return err
	}

	tpls, err := loadTemplates(cfg.templateDir)
	if err != nil {
		return err
	}

	posts, err := loadPosts(ctx, cfg, tpls.post)
	if err != nil {
		return err
	}
	if len(posts) == 0 {
		log.Println("게시물을 찾지 못했습니다. 새로운 글을 추가해 보세요.")
		return nil
	}
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date.After(posts[j].Date)
	})

	if err := renderIndex(cfg.outputDir, tpls.index, posts); err != nil {
		return err
	}
	tagGroups := buildTagGroups(posts)
	if err := renderTagIndex(cfg.outputDir, tpls.tags, tagGroups); err != nil {
		return err
	}
	if err := renderTagPages(cfg.outputDir, tpls.tag, tagGroups); err != nil {
		return err
	}
	if err := renderRSS(cfg, posts); err != nil {
		return err
	}
	if err := copyAssets(cfg.assetDir, filepath.Join(cfg.outputDir, "assets")); err != nil {
		return err
	}
	return nil
}

func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func loadTemplates(dir string) (*templateBundle, error) {
	layoutPath := filepath.Join(dir, "base.html")
	indexPath := filepath.Join(dir, "index.html")
	postPath := filepath.Join(dir, "post.html")
	tagsIndexPath := filepath.Join(dir, "tags.html")
	tagPath := filepath.Join(dir, "tag.html")

	layout, err := template.New("base").
		Funcs(template.FuncMap{
			"formatDate": formatDate,
			"timeNow":    time.Now,
			"tagURL":     tagURL,
		}).
		ParseFiles(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("parse base template: %w", err)
	}

	index, err := template.Must(layout.Clone()).ParseFiles(indexPath)
	if err != nil {
		return nil, fmt.Errorf("parse index template: %w", err)
	}

	post, err := template.Must(layout.Clone()).ParseFiles(postPath)
	if err != nil {
		return nil, fmt.Errorf("parse post template: %w", err)
	}

	tagsIndex, err := template.Must(layout.Clone()).ParseFiles(tagsIndexPath)
	if err != nil {
		return nil, fmt.Errorf("parse tags template: %w", err)
	}

	tag, err := template.Must(layout.Clone()).ParseFiles(tagPath)
	if err != nil {
		return nil, fmt.Errorf("parse tag template: %w", err)
	}

	return &templateBundle{
		layout: layout,
		index:  index,
		post:   post,
		tags:   tagsIndex,
		tag:    tag,
	}, nil
}

func loadPosts(ctx context.Context, cfg config, postTpl *template.Template) ([]post, error) {
	var posts []post
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)

	err := filepath.WalkDir(cfg.contentDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		fm, body, err := splitFrontMatter(src)
		if err != nil {
			return fmt.Errorf("front matter %s: %w", path, err)
		}
		if fm.Draft {
			return nil
		}

		slug := buildSlug(cfg.contentDir, path)

		htmlContent, err := renderMarkdown(md, body)
		if err != nil {
			return fmt.Errorf("markdown %s: %w", path, err)
		}

		post := post{
			Slug:        slug,
			Title:       pickTitle(fm, slug),
			Date:        fm.Date,
			Tags:        fm.Tags,
			Summary:     fm.Summary,
			Description: fm.Description,
			Draft:       fm.Draft,
			ContentHTML: template.HTML(htmlContent.String()),
			ContentRaw:  body,
			SourcePath:  path,
		}

		if err := writePost(cfg, postTpl, post); err != nil {
			return err
		}
		posts = append(posts, post)
		return nil
	})

	return posts, err
}

func renderIndex(outDir string, tpl *template.Template, posts []post) error {
	target := filepath.Join(outDir, "index.html")
	if err := ensureDir(filepath.Dir(target)); err != nil {
		return err
	}
	fh, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	defer fh.Close()
	data := map[string]any{
		"Title": "썸고 블로그",
		"Posts": posts,
	}
	if err := tpl.ExecuteTemplate(fh, "base", data); err != nil {
		return fmt.Errorf("render index: %w", err)
	}
	return nil
}

func renderTagIndex(outDir string, tpl *template.Template, tags []tagGroup) error {
	dir := filepath.Join(outDir, "tags")
	if err := ensureDir(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, "index.html")
	fh, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create tag index: %w", err)
	}
	defer fh.Close()
	data := map[string]any{
		"Title": "태그 모음",
		"Tags":  tags,
	}
	if err := tpl.ExecuteTemplate(fh, "base", data); err != nil {
		return fmt.Errorf("render tag index: %w", err)
	}
	return nil
}

func renderTagPages(outDir string, tpl *template.Template, tags []tagGroup) error {
	if len(tags) == 0 {
		return nil
	}
	dir := filepath.Join(outDir, "tags")
	for _, tag := range tags {
		tagDir := filepath.Join(dir, tag.Slug)
		if err := ensureDir(tagDir); err != nil {
			return err
		}
		target := filepath.Join(tagDir, "index.html")
		fh, err := os.Create(target)
		if err != nil {
			return fmt.Errorf("create tag page: %w", err)
		}
		data := map[string]any{
			"Title": fmt.Sprintf("태그: %s", tag.Name),
			"Tag":   tag,
			"Posts": tag.Posts,
		}
		if execErr := tpl.ExecuteTemplate(fh, "base", data); execErr != nil {
			fh.Close()
			return fmt.Errorf("render tag %s: %w", tag.Name, execErr)
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writePost(cfg config, tpl *template.Template, post post) error {
	targetDir := filepath.Join(cfg.outputDir, post.Slug)
	if err := ensureDir(targetDir); err != nil {
		return err
	}
	target := filepath.Join(targetDir, "index.html")
	fh, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create %s: %w", target, err)
	}
	defer fh.Close()

	data := map[string]any{
		"Title":       post.Title,
		"Post":        post,
		"Description": firstNonEmpty(post.Description, post.Summary),
		"BaseURL":     cfg.baseURL,
		"GithubRepo":  githubRepo,
	}
	if err := tpl.ExecuteTemplate(fh, "base", data); err != nil {
		return fmt.Errorf("render post: %w", err)
	}
	return nil
}

func buildTagGroups(posts []post) []tagGroup {
	groupMap := make(map[string]*tagGroup)
	seen := make(map[string]struct{})
	for _, p := range posts {
		for _, raw := range p.Tags {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			slug := tagSlug(name)
			key := slug + "@" + p.Slug
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			group, ok := groupMap[slug]
			if !ok {
				group = &tagGroup{Name: name, Slug: slug}
				groupMap[slug] = group
			}
			group.Posts = append(group.Posts, p)
		}
	}

	if len(groupMap) == 0 {
		return nil
	}

	result := make([]tagGroup, 0, len(groupMap))
	for _, g := range groupMap {
		sort.Slice(g.Posts, func(i, j int) bool {
			return g.Posts[i].Date.After(g.Posts[j].Date)
		})
		result = append(result, *g)
	}

	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

func plainExcerpt(src []byte, limit int) string {
	text := strings.TrimSpace(string(src))
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return text
}

func renderRSS(cfg config, posts []post) error {
	if len(posts) == 0 {
		return nil
	}

	rssDir := filepath.Join(cfg.outputDir, "feeds")
	if err := ensureDir(rssDir); err != nil {
		return err
	}
	target := filepath.Join(rssDir, "rss.xml")
	fh, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("create rss feed: %w", err)
	}
	defer fh.Close()

	base := cfg.baseURL
	if base == "" {
		base = "https://example.com"
	}

	channel := rssChannel{
		Title:         "썸고 블로그",
		Link:          base,
		Description:   "DevOps 엔지니어 썸고(thumbgo)의 블로그",
		Language:      "ko",
		LastBuildDate: formatRFC1123(posts[0].Date),
		AtomLink: atomLink{
			Href: base + "/feeds/rss.xml",
			Rel:  "self",
			Type: "application/rss+xml",
		},
	}

	const maxItems = 50
	for i, p := range posts {
		if i >= maxItems {
			break
		}
		link := base + "/" + p.Slug + "/"
		description := firstNonEmpty(
			p.Summary,
			p.Description,
			plainExcerpt(p.ContentRaw, 200),
		)
		channel.Items = append(channel.Items, rssItem{
			Title:       p.Title,
			Link:        link,
			GUID:        rssGUID{IsPermaLink: "true", Value: link},
			PubDate:     formatRFC1123(p.Date),
			Description: description,
		})
	}

	feed := rssFeed{
		Version:   "2.0",
		XMLNSAtom: "http://www.w3.org/2005/Atom",
		Channel:   channel,
	}

	if _, err := fh.WriteString(xml.Header); err != nil {
		return fmt.Errorf("write xml header: %w", err)
	}

	enc := xml.NewEncoder(fh)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		return fmt.Errorf("encode rss feed: %w", err)
	}
	if err := enc.Flush(); err != nil {
		return fmt.Errorf("flush rss feed: %w", err)
	}
	return nil
}

func renderMarkdown(md goldmark.Markdown, src []byte) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return nil, err
	}
	return &buf, nil
}

func splitFrontMatter(data []byte) (frontMatter, []byte, error) {
	var fm frontMatter
	var start int
	switch {
	case bytes.HasPrefix(data, []byte("---\r\n")):
		start = len("---\r\n")
	case bytes.HasPrefix(data, []byte("---\n")):
		start = len("---\n")
	default:
		return fm, data, nil
	}

	remaining := data[start:]
	end := bytes.Index(remaining, []byte("\n---"))
	sepLen := len("\n---")
	if end == -1 {
		end = bytes.Index(remaining, []byte("\r\n---"))
		sepLen = len("\r\n---")
	}
	if end == -1 {
		return fm, nil, fmt.Errorf("unterminated front matter")
	}

	meta := remaining[:end]
	body := remaining[end+sepLen:]
	body = bytes.TrimLeft(body, "\r\n")

	if err := yaml.Unmarshal(meta, &fm); err != nil {
		return fm, nil, err
	}
	if fm.Date.IsZero() {
		return fm, nil, fmt.Errorf("date is required in front matter")
	}

	return fm, body, nil
}

func buildSlug(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	rel = strings.ToLower(rel)
	return strings.ReplaceAll(rel, string(filepath.Separator), "/")
}

func tagSlug(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "tag"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || unicode.IsSpace(r):
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "tag"
	}
	return slug
}

func tagURL(name string) string {
	return "/tags/" + tagSlug(name) + "/"
}

func copyAssets(srcDir, dstDir string) error {
	if _, err := os.Stat(srcDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return ensureDir(target)
		}
		if err := ensureDir(filepath.Dir(target)); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open asset %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create asset %s: %w", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy asset %s: %w", dst, err)
	}
	return out.Close()
}

func pickTitle(fm frontMatter, slug string) string {
	if fm.Title != "" {
		return fm.Title
	}
	return strings.Title(strings.ReplaceAll(filepath.Base(slug), "-", " "))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func formatDate(t time.Time) string {
	return t.Format("2006-01-02")
}

func formatRFC1123(t time.Time) string {
	return t.UTC().Format(time.RFC1123Z)
}
