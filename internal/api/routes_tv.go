package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stashapp/stash/internal/manager"
	"github.com/stashapp/stash/internal/manager/config"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
)

// The /tv routes serve a server-rendered, near-JavaScript-free version of the
// UI, intended for TV and other low-powered browsers. Navigation is plain links
// and GET forms so that it works with a remote's d-pad.

//go:embed tv_templates/*.html
var tvTemplateFS embed.FS

const tvPerPage = 24

var tvTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"duration": tvFormatDuration,
	"card": func(prefix string, s tvSceneCard) interface{} {
		return struct {
			Prefix string
			Scene  tvSceneCard
		}{prefix, s}
	},
	"resumePct": func(resume, duration float64) float64 {
		return math.Min(100, resume/duration*100)
	},
}).ParseFS(tvTemplateFS, "tv_templates/*.html"))

type tvRoutes struct {
	routes
	repository models.Repository
	config     *config.Config
}

func (rs tvRoutes) Routes() chi.Router {
	r := chi.NewRouter()

	r.Get("/", rs.Home)
	r.Get("/scenes", rs.Scenes)
	r.Get("/scene/{sceneId}", rs.Scene)
	r.Get("/scene/{sceneId}/play", rs.Play)
	r.Get("/performers", rs.Performers)
	r.Get("/studios", rs.Studios)
	r.Get("/tags", rs.Tags)
	r.Post("/cast", rs.Cast)
	r.Get("/cast/wait", rs.CastWait)
	r.Get("/cast/status", rs.CastStatus)
	r.Post("/cast/status", rs.ReportCastStatus)

	return r
}

// view models

type tvSceneCard struct {
	ID         int
	Title      string
	Date       string
	Duration   float64
	Resolution string
	Resume     float64
}

type tvItemCard struct {
	ID    int
	Name  string
	Image string
	Link  string
}

type tvLink struct {
	Label  string
	URL    string
	Active bool
}

type tvRow struct {
	Title  string
	More   string
	Scenes []tvSceneCard
}

type tvPager struct {
	Page  int
	Pages int
	Prev  string
	Next  string
	Total int
}

type tvPage struct {
	Prefix string
	Title  string
	Nav    string
	Query  string
	// CastSeq is the current cast sequence number, so the page only reacts
	// to casts made after it was loaded.
	CastSeq int
}

type tvStream struct {
	Label string
	Index int
}

// helpers

func tvFormatDuration(secs float64) string {
	s := int(math.Round(secs))
	if s <= 0 {
		return ""
	}
	h, m, sec := s/3600, (s/60)%60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

func tvResolution(f *models.VideoFile) string {
	if f == nil || f.Height == 0 {
		return ""
	}
	h := min(f.Width, f.Height)
	switch {
	case h >= 2160:
		return "4K"
	case h >= 1440:
		return "1440p"
	case h >= 1080:
		return "1080p"
	case h >= 720:
		return "720p"
	default:
		return fmt.Sprintf("%dp", h)
	}
}

func (rs tvRoutes) render(w http.ResponseWriter, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tvTemplates.ExecuteTemplate(w, name, data); err != nil {
		logger.Errorf("[tv] error rendering %s: %v", name, err)
	}
}

func (rs tvRoutes) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	logger.Errorf("[tv] %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func tvIntParam(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return v
}

// pageURL returns the current URL with the given query parameters replaced.
func tvPageURL(r *http.Request, set map[string]string) string {
	q := r.URL.Query()
	for k, v := range set {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	u := getProxyPrefix(r) + r.URL.Path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	return u
}

func tvMakePager(r *http.Request, page, total int) tvPager {
	pages := max(1, (total+tvPerPage-1)/tvPerPage)
	p := tvPager{Page: page, Pages: pages, Total: total}
	if page > 1 {
		p.Prev = tvPageURL(r, map[string]string{"page": strconv.Itoa(page - 1)})
	}
	if page < pages {
		p.Next = tvPageURL(r, map[string]string{"page": strconv.Itoa(page + 1)})
	}
	return p
}

func tvFindFilter(q string, page, perPage int, sort string, dir models.SortDirectionEnum) *models.FindFilterType {
	f := &models.FindFilterType{
		Page:      &page,
		PerPage:   &perPage,
		Sort:      &sort,
		Direction: &dir,
	}
	if q != "" {
		f.Q = &q
	}
	return f
}

func (rs tvRoutes) basePage(r *http.Request, title, nav string) tvPage {
	return tvPage{
		Prefix:  getProxyPrefix(r),
		Title:   title,
		Nav:     nav,
		Query:   r.URL.Query().Get("q"),
		CastSeq: tvCast.current().Seq,
	}
}

func (rs tvRoutes) sceneCards(ctx context.Context, scenes []*models.Scene) ([]tvSceneCard, error) {
	ret := make([]tvSceneCard, 0, len(scenes))
	for _, s := range scenes {
		if err := s.LoadPrimaryFile(ctx, rs.repository.File); err != nil {
			return nil, err
		}
		c := tvSceneCard{
			ID:     s.ID,
			Title:  s.GetTitle(),
			Resume: s.ResumeTime,
		}
		if s.Date != nil {
			c.Date = s.Date.String()
		}
		if f := s.Files.Primary(); f != nil {
			c.Duration = f.Duration
			c.Resolution = tvResolution(f)
		}
		ret = append(ret, c)
	}
	return ret, nil
}

func (rs tvRoutes) queryScenes(ctx context.Context, sceneFilter *models.SceneFilterType, findFilter *models.FindFilterType) ([]tvSceneCard, int, error) {
	result, err := rs.repository.Scene.Query(ctx, models.SceneQueryOptions{
		QueryOptions: models.QueryOptions{
			FindFilter: findFilter,
			Count:      true,
		},
		SceneFilter: sceneFilter,
	})
	if err != nil {
		return nil, 0, err
	}
	scenes, err := result.Resolve(ctx)
	if err != nil {
		return nil, 0, err
	}
	cards, err := rs.sceneCards(ctx, scenes)
	return cards, result.Count, err
}

// handlers

func (rs tvRoutes) Home(w http.ResponseWriter, r *http.Request) {
	prefix := getProxyPrefix(r)
	const rowSize = 12

	type rowDef struct {
		title  string
		more   string
		filter *models.SceneFilterType
		sort   string
	}
	defs := []rowDef{
		{
			title: "Continue watching",
			more:  "/tv/scenes?sort=last_played_at&inprogress=1",
			filter: &models.SceneFilterType{
				ResumeTime: &models.IntCriterionInput{Value: 0, Modifier: models.CriterionModifierGreaterThan},
			},
			sort: "last_played_at",
		},
		{title: "Recently added", more: "/tv/scenes?sort=created_at", sort: "created_at"},
		{title: "Random", more: "/tv/scenes?sort=random", sort: "random"},
	}

	var rows []tvRow
	err := rs.withReadTxn(r, func(ctx context.Context) error {
		for _, d := range defs {
			cards, _, err := rs.queryScenes(ctx, d.filter, tvFindFilter("", 1, rowSize, d.sort, models.SortDirectionEnumDesc))
			if err != nil {
				return err
			}
			if len(cards) > 0 {
				rows = append(rows, tvRow{Title: d.title, More: prefix + d.more, Scenes: cards})
			}
		}
		return nil
	})
	if err != nil {
		rs.fail(w, err)
		return
	}

	rs.render(w, "home.html", struct {
		tvPage
		Rows []tvRow
	}{rs.basePage(r, "Home", "home"), rows})
}

var tvSceneSorts = []struct{ key, label string }{
	{"created_at", "Newest"},
	{"date", "Release date"},
	{"title", "Title"},
	{"rating", "Rating"},
	{"play_count", "Most played"},
	{"last_played_at", "Last played"},
	{"duration", "Longest"},
	{"random", "Random"},
}

func (rs tvRoutes) Scenes(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := qs.Get("q")
	page := max(1, tvIntParam(r, "page", 1))

	sort := qs.Get("sort")
	validSort := false
	for _, s := range tvSceneSorts {
		if s.key == sort {
			validSort = true
		}
	}
	if !validSort {
		sort = "created_at"
	}
	dir := models.SortDirectionEnumDesc
	if sort == "title" {
		dir = models.SortDirectionEnumAsc
	}

	sceneFilter := &models.SceneFilterType{}
	title := "Scenes"
	var heading string

	if qs.Get("inprogress") == "1" {
		sceneFilter.ResumeTime = &models.IntCriterionInput{Value: 0, Modifier: models.CriterionModifierGreaterThan}
		heading = "Continue watching"
	}

	performerID, _ := strconv.Atoi(qs.Get("performer"))
	studioID, _ := strconv.Atoi(qs.Get("studio"))
	tagID, _ := strconv.Atoi(qs.Get("tag"))
	if performerID > 0 {
		sceneFilter.Performers = &models.MultiCriterionInput{Value: []string{strconv.Itoa(performerID)}, Modifier: models.CriterionModifierIncludes}
	}
	if studioID > 0 {
		depth := -1
		sceneFilter.Studios = &models.HierarchicalMultiCriterionInput{Value: []string{strconv.Itoa(studioID)}, Modifier: models.CriterionModifierIncludes, Depth: &depth}
	}
	if tagID > 0 {
		sceneFilter.Tags = &models.HierarchicalMultiCriterionInput{Value: []string{strconv.Itoa(tagID)}, Modifier: models.CriterionModifierIncludes}
	}

	var cards []tvSceneCard
	var total int
	err := rs.withReadTxn(r, func(ctx context.Context) error {
		if performerID > 0 {
			if p, err := rs.repository.Performer.Find(ctx, performerID); err == nil && p != nil {
				heading = p.Name
			}
		}
		if studioID > 0 {
			if s, err := rs.repository.Studio.Find(ctx, studioID); err == nil && s != nil {
				heading = s.Name
			}
		}
		if tagID > 0 {
			if t, err := rs.repository.Tag.Find(ctx, tagID); err == nil && t != nil {
				heading = t.Name
			}
		}

		var err error
		cards, total, err = rs.queryScenes(ctx, sceneFilter, tvFindFilter(q, page, tvPerPage, sort, dir))
		return err
	})
	if err != nil {
		rs.fail(w, err)
		return
	}

	if heading != "" {
		title = heading
	}
	if q != "" {
		title = fmt.Sprintf("Search: %s", q)
	}

	var sorts []tvLink
	for _, s := range tvSceneSorts {
		sorts = append(sorts, tvLink{
			Label:  s.label,
			URL:    tvPageURL(r, map[string]string{"sort": s.key, "page": ""}),
			Active: s.key == sort,
		})
	}

	rs.render(w, "scenes.html", struct {
		tvPage
		Scenes []tvSceneCard
		Sorts  []tvLink
		Pager  tvPager
	}{rs.basePage(r, title, "scenes"), cards, sorts, tvMakePager(r, page, total)})
}

type tvSceneDetail struct {
	tvSceneCard
	Details    string
	Studio     *tvLink
	Performers []tvLink
	Tags       []tvLink
	Streams    []tvStream
	Rating     string
	PlayCount  int
}

func (rs tvRoutes) loadScene(r *http.Request, fn func(ctx context.Context, s *models.Scene) error) (bool, error) {
	sceneID, err := strconv.Atoi(chi.URLParam(r, "sceneId"))
	if err != nil {
		return false, nil
	}
	found := false
	err = rs.withReadTxn(r, func(ctx context.Context) error {
		s, err := rs.repository.Scene.Find(ctx, sceneID)
		if err != nil || s == nil {
			return err
		}
		found = true
		if err := s.LoadPrimaryFile(ctx, rs.repository.File); err != nil {
			return err
		}
		return fn(ctx, s)
	})
	return found, err
}

// sceneStreams returns the playable stream endpoints for a scene, excluding
// DASH and WebM which TV browsers generally can't play without JavaScript.
func (rs tvRoutes) sceneStreams(r *http.Request, s *models.Scene) ([]*manager.SceneStreamEndpoint, error) {
	base := &url.URL{Path: fmt.Sprintf("%s/scene/%d/stream", getProxyPrefix(r), s.ID)}
	all, err := manager.GetSceneStreamPaths(s, base, rs.config.GetMaxStreamingTranscodeSize())
	if err != nil {
		return nil, err
	}
	var ret []*manager.SceneStreamEndpoint
	for _, e := range all {
		p := strings.SplitN(e.URL, "?", 2)[0]
		if strings.HasSuffix(p, ".mpd") || strings.HasSuffix(p, ".webm") {
			continue
		}
		ret = append(ret, e)
	}
	return ret, nil
}

func (rs tvRoutes) Scene(w http.ResponseWriter, r *http.Request) {
	prefix := getProxyPrefix(r)
	var d tvSceneDetail

	found, err := rs.loadScene(r, func(ctx context.Context, s *models.Scene) error {
		cards, err := rs.sceneCards(ctx, []*models.Scene{s})
		if err != nil {
			return err
		}
		d.tvSceneCard = cards[0]
		d.Details = s.Details
		if s.Rating != nil {
			d.Rating = fmt.Sprintf("%d/5", int(math.Round(float64(*s.Rating)/20)))
		}

		if s.StudioID != nil {
			st, err := rs.repository.Studio.Find(ctx, *s.StudioID)
			if err != nil {
				return err
			}
			if st != nil {
				d.Studio = &tvLink{Label: st.Name, URL: fmt.Sprintf("%s/tv/scenes?studio=%d", prefix, st.ID)}
			}
		}

		if err := s.LoadPerformerIDs(ctx, rs.repository.Scene); err != nil {
			return err
		}
		performers, err := rs.repository.Performer.FindMany(ctx, s.PerformerIDs.List())
		if err != nil {
			return err
		}
		for _, p := range performers {
			d.Performers = append(d.Performers, tvLink{Label: p.Name, URL: fmt.Sprintf("%s/tv/scenes?performer=%d", prefix, p.ID)})
		}

		if err := s.LoadTagIDs(ctx, rs.repository.Scene); err != nil {
			return err
		}
		tags, err := rs.repository.Tag.FindMany(ctx, s.TagIDs.List())
		if err != nil {
			return err
		}
		for _, t := range tags {
			d.Tags = append(d.Tags, tvLink{Label: t.Name, URL: fmt.Sprintf("%s/tv/scenes?tag=%d", prefix, t.ID)})
		}

		d.PlayCount, err = rs.repository.Scene.CountViews(ctx, s.ID)
		if err != nil {
			return err
		}

		streams, err := rs.sceneStreams(r, s)
		if err != nil {
			return err
		}
		for i, e := range streams {
			label := ""
			if e.Label != nil {
				label = *e.Label
			}
			d.Streams = append(d.Streams, tvStream{Label: label, Index: i})
		}
		return nil
	})
	if err != nil {
		rs.fail(w, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	rs.render(w, "scene.html", struct {
		tvPage
		Scene tvSceneDetail
	}{rs.basePage(r, d.Title, "scenes"), d})
}

func (rs tvRoutes) Play(w http.ResponseWriter, r *http.Request) {
	streamIdx := max(0, tvIntParam(r, "s", 0))
	start := math.Max(0, float64(tvIntParam(r, "t", 0)))

	var title, src, mime string
	var sceneID int
	// direct streams can be seeked by the browser; transcoded streams are
	// restarted at the new position instead
	var direct bool
	// offset is the position in the scene that the stream's time 0 maps to.
	// Direct streams can seek using a media fragment; transcoded streams
	// start at the requested position instead.
	var offset float64

	found, err := rs.loadScene(r, func(ctx context.Context, s *models.Scene) error {
		sceneID = s.ID
		title = s.GetTitle()
		streams, err := rs.sceneStreams(r, s)
		if err != nil {
			return err
		}
		if len(streams) == 0 {
			return nil
		}
		if streamIdx >= len(streams) {
			streamIdx = 0
		}
		e := streams[streamIdx]
		src = e.URL
		direct = strings.HasSuffix(strings.SplitN(src, "?", 2)[0], "/stream")
		if e.MimeType != nil {
			mime = *e.MimeType
		}

		if start > 0 {
			if direct {
				src += fmt.Sprintf("#t=%d", int(start))
			} else if !strings.Contains(src, ".m3u8") {
				sep := "?"
				if strings.Contains(src, "?") {
					sep = "&"
				}
				src += fmt.Sprintf("%sstart=%d", sep, int(start))
				offset = start
			}
		}
		return nil
	})
	if err != nil {
		rs.fail(w, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if src == "" {
		http.Error(w, "no playable stream for this scene", http.StatusNotFound)
		return
	}

	rs.render(w, "play.html", struct {
		tvPage
		SceneID  int
		Src      string
		Mime     string
		Offset   float64
		Stream   int
		Seekable bool
	}{rs.basePage(r, title, "scenes"), sceneID, src, mime, offset, streamIdx, direct})
}

type tvListDef struct {
	nav   string
	title string
	query func(ctx context.Context, f *models.FindFilterType) ([]tvItemCard, int, error)
}

func (rs tvRoutes) list(w http.ResponseWriter, r *http.Request, def tvListDef) {
	q := r.URL.Query().Get("q")
	page := max(1, tvIntParam(r, "page", 1))

	sort := "scenes_count"
	dir := models.SortDirectionEnumDesc
	if r.URL.Query().Get("sort") == "name" {
		sort = "name"
		dir = models.SortDirectionEnumAsc
	}

	var items []tvItemCard
	var total int
	err := rs.withReadTxn(r, func(ctx context.Context) error {
		var err error
		items, total, err = def.query(ctx, tvFindFilter(q, page, tvPerPage, sort, dir))
		return err
	})
	if err != nil {
		rs.fail(w, err)
		return
	}

	sorts := []tvLink{
		{Label: "Most scenes", URL: tvPageURL(r, map[string]string{"sort": "", "page": ""}), Active: sort == "scenes_count"},
		{Label: "Name", URL: tvPageURL(r, map[string]string{"sort": "name", "page": ""}), Active: sort == "name"},
	}

	rs.render(w, "list.html", struct {
		tvPage
		Items  []tvItemCard
		Sorts  []tvLink
		Pager  tvPager
		Action string
	}{rs.basePage(r, def.title, def.nav), items, sorts, tvMakePager(r, page, total), getProxyPrefix(r) + "/tv/" + def.nav})
}

func tvHasScenes() *models.IntCriterionInput {
	return &models.IntCriterionInput{Value: 0, Modifier: models.CriterionModifierGreaterThan}
}

func (rs tvRoutes) Performers(w http.ResponseWriter, r *http.Request) {
	prefix := getProxyPrefix(r)
	rs.list(w, r, tvListDef{nav: "performers", title: "Performers", query: func(ctx context.Context, f *models.FindFilterType) ([]tvItemCard, int, error) {
		ps, n, err := rs.repository.Performer.Query(ctx, &models.PerformerFilterType{SceneCount: tvHasScenes()}, f)
		var ret []tvItemCard
		for _, p := range ps {
			ret = append(ret, tvItemCard{
				ID:    p.ID,
				Name:  p.Name,
				Image: fmt.Sprintf("%s/performer/%d/image", prefix, p.ID),
				Link:  fmt.Sprintf("%s/tv/scenes?performer=%d", prefix, p.ID),
			})
		}
		return ret, n, err
	}})
}

func (rs tvRoutes) Studios(w http.ResponseWriter, r *http.Request) {
	prefix := getProxyPrefix(r)
	rs.list(w, r, tvListDef{nav: "studios", title: "Studios", query: func(ctx context.Context, f *models.FindFilterType) ([]tvItemCard, int, error) {
		ss, n, err := rs.repository.Studio.Query(ctx, &models.StudioFilterType{SceneCount: tvHasScenes()}, f)
		var ret []tvItemCard
		for _, s := range ss {
			ret = append(ret, tvItemCard{
				ID:    s.ID,
				Name:  s.Name,
				Image: fmt.Sprintf("%s/studio/%d/image", prefix, s.ID),
				Link:  fmt.Sprintf("%s/tv/scenes?studio=%d", prefix, s.ID),
			})
		}
		return ret, n, err
	}})
}

func (rs tvRoutes) Tags(w http.ResponseWriter, r *http.Request) {
	prefix := getProxyPrefix(r)
	rs.list(w, r, tvListDef{nav: "tags", title: "Tags", query: func(ctx context.Context, f *models.FindFilterType) ([]tvItemCard, int, error) {
		filter := &models.TagFilterType{SceneCount: &models.HierarchicalCountInput{Value: 0, Modifier: models.CriterionModifierGreaterThan}}
		ts, n, err := rs.repository.Tag.Query(ctx, filter, f)
		var ret []tvItemCard
		for _, t := range ts {
			ret = append(ret, tvItemCard{
				ID:    t.ID,
				Name:  t.Name,
				Image: fmt.Sprintf("%s/tag/%d/image", prefix, t.ID),
				Link:  fmt.Sprintf("%s/tv/scenes?tag=%d", prefix, t.ID),
			})
		}
		return ret, n, err
	}})
}

// casting
//
// The normal UI posts a scene to /tv/cast. TV pages long-poll /tv/cast/wait and
// navigate to the player when a new cast arrives.

const tvCastWaitTimeout = 25 * time.Second

// tvCastCommand is sent to the TV. Action is one of play, pause, resume, seek
// or stop. Play includes the player URL; seek includes the scene position.
type tvCastCommand struct {
	Seq    int     `json:"seq"`
	Action string  `json:"action,omitempty"`
	URL    string  `json:"url,omitempty"`
	Time   float64 `json:"time,omitempty"`
}

// tvCastStatus is reported by the TV player.
type tvCastStatus struct {
	SceneID  int       `json:"scene_id"`
	Time     float64   `json:"time"`
	Paused   bool      `json:"paused"`
	Reported time.Time `json:"-"`
}

// a TV that hasn't reported for this long is treated as not playing
const tvCastStatusStale = 15 * time.Second

type tvCaster struct {
	mu      sync.Mutex
	cmd     tvCastCommand
	changed chan struct{}
	status  *tvCastStatus
}

var tvCast = &tvCaster{changed: make(chan struct{})}

func (c *tvCaster) current() tvCastCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cmd
}

func (c *tvCaster) send(cmd tvCastCommand) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd.Seq = c.cmd.Seq + 1
	c.cmd = cmd

	// reflect the command in the status straight away, so that controllers
	// don't flicker back to the old state before the TV next reports
	if c.status != nil {
		switch cmd.Action {
		case "pause":
			c.status.Time = c.status.position()
			c.status.Paused = true
			c.status.Reported = time.Now()
		case "resume":
			c.status.Paused = false
			c.status.Reported = time.Now()
		case "seek":
			c.status.Time = cmd.Time
			c.status.Reported = time.Now()
		case "stop":
			c.status = nil
		}
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

// wait returns the latest command once its sequence number is greater than
// since, or false if the context ends or the timeout is reached first.
func (c *tvCaster) wait(ctx context.Context, since int) (tvCastCommand, bool) {
	timeout := time.NewTimer(tvCastWaitTimeout)
	defer timeout.Stop()
	for {
		c.mu.Lock()
		cmd, changed := c.cmd, c.changed
		c.mu.Unlock()
		// the server restarted since the page was loaded
		if since > cmd.Seq {
			since = cmd.Seq
		}
		if cmd.Seq > since {
			return cmd, true
		}
		select {
		case <-changed:
		case <-timeout.C:
			return tvCastCommand{}, false
		case <-ctx.Done():
			return tvCastCommand{}, false
		}
	}
}

func (s *tvCastStatus) position() float64 {
	if s.Paused {
		return s.Time
	}
	return s.Time + time.Since(s.Reported).Seconds()
}

func (c *tvCaster) setStatus(st tvCastStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st.Reported = time.Now()
	c.status = &st
}

func (c *tvCaster) clearStatus() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = nil
}

func (c *tvCaster) getStatus() *tvCastStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == nil || time.Since(c.status.Reported) > tvCastStatusStale {
		return nil
	}
	ret := *c.status
	ret.Time = ret.position()
	return &ret
}

func (rs tvRoutes) Cast(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Action  string  `json:"action"`
		SceneID string  `json:"scene_id"`
		Time    float64 `json:"time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch input.Action {
	case "", "play":
	case "pause", "resume", "stop":
		tvCast.send(tvCastCommand{Action: input.Action})
		w.WriteHeader(http.StatusNoContent)
		return
	case "seek":
		tvCast.send(tvCastCommand{Action: input.Action, Time: math.Max(0, input.Time)})
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	sceneID, err := strconv.Atoi(input.SceneID)
	if err != nil {
		http.Error(w, "invalid scene_id", http.StatusBadRequest)
		return
	}

	var scene *models.Scene
	if err := rs.withReadTxn(r, func(ctx context.Context) error {
		var err error
		scene, err = rs.repository.Scene.Find(ctx, sceneID)
		return err
	}); err != nil {
		rs.fail(w, err)
		return
	}
	if scene == nil {
		http.NotFound(w, r)
		return
	}

	u := fmt.Sprintf("%s/tv/scene/%d/play", getProxyPrefix(r), sceneID)
	if t := int(input.Time); t > 0 {
		u += fmt.Sprintf("?t=%d", t)
	}
	tvCast.send(tvCastCommand{Action: "play", URL: u})
	logger.Infof("[tv] cast scene %d at %ds", sceneID, int(input.Time))
	w.WriteHeader(http.StatusNoContent)
}

func (rs tvRoutes) CastWait(w http.ResponseWriter, r *http.Request) {
	since := tvIntParam(r, "since", 0)
	w.Header().Set("Cache-Control", "no-store")

	cmd, ok := tvCast.wait(r.Context(), since)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cmd)
}

// CastStatus returns what the TV is playing, or null if nothing.
func (rs tvRoutes) CastStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	st := tvCast.getStatus()
	if st == nil {
		_, _ = w.Write([]byte("null"))
		return
	}

	ret := struct {
		SceneID  string  `json:"scene_id"`
		Title    string  `json:"title"`
		Time     float64 `json:"time"`
		Duration float64 `json:"duration"`
		Paused   bool    `json:"paused"`
	}{SceneID: strconv.Itoa(st.SceneID), Time: st.Time, Paused: st.Paused}

	if err := rs.withReadTxn(r, func(ctx context.Context) error {
		s, err := rs.repository.Scene.Find(ctx, st.SceneID)
		if err != nil || s == nil {
			return err
		}
		if err := s.LoadPrimaryFile(ctx, rs.repository.File); err != nil {
			return err
		}
		ret.Title = s.GetTitle()
		if f := s.Files.Primary(); f != nil {
			ret.Duration = f.Duration
		}
		return nil
	}); err != nil {
		rs.fail(w, err)
		return
	}
	if ret.Duration > 0 {
		ret.Time = math.Min(ret.Time, ret.Duration)
	}

	_ = json.NewEncoder(w).Encode(ret)
}

// ReportCastStatus is called periodically by the TV player.
func (rs tvRoutes) ReportCastStatus(w http.ResponseWriter, r *http.Request) {
	var st tvCastStatus
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if st.SceneID == 0 {
		tvCast.clearStatus()
	} else {
		tvCast.setStatus(st)
	}
	w.WriteHeader(http.StatusNoContent)
}
