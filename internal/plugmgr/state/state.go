// Package state persists per-object metadata so update/delete/report work
// across shell sessions. Zi keeps ices in in-shell hashes and ._zi dirs; here
// each installed object dir carries a .zi-go.json manifest.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/blairham/gish/internal/plugmgr/ice"
	"github.com/blairham/gish/internal/plugmgr/spec"
)

const manifestName = ".zi-go.json"

type Object struct {
	Kind        string            `json:"kind"` // "plugin" or "snippet"
	Raw         string            `json:"raw"`  // spec as the user wrote it
	User        string            `json:"user,omitempty"`
	Repo        string            `json:"repo,omitempty"`
	URL         string            `json:"url,omitempty"`
	ID          string            `json:"id"`
	Ices        map[string]string `json:"ices,omitempty"`
	InstalledAt time.Time         `json:"installed_at"`
}

func SaveObject(dir string, s *spec.Spec, ic *ice.Ices) error {
	kind := "plugin"
	if s.Kind == spec.Snippet {
		kind = "snippet"
	}
	obj := Object{
		Kind: kind, Raw: s.Raw, User: s.User, Repo: s.Repo,
		URL: s.URL, ID: s.ID, Ices: ic.Map(), InstalledAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestName), append(data, '\n'), 0o644)
}

func LoadObject(dir string) (*Object, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var obj Object
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

// ListObjects returns manifests for every installed object under root,
// sorted by ID. Dirs without a manifest (e.g. hand-copied) get a stub entry
// so list/delete still see them.
func ListObjects(root, kind string) ([]*Object, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Object
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		obj, err := LoadObject(dir)
		if err != nil {
			obj = &Object{Kind: kind, ID: e.Name(), Raw: e.Name()}
		}
		out = append(out, obj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
