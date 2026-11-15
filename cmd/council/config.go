package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// defaults is the config every install starts from: the seats, models,
// prices and search server, and nothing of one person's.
//
//go:embed defaults.json
var defaults []byte

// home is the council's folder: keys.env, council.json and sessions/.
func home() string { return expand("~/.council") }

// builtin returns the built-in config alone.
func builtin() config {
	var cfg config
	if err := json.Unmarshal(defaults, &cfg); err != nil {
		panic("defaults.json: " + err.Error())
	}
	return cfg
}

// loadConfig layers the config: the built-in one, then the user's
// ~/.council/council.json, then the file extra names, if any. A later layer
// replaces the fields it sets; maps such as rosters and prices merge by key.
func loadConfig(extra string) (config, error) {
	cfg := builtin()
	for _, path := range []string{filepath.Join(home(), "council.json"), extra} {
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) && path != extra {
			continue
		}
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	}
	return cfg, nil
}

// budget is a seat's output budget. Thinking and the answer share it, so it
// grows with the effort, and more on the Anthropic seats, whose requests
// stream. A seat's own maxTokens wins; base serves the lower efforts.
func budget(spec minds.Spec, base int64) int64 {
	if spec.MaxTokens > 0 {
		return spec.MaxTokens
	}
	switch effort := strings.ToLower(spec.Effort); {
	case spec.Provider == "anthropic" && (effort == "xhigh" || effort == "max"):
		return 64000
	case spec.Provider != "anthropic" && (effort == "high" || effort == "xhigh" || effort == "max"):
		return 32768
	}
	return base
}

// minSeats is the smallest council that debates.
const minSeats = 2

// efforts are the levels each lab's API takes.
var efforts = map[string][]string{
	"anthropic": {"low", "medium", "high", "xhigh", "max"},
	"gemini":    {"low", "medium", "high", "xhigh"},
	"openai":    {"low", "medium", "high", "xhigh", "max"},
}

type seatOption struct {
	Seat     string `json:"seat"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Lab      string `json:"lab"`
}

type modelOption struct {
	ID    string `json:"id"`
	Price price  `json:"price"`
}

// options is what the GUI draws its roster form from.
type options struct {
	Seats    []seatOption             `json:"seats"`
	Models   map[string][]modelOption `json:"models"`
	Efforts  map[string][]string      `json:"efforts"`
	MinSeats int                      `json:"minSeats"`
}

// options lists the council's seats, the priced models of each lab and the
// efforts each lab takes.
func (cfg config) options() options {
	o := options{Models: map[string][]modelOption{}, Efforts: efforts, MinSeats: minSeats}
	for _, s := range builtin().Rosters["minds"] {
		o.Seats = append(o.Seats, seatOption{Seat: s.Seat, Name: s.Name, Provider: s.Provider, Lab: minds.Lab(s.Provider)})
	}
	for id, p := range cfg.Prices {
		o.Models[p.Provider] = append(o.Models[p.Provider], modelOption{ID: id, Price: p})
	}
	for _, list := range o.Models {
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	}
	return o
}

// checkRoster checks a roster to save: the council's seats, each once, each
// with a priced model of its own lab and an effort that lab takes, two on at
// least. It returns the roster with each seat's name, lab and search taken
// from the council's, so only the switch, the model and the effort change.
func checkRoster(in []minds.Spec, cfg config) ([]minds.Spec, error) {
	fixed := builtin().Rosters["minds"]
	given := map[string]minds.Spec{}
	for _, s := range in {
		if _, twice := given[s.Seat]; twice {
			return nil, fmt.Errorf("seat %s is in the roster twice", s.Seat)
		}
		given[s.Seat] = s
	}
	out := make([]minds.Spec, 0, len(fixed))
	on := 0
	for _, f := range fixed {
		s, ok := given[f.Seat]
		if !ok {
			return nil, fmt.Errorf("the roster has no seat %s", f.Seat)
		}
		delete(given, f.Seat)
		if p, ok := cfg.Prices[s.Model]; !ok || p.Provider != f.Provider {
			return nil, fmt.Errorf("seat %s: %s is not a priced %s model", f.Seat, s.Model, minds.Lab(f.Provider))
		}
		if !slices.Contains(efforts[f.Provider], s.Effort) {
			return nil, fmt.Errorf("seat %s: %s takes the efforts %s", f.Seat, minds.Lab(f.Provider), strings.Join(efforts[f.Provider], ", "))
		}
		r := f
		r.Model, r.Effort, r.Off = s.Model, s.Effort, s.Off
		if !r.Off {
			on++
		}
		out = append(out, r)
	}
	for seat := range given {
		return nil, fmt.Errorf("%s is not one of the council's seats", seat)
	}
	if on < minSeats {
		return nil, fmt.Errorf("a council needs %d seats on at least", minSeats)
	}
	return out, nil
}

// saveRoster saves the default roster into ~/.council/council.json, keeping
// the user's other settings as they are.
func saveRoster(roster []minds.Spec) error {
	path := filepath.Join(home(), "council.json")
	user := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &user); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	rosters := map[string]json.RawMessage{}
	if raw, ok := user["rosters"]; ok {
		if err := json.Unmarshal(raw, &rosters); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	var err error
	if rosters["minds"], err = json.Marshal(roster); err != nil {
		return err
	}
	if user["rosters"], err = json.Marshal(rosters); err != nil {
		return err
	}
	b, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'))
}

// writePrivate replaces the file at path in one step, readable by its owner
// only, in a folder only the owner can enter.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// providers are the keys a full council needs, in the order they show.
var providers = []string{"anthropic", "gemini", "openai", "minimax"}

// keyStore is where keys set and keys import write: one NAME=value line per
// key, under the names the config gives them.
func keyStore() string { return filepath.Join(home(), "keys.env") }

// readEnv reads the NAME=value lines of an env file; a missing file is empty.
func readEnv(path string) map[string]string {
	vars := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return vars
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if ok && !strings.HasPrefix(name, "#") {
			vars[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return vars
}

// loadKeys returns each provider's key and where it came from. The
// environment wins over the key store, which wins over the config's envFile.
// Values are never printed.
func loadKeys(cfg config) (keys, from map[string]string) {
	keys, from = map[string]string{}, map[string]string{}
	for _, src := range []struct{ path, label string }{{expand(cfg.EnvFile), "envFile"}, {keyStore(), "keys.env"}} {
		if src.path == "" {
			continue
		}
		vars := readEnv(src.path)
		for provider, name := range cfg.Keys {
			if v := vars[name]; v != "" {
				keys[provider], from[provider] = v, src.label
			}
		}
	}
	for provider, name := range cfg.Keys {
		if v := os.Getenv(name); v != "" {
			keys[provider], from[provider] = v, "environment"
		}
	}
	return keys, from
}

// setKeys writes keys into the key store, keeping its other lines.
func setKeys(values map[string]string, cfg config) error {
	path := keyStore()
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, provider := range providers {
		value, ok := values[provider]
		if !ok {
			continue
		}
		name := cfg.Keys[provider]
		if name == "" {
			return fmt.Errorf("the config names no variable for the %s key", provider)
		}
		if value == "" || strings.ContainsAny(value, "\r\n\x00 \t") {
			return fmt.Errorf("the %s key is empty or holds spaces or line breaks", provider)
		}
		line, found := name+"="+value, false
		for i, l := range lines {
			if n, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(l), "export "), "="); ok && strings.TrimSpace(n) == name {
				lines[i], found = line, true
			}
		}
		if !found {
			lines = append(lines, line)
		}
	}
	return writePrivate(path, []byte(strings.TrimLeft(strings.Join(lines, "\n"), "\n")+"\n"))
}

// importKeys copies into the key store the keys an env file holds, under the
// names the config gives them, and returns which it copied.
func importKeys(file string, cfg config) ([]string, error) {
	if _, err := os.Stat(file); err != nil {
		return nil, err
	}
	vars := readEnv(file)
	values := map[string]string{}
	var imported []string
	for _, provider := range providers {
		if v := vars[cfg.Keys[provider]]; v != "" {
			values[provider] = v
			imported = append(imported, provider)
		}
	}
	if len(imported) == 0 {
		return nil, fmt.Errorf("%s holds none of the council's keys", file)
	}
	return imported, setKeys(values, cfg)
}

// keyState is one key as the GUI sees it: set or not, and from where.
type keyState struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Set      bool   `json:"set"`
	From     string `json:"from,omitempty"`
}

func keyStatus(cfg config) []keyState {
	keys, from := loadKeys(cfg)
	var out []keyState
	for _, p := range providers {
		out = append(out, keyState{Provider: p, Name: cfg.Keys[p], Set: keys[p] != "", From: from[p]})
	}
	return out
}

// checkBase is where each provider's models listing lives; tests point it at
// a fake server.
var checkBase = map[string]string{
	"anthropic": "https://api.anthropic.com",
	"gemini":    "https://generativelanguage.googleapis.com",
	"openai":    "https://api.openai.com",
}

// keyCheck is one key tried: "ok", "missing", "refused (401)" or the error.
type keyCheck struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
}

// checkKeys tries each key with a call that costs nothing: its provider's
// models listing. MiniMax has none, so search runs one search through the
// MCP server instead.
func checkKeys(ctx context.Context, keys map[string]string, search func(context.Context) error) []keyCheck {
	client := &http.Client{Timeout: 20 * time.Second}
	var out []keyCheck
	for _, p := range providers {
		key := keys[p]
		status := "ok"
		switch {
		case key == "":
			status = "missing"
		case p == "minimax":
			if search == nil {
				status = "error: no search server in the config"
			} else if err := search(ctx); err != nil {
				// The server's own words, without the key, should it echo it.
				status = "refused: " + cut(strings.ReplaceAll(err.Error(), key, "[key]"), 200)
			}
		default:
			status = listModels(ctx, client, p, key)
		}
		out = append(out, keyCheck{Provider: p, Status: status})
	}
	return out
}

func listModels(ctx context.Context, client *http.Client, provider, key string) string {
	url := map[string]string{
		"anthropic": "/v1/models?limit=1",
		"gemini":    "/v1beta/models?pageSize=1",
		"openai":    "/v1/models",
	}[provider]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkBase[provider]+url, nil)
	if err != nil {
		return "error: " + err.Error()
	}
	switch provider {
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "gemini":
		req.Header.Set("x-goog-api-key", key)
	case "openai":
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		return "ok"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest:
		return fmt.Sprintf("refused (%d)", resp.StatusCode)
	}
	return fmt.Sprintf("error: HTTP %d", resp.StatusCode)
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

const subcommandHelp = `council keys                     which keys are set, and from where (JSON, never a value)
council keys set <provider>      save a key from standard input into ~/.council/keys.env
council keys import <env file>   copy the council's keys from an env file
council keys check               try each key with a call that costs nothing (JSON)
council options                  the seats, priced models and efforts (JSON)
council roster                   the default roster (JSON)
council roster set               save a default roster from standard input (JSON)`

// subcommand runs the council's setup commands, which the GUI calls too.
func subcommand(name string, args []string, in io.Reader, out io.Writer) error {
	cfg, err := loadConfig("")
	if err != nil {
		return err
	}
	show := func(v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err == nil {
			_, err = fmt.Fprintf(out, "%s\n", b)
		}
		return err
	}
	switch {
	case name == "keys" && len(args) == 0:
		return show(keyStatus(cfg))
	case name == "keys" && len(args) == 2 && args[0] == "set":
		if !slices.Contains(providers, args[1]) {
			return fmt.Errorf("no provider %s: the providers are %s", args[1], strings.Join(providers, ", "))
		}
		value, err := io.ReadAll(io.LimitReader(in, 4096))
		if err != nil {
			return err
		}
		return setKeys(map[string]string{args[1]: string(bytes.TrimSpace(value))}, cfg)
	case name == "keys" && len(args) == 2 && args[0] == "import":
		imported, err := importKeys(expand(args[1]), cfg)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "imported into %s: %s\n", keyStore(), strings.Join(imported, ", "))
		return err
	case name == "keys" && len(args) == 1 && args[0] == "check":
		keys, _ := loadKeys(cfg)
		var search func(context.Context) error
		if s := cfg.searcher(keys); s != nil {
			defer s.Close()
			search = func(ctx context.Context) error {
				_, err := s.Search(ctx, "Go programming language release notes")
				return err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return show(checkKeys(ctx, keys, search))
	case name == "options" && len(args) == 0:
		return show(cfg.options())
	case name == "roster" && len(args) == 0:
		return show(cfg.Rosters["minds"])
	case name == "roster" && len(args) == 1 && args[0] == "set":
		var roster []minds.Spec
		if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&roster); err != nil {
			return fmt.Errorf("the roster is not a JSON list of seats: %w", err)
		}
		checked, err := checkRoster(roster, cfg)
		if err != nil {
			return err
		}
		return saveRoster(checked)
	}
	return errors.New("usage:\n" + subcommandHelp)
}
