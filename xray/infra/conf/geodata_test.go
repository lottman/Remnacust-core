package conf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/app/geodata"
	. "github.com/xtls/xray-core/infra/conf"
)

func prepareGeodataAssets(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("xray.location.asset", directory)
}

func TestGeodataConfig(t *testing.T) {
	prepareGeodataAssets(t)

	creator := func() Buildable {
		return new(GeodataConfig)
	}

	runMultiTestCase(t, []TestCase{
		{
			Input: `{
				"cron": "0 4 * * *",
				"outbound": "proxy",
				"assets": [
					{"url": "https://example.com/geoip.dat", "file": "geoip.dat"},
					{"url": "https://example.com/geosite.dat", "file": "geosite.dat"}
				]
			}`,
			Parser: loadJSON(creator),
			Output: &geodata.Config{
				Cron:     "0 4 * * *",
				Outbound: "proxy",
				Assets: []*geodata.Asset{
					{Url: "https://example.com/geoip.dat", File: "geoip.dat"},
					{Url: "https://example.com/geosite.dat", File: "geosite.dat"},
				},
			},
		},
	})
}

func TestGeodataAssetConfig(t *testing.T) {
	prepareGeodataAssets(t)

	if _, err := (&GeodataAssetConfig{
		URL:  "https://example.com/geoip.dat",
		File: "geoip.dat",
	}).Build(); err != nil {
		t.Fatal(err)
	}

	if _, err := (&GeodataAssetConfig{
		URL:  "https://example.com/geoip.dat",
		File: "missing.dat",
	}).Build(); err == nil {
		t.Fatal("expected error")
	}
}

func TestGeodataAssetConfigInvalidURL(t *testing.T) {
	prepareGeodataAssets(t)

	for _, rawURL := range []string{
		"",
		"http://example.com/geoip.dat",
		"ftp://example.com/geoip.dat",
		"https:///geoip.dat",
	} {
		if _, err := (&GeodataAssetConfig{
			URL:  rawURL,
			File: "geoip.dat",
		}).Build(); err == nil {
			t.Fatalf("expected error for %q", rawURL)
		}
	}
}
