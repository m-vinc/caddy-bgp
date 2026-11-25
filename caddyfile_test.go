package caddybgp

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
)

type adaptedConfig struct {
	Apps map[string]json.RawMessage `json:"apps"`
}

func adapt(t *testing.T, source string) (json.RawMessage, error) {
	t.Helper()

	data, _, err := caddyconfig.GetAdapter("caddyfile").Adapt([]byte(source), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		return nil, err
	}

	var config adaptedConfig
	must(t, json.Unmarshal(data, &config))
	return config.Apps["bgp"], nil
}

func decode(t *testing.T, data []byte) any {
	t.Helper()

	var value any
	must(t, json.Unmarshal(data, &value))
	return value
}

func TestCaddyfileAdaptsToJSON(t *testing.T) {
	source, err := os.ReadFile("testdata/full.caddyfile")
	must(t, err)
	want, err := os.ReadFile("testdata/full.json")
	must(t, err)
	got, err := adapt(t, string(source))
	must(t, err)
	if !reflect.DeepEqual(decode(t, got), decode(t, want)) {
		t.Fatalf("got %s", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	want, err := os.ReadFile("testdata/full.json")
	must(t, err)

	var app App
	must(t, caddy.StrictUnmarshalJSON(want, &app))
	again, err := json.Marshal(app)
	must(t, err)
	if !reflect.DeepEqual(decode(t, again), decode(t, want)) {
		t.Fatalf("got %s", again)
	}
}

func TestJSONRejectsUnknownFields(t *testing.T) {
	var app App
	if err := caddy.StrictUnmarshalJSON([]byte(`{"local_as": 65000, "nonsense": 1}`), &app); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestFullConfigurationValidates(t *testing.T) {
	t.Setenv("BGP_ROUTER_ID", "192.0.2.10")
	t.Setenv("BGP_PASSWORD", "secret")
	source, err := os.ReadFile("testdata/full.caddyfile")
	must(t, err)
	data, _, err := caddyconfig.GetAdapter("caddyfile").Adapt(source, nil)
	must(t, err)

	var config caddy.Config
	must(t, json.Unmarshal(data, &config))
	must(t, caddy.Validate(&config))
	if sharedRuntime() != nil {
		t.Error("validating left a runtime behind")
	}
}

type badCaddyfile struct {
	body string
	want string
}

func TestCaddyfileErrors(t *testing.T) {
	cases := map[string]badCaddyfile{
		"unknown option":        {"nonsense 1", "unknown bgp option"},
		"missing argument":      {"local_as", "wrong argument count"},
		"unknown peer option":   {"peer 192.0.2.1 {\n nonsense\n}", "unknown peer option"},
		"peer without address":  {"peer {\n remote_as 1\n}", "wrong argument count"},
		"bad duration":          {"drain_delay soon", "not a duration"},
		"bad timers":            {"peer 192.0.2.1 {\n timers 3s\n}", "wrong argument count"},
		"bad med":               {"announce a 198.51.100.1/32 {\n med many\n}", "med:"},
		"unknown announcement":  {"announce a 198.51.100.1/32 {\n nonsense\n}", "unknown announce option"},
		"unknown probe":         {"announce a 198.51.100.1/32 {\n check nonsense\n}", "bgp.probes.nonsense"},
		"check without name":    {"announce a 198.51.100.1/32 {\n check\n}", "wrong argument count"},
		"unknown http option":   {"announce a 198.51.100.1/32 {\n check http http://127.0.0.1 {\n nonsense 1\n}\n}", "unknown http check option"},
		"http without url":      {"announce a 198.51.100.1/32 {\n check http\n}", "wrong argument count"},
		"server option":         {"announce a 198.51.100.1/32 {\n check server s {\n nonsense\n}\n}", "unknown server check option"},
		"tcp option":            {"announce a 198.51.100.1/32 {\n check tcp 192.0.2.1:80 {\n nonsense\n}\n}", "unknown tcp check option"},
		"icmp without address":  {"announce a 198.51.100.1/32 {\n check icmp\n}", "wrong argument count"},
		"negative rise":         {"announce a 198.51.100.1/32 {\n check server s {\n rise -1\n}\n}", "not a positive number"},
		"flag with an argument": {"announce a 198.51.100.1/32 {\n always yes\n}", "wrong argument count"},
	}
	for name, item := range cases {
		_, err := adapt(t, "{\n bgp {\n"+item.body+"\n}\n}\n")
		if err == nil || !strings.Contains(err.Error(), item.want) {
			t.Errorf("%s: got %v, want %q", name, err, item.want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
