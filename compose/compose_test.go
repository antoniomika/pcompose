package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestFiles(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{name: "none"},
		{name: "default", files: map[string]string{"compose.yaml": ""}, want: []string{"compose.yaml"}},
		{
			name:  "first base and override, all pcompose files",
			files: map[string]string{"compose.yml": "", "docker-compose.yml": "", "docker-compose.override.yml": "", "pcompose.yml": "", "compose.pcompose.yaml": ""},
			want:  []string{"compose.yml", "docker-compose.override.yml", "compose.pcompose.yaml", "pcompose.yml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if got := Files(dir); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Files() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDotEnv(t *testing.T) {
	tests := []struct {
		dotEnv           string
		fromEnv, nameSet bool
	}{
		{dotEnv: "A=b\n"},
		{dotEnv: "#COMPOSE_FILE=x\nMY_COMPOSE_FILE=x\nCOMPOSE_FILES=x\n"},
		{dotEnv: "COMPOSE_FILE=x # comment\n", fromEnv: true},
		{dotEnv: "A=b\n  export COMPOSE_FILE = ${DIR}/x\r\n", fromEnv: true},
		{dotEnv: "COMPOSE_PROJECT_NAME='x'\n", nameSet: true},
	}

	for _, tt := range tests {
		dir := t.TempDir()
		for name, content := range map[string]string{".env": tt.dotEnv, "compose.yaml": ""} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		// Compose reads what .env sets itself, so it's given nothing over it.
		d := New("team/app", dir)
		if fromEnv := len(d.files) == 0; fromEnv != tt.fromEnv || !d.HasFiles() {
			t.Errorf("files with %q = %v, %v, want compose's: %v", tt.dotEnv, d.files, d.HasFiles(), tt.fromEnv)
		}
		if nameSet := len(d.env) == 0; nameSet != tt.nameSet {
			t.Errorf("env with %q = %v, want none: %v", tt.dotEnv, d.env, tt.nameSet)
		}
	}
}

func TestNestedNames(t *testing.T) {
	for name, want := range map[string]string{
		"team/app":     "team_app",
		"Team/my.App":  "team_myapp",
		"_team/app":    "team_app",
		"a/b-c/d_e":    "a_b-c_d_e",
		"team/1.2.3-x": "team_123-x",
	} {
		if got := Env(name, t.TempDir()); len(got) != 1 || got[0] != "COMPOSE_PROJECT_NAME="+want {
			t.Errorf("Env(%s) = %v, want %s", name, got, want)
		}
	}

	if got := Env("app", t.TempDir()); got != nil {
		t.Errorf("Env(app) = %v, want none", got)
	}
}

func TestDefaultRestart(t *testing.T) {
	p, err := parseProject(`{"name": "app", "services": {
		"web": {"deploy": {"replicas": 2}},
		"api": {},
		"db": {"restart": "always"},
		"once": {"restart": "no"},
		"job": {"deploy": {"restart_policy": {"condition": "none"}}}
	}}`)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := p.defaultRestart(), []string{"api", "web"}; !slices.Equal(got, want) {
		t.Errorf("defaultRestart() = %v, want %v", got, want)
	}
}
