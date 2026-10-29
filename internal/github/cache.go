package github

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MetadataCache is an optional, disposable local mirror of responses already
// fetched from GitHub. Callers must never use it as delivery authority.
type MetadataCache struct{ Root string }

func NewMetadataCache() (MetadataCache, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return MetadataCache{}, err
	}
	return MetadataCache{Root: filepath.Join(root, "quipu")}, nil
}

func (c MetadataCache) StoreJSON(key string, value any) error {
	clean := filepath.Clean(key)
	if key == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid cache key %q", key)
	}
	target := filepath.Join(c.Root, clean+".json")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".quipu-cache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	encoder := json.NewEncoder(tmp)
	if err := encoder.Encode(value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}
