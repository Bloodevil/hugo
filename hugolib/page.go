// Copyright 2016 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/hugo/helpers"
	"github.com/spf13/hugo/parser"

	"html/template"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spf13/cast"
	bp "github.com/spf13/hugo/bufferpool"
	"github.com/spf13/hugo/hugofs"
	"github.com/spf13/hugo/source"
	"github.com/spf13/hugo/tpl"
	jww "github.com/spf13/jwalterweatherman"
	"github.com/spf13/viper"
)

var (
	cjk = regexp.MustCompile(`\p{Han}|\p{Hangul}|\p{Hiragana}|\p{Katakana}`)
)

type Page struct {
	Params            map[string]interface{}
	Content           template.HTML
	Summary           template.HTML
	Aliases           []string
	Status            string
	Images            []Image
	Videos            []Video
	TableOfContents   template.HTML
	Truncated         bool
	Draft             bool
	PublishDate       time.Time
	ExpiryDate        time.Time
	Markup            string
	translations      Pages
	extension         string
	contentType       string
	renderable        bool
	Layout            string
	layoutsCalculated []string
	linkTitle         string
	frontmatter       []byte

	// rawContent isn't "raw" as in the same as in the content file.
	// Hugo cares about memory consumption, so we make changes to it to do
	// markdown rendering etc., but it is "raw enough" so we can do rebuilds
	// when shortcode changes etc.
	rawContent []byte

	// When running Hugo in watch mode, we do partial rebuilds and have to make
	// a copy of the rawContent to be prepared for rebuilds when shortcodes etc.
	// have changed.
	rawContentCopy []byte

	// state telling if this is a "new page" or if we have rendered it previously.
	rendered bool

	contentShortCodes   map[string]func() (string, error)
	shortcodes          map[string]shortcode
	plain               string // TODO should be []byte
	plainWords          []string
	plainInit           sync.Once
	plainWordsInit      sync.Once
	renderingConfig     *helpers.Blackfriday
	renderingConfigInit sync.Once
	pageMenus           PageMenus
	pageMenusInit       sync.Once
	isCJKLanguage       bool
	PageMeta
	Source
	Position `json:"-"`
	Node
}

type Source struct {
	Frontmatter []byte
	Content     []byte
	source.File
}
type PageMeta struct {
	wordCount      int
	fuzzyWordCount int
	readingTime    int
	pageMetaInit   sync.Once
	Weight         int
}

func (*PageMeta) WordCount() int {
	helpers.Deprecated("PageMeta", "WordCount", ".WordCount (on Page)")
	return 0
}

func (*PageMeta) FuzzyWordCount() int {
	helpers.Deprecated("PageMeta", "FuzzyWordCount", ".FuzzyWordCount (on Page)")
	return 0

}

func (*PageMeta) ReadingTime() int {
	helpers.Deprecated("PageMeta", "ReadingTime", ".ReadingTime (on Page)")
	return 0
}

type Position struct {
	Prev          *Page
	Next          *Page
	PrevInSection *Page
	NextInSection *Page
}

type Pages []*Page

func (ps Pages) FindPagePosByFilePath(inPath string) int {
	for i, x := range ps {
		if x.Source.Path() == inPath {
			return i
		}
	}
	return -1
}

// FindPagePos Given a page, it will find the position in Pages
// will return -1 if not found
func (ps Pages) FindPagePos(page *Page) int {
	for i, x := range ps {
		if x.Source.Path() == page.Source.Path() {
			return i
		}
	}
	return -1
}

func (p *Page) Plain() string {
	p.initPlain()
	return p.plain
}

func (p *Page) PlainWords() []string {
	p.initPlainWords()
	return p.plainWords
}

func (p *Page) initPlain() {
	p.plainInit.Do(func() {
		p.plain = helpers.StripHTML(string(p.Content))
		return
	})
}

func (p *Page) initPlainWords() {
	p.plainWordsInit.Do(func() {
		p.plainWords = strings.Fields(p.Plain())
		return
	})
}

func (p *Page) IsNode() bool {
	return false
}

func (p *Page) IsPage() bool {
	return true
}

// Param is a convenience method to do lookups in Page's and Site's Params map,
// in that order.
//
// This method is also implemented on Node and SiteInfo.
func (p *Page) Param(key interface{}) (interface{}, error) {
	keyStr, err := cast.ToStringE(key)
	if err != nil {
		return nil, err
	}
	keyStr = strings.ToLower(keyStr)
	if val, ok := p.Params[keyStr]; ok {
		return val, nil
	}
	return p.Site.Params[keyStr], nil
}

func (p *Page) Author() Author {
	authors := p.Authors()

	for _, author := range authors {
		return author
	}
	return Author{}
}

func (p *Page) Authors() AuthorList {
	authorKeys, ok := p.Params["authors"]
	if !ok {
		return AuthorList{}
	}
	authors := authorKeys.([]string)
	if len(authors) < 1 || len(p.Site.Authors) < 1 {
		return AuthorList{}
	}

	al := make(AuthorList)
	for _, author := range authors {
		a, ok := p.Site.Authors[author]
		if ok {
			al[author] = a
		}
	}
	return al
}

func (p *Page) UniqueID() string {
	return p.Source.UniqueID()
}

func (p *Page) Ref(ref string) (string, error) {
	return p.Node.Site.Ref(ref, p)
}

func (p *Page) RelRef(ref string) (string, error) {
	return p.Node.Site.RelRef(ref, p)
}

// for logging
func (p *Page) lineNumRawContentStart() int {
	return bytes.Count(p.frontmatter, []byte("\n")) + 1
}

var (
	internalSummaryDivider = []byte("HUGOMORE42")
)

// Returns the page as summary and main if a user defined split is provided.
func (p *Page) setUserDefinedSummaryIfProvided() (*summaryContent, error) {

	sc, err := splitUserDefinedSummaryAndContent(p.Markup, p.rawContentCopy)

	if err != nil {
		return nil, err
	}

	if sc == nil {
		// No divider found
		return nil, nil
	}

	p.Truncated = true
	if len(sc.content) < 20 {
		// only whitespace?
		p.Truncated = len(bytes.Trim(sc.content, " \n\r")) > 0
	}

	p.Summary = helpers.BytesToHTML(sc.summary)

	return sc, nil
}

// Make this explicit so there is no doubt about what is what.
type summaryContent struct {
	summary               []byte
	content               []byte
	contentWithoutSummary []byte
}

func splitUserDefinedSummaryAndContent(markup string, c []byte) (sc *summaryContent, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("summary split failed: %s", r)
		}
	}()

	c = bytes.TrimSpace(c)
	startDivider := bytes.Index(c, internalSummaryDivider)

	if startDivider == -1 {
		return
	}

	endDivider := startDivider + len(internalSummaryDivider)
	endSummary := startDivider

	var (
		startMarkup []byte
		endMarkup   []byte
		addDiv      bool
		divStart    = []byte("<div class=\"document\">")
	)

	switch markup {
	default:
		startMarkup = []byte("<p>")
		endMarkup = []byte("</p>")
	case "asciidoc":
		startMarkup = []byte("<div class=\"paragraph\">")
		endMarkup = []byte("</div>")
	case "rst":
		startMarkup = []byte("<p>")
		endMarkup = []byte("</p>")
		addDiv = true
	}

	// Find the closest end/start markup string to the divider
	fromStart := -1
	fromIdx := bytes.LastIndex(c[:startDivider], startMarkup)
	if fromIdx != -1 {
		fromStart = startDivider - fromIdx - len(startMarkup)
	}
	fromEnd := bytes.Index(c[endDivider:], endMarkup)

	if fromEnd != -1 && fromEnd <= fromStart {
		endSummary = startDivider + fromEnd + len(endMarkup)
	} else if fromStart != -1 && fromEnd != -1 {
		endSummary = startDivider - fromStart - len(startMarkup)
	}

	withoutDivider := bytes.TrimSpace(append(c[:startDivider], c[endDivider:]...))
	var (
		contentWithoutSummary []byte
		summary               []byte
	)

	if len(withoutDivider) > 0 {
		contentWithoutSummary = bytes.TrimSpace(withoutDivider[endSummary:])
		summary = bytes.TrimSpace(withoutDivider[:endSummary])
	}

	if addDiv {
		// For the rst
		summary = append(append([]byte(nil), summary...), []byte("</div>")...)
		// TODO(bep) include the document class, maybe
		contentWithoutSummary = append(divStart, contentWithoutSummary...)
	}

	if err != nil {
		return
	}

	sc = &summaryContent{
		summary:               summary,
		content:               withoutDivider,
		contentWithoutSummary: contentWithoutSummary,
	}

	return
}

func (p *Page) setAutoSummary() error {
	var summary string
	var truncated bool
	if p.isCJKLanguage {
		summary, truncated = helpers.TruncateWordsByRune(p.PlainWords(), helpers.SummaryLength)
	} else {
		summary, truncated = helpers.TruncateWordsToWholeSentence(p.Plain(), helpers.SummaryLength)
	}
	p.Summary = template.HTML(summary)
	p.Truncated = truncated

	return nil
}

func (p *Page) renderContent(content []byte) []byte {
	var fn helpers.LinkResolverFunc
	var fileFn helpers.FileResolverFunc
	if p.getRenderingConfig().SourceRelativeLinksEval {
		fn = func(ref string) (string, error) {
			return p.Node.Site.SourceRelativeLink(ref, p)
		}
		fileFn = func(ref string) (string, error) {
			return p.Node.Site.SourceRelativeLinkFile(ref, p)
		}
	}
	return helpers.RenderBytes(&helpers.RenderingContext{
		Content: content, RenderTOC: true, PageFmt: p.determineMarkupType(),
		ConfigProvider: p.Language(),
		DocumentID:     p.UniqueID(), DocumentName: p.Path(),
		Config: p.getRenderingConfig(), LinkResolver: fn, FileResolver: fileFn})
}

func (p *Page) getRenderingConfig() *helpers.Blackfriday {

	p.renderingConfigInit.Do(func() {
		pageParam := cast.ToStringMap(p.GetParam("blackfriday"))
		if p.Language() == nil {
			panic(fmt.Sprintf("nil language for %s with source lang %s", p.BaseFileName(), p.lang))
		}
		p.renderingConfig = helpers.NewBlackfriday(p.Language())

		if err := mapstructure.Decode(pageParam, p.renderingConfig); err != nil {
			jww.FATAL.Printf("Failed to get rendering config for %s:\n%s", p.BaseFileName(), err.Error())
		}

	})

	return p.renderingConfig
}

func newPage(filename string) *Page {
	page := Page{contentType: "",
		Source:       Source{File: *source.NewFile(filename)},
		Node:         Node{Keywords: []string{}, Sitemap: Sitemap{Priority: -1}},
		Params:       make(map[string]interface{}),
		translations: make(Pages, 0),
	}

	jww.DEBUG.Println("Reading from", page.File.Path())
	return &page
}

func (p *Page) IsRenderable() bool {
	return p.renderable
}

func (p *Page) Type() string {
	if p.contentType != "" {
		return p.contentType
	}

	if x := p.Section(); x != "" {
		return x
	}

	return "page"
}

func (p *Page) Section() string {
	return p.Source.Section()
}

func (p *Page) layouts(l ...string) []string {
	if len(p.layoutsCalculated) > 0 {
		return p.layoutsCalculated
	}

	if p.Layout != "" {
		return layouts(p.Type(), p.Layout)
	}

	layout := ""
	if len(l) == 0 {
		layout = "single"
	} else {
		layout = l[0]
	}

	return layouts(p.Type(), layout)
}

func layouts(types string, layout string) (layouts []string) {
	t := strings.Split(types, "/")

	// Add type/layout.html
	for i := range t {
		search := t[:len(t)-i]
		layouts = append(layouts, fmt.Sprintf("%s/%s.html", strings.ToLower(path.Join(search...)), layout))
	}

	// Add _default/layout.html
	layouts = append(layouts, fmt.Sprintf("_default/%s.html", layout))

	// Add theme/type/layout.html & theme/_default/layout.html
	for _, l := range layouts {
		layouts = append(layouts, "theme/"+l)
	}

	return
}

func NewPageFrom(buf io.Reader, name string) (*Page, error) {
	p, err := NewPage(name)
	if err != nil {
		return p, err
	}
	_, err = p.ReadFrom(buf)

	return p, err
}

func NewPage(name string) (*Page, error) {
	if len(name) == 0 {
		return nil, errors.New("Zero length page name")
	}

	// Create new page
	p := newPage(name)

	return p, nil
}

func (p *Page) ReadFrom(buf io.Reader) (int64, error) {
	// Parse for metadata & body
	if err := p.parse(buf); err != nil {
		jww.ERROR.Print(err)
		return 0, err
	}

	return int64(len(p.rawContent)), nil
}

func (p *Page) WordCount() int {
	p.analyzePage()
	return p.wordCount
}

func (p *Page) ReadingTime() int {
	p.analyzePage()
	return p.readingTime
}

func (p *Page) FuzzyWordCount() int {
	p.analyzePage()
	return p.fuzzyWordCount
}

func (p *Page) analyzePage() {
	p.pageMetaInit.Do(func() {
		if p.isCJKLanguage {
			p.wordCount = 0
			for _, word := range p.PlainWords() {
				runeCount := utf8.RuneCountInString(word)
				if len(word) == runeCount {
					p.wordCount++
				} else {
					p.wordCount += runeCount
				}
			}
		} else {
			p.wordCount = helpers.TotalWords(p.Plain())
		}

		// TODO(bep) is set in a test. Fix that.
		if p.fuzzyWordCount == 0 {
			p.fuzzyWordCount = (p.wordCount + 100) / 100 * 100
		}

		if p.isCJKLanguage {
			p.readingTime = (p.wordCount + 500) / 501
		} else {
			p.readingTime = (p.wordCount + 212) / 213
		}
	})
}

func (p *Page) permalink() (*url.URL, error) {
	baseURL := string(p.Site.BaseURL)
	dir := strings.TrimSpace(helpers.MakePath(filepath.ToSlash(strings.ToLower(p.Source.Dir()))))
	pSlug := strings.TrimSpace(helpers.URLize(p.Slug))
	pURL := strings.TrimSpace(helpers.URLize(p.URLPath.URL))
	var permalink string
	var err error

	if len(pURL) > 0 {
		return helpers.MakePermalink(baseURL, pURL), nil
	}

	if override, ok := p.Site.Permalinks[p.Section()]; ok {
		permalink, err = override.Expand(p)

		if err != nil {
			return nil, err
		}
		// fmt.Printf("have a section override for %q in section %s → %s\n", p.Title, p.Section, permalink)
	} else {
		if len(pSlug) > 0 {
			permalink = helpers.URLPrep(viper.GetBool("UglyURLs"), path.Join(dir, p.Slug+"."+p.Extension()))
		} else {
			t := p.Source.TranslationBaseName()
			permalink = helpers.URLPrep(viper.GetBool("UglyURLs"), path.Join(dir, helpers.ReplaceExtension(strings.TrimSpace(t), p.Extension())))
		}
	}

	permalink = p.addLangPathPrefix(permalink)

	return helpers.MakePermalink(baseURL, permalink), nil
}

func (p *Page) Extension() string {
	if p.extension != "" {
		return p.extension
	}
	return viper.GetString("DefaultExtension")
}

// AllTranslations returns all translations, including the current Page.
func (p *Page) AllTranslations() Pages {
	return p.translations
}

// IsTranslated returns whether this content file is translated to
// other language(s).
func (p *Page) IsTranslated() bool {
	return len(p.translations) > 1
}

// Translations returns the translations excluding the current Page.
func (p *Page) Translations() Pages {
	translations := make(Pages, 0)
	for _, t := range p.translations {
		if t != p {
			translations = append(translations, t)
		}
	}
	return translations
}

func (p *Page) LinkTitle() string {
	if len(p.linkTitle) > 0 {
		return p.linkTitle
	}
	return p.Title
}

func (p *Page) shouldBuild() bool {
	return shouldBuild(viper.GetBool("BuildFuture"), viper.GetBool("BuildExpired"),
		viper.GetBool("BuildDrafts"), p.Draft, p.PublishDate, p.ExpiryDate)
}

func shouldBuild(buildFuture bool, buildExpired bool, buildDrafts bool, Draft bool,
	publishDate time.Time, expiryDate time.Time) bool {
	if !(buildDrafts || !Draft) {
		return false
	}
	if !buildFuture && !publishDate.IsZero() && publishDate.After(time.Now()) {
		return false
	}
	if !buildExpired && !expiryDate.IsZero() && expiryDate.Before(time.Now()) {
		return false
	}
	return true
}

func (p *Page) IsDraft() bool {
	return p.Draft
}

func (p *Page) IsFuture() bool {
	if p.PublishDate.IsZero() {
		return false
	}
	return p.PublishDate.After(time.Now())
}

func (p *Page) IsExpired() bool {
	if p.ExpiryDate.IsZero() {
		return false
	}
	return p.ExpiryDate.Before(time.Now())
}

func (p *Page) Permalink() (string, error) {
	link, err := p.permalink()
	if err != nil {
		return "", err
	}
	return link.String(), nil
}

func (p *Page) URL() string {
	if p.URLPath.URL != "" {
		// This is the url set in front matter
		return p.URLPath.URL
	}
	// Fall back to the relative permalink.
	u, _ := p.RelPermalink()
	return u
}

func (p *Page) RelPermalink() (string, error) {
	link, err := p.permalink()
	if err != nil {
		return "", err
	}

	if viper.GetBool("CanonifyURLs") {
		// replacements for relpermalink with baseURL on the form http://myhost.com/sub/ will fail later on
		// have to return the URL relative from baseURL
		relpath, err := helpers.GetRelativePath(link.String(), string(p.Site.BaseURL))
		if err != nil {
			return "", err
		}
		return "/" + filepath.ToSlash(relpath), nil
	}

	link.Scheme = ""
	link.Host = ""
	link.User = nil
	link.Opaque = ""
	return link.String(), nil
}

var ErrHasDraftAndPublished = errors.New("both draft and published parameters were found in page's frontmatter")

func (p *Page) update(f interface{}) error {
	if f == nil {
		return fmt.Errorf("no metadata found")
	}
	m := f.(map[string]interface{})
	var err error
	var draft, published, isCJKLanguage *bool
	for k, v := range m {
		loki := strings.ToLower(k)
		switch loki {
		case "title":
			p.Title = cast.ToString(v)
		case "linktitle":
			p.linkTitle = cast.ToString(v)
		case "description":
			p.Description = cast.ToString(v)
			p.Params["description"] = p.Description
		case "slug":
			p.Slug = cast.ToString(v)
		case "url":
			if url := cast.ToString(v); strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				return fmt.Errorf("Only relative URLs are supported, %v provided", url)
			}
			p.URLPath.URL = cast.ToString(v)
		case "type":
			p.contentType = cast.ToString(v)
		case "extension", "ext":
			p.extension = cast.ToString(v)
		case "keywords":
			p.Keywords = cast.ToStringSlice(v)
		case "date":
			p.Date, err = cast.ToTimeE(v)
			if err != nil {
				jww.ERROR.Printf("Failed to parse date '%v' in page %s", v, p.File.Path())
			}
		case "lastmod":
			p.Lastmod, err = cast.ToTimeE(v)
			if err != nil {
				jww.ERROR.Printf("Failed to parse lastmod '%v' in page %s", v, p.File.Path())
			}
		case "publishdate", "pubdate":
			p.PublishDate, err = cast.ToTimeE(v)
			if err != nil {
				jww.ERROR.Printf("Failed to parse publishdate '%v' in page %s", v, p.File.Path())
			}
		case "expirydate", "unpublishdate":
			p.ExpiryDate, err = cast.ToTimeE(v)
			if err != nil {
				jww.ERROR.Printf("Failed to parse expirydate '%v' in page %s", v, p.File.Path())
			}
		case "draft":
			draft = new(bool)
			*draft = cast.ToBool(v)
		case "published": // Intentionally undocumented
			published = new(bool)
			*published = cast.ToBool(v)
		case "layout":
			p.Layout = cast.ToString(v)
		case "markup":
			p.Markup = cast.ToString(v)
		case "weight":
			p.Weight = cast.ToInt(v)
		case "aliases":
			p.Aliases = cast.ToStringSlice(v)
			for _, alias := range p.Aliases {
				if strings.HasPrefix(alias, "http://") || strings.HasPrefix(alias, "https://") {
					return fmt.Errorf("Only relative aliases are supported, %v provided", alias)
				}
			}
		case "status":
			p.Status = cast.ToString(v)
		case "sitemap":
			p.Sitemap = parseSitemap(cast.ToStringMap(v))
		case "iscjklanguage":
			isCJKLanguage = new(bool)
			*isCJKLanguage = cast.ToBool(v)
		default:
			// If not one of the explicit values, store in Params
			switch vv := v.(type) {
			case bool:
				p.Params[loki] = vv
			case string:
				p.Params[loki] = vv
			case int64, int32, int16, int8, int:
				p.Params[loki] = vv
			case float64, float32:
				p.Params[loki] = vv
			case time.Time:
				p.Params[loki] = vv
			default: // handle array of strings as well
				switch vvv := vv.(type) {
				case []interface{}:
					if len(vvv) > 0 {
						switch vvv[0].(type) {
						case map[interface{}]interface{}: // Proper parsing structured array from YAML based FrontMatter
							p.Params[loki] = vvv
						case map[string]interface{}: // Proper parsing structured array from JSON based FrontMatter
							p.Params[loki] = vvv
						default:
							a := make([]string, len(vvv))
							for i, u := range vvv {
								a[i] = cast.ToString(u)
							}

							p.Params[loki] = a
						}
					} else {
						p.Params[loki] = []string{}
					}
				default:
					p.Params[loki] = vv
				}
			}
		}
	}

	if draft != nil && published != nil {
		p.Draft = *draft
		jww.ERROR.Printf("page %s has both draft and published settings in its frontmatter. Using draft.", p.File.Path())
		return ErrHasDraftAndPublished
	} else if draft != nil {
		p.Draft = *draft
	} else if published != nil {
		p.Draft = !*published
	}

	if p.Date.IsZero() && viper.GetBool("UseModTimeAsFallback") {
		fi, err := hugofs.Source().Stat(filepath.Join(helpers.AbsPathify(viper.GetString("ContentDir")), p.File.Path()))
		if err == nil {
			p.Date = fi.ModTime()
		}
	}

	if p.Lastmod.IsZero() {
		p.Lastmod = p.Date
	}

	if isCJKLanguage != nil {
		p.isCJKLanguage = *isCJKLanguage
	} else if viper.GetBool("HasCJKLanguage") {
		if cjk.Match(p.rawContent) {
			p.isCJKLanguage = true
		} else {
			p.isCJKLanguage = false
		}
	}

	return nil

}

func (p *Page) GetParam(key string) interface{} {
	return p.getParam(key, true)
}

func (p *Page) getParam(key string, stringToLower bool) interface{} {
	v := p.Params[strings.ToLower(key)]

	if v == nil {
		return nil
	}

	switch val := v.(type) {
	case bool:
		return val
	case string:
		if stringToLower {
			return strings.ToLower(val)
		}
		return val
	case int64, int32, int16, int8, int:
		return cast.ToInt(v)
	case float64, float32:
		return cast.ToFloat64(v)
	case time.Time:
		return val
	case []string:
		if stringToLower {
			return helpers.SliceToLower(val)
		}
		return v
	case map[string]interface{}: // JSON and TOML
		return v
	case map[interface{}]interface{}: // YAML
		return v
	}

	jww.ERROR.Printf("GetParam(\"%s\"): Unknown type %s\n", key, reflect.TypeOf(v))
	return nil
}

func (p *Page) HasMenuCurrent(menu string, me *MenuEntry) bool {
	menus := p.Menus()
	sectionPagesMenu := helpers.Config().GetString("SectionPagesMenu")

	// page is labeled as "shadow-member" of the menu with the same identifier as the section
	if sectionPagesMenu != "" && p.Section() != "" && sectionPagesMenu == menu && p.Section() == me.Identifier {
		return true
	}

	if m, ok := menus[menu]; ok {
		if me.HasChildren() {
			for _, child := range me.Children {
				if child.IsEqual(m) {
					return true
				}
				if p.HasMenuCurrent(menu, child) {
					return true
				}
			}
		}
	}

	return false

}

func (p *Page) IsMenuCurrent(menu string, inme *MenuEntry) bool {
	menus := p.Menus()

	if me, ok := menus[menu]; ok {
		return me.IsEqual(inme)
	}

	return false
}

func (p *Page) Menus() PageMenus {
	p.pageMenusInit.Do(func() {
		p.pageMenus = PageMenus{}

		if ms, ok := p.Params["menu"]; ok {
			link, _ := p.RelPermalink()

			me := MenuEntry{Name: p.LinkTitle(), Weight: p.Weight, URL: link}

			// Could be the name of the menu to attach it to
			mname, err := cast.ToStringE(ms)

			if err == nil {
				me.Menu = mname
				p.pageMenus[mname] = &me
				return
			}

			// Could be a slice of strings
			mnames, err := cast.ToStringSliceE(ms)

			if err == nil {
				for _, mname := range mnames {
					me.Menu = mname
					p.pageMenus[mname] = &me
				}
				return
			}

			// Could be a structured menu entry
			menus, err := cast.ToStringMapE(ms)

			if err != nil {
				jww.ERROR.Printf("unable to process menus for %q\n", p.Title)
			}

			for name, menu := range menus {
				menuEntry := MenuEntry{Name: p.LinkTitle(), URL: link, Weight: p.Weight, Menu: name}
				if menu != nil {
					jww.DEBUG.Printf("found menu: %q, in %q\n", name, p.Title)
					ime, err := cast.ToStringMapE(menu)
					if err != nil {
						jww.ERROR.Printf("unable to process menus for %q: %s", p.Title, err)
					}

					menuEntry.marshallMap(ime)
				}
				p.pageMenus[name] = &menuEntry

			}
		}
	})

	return p.pageMenus
}

func (p *Page) Render(layout ...string) template.HTML {
	var l []string

	if len(layout) > 0 {
		l = layouts(p.Type(), layout[0])
	} else {
		l = p.layouts()
	}

	return tpl.ExecuteTemplateToHTML(p, l...)
}

func (p *Page) determineMarkupType() string {
	// Try markup explicitly set in the frontmatter
	p.Markup = helpers.GuessType(p.Markup)
	if p.Markup == "unknown" {
		// Fall back to file extension (might also return "unknown")
		p.Markup = helpers.GuessType(p.Source.Ext())
	}

	return p.Markup
}

func (p *Page) parse(reader io.Reader) error {
	psr, err := parser.ReadFrom(reader)
	if err != nil {
		return err
	}

	p.renderable = psr.IsRenderable()
	p.frontmatter = psr.FrontMatter()
	p.rawContent = psr.Content()
	p.lang = p.Source.File.Lang()

	meta, err := psr.Metadata()
	if meta != nil {
		if err != nil {
			jww.ERROR.Printf("Error parsing page meta data for %s", p.File.Path())
			jww.ERROR.Println(err)
			return err
		}
		if err = p.update(meta); err != nil {
			return err
		}
	}

	return nil
}

func (p *Page) RawContent() string {
	return string(p.rawContent)
}

func (p *Page) SetSourceContent(content []byte) {
	p.Source.Content = content
}

func (p *Page) SetSourceMetaData(in interface{}, mark rune) (err error) {
	// See https://github.com/spf13/hugo/issues/2458
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("error from marshal: %v", r)
			}
		}
	}()

	var by []byte

	by, err = parser.InterfaceToFrontMatter(in, mark)
	if err != nil {
		return
	}
	by = append(by, '\n')

	p.Source.Frontmatter = by

	return
}

func (p *Page) SafeSaveSourceAs(path string) error {
	return p.saveSourceAs(path, true)
}

func (p *Page) SaveSourceAs(path string) error {
	return p.saveSourceAs(path, false)
}

func (p *Page) saveSourceAs(path string, safe bool) error {
	b := bp.GetBuffer()
	defer bp.PutBuffer(b)

	b.Write(p.Source.Frontmatter)
	b.Write(p.Source.Content)

	bc := make([]byte, b.Len(), b.Len())
	copy(bc, b.Bytes())

	err := p.saveSource(bc, path, safe)
	if err != nil {
		return err
	}
	return nil
}

func (p *Page) saveSource(by []byte, inpath string, safe bool) (err error) {
	if !filepath.IsAbs(inpath) {
		inpath = helpers.AbsPathify(inpath)
	}
	jww.INFO.Println("creating", inpath)

	if safe {
		err = helpers.SafeWriteToDisk(inpath, bytes.NewReader(by), hugofs.Source())
	} else {
		err = helpers.WriteToDisk(inpath, bytes.NewReader(by), hugofs.Source())
	}
	if err != nil {
		return
	}
	return nil
}

func (p *Page) SaveSource() error {
	return p.SaveSourceAs(p.FullFilePath())
}

func (p *Page) ProcessShortcodes(t tpl.Template) {
	tmpContent, tmpContentShortCodes, _ := extractAndRenderShortcodes(string(p.rawContent), p, t)
	p.rawContent = []byte(tmpContent)
	p.contentShortCodes = tmpContentShortCodes
}

func (p *Page) FullFilePath() string {
	return filepath.Join(p.Dir(), p.LogicalName())
}

func (p *Page) TargetPath() (outfile string) {
	// Always use URL if it's specified
	if len(strings.TrimSpace(p.URLPath.URL)) > 2 {
		outfile = strings.TrimSpace(p.URLPath.URL)

		if strings.HasSuffix(outfile, "/") {
			outfile = outfile + "index.html"
		}
		outfile = filepath.FromSlash(outfile)
		return
	}

	// If there's a Permalink specification, we use that
	if override, ok := p.Site.Permalinks[p.Section()]; ok {
		var err error
		outfile, err = override.Expand(p)
		if err == nil {
			outfile, _ = url.QueryUnescape(outfile)
			if strings.HasSuffix(outfile, "/") {
				outfile += "index.html"
			}
			outfile = filepath.FromSlash(outfile)
			outfile = p.addLangFilepathPrefix(outfile)
			return
		}
	}

	if len(strings.TrimSpace(p.Slug)) > 0 {
		outfile = strings.TrimSpace(p.Slug) + "." + p.Extension()
	} else {
		// Fall back to filename
		outfile = helpers.ReplaceExtension(p.Source.TranslationBaseName(), p.Extension())
	}

	return p.addLangFilepathPrefix(filepath.Join(strings.ToLower(helpers.MakePath(p.Source.Dir())), strings.TrimSpace(outfile)))
}
