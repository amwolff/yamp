package web

import (
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

const pageSize = 48

// folder is one node of the library folder tree. Its ID is opaque, so folder
// names never appear in URLs.
type folder struct {
	ID       string
	Name     string
	Root     string
	Rel      string
	Parent   string
	Children []string
}

// folderTree builds the tree from the distinct (root, rel_dir) pairs. The
// node with ID "" is the top. With one library root the top is that root;
// with several, the roots are the children of a virtual top.
func folderTree(roots []string, dirs []db.ListDirsRow, salt string) map[string]*folder {
	tree := map[string]*folder{}
	top := &folder{ID: "", Name: "Library"}
	tree[""] = top
	idOf := func(root, rel string) string {
		if len(roots) == 1 && rel == "" {
			return ""
		}
		return privacy.DirID(salt, root, rel)
	}
	if len(roots) == 1 {
		top.Root = roots[0]
	}
	var add func(root, rel string) string
	add = func(root, rel string) string {
		id := idOf(root, rel)
		if _, ok := tree[id]; ok {
			return id
		}
		f := &folder{ID: id, Root: root, Rel: rel}
		switch rel {
		case "":
			f.Name = filepath.Base(root)
			f.Parent = ""
		default:
			f.Name = path.Base(rel)
			parentRel := path.Dir(rel)
			if parentRel == "." {
				parentRel = ""
			}
			f.Parent = add(root, parentRel)
		}
		tree[id] = f
		parent := tree[f.Parent]
		parent.Children = append(parent.Children, id)
		return id
	}
	for _, r := range roots {
		if len(roots) > 1 {
			add(r, "")
		}
	}
	for _, d := range dirs {
		add(d.Root, d.RelDir)
	}
	for _, f := range tree {
		sort.Slice(f.Children, func(i, j int) bool {
			return strings.ToLower(tree[f.Children[i]].Name) < strings.ToLower(tree[f.Children[j]].Name)
		})
	}
	return tree
}

type crumb struct{ ID, Name string }

type cardsData struct {
	Items     []store.GridItem
	Next      string
	Offset    int
	ShowEmpty bool
}

type homeData struct {
	SortOptions []sortOption
	Folder      *folder
	Crumbs      []crumb
	Folders     []crumb
	Sort        string
	Search      string
	Continue    cardsData
	Cards       cardsData
	Empty       bool
}

type sortOption struct{ Value, Label string }

var sortOptions = []sortOption{
	{"added", "Recently added"},
	{"name", "Name"},
	{"recent", "Recently watched"},
	{"views", "Most viewed"},
	{"rating", "Highest rated"},
	{"duration", "Longest"},
}

func (s *Server) loadFolder(r *http.Request) (*folder, map[string]*folder, error) {
	dirs, err := s.Store.ListDirs(r.Context())
	if err != nil {
		return nil, nil, err
	}
	tree := folderTree(s.Config.Libraries, dirs, s.Settings.Salt())
	f, ok := tree[r.URL.Query().Get("d")]
	if !ok {
		f = tree[""]
	}
	return f, tree, nil
}

func (s *Server) gridPage(r *http.Request, f *folder, offset int) (cardsData, error) {
	u := auth.UserFrom(r.Context())
	q := r.URL.Query()
	sortKey := q.Get("sort")
	if !store.ValidSort(sortKey) {
		sortKey = string(store.SortAdded)
	}
	items, err := s.Store.Grid(r.Context(), store.GridQuery{
		UserID: u.ID, Root: f.Root, Dir: f.Rel, Search: q.Get("q"),
		Sort: store.GridSort(sortKey), Limit: pageSize + 1, Offset: offset,
	})
	if err != nil {
		return cardsData{}, err
	}
	cd := cardsData{Items: items, Offset: offset, ShowEmpty: true}
	if len(items) > pageSize {
		cd.Items = items[:pageSize]
		v := url.Values{}
		v.Set("d", f.ID)
		v.Set("sort", sortKey)
		v.Set("q", q.Get("q"))
		v.Set("offset", strconv.Itoa(offset+pageSize))
		cd.Next = "/grid?" + v.Encode()
	}
	return cd, nil
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	f, tree, err := s.loadFolder(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	u := auth.UserFrom(r.Context())
	data := homeData{SortOptions: sortOptions, Folder: f, Sort: r.URL.Query().Get("sort"), Search: r.URL.Query().Get("q")}
	if !store.ValidSort(data.Sort) {
		data.Sort = string(store.SortAdded)
	}
	for id := f.ID; id != ""; id = tree[id].Parent {
		data.Crumbs = append([]crumb{{id, tree[id].Name}}, data.Crumbs...)
	}
	for _, c := range f.Children {
		data.Folders = append(data.Folders, crumb{c, tree[c].Name})
	}
	if data.Cards, err = s.gridPage(r, f, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	if f.ID == "" && data.Search == "" {
		items, err := s.Store.Grid(r.Context(), store.GridQuery{
			UserID: u.ID, Sort: store.SortRecent, ContinueOnly: true, Limit: 12,
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data.Continue = cardsData{Items: items}
	}
	data.Empty = len(data.Cards.Items) == 0 && len(data.Folders) == 0
	p := s.newPage(r, "YAMP", "home", data)
	s.render(w, r, http.StatusOK, "home", p)
}

// handleGrid returns one page of cards for search, sort and infinite scroll.
func (s *Server) handleGrid(w http.ResponseWriter, r *http.Request) {
	f, _, err := s.loadFolder(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	cd, err := s.gridPage(r, f, max(offset, 0))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderTemplate(w, r, http.StatusOK, "home", "cards", cd)
}

func (s *Server) handleDiscreet(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	on := int64(0)
	if r.PostFormValue("discreet") == "1" {
		on = 1
	}
	if err := s.Store.SetDiscreet(r.Context(), db.SetDiscreetParams{Discreet: on, ID: u.ID}); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther) //nolint:gosec // G710: backTo accepts only local paths
}
