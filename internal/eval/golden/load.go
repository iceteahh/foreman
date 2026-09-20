package golden

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads every *.json case directly under dir (sub-directories hold
// fixtures, not cases) and validates the suite as a whole.
func Load(dir string) (Suite, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Suite{}, fmt.Errorf("golden suite %s: %w", dir, err)
	}
	s := Suite{Dir: dir}
	var errs []error
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		c, err := LoadCase(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if prev, dup := seen[c.ID]; dup {
			errs = append(errs, fmt.Errorf("case id %q appears in both %s and %s", c.ID, prev, path))
			continue
		}
		seen[c.ID] = path
		s.Cases = append(s.Cases, c)
	}
	if err := errors.Join(errs...); err != nil {
		return s, err
	}
	if len(s.Cases) == 0 {
		return s, fmt.Errorf("golden suite %s: no *.json cases found", dir)
	}
	s.Sort()
	return s, nil
}

// LoadCase reads one case file. Unknown fields are rejected: a typo in an
// expectation would otherwise silently weaken the suite.
func LoadCase(path string) (*Case, error) {
	b, err := os.ReadFile(path) //nolint:gosec // suite paths come from the operator
	if err != nil {
		return nil, err
	}
	var c Case
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Path = path
	if c.ID == "" {
		c.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.checkFixture(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// checkFixture verifies the fixture exists at load time, so a renamed
// directory fails the suite immediately instead of after the first spawn.
func (c *Case) checkFixture() error {
	p := c.fixturePath()
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("fixture %s: %w", p, err)
	}
	if c.Repo.Dir != "" && !st.IsDir() {
		return fmt.Errorf("fixture %s: repo.dir is not a directory", p)
	}
	if c.Repo.Bundle != "" && st.IsDir() {
		return fmt.Errorf("fixture %s: repo.bundle is a directory (use repo.dir)", p)
	}
	return nil
}

// fixturePath resolves the fixture relative to the case file.
func (c *Case) fixturePath() string {
	rel := c.Repo.Bundle
	if rel == "" {
		rel = c.Repo.Dir
	}
	return filepath.Join(filepath.Dir(c.Path), rel)
}

// WriteCase serialises a case next to the others (used by import-reviews).
func WriteCase(path string, c *Case) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
