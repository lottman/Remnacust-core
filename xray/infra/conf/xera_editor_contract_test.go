package conf_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xtls/xray-core/infra/conf"
)

func editorContractFixturePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("REMNACUST_EDITOR_FIXTURES")
	if path == "" {
		path = filepath.Join("..", "..", "..", "..", "Remnacust-panel", "panel", "frontend", "tests", "xray-config-fixtures.json")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Skip("Remnacust-panel fixtures are unavailable; set REMNACUST_EDITOR_FIXTURES")
		}
	}
	return path
}

func TestXeraEditorContractFixtures(t *testing.T) {
	path := editorContractFixturePath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string][]struct {
		Name           string          `json:"name"`
		EditorContract bool            `json:"editorContract"`
		Config         json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for kind, cases := range fixtures {
		for _, fixture := range cases {
			if !fixture.EditorContract {
				continue
			}
			checked++
			t.Run(fixture.Name, func(t *testing.T) {
				var config conf.Config
				err := json.Unmarshal(fixture.Config, &config)
				if err == nil {
					_, err = config.Build()
				}
				if (err == nil) != (kind == "valid") {
					t.Fatalf("expected %s config; Build error: %v", kind, err)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("no editor contract fixtures were checked")
	}
}

func TestXeraEditorSchemaIncludesBuilderFields(t *testing.T) {
	frontend := filepath.Dir(filepath.Dir(editorContractFixturePath(t)))
	for _, filename := range []string{"xray.schema.json", "xray.schema.cn.json"} {
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(frontend, "public", "assets", filename))
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Definitions map[string]struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"definitions"`
			}
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				name  string
				value any
			}{
				{"RemnacustCoreXeraHTTPConfig", conf.XeraHTTPConfig{}},
				{"RemnacustCoreXeraDownlinkPaddingConfig", conf.XeraDownlinkPaddingConfig{}},
				{"RemnacustCoreXeraMuxConfig", conf.XeraMuxConfig{}},
			} {
				typeInfo := reflect.TypeOf(item.value)
				for index := range typeInfo.NumField() {
					field := strings.Split(typeInfo.Field(index).Tag.Get("json"), ",")[0]
					if field == "" || field == "-" {
						continue
					}
					if _, ok := schema.Definitions[item.name].Properties[field]; !ok {
						t.Errorf("missing %s.%s", item.name, field)
					}
				}
			}
			var h2 struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(schema.Definitions["RemnacustCoreXeraExtra"].Properties["h2Params"], &h2); err != nil {
				t.Fatal(err)
			}
			typeInfo := reflect.TypeOf(http.HTTP2Config{})
			for index := range typeInfo.NumField() {
				field := typeInfo.Field(index)
				if field.IsExported() {
					if _, ok := h2.Properties[field.Name]; !ok {
						t.Errorf("missing h2Params.%s", field.Name)
					}
				}
			}
		})
	}
}
