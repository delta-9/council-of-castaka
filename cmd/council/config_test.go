package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/delta-9/council-of-castaka/internal/minds"
)

// noKeysInEnv clears the key variables a test reads, so the keys of the
// person running it never reach the test, nor its messages.
func noKeysInEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range append(names, "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "OPENAI_API_KEY", "MINIMAX_API_KEY") {
		t.Setenv(name, "")
	}
}

// sources says, for a test's messages, where each key came from, never what
// it is.
func sources(keys, from map[string]string) string {
	var b strings.Builder
	for _, p := range providers {
		fmt.Fprintf(&b, "%s: set %v from %q; ", p, keys[p] != "", from[p])
	}
	return b.String()
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBuiltinIsGeneric checks that the config every install starts from holds
// nothing of one person's: no key file, the usual key names, the council's
// folder, a roster at high effort, and only models it can price for /cost.
func TestBuiltinIsGeneric(t *testing.T) {
	cfg := builtin()
	if cfg.EnvFile != "" || cfg.Sessions != "~/.council/sessions" {
		t.Errorf("envFile %q and sessions %q, want none and ~/.council/sessions", cfg.EnvFile, cfg.Sessions)
	}
	for provider, name := range map[string]string{"anthropic": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY", "openai": "OPENAI_API_KEY", "minimax": "MINIMAX_API_KEY"} {
		if cfg.Keys[provider] != name {
			t.Errorf("the %s key is read from %q, want %q", provider, cfg.Keys[provider], name)
		}
	}
	for name, roster := range cfg.Rosters {
		for _, s := range roster {
			if p, ok := cfg.Prices[s.Model]; !ok || p.Provider != s.Provider {
				t.Errorf("roster %s, seat %s: %s is not a priced %s model", name, s.Seat, s.Model, s.Provider)
			}
			if name == "minds" && (s.Effort != "high" || s.MaxTokens != 0 || s.Name == "") {
				t.Errorf("seat %+v: the built-in roster is named seats at high effort, budgets following the effort", s)
			}
		}
	}
	if len(cfg.Rosters["minds"]) != 5 {
		t.Errorf("the built-in roster has %d seats, want the five", len(cfg.Rosters["minds"]))
	}
}

// TestConfigLayers checks the order: the built-in config, then the user's
// ~/.council/council.json, then the file -config names. A user's roster
// replaces the built-in one of the same name and keeps the others; prices
// merge; with no other file, the built-in config is what runs.
func TestConfigLayers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".council", "council.json"), `{
		"rosters": {"minds": [{"seat": "opus", "name": "Morkaï", "provider": "anthropic", "model": "claude-fable-5-2", "effort": "max"}]},
		"prices": {"x-model": {"provider": "openai", "in": 1, "out": 2}}}`)
	extra := filepath.Join(t.TempDir(), "extra.json")
	writeFile(t, extra, `{"rounds": 5}`)
	cfg, err := loadConfig(extra)
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Rosters["minds"]; len(r) != 1 || r[0].Model != "claude-fable-5-2" {
		t.Errorf("minds roster %+v, want the user's", r)
	}
	if len(cfg.Rosters["rehearsal"]) == 0 {
		t.Error("the built-in rehearsal roster was lost")
	}
	if _, ok := cfg.Prices["x-model"]; !ok || cfg.Prices["claude-opus-5-5"].Out == 0 {
		t.Error("prices did not merge")
	}
	if cfg.Rounds != 5 || cfg.Words != builtin().Words {
		t.Errorf("rounds %d and words %d, want -config's rounds and the built-in words", cfg.Rounds, cfg.Words)
	}

	t.Setenv("HOME", t.TempDir())
	plain, err := loadConfig("")
	if err != nil || !reflect.DeepEqual(plain, builtin()) {
		t.Errorf("with no file, the config is not the built-in one: %v", err)
	}
}

// TestBudgetFollowsEffort checks the output budgets: thinking and the answer
// share them, so they grow with the effort, and more on the Anthropic seats,
// whose requests stream. A seat's own maxTokens wins.
func TestBudgetFollowsEffort(t *testing.T) {
	cases := []struct {
		provider, effort string
		own, want        int64
	}{
		{"anthropic", "low", 0, 16000},
		{"anthropic", "high", 0, 16000},
		{"anthropic", "xhigh", 0, 64000},
		{"anthropic", "max", 0, 64000},
		{"gemini", "medium", 0, 16000},
		{"gemini", "high", 0, 32768},
		{"gemini", "xhigh", 0, 32768},
		{"openai", "high", 0, 32768},
		{"openai", "max", 0, 32768},
		{"openai", "max", 8000, 8000},
	}
	for _, c := range cases {
		spec := minds.Spec{Provider: c.provider, Effort: c.effort, MaxTokens: c.own}
		if got := budget(spec, 16000); got != c.want {
			t.Errorf("%s at %s (own %d): budget %d, want %d", c.provider, c.effort, c.own, got, c.want)
		}
	}
}

// TestRosterSet checks what the GUI may save: the council's five seats, each
// once, a model of its own lab that /cost can price, an effort that lab
// takes, and two seats on at least. A seat's name, lab and search stay the
// council's, and the user's other settings survive the save.
func TestRosterSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".council", "council.json"), `{"keys": {"anthropic": "MY_ANTHROPIC"}}`)
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	roster := builtin().Rosters["minds"]

	edited := slices.Clone(roster)
	edited[0].Effort = "max"
	edited[1].Model, edited[1].Name, edited[1].Search = "claude-opus-5-5", "Someone else", "native"
	edited[2].Off, edited[3].Off, edited[4].Off = true, true, true
	got, err := checkRoster(edited, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Name != roster[1].Name || got[1].Search != roster[1].Search {
		t.Errorf("seat %+v: its name and search must stay the council's", got[1])
	}
	if err := saveRoster(got); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".council", "council.json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("council.json: %v, mode %v, want 0600", err, info.Mode().Perm())
	}
	cfg, err = loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Rosters["minds"]; r[0].Effort != "max" || r[1].Model != "claude-opus-5-5" || !r[4].Off || cfg.Keys["anthropic"] != "MY_ANTHROPIC" {
		t.Errorf("saved roster %+v, keys %v: the roster or the other settings were lost", r, cfg.Keys)
	}

	bad := map[string]func([]minds.Spec) []minds.Spec{
		"one seat on": func(r []minds.Spec) []minds.Spec {
			for i := 1; i < len(r); i++ {
				r[i].Off = true
			}
			return r
		},
		"a model of another lab":           func(r []minds.Spec) []minds.Spec { r[0].Model = "gpt-6-astra"; return r },
		"a model with no price":            func(r []minds.Spec) []minds.Spec { r[0].Model = "claude-unknown"; return r },
		"an effort its lab does not take":  func(r []minds.Spec) []minds.Spec { r[2].Effort = "max"; return r },
		"a seat missing":                   func(r []minds.Spec) []minds.Spec { return r[:4] },
		"a seat twice":                     func(r []minds.Spec) []minds.Spec { return append(r[:4], r[0]) },
		"a seat that is not the council's": func(r []minds.Spec) []minds.Spec { r[0].Seat = "sonnet"; return r },
	}
	for name, edit := range bad {
		if _, err := checkRoster(edit(slices.Clone(roster)), cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestSessionLeavesOffSeats checks that a seat switched off is not in the
// session at all, its charter's roster included, and that a council of one
// does not start.
func TestSessionLeavesOffSeats(t *testing.T) {
	cfg := builtin()
	roster := slices.Clone(cfg.Rosters["minds"])
	roster[4].Off = true
	cfg.Rosters = map[string][]minds.Spec{"minds": roster}
	s, err := openSession(t.TempDir(), cfg, "minds", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Roster) != 4 || slices.ContainsFunc(s.Roster, func(m minds.Spec) bool { return m.Seat == roster[4].Seat }) {
		t.Errorf("session roster %+v, want the four seats on", s.Roster)
	}
	if strings.Contains(s.Systems["opus"], roster[4].Name) {
		t.Errorf("the charter names the seat that is off: %s", s.Systems["opus"])
	}
	for i := 1; i < 4; i++ {
		roster[i].Off = true
	}
	if _, err := openSession(t.TempDir(), cfg, "minds", nil, false); err == nil {
		t.Error("a council of one seat started")
	}
}

// TestKeysStore checks where the keys come from (the environment, then
// ~/.council/keys.env, then the config's envFile), that setting and importing
// write the store for its owner only, and that no status shows a value.
func TestKeysStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	noKeysInEnv(t, "OLD_ANTHROPIC")
	legacy := filepath.Join(t.TempDir(), "legacy.env")
	writeFile(t, legacy, "export OLD_ANTHROPIC=from-legacy\nGEMINI_API_KEY=\"gem-legacy\"\nOTHER=unrelated\n")
	writeFile(t, filepath.Join(home, ".council", "council.json"), `{"envFile": "`+legacy+`", "keys": {"anthropic": "OLD_ANTHROPIC"}}`)
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	keys, from := loadKeys(cfg)
	if keys["anthropic"] != "from-legacy" || from["gemini"] != "envFile" || keys["openai"] != "" {
		t.Errorf("keys from the envFile: %s", sources(keys, from))
	}

	if err := setKeys(map[string]string{"anthropic": "sk-store"}, cfg); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(home, ".council", "keys.env")
	if info, err := os.Stat(store); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("keys.env: %v, want mode 0600", err)
	}
	if keys, from = loadKeys(cfg); keys["anthropic"] != "sk-store" || from["anthropic"] != "keys.env" {
		t.Errorf("the store must win over the envFile: %s", sources(keys, from))
	}
	t.Setenv("OLD_ANTHROPIC", "sk-env")
	if keys, from = loadKeys(cfg); keys["anthropic"] != "sk-env" || from["anthropic"] != "environment" {
		t.Errorf("the environment must win over the store: %s", sources(keys, from))
	}

	imported, err := importKeys(legacy, cfg)
	if err != nil || !slices.Equal(imported, []string{"anthropic", "gemini"}) {
		t.Fatalf("import: %v, %v, want anthropic and gemini", imported, err)
	}
	if b, _ := os.ReadFile(store); !strings.Contains(string(b), "GEMINI_API_KEY=gem-legacy\n") || strings.Contains(string(b), "unrelated") {
		t.Errorf("the store after the import lacks the imported Gemini key, or holds an unrelated variable")
	}

	status, err := json.Marshal(keyStatus(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-", "legacy", "gem-"} {
		if strings.Contains(string(status), secret) {
			t.Errorf("the key status shows a value: %s", status)
		}
	}
	var states []keyState
	_ = json.Unmarshal(status, &states)
	if len(states) != 4 || states[0].Provider != "anthropic" || !states[0].Set || states[2].Set {
		t.Errorf("status %s, want the four providers, anthropic set and openai not", status)
	}
}

// TestKeysCheck checks that each key is tried with one models listing, in the
// header its provider expects, and that a refusal and a missing key say so.
func TestKeysCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good := r.Header.Get("x-api-key") == "good" || r.Header.Get("x-goog-api-key") == "good" || r.Header.Get("Authorization") == "Bearer good"
		if !good || !strings.Contains(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()
	saved := checkBase
	t.Cleanup(func() { checkBase = saved })
	checkBase = map[string]string{"anthropic": srv.URL, "gemini": srv.URL, "openai": srv.URL}

	searched := false
	got := checkKeys(context.Background(), map[string]string{"anthropic": "good", "gemini": "bad", "minimax": "m"},
		func(context.Context) error { searched = true; return nil })
	want := map[string]string{"anthropic": "ok", "gemini": "refused", "openai": "missing", "minimax": "ok"}
	for _, c := range got {
		if !strings.HasPrefix(c.Status, want[c.Provider]) {
			t.Errorf("%s: %q, want %s", c.Provider, c.Status, want[c.Provider])
		}
	}
	if len(got) != 4 || !searched {
		t.Errorf("checks %+v, want the four, MiniMax through one search", got)
	}
}
