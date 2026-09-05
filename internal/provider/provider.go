// Package provider turns a parsed config into usable provider/model bindings
// and provides the first client implementation (OpenAI-compatible chat
// completions). A model is referenced as "provider/model", matching pi's
// /model usage.
package provider

import (
	"fmt"
	"sort"
	"strings"

	"ruyishell/internal/config"
)

// Spec is a resolved, ready-to-use provider/model binding.
type Spec struct {
	Provider string // provider id
	Name     string // provider display name (falls back to id)
	API      string // API type, e.g. "openai-completions"
	BaseURL  string
	APIKey   string
	Headers  map[string]string
	Model    config.Model
}

// Ref returns the "provider/model" reference for the spec.
func (s *Spec) Ref() string {
	return s.Provider + "/" + s.Model.ID
}

// ToolsEnabled reports whether native function calling is enabled for this
// model: the per-model tools switch, defaulting to on when unset.
func (s *Spec) ToolsEnabled() bool {
	return s.Model.Tools == nil || *s.Model.Tools
}

// Resolve looks up a "provider/model" reference in cfg and returns its spec,
// resolving credential environment-variable references at this call.
func Resolve(cfg *config.Config, ref string) (*Spec, error) {
	prov, model, err := splitRef(ref)
	if err != nil {
		return nil, err
	}
	p, ok := cfg.Providers[prov]
	if !ok {
		return nil, fmt.Errorf("provider %q not found", prov)
	}
	for _, m := range p.Models {
		if m.ID == model {
			return buildSpec(prov, p, m)
		}
	}
	return nil, fmt.Errorf("model %q not found under provider %q", model, prov)
}

// List returns the sorted "provider/model" references for all configured
// models.
func List(cfg *config.Config) []string {
	var refs []string
	for provID, p := range cfg.Providers {
		for _, m := range p.Models {
			refs = append(refs, provID+"/"+m.ID)
		}
	}
	sort.Strings(refs)
	return refs
}

// Default returns the "provider/model" reference to use at startup: the
// configured default when it resolves, otherwise the first available model,
// otherwise "".
func Default(cfg *config.Config) string {
	if cfg.Default != "" {
		if _, err := Resolve(cfg, cfg.Default); err == nil {
			return cfg.Default
		}
	}
	refs := List(cfg)
	if len(refs) > 0 {
		return refs[0]
	}
	return ""
}

func buildSpec(provID string, p *config.Provider, m config.Model) (*Spec, error) {
	key, err := config.ResolveSecret(p.APIKey)
	if err != nil {
		return nil, err
	}
	api := p.API
	if m.API != "" {
		api = m.API
	}
	base := p.BaseURL
	if m.BaseURL != "" {
		base = m.BaseURL
	}
	spec := &Spec{
		Provider: provID,
		Name:     m.Name,
		API:      api,
		BaseURL:  base,
		APIKey:   key,
		Headers:  mergeHeaders(p.Headers, m.Headers),
		Model:    m,
	}
	if spec.Name == "" {
		spec.Name = m.ID
	}
	if spec.API == "" {
		spec.API = "openai-completions"
	}
	return spec, nil
}

func splitRef(ref string) (string, string, error) {
	prov, model, ok := strings.Cut(ref, "/")
	if !ok || prov == "" || model == "" {
		return "", "", fmt.Errorf("model reference must be %q, got %q", "provider/model", ref)
	}
	return prov, model, nil
}

func mergeHeaders(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
